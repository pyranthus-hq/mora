package imessage

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestMessageGUIDSurvivesCollapseRenderAndTruncation(t *testing.T) {
	chat := seedChat{rowid: 1, guid: "chat-guid", identifier: "+14155550100", participants: []string{"+14155550100"}}
	msgs := []seedMsg{
		{chatID: 1, guid: "message-a", date: localDate(2026, 8, 1, 9, 0), handle: "+14155550100", text: "identical words", attFile: "~/a.png"},
		{chatID: 1, guid: "message-b", date: localDate(2026, 8, 1, 9, 1), handle: "+14155550100", text: "identical words"},
	}
	f := imNewFetcher(t, []seedChat{chat}, msgs)
	defer f.Close()
	items := fetchAll(t, f, FetchWindow{})
	if len(items) != 1 {
		t.Fatalf("items = %d", len(items))
	}
	c := items[0].Payload.(convInput)
	if len(c.messages) != 2 || c.messages[0].guid != "message-a" || c.messages[1].guid != "message-b" {
		t.Fatalf("collapsed GUIDs = %+v", c.messages)
	}
	mm := mapConversation(c, resolver1to1(), 0)
	b, _ := json.Marshal(mm.Meta["message_evidence"])
	if mm.Meta["message_evidence_schema"] != 1 {
		t.Fatalf("message evidence schema = %#v", mm.Meta["message_evidence_schema"])
	}
	if !containsAll(string(b), "imessage_chat/chat-guid#message-a", "imessage_chat/chat-guid#message-b", `"from_me":false`) {
		t.Fatalf("message evidence = %s", b)
	}
	// A tight newest-first budget may retain only message-b. Dropped message-a
	// must not remain addressable.
	truncated := mapConversation(c, resolver1to1(), len([]rune(mm.Body))-1)
	b, _ = json.Marshal(truncated.Meta["message_evidence"])
	if string(b) == "null" || containsAll(string(b), "message-a") || !containsAll(string(b), "message-b") {
		t.Fatalf("truncated evidence leaked/dropped wrong refs: %s", b)
	}
	if mm.ContentHash != memory.ContentHash(mm.Title, mm.Body, mustIdentityMeta(t, c)) {
		t.Fatal("message evidence must not change the meaningful-content hash")
	}
}

func TestMissingMessageGUIDProducesDiagnosticNotSyntheticRef(t *testing.T) {
	r := resolver1to1()
	messages := []renderMessage{{date: localDate(2026, 8, 1, 9, 0), sender: "+14155550100", text: "no provider identity"}}
	body, rendered := renderBody(messages, r, 0)
	evidence, diagnostics := messageEvidenceMeta("imessage_chat/chat-guid", body, rendered.retained, r)
	if len(evidence) != 0 || len(diagnostics) != 1 || diagnostics[0]["reason"] != "missing_provider_guid" {
		t.Fatalf("missing GUID result: evidence=%v diagnostics=%v", evidence, diagnostics)
	}
	b, _ := json.Marshal(diagnostics)
	if strings.Contains(string(b), "imessage_chat/chat-guid#") {
		t.Fatalf("missing GUID fabricated an evidence ref: %s", b)
	}
	mm := mapConversation(convInput{
		guid: "chat-guid", chat: conversation{identifier: "+14155550100", participants: []string{"+14155550100"}}, messages: messages,
	}, r, 0)
	if mm.Meta["message_evidence_schema"] != 1 || mm.Meta["message_evidence"] != nil || mm.Meta["message_evidence_diagnostics"] == nil {
		t.Fatalf("all-missing-GUID mapped metadata = %#v", mm.Meta)
	}
}

func TestFinalMessageTrailingNewlineKeepsEvidenceBoundary(t *testing.T) {
	r := resolver1to1()
	messages := []renderMessage{
		{guid: "message-guid", date: localDate(2026, 8, 1, 9, 0), sender: "+14155550100", text: "line one \n"},
		{guid: "skipped-guid", date: localDate(2026, 8, 1, 9, 1), kind: msgSkip},
	}
	body, rendered := renderBody(messages, r, 0)
	evidence, diagnostics := messageEvidenceMeta("imessage_chat/chat-guid", body, rendered.retained, r)
	if len(diagnostics) != 0 || len(evidence) != 1 {
		t.Fatalf("trailing newline boundary: evidence=%v diagnostics=%v body=%q", evidence, diagnostics, body)
	}
	if end, _ := evidence[0]["block_end"].(int); end > len(strings.TrimSpace(body)) {
		t.Fatalf("boundary %d exceeds durable parsed body length %d", end, len(strings.TrimSpace(body)))
	}
}

func mustIdentityMeta(t *testing.T, c convInput) string {
	t.Helper()
	s, err := memory.CanonicalMeta(conversationMeta(c, resolver1to1()))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func containsAll(s string, wants ...string) bool {
	for _, want := range wants {
		if !strings.Contains(s, want) {
			return false
		}
	}
	return true
}
