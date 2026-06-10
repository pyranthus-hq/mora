package imessage

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// realPreamble mirrors the typedstream bytes a modern macOS chat.db writes between
// the "NSString" class marker and the string's length-prefixed UTF-8 run:
//
//	01 9X 84 01 2b
//
// — a few class-version / object-reference bytes, then the 0x2b ('+') content marker
// that ALWAYS immediately precedes the length prefix (verified across the whole live
// chat.db corpus). The original synthetic blobs OMITTED this preamble and put the
// length prefix flush against "NSString", so they never exercised the marker-skip
// path — which is exactly why the received-message drop bug (every real
// attributedBody-only message decoded to "") shipped undetected. Every synthetic blob
// now carries it.
var realPreamble = []byte{0x01, 0x94, 0x84, 0x01, 0x2b}

// buildBlob builds a synthetic typedstream-shaped blob matching the REAL modern
// layout: the "NSString" marker, the class preamble (realPreamble, ending in the 0x2b
// content marker), the length prefix for payload, then the payload bytes. This is the
// layout decodeAttributedBody scans for. prefixOverride, when non-nil, replaces the
// computed length-prefix bytes (used to exercise truncated/malformed cases).
func buildBlob(payload []byte, prefixOverride []byte) []byte {
	var b []byte
	b = append(b, []byte("NSString")...)
	b = append(b, realPreamble...)
	if prefixOverride != nil {
		b = append(b, prefixOverride...)
	} else {
		n := len(payload)
		switch {
		case n < 0x80:
			b = append(b, byte(n))
		case n <= 0xFFFF:
			lp := make([]byte, 2)
			binary.LittleEndian.PutUint16(lp, uint16(n))
			b = append(b, 0x81, lp[0], lp[1])
		default:
			lp := make([]byte, 4)
			binary.LittleEndian.PutUint32(lp, uint32(n))
			b = append(b, 0x82, lp[0], lp[1], lp[2], lp[3])
		}
	}
	b = append(b, payload...)
	return b
}

