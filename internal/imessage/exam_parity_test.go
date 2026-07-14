package imessage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
	"github.com/pyranthus-hq/mora/internal/mora/exam"
)

func TestMain(m *testing.M) {
	time.Local = time.FixedZone("exam-render", 0)
	os.Exit(m.Run())
}

func loadExamIMessageLedger(t *testing.T) exam.Ledger {
	t.Helper()
	l, err := exam.Load(filepath.Join("..", "mora", "eval", "obligations-v1", "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func imessageExamIdentities(l exam.Ledger) map[string]exam.Identity {
	ids := map[string]exam.Identity{l.Self.ID: l.Self}
	for _, p := range l.People {
		ids[p.ID] = p
	}
	return ids
}

func imessageExamHandle(id exam.Identity) string {
	if len(id.Handles) == 0 {
		return ""
	}
	return id.Handles[0]
}

func imessageExamBlocks(blocks []exam.Block) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		parts = append(parts, block.Text)
	}
	return strings.Join(parts, "\n\n")
}

func examConversation(t *testing.T, a exam.Artifact, ids map[string]exam.Identity, selfID string) convInput {
	t.Helper()
	handles := make([]string, 0, len(a.Participants))
	for _, id := range a.Participants {
		handles = append(handles, imessageExamHandle(ids[id]))
	}
	c := convInput{guid: strings.TrimPrefix(a.MemoryID, "imessage_chat/")}
	if len(handles) == 1 {
		c.chat.identifier = handles[0]
	} else {
		c.chat.participants = handles
		c.chat.isGroup = true
	}
	for _, msg := range a.Messages {
		at, err := time.Parse(time.RFC3339, msg.At)
		if err != nil {
			t.Fatal(err)
		}
		rm := renderMessage{date: at, fromMe: msg.From == selfID, text: imessageExamBlocks(msg.Body)}
		if !rm.fromMe {
			rm.sender = imessageExamHandle(ids[msg.From])
		}
		c.messages = append(c.messages, rm)
	}
	return c
}

func readExamIMessageMemory(t *testing.T, memoryID string) (string, map[string]any) {
	t.Helper()
	path := filepath.Join("..", "mora", "eval", "obligations-v1", "vault", "sources", "imessage", memory.SafeFilename(memoryID)+".md")
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

func TestExamIMessageBodiesMatchRenderer(t *testing.T) {
	l := loadExamIMessageLedger(t)
	ids := imessageExamIdentities(l)
	contacts := map[string]string{}
	for _, id := range ids {
		if id.Display != "" && imessageExamHandle(id) != "" {
			contacts[imessageExamHandle(id)] = id.Display
		}
	}
	resolver := newResolverFromMap(contacts)
	for _, a := range l.Artifacts {
		if a.Channel != "imessage" {
			continue
		}
		mapped := mapConversation(examConversation(t, a, ids, l.Self.ID), resolver, 0)
		body, meta := readExamIMessageMemory(t, a.MemoryID)
		if mapped.Body != body {
			t.Errorf("%s body differs from renderTranscript\ngot: %q\nwant: %q", a.ID, body, mapped.Body)
		}
		gotMeta, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		wantMeta, err := json.Marshal(mapped.Meta)
		if err != nil {
			t.Fatal(err)
		}
		if string(gotMeta) != string(wantMeta) {
			t.Errorf("%s meta differs from conversationMeta\ngot: %s\nwant: %s", a.ID, gotMeta, wantMeta)
		}
		if mapped.Title != a.Subject {
			t.Errorf("%s title = %q, want %q", a.ID, mapped.Title, a.Subject)
		}
	}
}
