package imessage

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// update regenerates the golden files when set: `go test ./internal/imessage/ -update`.
var update = flag.Bool("update", false, "regenerate golden testdata files")

// goldenOrUpdate compares got against testdata/<name>.golden, writing the golden
// file when -update is passed (run: `go test ./internal/imessage/ -run TestRender -update`).
func goldenOrUpdate(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update to create): %v", path, err)
	}
	if got != string(want) {
		t.Fatalf("render mismatch for %s.\n--- got ---\n%s\n--- want ---\n%s", name, got, string(want))
	}
}

// localDate builds a time.Time at a fixed local clock time on the given Y-M-D in UTC
// (the renderer groups by the message's local calendar date; tests pin UTC so the
// golden day headers are deterministic regardless of the test host's zone).
func localDate(y int, m time.Month, d, hh, mm int) time.Time {
	return time.Date(y, m, d, hh, mm, 0, 0, time.UTC)
}

// resolver1to1 resolves the demo handle to a name; everything else falls back raw.
func resolver1to1() *Resolver {
	return newResolverFromMap(map[string]string{"+14155551234": "Neil Patel"})
}

// TestRenderTitle covers D-10: explicit group display name → verbatim; group without
// a name → synthesized from resolved participant names in order (unknown handles raw);
// 1:1 → the other participant's resolved name/handle.
func TestRenderTitle(t *testing.T) {
	r := resolver1to1()

	t.Run("group explicit name verbatim", func(t *testing.T) {
		c := conversation{
			displayName:  "Wink Launch",
			participants: []string{"+14155551234", "+19998887777"},
			isGroup:      true,
		}
		if got := renderTitle(c, r); got != "Wink Launch" {
			t.Fatalf("title = %q, want %q", got, "Wink Launch")
		}
	})

	t.Run("group synthesized from resolved participants, unknown raw", func(t *testing.T) {
		c := conversation{
			participants: []string{"+14155551234", "+19998887777"},
			isGroup:      true,
		}
		want := "Neil Patel, +19998887777"
		if got := renderTitle(c, r); got != want {
			t.Fatalf("title = %q, want %q", got, want)
		}
	})

	t.Run("1:1 other participant resolved", func(t *testing.T) {
		c := conversation{participants: []string{"+14155551234"}, identifier: "+14155551234"}
		if got := renderTitle(c, r); got != "Neil Patel" {
			t.Fatalf("title = %q, want %q", got, "Neil Patel")
		}
	})

	t.Run("1:1 unknown falls back to raw handle", func(t *testing.T) {
		c := conversation{participants: []string{"+19998887777"}, identifier: "+19998887777"}
		if got := renderTitle(c, r); got != "+19998887777" {
			t.Fatalf("title = %q, want %q", got, "+19998887777")
		}
	})
}

// TestAttachmentMarker covers the D-11 inline attachment marker formats (UI-SPEC
// Surface 5): image, generic-with-type (` · ` separator), filename-only, mime-only —
// metadata only, never bytes/paths.
func TestAttachmentMarker(t *testing.T) {
	cases := []struct {
		name string
		att  Attachment
		want string
	}{
		{"image by mime", Attachment{Filename: "IMG_2031.HEIC", MimeType: "image/heic"}, "[image: IMG_2031.HEIC]"},
		{"image by extension, no mime", Attachment{Filename: "photo.jpg"}, "[image: photo.jpg]"},
		{"generic known type", Attachment{Filename: "deck.pdf", MimeType: "application/pdf"}, "[attachment: deck.pdf · application/pdf]"},
		{"filename only", Attachment{Filename: "notes.txt"}, "[attachment: notes.txt]"},
		{"mime only, no filename", Attachment{MimeType: "image/heic"}, "[image: image/heic]"},
		{"neither", Attachment{}, "[attachment]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := attachmentMarker(c.att); got != c.want {
				t.Fatalf("attachmentMarker(%+v) = %q, want %q", c.att, got, c.want)
			}
		})
	}
}

// TestAttachmentRender proves a message with both text and an attachment renders the
// text line then the attachment marker as its OWN sender-prefixed line (UI-SPEC
// Surface 5 example), and an attachment-only message renders just the marker line —
// never byte size or a file path.
func TestAttachmentRender(t *testing.T) {
	r := resolver1to1()
	msgs := []renderMessage{
		{date: localDate(2026, 6, 1, 10, 0), fromMe: false, sender: "+14155551234", text: "here's the deck",
			attachments: []Attachment{{Filename: "deck.pdf", MimeType: "application/pdf", Size: 999999}}},
		{date: localDate(2026, 6, 1, 10, 1), fromMe: true, text: "🔥"},
		{date: localDate(2026, 6, 1, 10, 2), fromMe: false, sender: "+14155551234",
			attachments: []Attachment{{Filename: "IMG_2031.HEIC", MimeType: "image/heic"}}},
	}
	body, _ := renderBody(msgs, r, 0)
	goldenOrUpdate(t, "render_attachments", body)

	if strings.Contains(body, "999999") {
		t.Fatalf("rendered body leaked attachment byte size:\n%s", body)
	}
	want := "Neil Patel: here's the deck\nNeil Patel: [attachment: deck.pdf · application/pdf]"
	if !strings.Contains(body, want) {
		t.Fatalf("text+attachment should be two prefixed lines.\nwant substring:\n%s\nbody:\n%s", want, body)
	}
}

