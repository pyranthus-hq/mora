package google

import (
	"testing"

	gmail "google.golang.org/api/gmail/v1"
)

// Issue #62 defect 2 (enrichment): the Gmail thread captures the actionability labels
// UNREAD/IMPORTANT/STARRED (union across messages) so the brief's urgent lane has a
// first-class signal. Only those three are captured — never routing labels like INBOX.
func TestGmailThreadCapturesUrgencyLabels(t *testing.T) {
	th := &gmail.Thread{Id: "t1", Messages: []*gmail.Message{
		{InternalDate: 1700000000000, LabelIds: []string{"INBOX", "UNREAD", "IMPORTANT"}, Payload: &gmail.MessagePart{Headers: []*gmail.MessagePartHeader{
			hdr("From", "sarah@client.com"),
			hdr("Subject", "MSA"),
		}}},
		{InternalDate: 1700000100000, LabelIds: []string{"INBOX", "STARRED"}, Payload: &gmail.MessagePart{Headers: []*gmail.MessagePartHeader{
			hdr("From", "sarah@client.com"),
		}}},
	}}
	it := gmailThreadToItem(th)
	labels := metaStrings(it.Meta["labels"])
	for _, want := range []string{"UNREAD", "IMPORTANT", "STARRED"} {
		if !has(labels, want) {
			t.Fatalf("labels = %v, want %s captured", labels, want)
		}
	}
	if has(labels, "INBOX") {
		t.Fatalf("only UNREAD/IMPORTANT/STARRED must be captured; got %v", labels)
	}
}

func TestGmailThreadNoUrgencyLabelsMetaAbsent(t *testing.T) {
	th := &gmail.Thread{Id: "t2", Messages: []*gmail.Message{
		{InternalDate: 1700000000000, LabelIds: []string{"INBOX", "CATEGORY_PERSONAL"}, Payload: &gmail.MessagePart{Headers: []*gmail.MessagePartHeader{hdr("From", "x@y.com")}}},
	}}
	it := gmailThreadToItem(th)
	if _, ok := it.Meta["labels"]; ok {
		t.Fatalf("no actionability labels => no labels key (backward compat)")
	}
}