func TestDecodeAttributedBody(t *testing.T) {
	t.Run("modern attributedBody run (NSString 01 94 84 01 2b ...) decodes non-empty — Phase 2.1 received-drop regression", func(t *testing.T) {
		// The exact layout a modern macOS chat.db writes for a message whose body
		// lives ONLY in attributedBody (text column NULL) — e.g. every received
		// message once macOS migrates it off the text column. The shipped decoder
		// treated the first post-marker byte (0x01) as the length prefix and never
		// reached the 0x2b content run, so it returned "" and the connector silently
		// dropped the message. Assert the real run decodes.
		want := "yeah 90 days should be fine"
		got := decodeAttributedBody(buildBlob([]byte(want), nil))
		if got != want {
			t.Fatalf("got %q, want %q — decoder must skip the class preamble to the 0x2b run", got, want)
		}
	})

	t.Run("short body (<128, single-byte length)", func(t *testing.T) {
		want := "hey are we still on for the demo?"
		got := decodeAttributedBody(buildBlob([]byte(want), nil))
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("0x81 length prefix at 128-255 boundary keeps the FINAL byte (#1455 advance-by-3)", func(t *testing.T) {
		// 200-char ASCII string -> length 200 needs the 0x81 2-byte prefix. The
		// #1455 bug advanced the cursor by 2 not 3, dropping the final byte and
		// yielding 199 chars. Assert we get the FULL 200.
		want := strings.Repeat("a", 199) + "Z" // distinct final char proves no drop
		got := decodeAttributedBody(buildBlob([]byte(want), nil))
		if got != want {
			t.Fatalf("len(got)=%d last=%q; len(want)=%d last=%q (advance-by-3 fix)",
				len(got), lastRune(got), len(want), "Z")
		}
		if !strings.HasSuffix(got, "Z") {
			t.Fatalf("final byte dropped: got suffix %q, want trailing Z", lastRune(got))
		}
	})

	t.Run("multi-byte length prefixes: long emoji body (0x81) and the 0x82 branch decode", func(t *testing.T) {
		// A >128-byte body of multi-byte runes forces the 0x81 (uint16) length path AND
		// ToValidUTF8 — the realistic long-emoji message, untested by the ASCII #1455
		// case. Real corpus never hits 0x82/0x83, so cover the 0x82 branch synthetically.
		long := strings.Repeat("café 🍣 ", 40) // 440 bytes > 128, multi-byte runes
		if got := decodeAttributedBody(buildBlob([]byte(long), nil)); got != long {
			t.Fatalf("long multi-byte body via the 0x81 prefix did not round-trip (len got=%d want=%d)", len(got), len(long))
		}
		lp82 := []byte{0x82, 0x05, 0x00, 0x00, 0x00} // 0x82 (uint32) length = 5
		if got := decodeAttributedBody(buildBlob([]byte("hello"), lp82)); got != "hello" {
			t.Fatalf("0x82 length-prefix decode: got %q, want %q", got, "hello")
		}
	})

	t.Run("emoji/CJK body stays valid UTF-8 with no mid-rune corruption", func(t *testing.T) {
		want := "你好 👋 sushi 🍣 — café"
		got := decodeAttributedBody(buildBlob([]byte(want), nil))
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("decoded body is not valid UTF-8: %q", got)
		}
	})

	t.Run("nil blob returns empty", func(t *testing.T) {
		if got := decodeAttributedBody(nil); got != "" {
			t.Fatalf("decodeAttributedBody(nil) = %q, want \"\"", got)
		}
	})

	t.Run("no NSString marker returns empty", func(t *testing.T) {
		if got := decodeAttributedBody([]byte("totally unrelated bytes here")); got != "" {
			t.Fatalf("got %q, want \"\" when no NSString marker", got)
		}
	})

	t.Run("truncated: length prefix claims more bytes than remain — clamped, no panic", func(t *testing.T) {
		// A 0x81 prefix claiming 5000 bytes but only ~10 payload bytes present.
		lp := make([]byte, 2)
		binary.LittleEndian.PutUint16(lp, 5000)
		blob := buildBlob([]byte("short tail"), []byte{0x81, lp[0], lp[1]})
		got := decodeAttributedBody(blob) // must not panic
		// Clamped to the remaining bytes — never reads out of bounds.
		if got != "short tail" {
			t.Fatalf("clamped decode = %q, want %q", got, "short tail")
		}
	})

	t.Run("malformed: length prefix bytes themselves run past the blob — empty, no panic", func(t *testing.T) {
		// "NSString" + class preamble + 0x2b content marker, then a lone 0x81 with no
		// following length bytes.
		blob := append([]byte("NSString"), 0x01, 0x94, 0x84, 0x01, 0x2b, 0x81)
		if got := decodeAttributedBody(blob); got != "" {
			t.Fatalf("got %q, want \"\" for a length prefix past the blob", got)
		}
	})

	t.Run("no 0x2b content marker after NSString — empty, no panic", func(t *testing.T) {
		// Class-chain bytes but no 0x2b run marker within the scan window: not a
		// decodable string run, so the decoder must return "" rather than mis-reading
		// a class-version byte as a length prefix (the shipped bug's failure mode).
		blob := append([]byte("NSString"), 0x01, 0x94, 0x84, 0x01, 0x99, 0x05, 0x41, 0x41)
		if got := decodeAttributedBody(blob); got != "" {
			t.Fatalf("got %q, want \"\" when no 0x2b content marker present", got)
		}
	})

	t.Run("marker at the very end of the blob — empty, no panic", func(t *testing.T) {
		if got := decodeAttributedBody([]byte("NSString")); got != "" {
			t.Fatalf("got %q, want \"\" when nothing follows the marker", got)
		}
	})

	t.Run("0x83 (uint64) huge length must not panic — overflow-safe clamp (T-02-DOS)", func(t *testing.T) {
		// A crafted 0x83 length prefix claiming 0x7FFFFFFFFFFFFFFF bytes. n stays a
		// large positive int, so the clamp guard must not be defeated by p+n
		// overflowing int64 to a negative value (which would skip the clamp and panic
		// on blob[p:p+n]). The decoder must clamp to the bytes that remain, never panic.
		huge := []byte{0x83, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f}
		got := decodeAttributedBody(buildBlob([]byte("tail"), huge)) // must not panic
		if got != "tail" {
			t.Fatalf("got %q, want %q (overflow-safe clamp to remaining bytes)", got, "tail")
		}
	})

	t.Run("inline-attachment placeholder U+FFFC is stripped from the decoded body", func(t *testing.T) {
		// Messages with inline attachments carry the Object Replacement Char (U+FFFC)
		// in the NSString run. It is not message text — the attachment is surfaced
		// separately via its own marker — so the decoder must strip it rather than leak
		// a "￼" glyph into the rendered transcript.
		if got := decodeAttributedBody(buildBlob([]byte("￼Looks like a good spot"), nil)); got != "Looks like a good spot" {
			t.Fatalf("got %q, want the caption without the U+FFFC placeholder", got)
		}
		// A pure-placeholder bubble (no caption) must decode to "" so it falls through
		// to its attachment marker instead of rendering a junk glyph-only line.
		if got := decodeAttributedBody(buildBlob([]byte("￼￼￼"), nil)); got != "" {
			t.Fatalf("got %q, want \"\" for a pure inline-attachment-placeholder message", got)
		}
	})
}

func lastRune(s string) string {
	if s == "" {
		return ""
	}
	r := []rune(s)
	return string(r[len(r)-1])
}

// realFixtureDir is the consented blob corpus; populated manually in an FDA-granted
// terminal (see fixtures/README.md). The real-fixture assertion is informational in
// default CI (skips when empty) so CI stays green on synthetic blobs.
func TestDecodeAttributedBodyRealFixtures(t *testing.T) {
	dir := "fixtures"
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no fixtures dir: %v", err)
	}
	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".bin") {
			continue
		}
		found++
		blob, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read fixture %s: %v", e.Name(), err)
		}
		got := decodeAttributedBody(blob)
		if got == "" {
			t.Errorf("fixture %s decoded to empty body", e.Name())
		}
		if !utf8.ValidString(got) {
			t.Errorf("fixture %s decoded to invalid UTF-8", e.Name())
		}
	}
	if found == 0 {
		t.Skip("no real .bin fixtures present — synthetic blobs carry the gate (see fixtures/README.md)")
	}
}
