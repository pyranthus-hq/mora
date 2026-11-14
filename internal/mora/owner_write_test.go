package mora

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// The tests in this file cover `mora write --mine` and `mora confirm --mine`.
// A test that calls reviewTerminal fakes the terminal check (rulesInteractive
// returns true); every refusal test leaves the real check in place.

const mineYes = "y\n"

var mineNote = []string{"--scope", "project:demo", "--type", "decision",
	"--title", "Heron cache choice", "--text", "Use the heron cache for all lookups."}

// mineRun runs one command with the given stdin and keeps stdout and stderr apart.
func mineRun(t *testing.T, stdin io.Reader, args ...string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := Run(testCtx(t), args, &out, &errOut, stdin)
	return out.String(), errOut.String(), err
}

func mineArgs(cmd string, extra ...string) []string {
	return append(append([]string{cmd}, mineNote...), extra...)
}

// minePlain saves an ordinary agent note and returns its id.
func minePlain(t *testing.T, extra ...string) string {
	t.Helper()
	title := "Heron cache choice"
	for i := 0; i+1 < len(extra); i++ {
		if extra[i] == "--title" {
			title = extra[i+1]
		}
	}
	cfg := mustConfig(t)
	m, op, err := createMemory(testCtx(t), cfg, Memory{
		Scope: "project:demo", Type: "decision", Title: title,
		Text: "Use the heron cache for all lookups.", Source: "manual", CreatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := indexUpsert(testCtx(t), cfg, m); err != nil {
		t.Fatal(err)
	}
	if authoredWriteProjectionPending(cfg, op) {
		if err := markFTSCommitted(cfg, op); err != nil {
			t.Fatal(err)
		}
	}
	return m.ID
}

func mineHasTag(m Memory) bool {
	for _, tag := range m.Tags {
		if strings.HasPrefix(tag, memory.OwnerConfirmedTagPrefix) {
			return true
		}
	}
	return false
}

func mineTitles(t *testing.T) []string {
	t.Helper()
	rows, err := decodeMemoriesJSON(t, run(t, "list", "--json"))
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, m := range rows {
		titles = append(titles, m.Title)
	}
	return titles
}

func mineSaved(t *testing.T, cfg Config, id string) bool {
	t.Helper()
	return memory.IsOwnerConfirmed(ownerMemory(t, cfg, id))
}

func TestWriteMineSavesOwnerConfirmedAfterYes(t *testing.T) {
	cfg := reviewSetup(t)
	reviewTerminal(t)

	out, errOut, err := mineRun(t, strings.NewReader("YES\n"), append(mineArgs("write", "--mine"), "--json")...)
	if err != nil {
		t.Fatalf("write --mine: %v\n%s", err, errOut)
	}
	// The note is shown on stderr before the question, so stdout stays JSON.
	for _, want := range []string{"project:demo", "Heron cache choice", "Use the heron cache for all lookups.", "Save this as your own statement? [y/N]"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("prompt does not show %q:\n%s", want, errOut)
		}
	}
	var r struct {
		ID             string `json:"id"`
		OwnerConfirmed bool   `json:"owner_confirmed"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil || !r.OwnerConfirmed || !mineSaved(t, cfg, r.ID) {
		t.Fatalf("receipt %q, %v", out, err)
	}

	// Reads: agents see the owner's statement.
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
	for _, want := range []string{"the owner then confirmed this exact text in their own terminal", "Adopted by user: the owner confirmed this exact text"} {
		if !strings.Contains(got, want) {
			t.Fatalf("context_memory lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"needs_review", "not checked against the user's words", "no record either way"} {
		if strings.Contains(got, bad) {
			t.Fatalf("context_memory still says %q:\n%s", bad, got)
		}
	}

	// The human receipt says so too.
	human, _, err := mineRun(t, strings.NewReader("y\n"), mineArgs("write", "--mine")...)
	if err != nil || !strings.Contains(human, "saved as owner-confirmed") {
		t.Fatalf("human receipt %q, %v", human, err)
	}
}

func TestWriteMineSavesNothingWithoutAYes(t *testing.T) {
	reviewSetup(t)
	reviewTerminal(t)
	for _, answer := range []string{"n\n", "no\n", "\n", "", "maybe\n", "yy\n"} {
		_, _, err := mineRun(t, strings.NewReader(answer), mineArgs("write", "--mine")...)
		if err == nil || !strings.Contains(err.Error(), "Not saved.") {
			t.Fatalf("answer %q: err = %v", answer, err)
		}
	}
	if titles := mineTitles(t); len(titles) != 0 {
		t.Fatalf("a declined write saved notes: %v", titles)
	}
}

func TestWriteMineRefusesOutsideTheOwnerTerminal(t *testing.T) {
	reviewSetup(t)
	withUnavailableOwnerVerifier(t)
	clearAgentEnv(t)

	// A pipe and a terminal-looking stdin are not trusted owner authority.
	_, _, err := mineRun(t, strings.NewReader(mineYes), mineArgs("write", "--mine")...)
	if !errors.Is(err, errOwnerVerificationUnavailable) {
		t.Fatalf("pipe: %v", err)
	}
	_, _, err = mineRun(t, nil, mineArgs("write", "--mine")...)
	if !errors.Is(err, errOwnerVerificationUnavailable) {
		t.Fatal("nil stdin was accepted")
	}

	// Each agent variable on its own, with a terminal on stdin.
	reviewTerminal(t)
	for _, key := range rulesAgentEnv {
		func() {
			if err := os.Setenv(key, "1"); err != nil {
				t.Fatal(err)
			}
			defer os.Unsetenv(key)
			_, _, err := mineRun(t, strings.NewReader(mineYes), mineArgs("write", "--mine")...)
			if !errors.Is(err, errOwnerVerificationUnavailable) {
				t.Fatalf("%s set: err = %v", key, err)
			}
		}()
	}
	if titles := mineTitles(t); len(titles) != 0 {
		t.Fatalf("a refused write saved notes: %v", titles)
	}
}

func TestPlainWriteIsNeverOwnerConfirmed(t *testing.T) {
	cfg := reviewSetup(t)
	cfg.MCPWritePolicy = mcpWritePolicyOpen
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	reviewTerminal(t) // even in the owner terminal
	out := run(t, mineArgs("write")...)
	// run merges stderr: the one-line terminal hint is expected, the receipt is the rest.
	var kept []string
	for _, l := range strings.SplitAfter(out, "\n") {
		if !strings.HasPrefix(l, "note: saved as unconfirmed") {
			kept = append(kept, l)
		}
	}
	out = strings.Join(kept, "")
	if strings.Contains(out, "owner-confirmed") || strings.Count(out, "\n") != 1 {
		t.Fatalf("plain write receipt changed:\n%s", out)
	}
	id := minePlain(t)
	if mineHasTag(ownerMemory(t, cfg, id)) {
		t.Fatal("plain write carries an owner-confirmed tag")
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(run(t, append(mineArgs("write"), "--json")...)), &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["owner_confirmed"]; ok {
		t.Fatal("plain write receipt gained owner_confirmed")
	}
}

func TestWriteMineRefusesACallerSuppliedOwnerConfirmedTag(t *testing.T) {
	reviewSetup(t)
	reviewTerminal(t)
	_, _, err := mineRun(t, strings.NewReader(mineYes), mineArgs("write", "--mine", "--tags", "keep,"+memory.OwnerConfirmedTagPrefix+"abc")...)
	if err == nil || !strings.Contains(err.Error(), memory.OwnerConfirmedTagPrefix) {
		t.Fatalf("tag not refused: %v", err)
	}
	if titles := mineTitles(t); len(titles) != 0 {
		t.Fatalf("a refused write saved notes: %v", titles)
	}
}

func TestPlainConfirmIsUnchangedInTheOwnerTerminal(t *testing.T) {
	cfg := reviewSetup(t)
	clearAgentEnv(t)
	plain := minePlain(t)
	tentative := ownerSaved(t, "Tentative heron")
	reviewTerminal(t)

	if _, err := runErr(t, "confirm", plain); err == nil || !strings.Contains(err.Error(), "not tentative") {
		t.Fatalf("plain confirm of a plain note: %v", err)
	}
	if out := run(t, "confirm", tentative); !strings.HasPrefix(out, "confirmed ") {
		t.Fatalf("plain confirm output = %q", out)
	}
	if mineSaved(t, cfg, plain) || mineSaved(t, cfg, tentative) || mineHasTag(ownerMemory(t, cfg, tentative)) {
		t.Fatal("plain confirm made a note owner-confirmed")
	}
}

func TestConfirmMineAsksThenConfersOwnerConfirmed(t *testing.T) {
	cfg := reviewSetup(t)
	clearAgentEnv(t)
	plain := minePlain(t)
	tentative := ownerSaved(t, "Tentative heron")
	reviewTerminal(t)

	for _, id := range []string{plain, tentative} {
		// Declined: nothing changes.
		before, _ := os.ReadFile(ownerMemory(t, cfg, id).Path)
		_, errOut, err := mineRun(t, strings.NewReader("n\n"), "confirm", id, "--mine")
		if err == nil || !strings.Contains(err.Error(), "Not confirmed") || !strings.Contains(errOut, "Confirm this exact text as your own statement? [y/N]") {
			t.Fatalf("%s declined: %v\n%s", id, err, errOut)
		}
		if after, _ := os.ReadFile(ownerMemory(t, cfg, id).Path); !bytes.Equal(before, after) {
			t.Fatalf("%s: a declined confirm changed the file", id)
		}

		out, errOut, err := mineRun(t, strings.NewReader("yes\n"), "confirm", id, "--mine", "--json")
		if err != nil {
			t.Fatalf("%s: %v\n%s", id, err, errOut)
		}
		var r confirmReceipt
		if json.Unmarshal([]byte(out), &r) != nil || !r.OwnerConfirmed || !mineSaved(t, cfg, id) {
			t.Fatalf("%s: receipt %q, owner-confirmed %v", id, out, mineSaved(t, cfg, id))
		}
		if first, _, _ := strings.Cut(ownerMemory(t, cfg, id).Text, "\n"); !strings.Contains(errOut, first) {
			t.Fatalf("%s: the text was not shown:\n%s", id, errOut)
		}
		if got := ownerCall(t, "read_memory", map[string]any{"id": id}); !strings.Contains(got, ownerTrustJSON) {
			t.Fatalf("read_memory of %s:\n%s", id, got)
		}
	}
	if log := readAgentWriteLog(t, cfg); !strings.Contains(log, " owner-confirm "+plain+" ") {
		t.Fatalf("log has no owner-confirm line:\n%s", log)
	}

	// Already confirmed: say so, change nothing.
	before, _ := os.ReadFile(ownerMemory(t, cfg, plain).Path)
	out, _, err := mineRun(t, strings.NewReader(mineYes), "confirm", plain, "--mine")
	if err != nil || !strings.Contains(out, "already owner-confirmed") {
		t.Fatalf("already confirmed: %q, %v", out, err)
	}
	if after, _ := os.ReadFile(ownerMemory(t, cfg, plain).Path); !bytes.Equal(before, after) {
		t.Fatal("confirm --mine rewrote an owner-confirmed note")
	}
}

func TestConfirmMineRefusesOutsideTheOwnerTerminal(t *testing.T) {
	cfg := reviewSetup(t)
	withUnavailableOwnerVerifier(t)
	clearAgentEnv(t)
	id := minePlain(t)
	before, _ := os.ReadFile(ownerMemory(t, cfg, id).Path)

	_, _, err := mineRun(t, strings.NewReader(mineYes), "confirm", id, "--mine")
	if !errors.Is(err, errOwnerVerificationUnavailable) {
		t.Fatalf("pipe: %v", err)
	}
	reviewTerminal(t)
	for _, key := range rulesAgentEnv {
		func() {
			if err := os.Setenv(key, "1"); err != nil {
				t.Fatal(err)
			}
			defer os.Unsetenv(key)
			_, _, err := mineRun(t, strings.NewReader(mineYes), "confirm", id, "--mine")
			if !errors.Is(err, errOwnerVerificationUnavailable) {
				t.Fatalf("%s set: err = %v", key, err)
			}
		}()
	}
	if after, _ := os.ReadFile(ownerMemory(t, cfg, id).Path); !bytes.Equal(before, after) {
		t.Fatal("a refused confirm changed the file")
	}
}

func TestConfirmMineRefusesRecordsThatAreNotAgentNotes(t *testing.T) {
	cfg := reviewSetup(t)
	plant := func(m Memory, rel string) {
		body, err := renderMemory(m)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(cfg.VaultDir, "sources", rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plant(Memory{ID: "gmail_thread/mine1", Scope: "personal", Type: "email", Title: "Mail", Text: "Mail body.", Provider: "gmail", CreatedAt: "2026-11-01T00:00:00Z"}, "gmail/gmail_thread_mine1.md")
	plant(Memory{ID: "src_mine2", Scope: "project:demo", Type: "source", Title: "mine.md", Text: "File body.", Tags: []string{"filesystem", "notes"}, CreatedAt: "2026-11-01T00:00:00Z"}, "filesystem/notes/src_mine2.md")
	run(t, "index", "rebuild")
	reviewTerminal(t)

	for _, id := range []string{"gmail_thread/mine1", "src_mine2"} {
		if _, err := findMemory(cfg, id); err != nil {
			t.Fatalf("fixture %s is not findable: %v", id, err)
		}
		_, errOut, err := mineRun(t, strings.NewReader(mineYes), "confirm", id, "--mine")
		if err == nil || !strings.Contains(err.Error(), "not an agent-written note") || strings.Contains(errOut, "[y/N]") {
			t.Fatalf("%s: err = %v\n%s", id, err, errOut)
		}
		if mineHasTag(ownerMemory(t, cfg, id)) {
			t.Fatalf("%s got an owner-confirmed tag", id)
		}
	}
}

// racyStdin edits the note once, as the owner's answer is read, so the file
// differs from what was shown.
type racyStdin struct {
	path string
	done bool
}

func (r *racyStdin) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		raw, err := os.ReadFile(r.path)
		if err != nil {
			return 0, err
		}
		if err := os.WriteFile(r.path, bytes.Replace(raw, []byte("all lookups"), []byte("no lookups"), 1), 0o644); err != nil {
			return 0, err
		}
	}
	return copy(p, "y\n"), nil
}

func TestConfirmMineRefusesANoteThatChangedAfterItWasShown(t *testing.T) {
	cfg := reviewSetup(t)
	clearAgentEnv(t)
	id := minePlain(t)
	reviewTerminal(t)
	m := ownerMemory(t, cfg, id)

	_, _, err := mineRun(t, &racyStdin{path: m.Path}, "confirm", id, "--mine")
	if err == nil || !strings.Contains(err.Error(), "changed after it was shown") {
		t.Fatalf("err = %v", err)
	}
	if mineSaved(t, cfg, id) || mineHasTag(ownerMemory(t, cfg, id)) {
		t.Fatal("a changed note was confirmed")
	}
}

func TestEditingAnOwnerWrittenNoteDropsTheStatus(t *testing.T) {
	cfg := reviewSetup(t)
	reviewTerminal(t)
	out, _, err := mineRun(t, strings.NewReader(mineYes), append(mineArgs("write", "--mine"), "--json")...)
	if err != nil {
		t.Fatal(err)
	}
	var r struct{ ID string }
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	m := ownerMemory(t, cfg, r.ID)
	if !memory.IsOwnerConfirmed(m) {
		t.Fatal("note is not owner-confirmed before the edit")
	}
	raw, _ := os.ReadFile(m.Path)
	edited := strings.Replace(string(raw), "all lookups", "no lookups", 1)
	if edited == string(raw) {
		t.Fatalf("edit did not apply:\n%s", raw)
	}
	if err := os.WriteFile(m.Path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if mineSaved(t, cfg, r.ID) {
		t.Fatal("an edited note is still owner-confirmed")
	}
	var read struct{ Trust string }
	if err := json.Unmarshal([]byte(run(t, "read", r.ID, "--json")), &read); err != nil || read.Trust != "" {
		t.Fatalf("read --json trust = %q, %v", read.Trust, err)
	}
}

// mineDecision writes one decision and returns its id. owner picks --mine.
func mineDecision(t *testing.T, owner bool, extra ...string) string {
	t.Helper()
	args := append([]string{"write", "--scope", "project:demo", "--type", "decision",
		"--title", "Heron cache decision", "--text", "Use the heron cache."}, extra...)
	if owner {
		out, errOut, err := mineRun(t, strings.NewReader(mineYes), append(args, "--mine", "--json")...)
		if err != nil {
			t.Fatalf("%v\n%s", err, errOut)
		}
		var r struct{ ID string }
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	var r struct{ ID string }
	if err := json.Unmarshal([]byte(run(t, append(args, "--json")...)), &r); err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func mineDecisionStatus(t *testing.T, id string) string {
	t.Helper()
	var read struct {
		DecisionStatus string `json:"decision_status"`
	}
	if err := json.Unmarshal([]byte(run(t, "read", id, "--json")), &read); err != nil {
		t.Fatal(err)
	}
	return read.DecisionStatus
}

func mineDecisionContext(t *testing.T) string {
	t.Helper()
	// The tool result is JSON, so line breaks arrive escaped.
	return strings.ReplaceAll(ownerCall(t, "context_memory", map[string]any{"query": "heron cache decision"}), `\n`, "\n")
}

func TestOwnerDecisionWithoutValidityReadsCurrent(t *testing.T) {
	reviewSetup(t)
	reviewTerminal(t)

	// Not the owner's: unchanged, needs review.
	plain := mineDecision(t, false)
	if got := mineDecisionStatus(t, plain); got != decisionNeedsReview {
		t.Fatalf("plain decision status = %q", got)
	}
	if got := mineDecisionContext(t); !strings.Contains(got, "Decision status: needs_review") || !strings.Contains(got, "Durability: provisional") {
		t.Fatalf("plain decision context changed:\n%s", got)
	}

	// The owner's own decision, no validity chosen: current.
	id := mineDecision(t, true)
	if got := mineDecisionStatus(t, id); got != decisionCurrent {
		t.Fatalf("owner decision status = %q", got)
	}
	if got := ownerCall(t, "read_memory", map[string]any{"id": id}); !strings.Contains(got, `"decision_status":"current"`) {
		t.Fatalf("read_memory:\n%s", got)
	}
	got := mineDecisionContext(t)
	owned := got[strings.Index(got, "Decision status: current"):]
	if !strings.Contains(got, "Decision status: current (owner-confirmed; no review date set)\nAs of: ") {
		t.Fatalf("context lacks the current line:\n%s", got)
	}
	// The owner block carries no defaulted durability or empty flip line.
	if end := strings.Index(owned, "Use the heron cache."); end < 0 || strings.Contains(owned[:end], "Durability:") || strings.Contains(owned[:end], "Flip conditions:") {
		t.Fatalf("owner decision block shows defaulted fields:\n%s", owned)
	}

	// A review date that has passed still needs review, owner-confirmed or not.
	past := mineDecision(t, true, "--review-by", time.Now().Add(-48*time.Hour).Format(time.RFC3339))
	if got := mineDecisionStatus(t, past); got != decisionNeedsReview {
		t.Fatalf("passed review date status = %q", got)
	}
	complete := mineDecision(t, true, "--durability", "standing", "--flip-conditions", "the team drops it", "--review-by", time.Now().Add(-48*time.Hour).Format(time.RFC3339))
	if got := mineDecisionStatus(t, complete); got != decisionNeedsReview {
		t.Fatalf("complete decision past its review date = %q", got)
	}
}

// The digest list reads the same field, so it follows the status.
func TestOwnerDecisionStatusIsOneFieldForEveryReader(t *testing.T) {
	m := Memory{ID: "mem_1", Scope: "project:demo", Type: "decision", Title: "T", Text: "body", Source: "manual", CreatedAt: "2026-11-10T00:00:00Z",
		Decision: decisionValidityFromFlags("2026-11-10T00:00:00Z", "", "", "", "")}
	if got := decorateDecision(m, time.Now()).DecisionStatus; got != decisionNeedsReview {
		t.Fatalf("not confirmed: %q", got)
	}
	m.Tags = []string{memory.OwnerConfirmedTagPrefix + memory.OwnerConfirmedDigest(m)}
	if got := decorateDecision(m, time.Now()).DecisionStatus; got != decisionCurrent {
		t.Fatalf("owner-confirmed: %q", got)
	}
	m.Decision.FlipConditions = nil
	m.Decision.ReviewBy = "2020-01-01T00:00:00Z"
	m.Tags = []string{memory.OwnerConfirmedTagPrefix + memory.OwnerConfirmedDigest(m)}
	if got := decorateDecision(m, time.Now()).DecisionStatus; got != decisionNeedsReview {
		t.Fatalf("past review date: %q", got)
	}
}

// Only a complete line counts: "y" then EOF must not be taken as yes.
func TestMineAcceptsYesOnlyAsACompleteLine(t *testing.T) {
	cfg := reviewSetup(t)
	clearAgentEnv(t)
	reviewTerminal(t)

	for _, in := range []string{"y", "yes", "", "\n", "maybe\n"} {
		before := len(mineTitles(t))
		if _, _, err := mineRun(t, strings.NewReader(in), mineArgs("write", "--mine")...); err == nil {
			t.Fatalf("write --mine with %q: want non-zero result", in)
		}
		if after := len(mineTitles(t)); after != before {
			t.Fatalf("write --mine with %q saved a note", in)
		}
		id := minePlain(t, "--title", "Plain "+strings.TrimSpace(in)+"x")
		if _, _, err := mineRun(t, strings.NewReader(in), "confirm", id, "--mine"); err == nil || mineSaved(t, cfg, id) {
			t.Fatalf("confirm --mine with %q: want not confirmed", in)
		}
	}
	if _, _, err := mineRun(t, strings.NewReader("yes\n"), mineArgs("write", "--mine")...); err != nil {
		t.Fatalf("write --mine yes\\n: %v", err)
	}
	id := minePlain(t, "--title", "Plain ok")
	if _, _, err := mineRun(t, strings.NewReader("yes\n"), "confirm", id, "--mine"); err != nil || !mineSaved(t, cfg, id) {
		t.Fatalf("confirm --mine yes\\n: %v", err)
	}
}
