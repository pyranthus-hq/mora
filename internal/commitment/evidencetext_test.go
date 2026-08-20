package commitment

import (
	"reflect"
	"testing"
)

func TestEvidenceTextSegments(t *testing.T) {
	if got := Segments("I will send the deck tomorrow.\nThanks."); !reflect.DeepEqual(got, []string{"I will send the deck tomorrow."}) {
		t.Fatalf("segments=%v", got)
	}
	for _, text := range []string{"Earlier request from the archive: I will send it.", "Quoted request below: I will send it.", "Forwarded below: I will send it."} {
		if got := Segments(text); got != nil {
			t.Fatalf("archive guard %q=%v", text, got)
		}
	}
	if got := Segments("Birthday next week. I will send https://example.com/a the deck."); !reflect.DeepEqual(got, []string{"I will send the deck."}) {
		t.Fatalf("filtered=%v", got)
	}
	if got := ClosureSegments("FYI:\nI sent the deck.\n--\nsignature"); !reflect.DeepEqual(got, []string{"I sent the deck."}) {
		t.Fatalf("closure=%v", got)
	}
	if got := ClosureSegments("On Tue, Sam wrote:\n> I sent it"); got != nil {
		t.Fatalf("quoted closure=%v", got)
	}
}
func TestFulfilledQuotedRequest(t *testing.T) {
	body := "I sent the review notes.\nOn Tue, Aug 5, 2026 at 9:00 AM Lucia wrote:\n> Can you send the review notes?\n\nSignature"
	delivery, request, author, block, ok := FulfilledQuotedRequest(body, []string{"authored", "quoted"}, map[string]string{"lucia@example.com": "Lucia Rivera"})
	if !ok || delivery != "I sent the review notes." || request != "Can you send the review notes?" || author != "lucia@example.com" || block != "quoted" {
		t.Fatalf("got=%q,%q,%q,%q,%v", delivery, request, author, block, ok)
	}
	tests := []struct {
		name, body string
		refs       []string
		names      map[string]string
	}{{"refs", body, []string{"one"}, map[string]string{"lucia@example.com": "Lucia"}}, {"no attribution", "I sent it.", []string{"a", "b"}, map[string]string{"lucia@example.com": "Lucia"}}, {"not delivered", "I will send it.\nOn Tue, Lucia wrote:\n> Can you send it?", []string{"a", "b"}, map[string]string{"lucia@example.com": "Lucia"}}, {"ambiguous author", body, []string{"a", "b"}, map[string]string{"a@x": "Lucia", "b@x": "Lucia"}}, {"no author", body, []string{"a", "b"}, map[string]string{"a@x": "Bob"}}, {"not request", "I sent it.\nOn Tue, Lucia wrote:\n> Nice weather.", []string{"a", "b"}, map[string]string{"a@x": "Lucia"}}, {"multiple requests", "I sent it.\nOn Tue, Lucia wrote:\n> Can you send one? Can you send two?", []string{"a", "b"}, map[string]string{"a@x": "Lucia"}}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, _, _, ok := FulfilledQuotedRequest(tt.body, tt.refs, tt.names); ok {
				t.Fatal("accepted")
			}
		})
	}
}
