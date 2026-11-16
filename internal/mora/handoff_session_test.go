package mora

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/leasefile"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func sessionGit(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func newHandoffSessionConfig(t *testing.T, policy string) Config {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	cfg.MCPWritePolicy = policy
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newHandoffSessionReceipt(t *testing.T, cfg Config) handoffSessionReceipt {
	t.Helper()
	root := t.TempDir()
	if _, _, err := bindProjectDir(testCtx(t), root, "project:demo"); err != nil {
		t.Fatal(err)
	}
	_, root, _, _ = lookupProject(cfg, root)
	transcript := filepath.Join(cfg.HomeDir(), ".claude", "projects", filepath.Base(root), "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0700); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]any{"type": "user", "sessionId": "session-1", "cwd": root, "message": map[string]any{"content": "Implement the focused fix"}})
	if err := os.WriteFile(transcript, append(line, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	transcript, _ = handoffSessionResolvedTranscript(cfg, transcript)
	r, err := startHandoffSession(cfg, "claude", "session-1", root, "project:demo", transcript)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHandoffSessionSavedAgentHandoffAndPendingProposalDeduplicate(t *testing.T) {
	cfg := newHandoffSessionConfig(t, mcpWritePolicyOpen)
	r := newHandoffSessionReceipt(t, cfg)
	// CreatedAt intentionally drops sub-second precision. Exact source wins even
	// though receipt start can be later within the same second.
	m := Memory{ID: "mem_agent", Scope: r.Scope, Type: memory.TypeHandoff, Source: handoffSessionSource(r.Token), Title: "Agent handoff", Text: "saved", CreatedAt: hookNow().UTC().Format(time.RFC3339)}
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
	got := scanHandoffAssociation(testCtx(t), cfg, r)
	if got.state != "match" || got.memoryID != m.ID {
		t.Fatalf("same-second agent handoff = %#v", got)
	}
	if captured := handoffSessionCapture(testCtx(t), cfg, r); captured.Outcome != "saved" || captured.MemoryID != m.ID {
		t.Fatalf("capture should use saved handoff: %#v", captured)
	}
}

func TestHandoffSessionUnknownScanMakesNoMutation(t *testing.T) {
	cfg := newHandoffSessionConfig(t, mcpWritePolicyOpen)
	r := newHandoffSessionReceipt(t, cfg)
	dir := handoffSessionScopeDir(cfg, r.Scope)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= handoffSessionScopeEntries; i++ {
		if err := os.WriteFile(filepath.Join(dir, "cap-"+string(rune('a'+i%26))+"-"+time.Now().Add(time.Duration(i)).Format("150405.000000000")+".md"), []byte("---\nid: x\n---\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := handoffSessionCapture(testCtx(t), cfg, r)
	if got.Outcome != "association_unknown" || got.Uncertainty != "scan_incomplete" {
		t.Fatalf("cap outcome = %#v", got)
	}
	if _, err := findMemoryRaw(cfg, handoffSessionIntent(r.Token)); err == nil {
		t.Fatal("unknown scan must not publish a machine handoff")
	}
}

func TestHandoffSessionOrdinaryNoteBlocksIdleNotCapture(t *testing.T) {
	cfg := newHandoffSessionConfig(t, mcpWritePolicyOpen)
	r := newHandoffSessionReceipt(t, cfg)
	m := Memory{ID: "mem_other", Scope: r.Scope, Type: "fact", Source: "mcp", Title: "Other session", Text: "unassociated", CreatedAt: hookNow().UTC().Format(time.RFC3339)}
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
	assoc := scanHandoffAssociation(testCtx(t), cfg, r)
	if assoc.state != "none" || !assoc.otherNote {
		t.Fatalf("ordinary note association = %#v", assoc)
	}
	got := handoffSessionCapture(testCtx(t), cfg, r)
	if got.Outcome != "saved" || got.MemoryID != handoffSessionIntent(r.Token) {
		t.Fatalf("ordinary note must not suppress capture: %#v", got)
	}
}

func TestHandoffSessionPolicyTighteningAndLockTimeout(t *testing.T) {
	t.Run("tightening", func(t *testing.T) {
		cfg := newHandoffSessionConfig(t, mcpWritePolicyReadonly)
		r := newHandoffSessionReceipt(t, cfg)
		r.StartPolicy = mcpWritePolicyOpen
		if got := handoffSessionCapture(testCtx(t), cfg, r); got.Outcome != "refused" || got.Uncertainty != "policy" {
			t.Fatalf("tightened policy = %#v", got)
		}
	})
	t.Run("open still holds unsafe assembled content", func(t *testing.T) {
		unsafe := Memory{Scope: "project:demo", Type: memory.TypeHandoff, Title: "Handoff", Text: "token ghp_abcdefghijklmnopqrstuvwxyz123456"}
		if handoffSessionContentAllowed(unsafe) {
			t.Fatal("open-mode capture must refuse content held by the tentative safety gate")
		}
	})
	t.Run("lock timeout", func(t *testing.T) {
		cfg := newHandoffSessionConfig(t, mcpWritePolicyOpen)
		r := newHandoffSessionReceipt(t, cfg)
		path := handoffSessionPath(cfg, r.Client, r.SessionID, r.Root)
		guard := leasefile.GuardPath(path)
		if err := os.MkdirAll(filepath.Dir(guard), 0o700); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(guard, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := leasefile.TryLock(f); err != nil {
			t.Fatal(err)
		}
		defer leasefile.Unlock(f)
		old := handoffSessionEndDeadline
		handoffSessionEndDeadline = 20 * time.Millisecond
		t.Cleanup(func() { handoffSessionEndDeadline = old })
		if got := handoffSessionCapture(testCtx(t), cfg, r); got.Outcome != "timeout" || got.Uncertainty != "lease_busy" {
			t.Fatalf("lock timeout = %#v", got)
		}
	})
}

func TestHandoffSessionProposeDeduplicatesAndRefusalStaysFinal(t *testing.T) {
	t.Run("proposal", func(t *testing.T) {
		cfg := newHandoffSessionConfig(t, mcpWritePolicyPropose)
		r := newHandoffSessionReceipt(t, cfg)
		first := handoffSessionCapture(testCtx(t), cfg, r)
		if first.Outcome != "pending" || first.ProposalID == "" {
			t.Fatalf("first propose = %#v", first)
		}
		second := handoffSessionCapture(testCtx(t), cfg, first)
		if second.Outcome != "pending" || second.ProposalID != first.ProposalID {
			t.Fatalf("repeated propose = %#v", second)
		}
	})
	t.Run("refusal final", func(t *testing.T) {
		cfg := newHandoffSessionConfig(t, mcpWritePolicyReadonly)
		r := newHandoffSessionReceipt(t, cfg)
		r.StartPolicy = mcpWritePolicyOpen
		refused := handoffSessionCapture(testCtx(t), cfg, r)
		if refused.Outcome != "refused" {
			t.Fatalf("refusal = %#v", refused)
		}
		cfg.MCPWritePolicy = mcpWritePolicyOpen
		if err := writeConfig(cfg); err != nil {
			t.Fatal(err)
		}
		if repeated := handoffSessionCapture(testCtx(t), cfg, refused); repeated.Outcome != "refused" {
			t.Fatalf("loosened repeated end reopened refusal: %#v", repeated)
		}
	})
}

func TestHandoffSessionRepeatedEndAndResumedStartGeneration(t *testing.T) {
	cfg := newHandoffSessionConfig(t, mcpWritePolicyOpen)
	r := newHandoffSessionReceipt(t, cfg)
	first := handoffSessionCapture(testCtx(t), cfg, r)
	if first.Outcome != "saved" {
		t.Fatalf("first end = %#v", first)
	}
	second := handoffSessionCapture(testCtx(t), cfg, first)
	if second.Outcome != "saved" || second.MemoryID != first.MemoryID {
		t.Fatalf("repeated end = %#v", second)
	}
	if err := saveHandoffSession(handoffSessionPath(cfg, r.Client, r.SessionID, r.Root), first); err != nil {
		t.Fatal(err)
	}
	resumed, err := startHandoffSession(cfg, r.Client, r.SessionID, r.Root, r.Scope, r.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Token == r.Token || resumed.IntentID != "" {
		t.Fatalf("resumed generation = %#v, original=%#v", resumed, r)
	}
}

func TestHandoffSessionClaudeStartEndNewStart(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	root := t.TempDir()
	if _, _, err := bindProjectDir(testCtx(t), root, "project:demo"); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(cfg.HomeDir(), ".claude", "projects", "fixture", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	line, _ := json.Marshal(map[string]any{"type": "user", "cwd": root, "session_id": "session-h3", "message": map[string]any{"content": "Please make the focused change."}})
	if err := os.WriteFile(transcript, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if resolved, ok := handoffSessionResolvedTranscript(cfg, transcript); !ok || resolved == "" {
		t.Fatalf("transcript resolution = %q, %v", resolved, ok)
	}
	_, boundRoot, found, err := lookupProject(cfg, root)
	if err != nil || !found {
		t.Fatalf("bound root = %q, %v, %v", boundRoot, found, err)
	}
	input := `{"cwd":` + quoteJSON(root) + `,"transcript_path":` + quoteJSON(transcript) + `,"session_id":"session-h3"}`
	var start bytes.Buffer
	if err := cmdHook(testCtx(t), []string{"session-start", "--project-dir", root}, &start, testStderr, strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if got := decodeHookOutput(t, start.String()).HookSpecificOutput.AdditionalContext; !strings.Contains(got, "source handoff-session:") {
		t.Fatalf("start context = %q", got)
	}
	path := handoffSessionPath(cfg, "claude", "session-h3", boundRoot)
	first, err := loadHandoffSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cmdHook(testCtx(t), []string{"session-end", "--project-dir", root}, io.Discard, testStderr, strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	ended, err := loadHandoffSession(path)
	if err != nil || ended.Outcome != "saved" {
		t.Fatalf("end receipt = %#v, %v", ended, err)
	}
	var resumed bytes.Buffer
	if err := cmdHook(testCtx(t), []string{"session-start", "--project-dir", root}, &resumed, testStderr, strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	last, err := loadHandoffSession(path)
	if err != nil || last.Token == first.Token || last.Outcome != "" {
		t.Fatalf("resumed receipt = %#v, %v", last, err)
	}
}

func TestHandoffSessionStatusOmitsCorrelationData(t *testing.T) {
	cfg := newHandoffSessionConfig(t, mcpWritePolicyOpen)
	r := newHandoffSessionReceipt(t, cfg)
	r.Outcome, r.MemoryID = "saved", "mem_handoff_status"
	if err := saveHandoffSession(handoffSessionPath(cfg, r.Client, r.SessionID, r.Root), r); err != nil {
		t.Fatal(err)
	}
	rows, complete := handoffSessionStatuses(cfg)
	if !complete || len(rows) != 1 || rows[0].MemoryID != r.MemoryID {
		t.Fatalf("status = %#v, complete=%v", rows, complete)
	}
	body, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), r.Token) || strings.Contains(string(body), r.SessionID) || strings.Contains(string(body), r.TranscriptPath) {
		t.Fatalf("status leaked receipt correlation data: %s", body)
	}
}

func TestHandoffSessionGitDetectsTrackedAndUntrackedEdits(t *testing.T) {
	root := t.TempDir()
	sessionGit(t, root, "init")
	sessionGit(t, root, "config", "user.email", "test@example.invalid")
	sessionGit(t, root, "config", "user.name", "Mora Test")
	tracked := filepath.Join(root, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionGit(t, root, "add", "tracked.txt")
	sessionGit(t, root, "commit", "-m", "base")
	_, before, ok := handoffSessionGit(testCtx(t), root)
	if !ok {
		t.Fatal("baseline git observation failed")
	}
	if err := os.WriteFile(tracked, []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, changed, ok := handoffSessionGit(testCtx(t), root)
	if !ok || changed == before {
		t.Fatalf("tracked edit not observed: before=%q changed=%q ok=%v", before, changed, ok)
	}
	untracked := filepath.Join(root, "new.txt")
	if err := os.WriteFile(untracked, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, firstUntracked, ok := handoffSessionGit(testCtx(t), root)
	if err := os.WriteFile(untracked, []byte("longer content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, secondUntracked, ok := handoffSessionGit(testCtx(t), root)
	if !ok || firstUntracked == secondUntracked {
		t.Fatalf("untracked edit not observed: %q == %q", firstUntracked, secondUntracked)
	}
}