// TestSpecialMessages covers D-12: system events render as an italic line (no sender
// prefix), retracted renders as `[message removed]`, shared links render their URL
// inline, and a tapback/skip message produces NO output (not even a day header).
func TestSpecialMessages(t *testing.T) {
	r := resolver1to1()

	t.Run("system event italic, no prefix", func(t *testing.T) {
		msgs := []renderMessage{
			{date: localDate(2026, 5, 29, 9, 0), kind: msgSystem, text: `Neil named the conversation "Wink Launch"`},
			{date: localDate(2026, 5, 29, 9, 1), fromMe: true, text: "nice"},
		}
		body, _ := renderBody(msgs, r, 0)
		if !strings.Contains(body, "*Neil named the conversation \"Wink Launch\"*") {
			t.Fatalf("system event not italic:\n%s", body)
		}
	})

	t.Run("retracted message", func(t *testing.T) {
		msgs := []renderMessage{{date: localDate(2026, 5, 29, 9, 0), fromMe: false, sender: "+14155551234", kind: msgRetracted, text: "this should be hidden"}}
		body, _ := renderBody(msgs, r, 0)
		if !strings.Contains(body, "Neil Patel: [message removed]") {
			t.Fatalf("retracted not rendered as [message removed]:\n%s", body)
		}
		if strings.Contains(body, "this should be hidden") {
			t.Fatalf("retracted message leaked original text:\n%s", body)
		}
	})

	t.Run("shared link inline as URL", func(t *testing.T) {
		msgs := []renderMessage{{date: localDate(2026, 5, 29, 9, 0), fromMe: false, sender: "+14155551234", text: "https://wink.com/launch"}}
		body, _ := renderBody(msgs, r, 0)
		if !strings.Contains(body, "Neil Patel: https://wink.com/launch") {
			t.Fatalf("link not inline:\n%s", body)
		}
	})

	t.Run("skip/tapback produces no output and no empty day header", func(t *testing.T) {
		msgs := []renderMessage{{date: localDate(2026, 5, 29, 9, 0), fromMe: false, sender: "+14155551234", kind: msgSkip, text: "Loved an image"}}
		body, _ := renderBody(msgs, r, 0)
		if strings.TrimSpace(body) != "" {
			t.Fatalf("skip/tapback should produce empty body, got:\n%s", body)
		}
	})
}

// TestRender covers the day-grouped body contract (UI-SPEC Surface 5):
// (a) multi-day 1:1 with Me: + a resolved name; (b) group with an unknown raw handle;
// (c) a truncated conversation asserting the exact marker copy at the TOP and the
// NEWEST messages kept.
func TestRender(t *testing.T) {
	r := resolver1to1()

	t.Run("multi-day 1:1 Me and resolved name", func(t *testing.T) {
		msgs := []renderMessage{
			{date: localDate(2026, 5, 30, 9, 0), fromMe: false, sender: "+14155551234", text: "are we still on for the demo?"},
			{date: localDate(2026, 5, 30, 9, 1), fromMe: true, text: "yes, 3pm"},
			{date: localDate(2026, 5, 31, 8, 0), fromMe: false, sender: "+14155551234", text: "pushed to 4"},
			{date: localDate(2026, 5, 31, 8, 5), fromMe: true, text: "works for me"},
		}
		body, res := renderBody(msgs, r, 0)
		if res.Truncated {
			t.Fatalf("unbounded render should not truncate")
		}
		goldenOrUpdate(t, "render_1to1_multiday", body)
	})

	t.Run("group with unknown raw handle and multi-line text", func(t *testing.T) {
		msgs := []renderMessage{
			{date: localDate(2026, 6, 1, 10, 0), fromMe: false, sender: "+14155551234", text: "here's the plan"},
			{date: localDate(2026, 6, 1, 10, 1), fromMe: false, sender: "+19998887777", text: "line one\nline two"},
			{date: localDate(2026, 6, 1, 10, 2), fromMe: true, text: "🔥"},
		}
		body, _ := renderBody(msgs, r, 0)
		goldenOrUpdate(t, "render_group_unknown", body)
	})

	t.Run("truncated keeps newest with marker at TOP", func(t *testing.T) {
		// Three days; a small budget that forces dropping the oldest day(s).
		msgs := []renderMessage{
			{date: localDate(2026, 5, 28, 9, 0), fromMe: false, sender: "+14155551234", text: "OLDEST should be dropped from the body"},
			{date: localDate(2026, 5, 29, 9, 0), fromMe: true, text: "middle filler message of moderate length here"},
			{date: localDate(2026, 5, 30, 9, 0), fromMe: false, sender: "+14155551234", text: "NEWEST must remain in the body"},
		}
		// Budget chosen to fit only the newest day plus the marker.
		body, res := renderBody(msgs, r, 80)
		if !res.Truncated {
			t.Fatalf("expected truncation at budget 80, got Truncated=false (body len=%d)", len(body))
		}
		marker := "Truncated — showing the most recent messages of this conversation; older messages were dropped to fit the size budget."
		if !strings.Contains(body, marker) {
			t.Fatalf("missing exact truncation marker copy.\nbody:\n%s", body)
		}
		// Marker must appear ABOVE the first day header.
		mi := strings.Index(body, marker)
		hi := strings.Index(body, "## ")
		if mi < 0 || hi < 0 || mi > hi {
			t.Fatalf("marker must be ABOVE the first '## ' header (marker at %d, header at %d)\nbody:\n%s", mi, hi, body)
		}
		// NEWEST kept, OLDEST dropped.
		if !strings.Contains(body, "NEWEST must remain") {
			t.Fatalf("newest message was dropped — recency-first truncation violated\nbody:\n%s", body)
		}
		if strings.Contains(body, "OLDEST should be dropped") {
			t.Fatalf("oldest message survived — should have been dropped\nbody:\n%s", body)
		}
		// Marker is a blockquote.
		if !strings.HasPrefix(body, "> "+marker) {
			t.Fatalf("marker must be rendered as a `> ` blockquote at the top\nbody:\n%s", body)
		}
	})
}
