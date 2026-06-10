package imessage

import (
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// sampleConv builds a structured 1:1 conversation input across two days for the
// mapper tests. The handle resolves to "Neil Patel" via resolver1to1.
func sampleConv() convInput {
	return convInput{
		guid: "iMessage;-;+14155551234",
		chat: conversation{participants: []string{"+14155551234"}, identifier: "+14155551234"},
		messages: []renderMessage{
			{date: localDate(2026, 5, 30, 9, 0), fromMe: false, sender: "+14155551234", text: "are we still on for the demo?"},
			{date: localDate(2026, 5, 30, 9, 1), fromMe: true, text: "yes, 3pm"},
			{date: localDate(2026, 5, 31, 8, 5), fromMe: true, text: "works for me"},
		},
		attachments: []Attachment{{Filename: "IMG_0001.HEIC", MimeType: "image/heic", Size: 1234}},
	}
}

// TestMapConversationGrain proves one conversation → exactly one MappedMemory with
// the provider-identity StableID/Provider and the rendered Title/Body (IMSG-03, D-04).
func TestMapConversationGrain(t *testing.T) {
	r := resolver1to1()
	c := sampleConv()

	mm := mapConversation(c, r, 0)

	if want := "imessage_chat/" + c.guid; mm.StableID != want {
		t.Fatalf("StableID = %q, want %q", mm.StableID, want)
	}
	if mm.Provider != "imessage" {
		t.Fatalf("Provider = %q, want %q", mm.Provider, "imessage")
	}
	if mm.ProviderID != c.guid {
		t.Fatalf("ProviderID = %q, want %q", mm.ProviderID, c.guid)
	}
	if mm.Title != "Neil Patel" {
		t.Fatalf("Title = %q, want %q", mm.Title, "Neil Patel")
	}
	wantBody, _ := renderBody(c.messages, r, 0)
	if mm.Body != wantBody {
		t.Fatalf("Body mismatch.\n--- got ---\n%s\n--- want ---\n%s", mm.Body, wantBody)
	}
	if mm.Truncated {
		t.Fatal("unbounded mapping should not be truncated")
	}
	// CreatedAt = newest message time (D-03 recency anchor).
	wantCreated := localDate(2026, 5, 31, 8, 5).UTC().Format(time.RFC3339)
	if mm.CreatedAt != wantCreated {
		t.Fatalf("CreatedAt = %q, want %q (newest message)", mm.CreatedAt, wantCreated)
	}
}

// TestMapConversationContentHashStable proves re-mapping identical input yields an
// identical ContentHash — the precondition for writeMappedMemory's content-hash
// skip (D-05).
func TestMapConversationContentHashStable(t *testing.T) {
	r := resolver1to1()
	c := sampleConv()

	a := mapConversation(c, r, 0)
	b := mapConversation(c, r, 0)

	if a.ContentHash == "" {
		t.Fatal("ContentHash is empty")
	}
	if a.ContentHash != b.ContentHash {
		t.Fatalf("ContentHash unstable across identical input: %q vs %q", a.ContentHash, b.ContentHash)
	}
	// The hash now folds the canonical participant Meta (S2) so recovered names /
	// new participants rewrite the file; an untouched conversation still hashes
	// identically across syncs (the D-05 skip-rewrite property, just meta-aware).
	metaJSON, _ := memory.CanonicalMeta(a.Meta)
	want := memory.ContentHash(a.Title, a.Body)
	if metaJSON != "" {
		want = memory.ContentHash(a.Title, a.Body, metaJSON)
	}
	if a.ContentHash != want {
		t.Fatalf("ContentHash is not ContentHash(Title, Body, meta)")
	}
}

// TestMapConversationMetaParticipants proves Meta carries STRUCTURED handle↔name
// pairs (S3) — not the old parallel comma-joined lists (which broke whenever a
// resolved name contained a comma) — plus occurred_at, and never message bytes.
func TestMapConversationMetaParticipants(t *testing.T) {
	r := resolver1to1()
	c := sampleConv()

	mm := mapConversation(c, r, 0)

	if mm.Meta == nil {
		t.Fatal("Meta is nil; want participant identity metadata")
	}
	pairs, ok := mm.Meta["participants"].([]map[string]string)
	if !ok {
		t.Fatalf("Meta[participants] = %T, want []map[string]string of handle↔name pairs", mm.Meta["participants"])
	}
	var found bool
	for _, p := range pairs {
		if p["handle"] == "+14155551234" && p["name"] == "Neil Patel" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Meta[participants] = %v, want a {handle:+14155551234, name:Neil Patel} pair", pairs)
	}
	if s, _ := mm.Meta["occurred_at"].(string); s == "" {
		t.Fatal("occurred_at missing from iMessage Meta")
	}
	// No message body must leak into any string-valued meta field (IMSG-07).
	for k, v := range mm.Meta {
		if s, ok := v.(string); ok && strings.Contains(s, "are we still on for the demo?") {
			t.Fatalf("Meta[%s] leaked message body text: %q", k, v)
		}
	}
}

// TestMapConversationTruncationInverted proves the mapper propagates the renderer's
// newest-first bounding result honestly (D-03/D-04): a tight budget truncates,
// IngestedSize < OriginalSize, and the newest message survives while the oldest is
// dropped (the inverse of Gmail start-keep).
func TestMapConversationTruncationInverted(t *testing.T) {
	r := resolver1to1()
	msgs := []renderMessage{
		{date: localDate(2026, 5, 28, 9, 0), fromMe: false, sender: "+14155551234", text: "OLDEST should be dropped from the body entirely here"},
	}
	// Many short messages so the kept-window + marker is clearly smaller than the full
	// transcript (marker overhead does not dominate the size comparison).
	for i := 0; i < 12; i++ {
		msgs = append(msgs, renderMessage{date: localDate(2026, 5, 29, 9, i), fromMe: true, text: "filler message number with some length to it"})
	}
	msgs = append(msgs, renderMessage{date: localDate(2026, 5, 30, 9, 0), fromMe: true, text: "NEWEST must remain in the body for sure"})
	c := convInput{
		guid:     "iMessage;-;+14155551234",
		chat:     conversation{participants: []string{"+14155551234"}, identifier: "+14155551234"},
		messages: msgs,
	}

	mm := mapConversation(c, r, 300)

	if !mm.Truncated {
		t.Fatalf("expected Truncated=true at budget 70 (body=%q)", mm.Body)
	}
	if mm.IngestedSize >= mm.OriginalSize {
		t.Fatalf("IngestedSize (%d) should be < OriginalSize (%d) when truncated", mm.IngestedSize, mm.OriginalSize)
	}
	if !strings.Contains(mm.Body, "NEWEST must remain") {
		t.Fatalf("newest message dropped — recency-first truncation violated:\n%s", mm.Body)
	}
	if strings.Contains(mm.Body, "OLDEST should be dropped") {
		t.Fatalf("oldest message survived — should have been dropped:\n%s", mm.Body)
	}
}

// TestMapConversationAttachments proves attachment METADATA (filename/MIME/size) is
// carried onto the MappedMemory struct field, never the message bytes (IMSG-07).
func TestMapConversationAttachments(t *testing.T) {
	r := resolver1to1()
	c := sampleConv()

	mm := mapConversation(c, r, 0)

	if len(mm.Attachments) != 1 {
		t.Fatalf("Attachments len = %d, want 1", len(mm.Attachments))
	}
	if mm.Attachments[0].Filename != "IMG_0001.HEIC" || mm.Attachments[0].MimeType != "image/heic" {
		t.Fatalf("attachment metadata not carried: %+v", mm.Attachments[0])
	}
}
