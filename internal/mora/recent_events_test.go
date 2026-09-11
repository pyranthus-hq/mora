package mora

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRecentSourceEventsUsesOccurrenceBeforeLimit(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	items := []Memory{
		{ID: "new-import-old-event", Meta: map[string]any{"occurred_at": "2026-06-01T12:00:00Z"}},
		{ID: "old-thread-new-reply", Meta: map[string]any{"occurred_at": "2026-09-09T11:00:00Z"}},
		{ID: "missing-event"},
		{ID: "future", Meta: map[string]any{"occurred_at": "2026-09-10T12:00:00Z"}},
		{ID: "yesterday", Meta: map[string]any{"occurred_at": "2026-09-08T12:00:00Z"}},
	}
	for i := range items {
		items[i].Provider = "gmail"
	}
	r := recentSourceEvents(items, now, 168, 1)
	if len(r) != 1 || r[0].ID != "old-thread-new-reply" {
		t.Fatal(r)
	}
}

func TestRecentEmailCarriesNewestVerifiedMessageRatherThanThreadPrefix(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	m := Memory{ID: "gmail_thread/fixture", Provider: "gmail", Text: "From: owner@example.test\n\nDid the offer arrive?\n\n---\n\nFrom: client@example.test\n\nApproval received; preparing the offer.", Meta: map[string]any{
		"occurred_at": "2026-09-09T11:00:00Z",
		"messages":    []map[string]any{{"message_ref": "gmail_thread/fixture#one", "sender": "owner@example.test", "at": "2026-09-01T11:00:00Z"}, {"message_ref": "gmail_thread/fixture#two", "sender": "client@example.test", "at": "2026-09-09T11:00:00Z"}},
	}}
	r := recentSourceEvents([]Memory{m}, now, 168, 10)
	if len(r) != 1 || r[0].Evidence == nil || r[0].Evidence.EvidenceRef != "gmail_thread/fixture#two" || r[0].Evidence.Snippet != "Approval received; preparing the offer." {
		t.Fatal(r)
	}
	m.Text = "Missing message blocks"
	r = recentSourceEvents([]Memory{m}, now, 168, 10)
	if r[0].Evidence != nil {
		t.Fatal("invented message correspondence")
	}
}

func TestRecentEmailCarriesReadingProjectionBesideSourceSnippet(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	latest := "Hi Sam,\r\n\r\nI received the approval. We will release it later today or early tomorrow.\r\n\r\nBest Regards,\r\n\r\n[A0JjnJsIF8QYAAAAAElFTkSuQmCC]<https://www.example.test/>\r\n  Priya Example | Specialist\r\n  priya@example.test\r\n\r\n________________________________\r\nFrom: owner@example.test\r\nSent: Wednesday\r\nTo: Priya\r\nSubject: Re\r\n\r\nDid the offer arrive?"
	m := Memory{ID: "gmail_thread/fixture", Provider: "gmail", Text: "From: owner@example.test\n\nDid the offer arrive?\n\n---\n\nFrom: client@example.test\n\n" + latest, Meta: map[string]any{
		"occurred_at": "2026-09-09T11:00:00Z",
		"messages":    []map[string]any{{"message_ref": "gmail_thread/fixture#one", "sender": "owner@example.test", "at": "2026-09-01T11:00:00Z"}, {"message_ref": "gmail_thread/fixture#two", "sender": "client@example.test", "at": "2026-09-09T11:00:00Z"}},
	}}
	r := recentSourceEvents([]Memory{m}, now, 168, 10)
	ev := r[0].Evidence
	if ev == nil || ev.Snippet != latest || ev.Readable != "Hi Sam,\n\nI received the approval. We will release it later today or early tomorrow.\n\nBest Regards,\n\nPriya Example" || ev.ReadableTruncated {
		t.Fatalf("%+v", ev)
	}
	if len(ev.Omitted) != 3 || ev.Omitted[0] != "earlier quoted messages" || ev.Omitted[2] != "signature" {
		t.Fatalf("omitted=%v", ev.Omitted)
	}
	m.Text = "From: owner@example.test\n\nDid the offer arrive?\n\n---\n\nFrom: client@example.test\n\nPlain reply."
	if ev := recentSourceEvents([]Memory{m}, now, 168, 10)[0].Evidence; ev.Readable != "" || ev.Omitted != nil {
		t.Fatalf("clean text must not invent a projection: %+v", ev)
	}
}

func TestExtractSourceFlagKeepsFreeTextAndFailsOnMissingValue(t *testing.T) {
	rest, source, err := extractSourceFlag([]string{"offer", "--source", "gmail:work", "status", "--json"})
	if err != nil || source != "gmail:work" || strings.Join(rest, " ") != "offer status --json" {
		t.Fatal(rest, source, err)
	}
	if rest, source, err = extractSourceFlag([]string{"--source=imessage", "x"}); err != nil || source != "imessage" || len(rest) != 1 {
		t.Fatal(rest, source, err)
	}
	if _, _, err = extractSourceFlag([]string{"x", "--source"}); err == nil {
		t.Fatal("missing value must fail")
	}
	if _, _, err = extractSourceFlag([]string{"--source="}); err == nil {
		t.Fatal("empty value must fail")
	}
}

func TestListSourceReceiptEchoesWithoutEventWindowAndRejectsExplicitZero(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	out := run(t, "list", "--source", "gmail", "--json")
	var receipt struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal([]byte(out), &receipt); err != nil || receipt.Source != "gmail" {
		t.Fatalf("source-only receipt=%q err=%v", out, err)
	}
	var stdout, stderr bytes.Buffer
	if err := Run(testCtx(t), []string{"list", "--event-since-hours", "0", "--json"}, &stdout, &stderr, strings.NewReader("")); err == nil {
		t.Fatal("explicit zero event window accepted")
	}
	if hasIntFlag([]string{"--limit", "20"}, "--event-since-hours") || !hasIntFlag([]string{"--event-since-hours=1"}, "--event-since-hours") {
		t.Fatal("event flag presence detection")
	}
}
