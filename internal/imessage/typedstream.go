package imessage

import (
	"bytes"
	"encoding/binary"
	"strings"
)

// decodeAttributedBody extracts the plain message text from an Apple typedstream
// `attributedBody` BLOB. On modern macOS the `message.text` column is frequently
// NULL and the body lives only here (IMSG-02), so this is the connector's long pole.
//
// REAL LAYOUT (verified against the whole live chat.db corpus): the message text is
// the first NSString run in the archive, laid out as:
//
//	"NSString" 01 9X 84 01 2b <length-prefix> <utf-8 bytes>
//
// i.e. the "NSString" class marker, a few typedstream class-version / object-
// reference bytes, then a 0x2b ('+') content marker that ALWAYS immediately precedes
// the length prefix. We anchor on that 0x2b (scanning a small window so older-macOS
// preamble variants still resolve) and read the run that follows. The previous
// decoder mistook the first post-marker byte (0x01) for the length prefix, never
// reached the 0x2b run, and returned "" for every modern attributedBody-only message
// — silently dropping it (the Phase 2.1 received-message drop bug).
//
// LENGTH PREFIX ENCODING (the bug-prone part):
//   - prefix byte < 0x80: that single byte IS the length (int8); advance 1.
//   - 0x81: next 2 bytes are the length, little-endian; advance 3 (1 marker + 2 len).
//   - 0x82: next 4 bytes LE; advance 5.
//   - 0x83: next 8 bytes LE; advance 9.
//
// CRITICAL: for the 0x81 case the cursor advances by 3, not 2 — the documented
// #1455 bug advanced by one too few and dropped the final payload byte for lengths
// 128-255. That off-by-one is the load-bearing fix here.
//
// This is NOT a full NSKeyedArchiver/plist parse (typedstream is a different, older
// format). It is a bounds-checked NSString-run extractor: every read is guarded, a
// length prefix claiming more bytes than remain is clamped, and ANY anomaly returns
// "" rather than panicking (DoS mitigation, T-02-DOS). The result is run through
// strings.ToValidUTF8 so emoji/CJK bodies never carry mid-rune corruption, and the
// U+FFFC inline-attachment placeholder is stripped (the attachment is surfaced
// separately, so the bare glyph never leaks into the transcript).
func decodeAttributedBody(blob []byte) string {
	if len(blob) == 0 {
		return ""
	}
	const marker = "NSString"
	i := bytes.Index(blob, []byte(marker))
	if i < 0 {
		return ""
	}
	p := i + len(marker)

	// Anchor on the 0x2b ('+') content marker that precedes the string's length
	// prefix. In real archives it sits a few class-version / object-reference bytes
	// past "NSString" (e.g. 01 9X 84 01 2b); scan a bounded window for it rather than
	// hardcode the offset so preamble variants across macOS versions still resolve. No
	// 0x2b in the window ⇒ not a decodable string run ⇒ "".
	const maxMarkerScan = 16
	end := p + maxMarkerScan
	if end > len(blob) {
		end = len(blob)
	}
	for p < end && blob[p] != 0x2b {
		p++
	}
	if p >= end {
		return "" // no content marker found near the NSString class marker
	}
	p++ // step past the 0x2b content marker to the length prefix
	if p >= len(blob) {
		return ""
	}

	var n int
	switch {
	case blob[p] == 0x81:
		if p+3 > len(blob) {
			return "" // length bytes themselves run past the blob
		}
		n = int(binary.LittleEndian.Uint16(blob[p+1 : p+3]))
		p += 3 // advance past marker byte + 2 length bytes (the #1455 fix)
	case blob[p] == 0x82:
		if p+5 > len(blob) {
			return ""
		}
		n = int(binary.LittleEndian.Uint32(blob[p+1 : p+5]))
		p += 5
	case blob[p] == 0x83:
		if p+9 > len(blob) {
			return ""
		}
		n = int(binary.LittleEndian.Uint64(blob[p+1 : p+9]))
		p += 9
	case blob[p] < 0x80:
		n = int(blob[p])
		p++
	default:
		return ""
	}

	if n < 0 {
		return ""
	}
	// Clamp to the bytes that remain. Compare against len(blob)-p, NOT p+n: a 0x83
	// (uint64) prefix can make n large enough that p+n overflows int64 to a negative
	// value, which would defeat a `p+n > len(blob)` guard and panic on blob[p:p+n].
	// p <= len(blob) here, so len(blob)-p is a safe non-negative bound (T-02-DOS).
	if n > len(blob)-p {
		n = len(blob) - p
	}
	if n <= 0 {
		return ""
	}
	return stripAttachmentPlaceholder(strings.ToValidUTF8(string(blob[p:p+n]), ""))
}

// objectReplacementChar (U+FFFC) marks where an inline attachment sits inside a message
// body. macOS writes it into BOTH the text column and the attributedBody run, so it is
// stripped at every body source — the attachment is surfaced separately via its own
// marker, and leaving the glyph would leak a raw "￼" into the transcript.
const objectReplacementChar = "￼"

// stripAttachmentPlaceholder removes the U+FFFC inline-attachment placeholder so a
// caption keeps only its text and a bare-placeholder bubble collapses to "" (it then
// renders via its attachment marker instead of a junk glyph line).
func stripAttachmentPlaceholder(s string) string {
	return strings.ReplaceAll(s, objectReplacementChar, "")
}
