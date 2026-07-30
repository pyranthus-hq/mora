package google

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	gmail "google.golang.org/api/gmail/v1"
)

func gmailEvidenceMessage(id, from, to, cc string, at int64, body string) *gmail.Message {
	headers := []*gmail.MessagePartHeader{
		hdr("From", from),
		hdr("To", to),
	}
	if cc != "" {
		headers = append(headers, hdr("Cc", cc))
	}
	return &gmail.Message{
		Id:           id,
		InternalDate: at,
		Payload: &gmail.MessagePart{
			MimeType: "text/plain",
			Headers:  headers,
			Body: &gmail.MessagePartBody{
				Data: base64.RawURLEncoding.EncodeToString([]byte(body)),
			},
		},
	}
}

func TestGmailPreservesPerMessageEvidence(t *testing.T) {
	thread := &gmail.Thread{Id: "thread-7", Messages: []*gmail.Message{
		gmailEvidenceMessage(
			"msg-a",
			`"Casey Client" <CASEY@example.org>`,
			`Alex <alex@example.com>`,
			`Pat <pat@example.net>`,
			1_721_123_200_000,
			"Could you send the receipt?",
		),
		gmailEvidenceMessage(
			"msg-b",
			`Alex <alex@example.com>`,
			`Casey Client <casey@example.org>`,
			"",
			1_721_126_800_000,
			"I will send it today.",
		),
	}}

	item := gmailThreadToItem(thread)
	got, ok := item.Meta["messages"].([]gmailMessageEvidence)
	if !ok {
		t.Fatalf("meta.messages type = %T, want []gmailMessageEvidence", item.Meta["messages"])
	}
	want := []gmailMessageEvidence{
		{
			MessageRef: "gmail_thread/thread-7#msg-a",
			Sender:     "casey@example.org",
			To:         []string{"alex@example.com"},
			Cc:         []string{"pat@example.net"},
			At:         "2024-07-16T09:46:40Z",
			BlockRefs:  []string{"body"},
		},
		{
			MessageRef: "gmail_thread/thread-7#msg-b",
			Sender:     "alex@example.com",
			To:         []string{"casey@example.org"},
			Cc:         []string{},
			At:         "2024-07-16T10:46:40Z",
			BlockRefs:  []string{"body"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("per-message evidence mismatch:\n got: %#v\nwant: %#v", got, want)
	}

	// The thread-level identity union is a backward-compatibility contract.
	if from := metaStrings(item.Meta["from"]); !reflect.DeepEqual(from, []string{"alex@example.com", "casey@example.org"}) {
		t.Fatalf("thread-level from union = %v", from)
	}
	if to := metaStrings(item.Meta["to"]); !reflect.DeepEqual(to, []string{"alex@example.com", "casey@example.org"}) {
		t.Fatalf("thread-level to union = %v", to)
	}
	if cc := metaStrings(item.Meta["cc"]); !reflect.DeepEqual(cc, []string{"pat@example.net"}) {
		t.Fatalf("thread-level cc union = %v", cc)
	}
	for _, want := range []string{
		`From: "Casey Client" <CASEY@example.org>` + "\n\nCould you send the receipt?",
		"From: Alex <alex@example.com>\n\nI will send it today.",
	} {
		if !strings.Contains(item.Body, want) {
			t.Errorf("thread body does not preserve per-message sender header %q:\n%s", want, item.Body)
		}
	}
}

func TestGmailPreservesLastSenderAndOrder(t *testing.T) {
	thread := &gmail.Thread{Id: "ordered", Messages: []*gmail.Message{
		gmailEvidenceMessage("first", "first@example.org", "self@example.com", "", 1_721_123_200_000, "first body"),
		gmailEvidenceMessage("second", "self@example.com", "first@example.org", "", 1_721_126_800_000, "second body"),
		gmailEvidenceMessage("last", "last@example.net", "self@example.com", "", 1_721_130_400_000, "last body"),
	}}

	item := gmailThreadToItem(thread)
	messages := item.Meta["messages"].([]gmailMessageEvidence)
	var refs []string
	for _, message := range messages {
		refs = append(refs, message.MessageRef)
	}
	wantRefs := []string{
		"gmail_thread/ordered#first",
		"gmail_thread/ordered#second",
		"gmail_thread/ordered#last",
	}
	if !reflect.DeepEqual(refs, wantRefs) {
		t.Fatalf("message order = %v, want %v", refs, wantRefs)
	}
	if got := item.Meta["last_sender"]; got != "last@example.net" {
		t.Fatalf("last_sender = %#v, want last@example.net", got)
	}
}
