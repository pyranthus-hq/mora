package google

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
	"github.com/pyranthus-hq/mora/internal/mora/exam"
	gmail "google.golang.org/api/gmail/v1"
)

func loadExamGmailLedger(t *testing.T) exam.Ledger {
	t.Helper()
	l, err := exam.Load(filepath.Join("..", "mora", "eval", "obligations-v1", "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func examIdentities(l exam.Ledger) map[string]exam.Identity {
	ids := map[string]exam.Identity{l.Self.ID: l.Self}
	for _, p := range l.People {
		ids[p.ID] = p
	}
	return ids
}

func examEmail(id exam.Identity) string {
	if len(id.Emails) == 0 {
		return ""
	}
	return id.Emails[0]
}

func examHeader(id exam.Identity) string {
	if id.Display == "" {
		return examEmail(id)
	}
	return fmt.Sprintf("%s <%s>", id.Display, examEmail(id))
}

func examBlockText(blocks []exam.Block) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		parts = append(parts, block.Text)
	}
	return strings.Join(parts, "\n\n")
}

func examAddressList(refs []string, ids map[string]exam.Identity) string {
	values := make([]string, 0, len(refs))
	for _, ref := range refs {
		values = append(values, examHeader(ids[ref]))
	}
	return strings.Join(values, ", ")
}

func gmailThreadFromLedger(t *testing.T, a exam.Artifact, ids map[string]exam.Identity) *gmail.Thread {
	t.Helper()
	th := &gmail.Thread{Id: strings.TrimPrefix(a.MemoryID, "gmail_thread/")}
	for _, msg := range a.Messages {
		at, err := time.Parse(time.RFC3339, msg.At)
		if err != nil {
			t.Fatal(err)
		}
		headers := []*gmail.MessagePartHeader{
			{Name: "Subject", Value: a.Subject},
			{Name: "From", Value: examHeader(ids[msg.From])},
			{Name: "To", Value: examAddressList(msg.To, ids)},
		}
		if len(msg.Cc) > 0 {
			headers = append(headers, &gmail.MessagePartHeader{Name: "Cc", Value: examAddressList(msg.Cc, ids)})
		}
		body := base64.RawURLEncoding.EncodeToString([]byte(examBlockText(msg.Body)))
		th.Messages = append(th.Messages, &gmail.Message{InternalDate: at.UnixMilli(), Payload: &gmail.MessagePart{MimeType: "text/plain", Headers: headers, Body: &gmail.MessagePartBody{Data: body}}})
	}
	return th
}

func readExamGmailMemory(t *testing.T, memoryID string) (string, map[string]any) {
	t.Helper()
	path := filepath.Join("..", "mora", "eval", "obligations-v1", "vault", "sources", "gmail", memory.SafeFilename(memoryID)+".md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(b)[4:], "\n---\n", 2)
	if len(parts) != 2 {
		t.Fatalf("invalid memory %s", path)
	}
	meta := map[string]any{}
	for _, line := range strings.Split(parts[0], "\n") {
		if strings.HasPrefix(line, "meta: ") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "meta: ")), &meta); err != nil {
				t.Fatal(err)
			}
		}
	}
	return strings.TrimSpace(parts[1]), meta
}

func TestExamGmailBodiesSurviveStripQuoted(t *testing.T) {
	l := loadExamGmailLedger(t)
	for _, a := range l.Artifacts {
		if a.Channel != "gmail" {
			continue
		}
		body, _ := readExamGmailMemory(t, a.MemoryID)
		if got := stripQuoted(body); got != body {
			t.Errorf("%s is not a stripQuoted fixed point\ngot: %q\nwant: %q", a.ID, got, body)
		}
	}
}

func TestExamGmailMetaMatchesMapperContract(t *testing.T) {
	l := loadExamGmailLedger(t)
	ids := examIdentities(l)
	for _, a := range l.Artifacts {
		if a.Channel != "gmail" {
			continue
		}
		item := gmailThreadToItem(gmailThreadFromLedger(t, a, ids))
		body, meta := readExamGmailMemory(t, a.MemoryID)
		if item.Body != body {
			t.Errorf("%s body differs from gmailThreadToItem\ngot: %q\nwant: %q", a.ID, body, item.Body)
		}
		gotMeta, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		wantMeta, err := json.Marshal(item.Meta)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotMeta) != string(wantMeta) {
			t.Errorf("%s meta differs from mapper\ngot: %s\nwant: %s", a.ID, gotMeta, wantMeta)
		}
		if item.Title != a.Subject || item.ProviderID != strings.TrimPrefix(a.MemoryID, "gmail_thread/") {
			t.Errorf("%s mapper identity mismatch: title=%q provider_id=%q", a.ID, item.Title, item.ProviderID)
		}
	}
}
