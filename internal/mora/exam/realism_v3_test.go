package exam

import (
	"reflect"
	"strings"
	"testing"
)

// v3Ledger is intentionally test-local. The human-authored obligations-v3
// ledger/corpus is a separate sealed validation step; machinery tests must not
// manufacture a candidate corpus in eval/.
func v3Ledger() Ledger {
	l := v2Ledger()
	l.Version = SchemaV3
	return l
}

func TestSchemaV3AcceptsValidatorAndLintMachinery(t *testing.T) {
	l := v3Ledger()
	for name, check := range map[string]func(Ledger) error{
		"validate":          Validate,
		"identity lint":     Lint,
		"leakage lint":      LintLeakage,
		"date fingerprint":  LintDateFingerprint,
		"title fingerprint": LintTitleFingerprint,
	} {
		if err := check(l); err != nil {
			t.Fatalf("%s rejected schema v3 fixture: %v", name, err)
		}
	}
	rendered, err := Render(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := LintCorpus(rendered); err != nil {
		t.Fatalf("corpus lint rejected schema v3 render: %v", err)
	}
}

func TestSchemaV3RequiresTwoWayGmailEvidenceShape(t *testing.T) {
	l := v3Ledger()
	for ai := range l.Artifacts {
		if l.Artifacts[ai].Channel != "gmail" {
			continue
		}
		for mi := range l.Artifacts[ai].Messages {
			l.Artifacts[ai].Messages[mi].From = l.People[0].ID
		}
	}
	if err := Validate(l); !hasNamedError(err, RuleReplyChainQuotes) {
		t.Fatalf("one-way-only schema v3 ledger error = %v, want %s", err, RuleReplyChainQuotes)
	}
}

func TestSchemaV3GmailRenderPreservesPerMessageEvidence(t *testing.T) {
	l := v3Ledger()
	ids := map[string]Identity{l.Self.ID: l.Self}
	for _, person := range l.People {
		ids[person.ID] = person
	}
	var thread Artifact
	for _, artifact := range l.Artifacts {
		if artifact.ID == "a/thread" {
			thread = artifact
			break
		}
	}

	v2Body, v2Meta := renderGmail(thread, ids, SchemaV2)
	v3Body, v3Meta := renderGmail(thread, ids, SchemaV3)
	wantParts := make([]string, 0, len(thread.Messages))
	for _, message := range thread.Messages {
		wantParts = append(wantParts, "From: "+identityHeader(ids[message.From])+"\n\n"+renderMessageBody(message, SchemaV3))
	}
	if want := strings.Join(wantParts, "\n\n---\n\n"); v3Body != want {
		t.Fatalf("schema v3 Gmail body does not expose every message sender:\ngot:\n%s\nwant:\n%s", v3Body, want)
	}
	if strings.Count(v2Body, "From: ") != 1 {
		t.Fatalf("frozen schema v2 Gmail body has %d sender headers, want 1", strings.Count(v2Body, "From: "))
	}
	if _, ok := v2Meta["messages"]; ok {
		t.Fatal("schema v2 is frozen and must not gain per-message metadata")
	}
	if _, ok := v2Meta["last_sender"]; ok {
		t.Fatal("schema v2 is frozen and must not gain last_sender")
	}

	messages, ok := v3Meta["messages"].([]gmailMessageEvidence)
	if !ok || len(messages) != len(thread.Messages) {
		t.Fatalf("schema v3 messages = %#v, want %d ordered entries", v3Meta["messages"], len(thread.Messages))
	}
	for i, message := range messages {
		source := thread.Messages[i]
		if message.MessageRef != thread.MemoryID+"#"+source.ID {
			t.Errorf("message %d ref = %q", i, message.MessageRef)
		}
		wantBlocks := make([]string, 0, len(source.Body))
		for _, block := range source.Body {
			wantBlocks = append(wantBlocks, block.ID)
		}
		if !reflect.DeepEqual(message.BlockRefs, wantBlocks) {
			t.Errorf("message %d block refs = %v, want %v", i, message.BlockRefs, wantBlocks)
		}
		if message.Sender != identityEmail(ids[source.From]) {
			t.Errorf("message %d sender = %q", i, message.Sender)
		}
	}
	if got := v3Meta["last_sender"]; got != messages[len(messages)-1].Sender {
		t.Fatalf("last_sender = %#v, want %q", got, messages[len(messages)-1].Sender)
	}
}

func TestRendererVersionForSchemaV3(t *testing.T) {
	if got := RendererVersionFor(SchemaV3); got != "exam-render-v3.1" {
		t.Fatalf("schema v3 renderer = %q", got)
	}
	if strings.TrimSpace(RendererVersionFor(SchemaV3)) == RendererVersionFor(SchemaV2) {
		t.Fatal("schema v3 must not reuse the frozen v2 renderer name")
	}
}
