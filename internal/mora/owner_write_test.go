package mora

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// ownerWriteArgs is one complete decision note. Decision status (current or
// needs_review) comes from the validity fields, not from trust: a decision
// written without durability and flip conditions reads needs_review whoever
// wrote it.
var ownerWriteArgs = []string{"write", "--scope", "project:demo", "--type", "decision",
	"--title", "Heron cache choice", "--text", "Use the heron cache for all lookups.",
	"--durability", "standing", "--flip-conditions", "The heron team drops the cache"}

func ownerWriteID(t *testing.T, args ...string) string {
	t.Helper()
	var r struct {
		ID             string `json:"id"`
		OwnerConfirmed bool   `json:"owner_confirmed"`
	}
	if err := json.Unmarshal([]byte(run(t, append(append([]string{}, args...), "--json")...)), &r); err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func ownerWriteConfirmed(t *testing.T, cfg Config, id string) bool {
	t.Helper()
	return memory.IsOwnerConfirmed(ownerMemory(t, cfg, id))
}

func ownerWriteHasTag(t *testing.T, cfg Config, id string) bool {
	t.Helper()
	for _, tag := range ownerMemory(t, cfg, id).Tags {
		if strings.HasPrefix(tag, memory.OwnerConfirmedTagPrefix) {
			return true
		}
	}
	return false
}

func TestWriteInOwnerTerminalIsOwnerConfirmed(t *testing.T) {
	cfg := reviewSetup(t)
	reviewTerminal(t)

	human := run(t, ownerWriteArgs...)
	if !strings.Contains(human, "saved as owner-confirmed") {
		t.Fatalf("receipt does not say the note is owner-confirmed:\n%s", human)
	}
	var r struct {
		ID             string `json:"id"`
		OwnerConfirmed bool   `json:"owner_confirmed"`
		IndexUpdated   bool   `json:"index_updated"`
	}
	if err := json.Unmarshal([]byte(run(t, append(append([]string{}, ownerWriteArgs...), "--json")...)), &r); err != nil {
		t.Fatal(err)
	}
	if !r.OwnerConfirmed || !r.IndexUpdated || !ownerWriteConfirmed(t, cfg, r.ID) {
		t.Fatalf("json receipt %+v, owner-confirmed = %v", r, ownerWriteConfirmed(t, cfg, r.ID))
	}

	// Contract point 4: agents read the note as the owner's own statement.
	for tool, args := range map[string]map[string]any{
		"read_memory":   {"id": r.ID},
		"search_memory": {"query": "heron cache"},
		"list_memory":   {},
	} {
		if got := ownerCall(t, tool, args); !strings.Contains(got, ownerTrustJSON) {
			t.Fatalf("%s has no owner-confirmed row:\n%s", tool, got)
		}
	}
	got := ownerCall(t, "context_memory", map[string]any{"query": "heron cache"})
	if !strings.Contains(got, "the owner then confirmed this exact text in their own terminal") {
		t.Fatalf("context_memory has no owner-confirmed wording:\n%s", got)
	}
	if !strings.Contains(got, "Adopted by user: the owner confirmed this exact text") {
		t.Fatalf("context_memory has no adoption line:\n%s", got)
	}
	// The unconfirmed lines: the pinned owner-confirmed wording still names the
	// writer ("agent-written ...; the owner then confirmed"), so that word
	// alone is not the test.
	for _, bad := range []string{"needs_review", "not checked against the user's words", "no record either way"} {
		if strings.Contains(got, bad) {
			t.Fatalf("context_memory still says %q:\n%s", bad, got)
		}
	}
}

func TestWriteOutsideOwnerTerminalIsNotOwnerConfirmed(t *testing.T) {
	cfg := reviewSetup(t)

	reviewTerminal(t)
	t.Setenv("CLAUDECODE", "1") // an agent session, even with a terminal on stdin
	out := run(t, ownerWriteArgs...)
	if strings.Contains(out, "owner-confirmed") {
		t.Fatalf("agent-session write claims owner-confirmed:\n%s", out)
	}
	id := ownerWriteID(t, ownerWriteArgs...)
	if ownerWriteHasTag(t, cfg, id) || ownerWriteConfirmed(t, cfg, id) {
		t.Fatal("agent-session write carries an owner-confirmed tag")
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(run(t, append(append([]string{}, ownerWriteArgs...), "--json")...)), &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["owner_confirmed"]; ok {
		t.Fatal("receipt gained owner_confirmed for an agent-session write")
	}
}

func TestWriteWithNonInteractiveStdinIsNotOwnerConfirmed(t *testing.T) {
	cfg := reviewSetup(t)
	clearAgentEnv(t) // no agent variable, but stdin is not a terminal (the real check)

	out := run(t, ownerWriteArgs...)
	if strings.Contains(out, "owner-confirmed") || strings.Count(out, "\n") != 1 {
		t.Fatalf("receipt changed for non-interactive stdin:\n%s", out)
	}
	id := ownerWriteID(t, ownerWriteArgs...)
	if ownerWriteHasTag(t, cfg, id) {
		t.Fatal("non-interactive write carries an owner-confirmed tag")
	}
}

func TestWriteRefusesACallerSuppliedOwnerConfirmedTag(t *testing.T) {
	cfg := reviewSetup(t)
	reviewTerminal(t)

	_, err := runErr(t, "write", "--title", "Forged", "--text", "Forged body.", "--tags", "keep,"+memory.OwnerConfirmedTagPrefix+"abc")
	if err == nil || !strings.Contains(err.Error(), memory.OwnerConfirmedTagPrefix) {
		t.Fatalf("tag not refused in the owner terminal: %v", err)
	}
	rows, err := decodeMemoriesJSON(t, run(t, "list", "--json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range rows {
		if m.Title == "Forged" {
			t.Fatalf("a refused write saved a note: %+v", m)
		}
	}
	_ = cfg
}

func TestConfirmInOwnerTerminalMakesAnyAgentNoteOwnerConfirmed(t *testing.T) {
	cfg := reviewSetup(t)
	clearAgentEnv(t)
	plain := ownerWriteID(t, ownerWriteArgs...) // saved by a non-terminal write: a plain agent note
	tentative := ownerSaved(t, "Tentative heron")

	// Outside the owner terminal, confirm is unchanged.
	if _, err := runErr(t, "confirm", plain); err == nil || !strings.Contains(err.Error(), "not tentative") {
		t.Fatalf("confirm of a plain note outside the terminal: %v", err)
	}
	if out := run(t, "confirm", tentative); !strings.HasPrefix(out, "confirmed ") {
		t.Fatalf("confirm output changed outside the terminal: %q", out)
	}
	if ownerWriteConfirmed(t, cfg, plain) || ownerWriteConfirmed(t, cfg, tentative) || ownerWriteHasTag(t, cfg, tentative) {
		t.Fatal("confirm outside the owner terminal set owner-confirmed")
	}

	reviewTerminal(t)
	for _, id := range []string{plain, tentative} {
		if out := run(t, "confirm", id); !strings.HasPrefix(out, "owner-confirmed "+id) {
			t.Fatalf("confirm output = %q", out)
		}
		if !ownerWriteConfirmed(t, cfg, id) {
			t.Fatalf("%s is not owner-confirmed after confirm in the owner terminal", id)
		}
		if got := ownerCall(t, "read_memory", map[string]any{"id": id}); !strings.Contains(got, ownerTrustJSON) {
			t.Fatalf("read_memory of %s:\n%s", id, got)
		}
	}
	var r confirmReceipt
	if err := json.Unmarshal([]byte(run(t, "confirm", plain, "--json")), &r); err != nil || !r.OwnerConfirmed || !r.Confirmed {
		t.Fatalf("receipt = %+v, %v", r, err)
	}
	if log := readAgentWriteLog(t, cfg); !strings.Contains(log, " owner-confirm "+plain+" ") {
		t.Fatalf("log has no owner-confirm line:\n%s", log)
	}

	// A connector record is not an agent note: confirm refuses it.
	if _, err := runErr(t, "confirm", "gmail_thread/none"); err == nil {
		t.Fatal("confirm accepted an id that is not an agent note")
	}
}

func TestEditingAnOwnerWrittenNoteDropsTheStatus(t *testing.T) {
	cfg := reviewSetup(t)
	reviewTerminal(t)
	id := ownerWriteID(t, ownerWriteArgs...)
	m := ownerMemory(t, cfg, id)
	if !memory.IsOwnerConfirmed(m) {
		t.Fatal("note is not owner-confirmed before the edit")
	}
	raw, err := os.ReadFile(m.Path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "all lookups", "no lookups", 1)
	if edited == string(raw) {
		t.Fatalf("edit did not apply:\n%s", raw)
	}
	if err := os.WriteFile(m.Path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if ownerWriteConfirmed(t, cfg, id) {
		t.Fatal("an edited note is still owner-confirmed")
	}
	var read struct{ Trust string }
	if err := json.Unmarshal([]byte(run(t, "read", id, "--json")), &read); err != nil || read.Trust != "" {
		t.Fatalf("read --json trust = %q, %v", read.Trust, err)
	}
}

// outsideOwnerTerminal runs fn as a non-terminal shell would, even in a test
// that faked the owner terminal earlier.
func outsideOwnerTerminal(t *testing.T, fn func()) {
	t.Helper()
	prev := rulesInteractive
	rulesInteractive = func(io.Reader) bool { return false }
	defer func() { rulesInteractive = prev }()
	fn()
}
