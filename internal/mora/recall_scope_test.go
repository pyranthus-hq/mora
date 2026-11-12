package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

const rsQuery = "quillfish rule"

// rsMark is the text a note carries so a test can find it in hook output.
func rsMark(id string) string { return "MARK_" + strings.ToUpper(id) }

// rsSeed writes one owner-confirmed decision per scope, all matching rsQuery.
func rsSeed(t *testing.T, cfg Config) {
	t.Helper()
	for i, n := range []struct{ id, scope string }{
		{"g", "global"}, {"p", "personal"}, {"a", "project:a"}, {"b", "project:b"},
	} {
		pbOwner(t, cfg, "mem_"+n.id, n.scope, "decision", "Quillfish "+n.id, rsQuery+" "+rsMark(n.id), i+1)
	}
	run(t, "index", "rebuild")
}

func rsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// rsRecall runs the hook recall form given by args for one prompt in cwd.
func rsRecall(t *testing.T, cwd string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	in := fmt.Sprintf(`{"prompt":%s,"cwd":%s}`, quoteJSON(rsQuery), quoteJSON(cwd))
	if err := cmdHook(testCtx(t), append([]string{"recall"}, args...), &out, testStderr, strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func rsWant(t *testing.T, got string, present, absent []string) {
	t.Helper()
	for _, id := range present {
		if !strings.Contains(got, rsMark(id)) {
			t.Errorf("note %s missing from:\n%s", id, got)
		}
	}
	for _, id := range absent {
		if strings.Contains(got, rsMark(id)) {
			t.Errorf("note %s must not appear in:\n%s", id, got)
		}
	}
}

func TestRecallScopeBoundFolder(t *testing.T) {
	cfg := projectBriefSetup(t)
	rsSeed(t, cfg)
	dir := rsDir(t)
	run(t, "project", "bind", "--scope", "project:a", dir)

	// Recall: project a and global; never project b; never personal.
	rsWant(t, rsRecall(t, filepath.Join(dir, "sub")), []string{"g", "a"}, []string{"b", "p"})

	// Session start: the project brief reads project a and global.
	brief := pbHook(t, dir)
	for _, want := range []string{"mem_g", "mem_a"} {
		if !strings.Contains(brief, want) {
			t.Errorf("session start missing %s:\n%s", want, brief)
		}
	}
	for _, bad := range []string{"mem_b", "mem_p"} {
		if strings.Contains(brief, bad) {
			t.Errorf("session start must not show %s:\n%s", bad, brief)
		}
	}

	// owner_only adds personal, and list and which show the flag.
	out := run(t, "project", "bind", "--scope", "project:a", "--owner-only", dir)
	if !strings.Contains(out, "owner-only") {
		t.Fatalf("bind output = %q", out)
	}
	rsWant(t, rsRecall(t, dir), []string{"g", "a", "p"}, []string{"b"})
	var listed []projectBinding
	if err := json.Unmarshal([]byte(run(t, "project", "list", "--json")), &listed); err != nil || len(listed) != 1 || !listed[0].OwnerOnly {
		t.Fatalf("list --json = %v, %v", listed, err)
	}
	var which projectBinding
	if err := json.Unmarshal([]byte(run(t, "project", "which", "--json", dir)), &which); err != nil || !which.OwnerOnly {
		t.Fatalf("which --json = %v, %v", which, err)
	}

	// A plain rebind clears the flag.
	run(t, "project", "bind", "--scope", "project:a", dir)
	rsWant(t, rsRecall(t, dir), []string{"g", "a"}, []string{"b", "p"})
}

func TestRecallScopeUnboundFolder(t *testing.T) {
	cfg := projectBriefSetup(t)
	rsSeed(t, cfg)
	// Project a is bound elsewhere; the folder under test is not bound.
	run(t, "project", "bind", "--scope", "project:a", rsDir(t))
	rsWant(t, rsRecall(t, rsDir(t)), []string{"g", "p"}, []string{"a", "b"})
	// No cwd at all is treated as unbound.
	rsWant(t, rsRecall(t, ""), []string{"g", "p"}, []string{"a", "b"})
}

func TestRecallScopeInstallFormIncludesGlobal(t *testing.T) {
	cfg := projectBriefSetup(t)
	rsSeed(t, cfg)
	root := rsDir(t)
	args := []string{"--scope", "project:a", "--project-dir", root}
	rsWant(t, rsRecall(t, root, args...), []string{"g", "a"}, []string{"b", "p"})
	// Outside the project dir it stays silent.
	if got := rsRecall(t, rsDir(t), args...); got != "" {
		t.Fatalf("outside the project dir: %s", got)
	}
	// An owner_only binding for the dir adds personal.
	run(t, "project", "bind", "--scope", "project:a", "--owner-only", root)
	rsWant(t, rsRecall(t, root, args...), []string{"g", "a", "p"}, []string{"b"})
}

func TestRecallScopeCorruptBindingsFailClosed(t *testing.T) {
	cfg := projectBriefSetup(t)
	rsSeed(t, cfg)
	dir := rsDir(t)
	run(t, "project", "bind", "--scope", "project:a", dir)
	if err := os.WriteFile(projectBindingsPath(cfg), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := len(givenLinesOf(t, cfg))
	for _, args := range [][]string{nil, {"--scope", "project:a", "--project-dir", dir}} {
		if got := rsRecall(t, dir, args...); got != "" {
			t.Fatalf("recall %v with a corrupt bindings file wrote %q", args, got)
		}
	}
	if after := len(givenLinesOf(t, cfg)); after != before {
		t.Fatalf("recall wrote %d given lines on a bindings error", after-before)
	}
}

func TestRecallScopeOldBindingsFileLoads(t *testing.T) {
	cfg := projectBriefSetup(t)
	rsSeed(t, cfg)
	dir := rsDir(t)
	old := fmt.Sprintf(`{"bindings":[{"dir":%s,"scope":"project:a"}]}`, quoteJSON(dir))
	if err := os.WriteFile(projectBindingsPath(cfg), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := loadProjectBindings(cfg)
	if err != nil || len(list) != 1 || list[0].OwnerOnly {
		t.Fatalf("old file: %v, %v", list, err)
	}
	rsWant(t, rsRecall(t, dir), []string{"g", "a"}, []string{"b", "p"})
}

func TestRecallScopeDeliberateSearchUnchangedByBinding(t *testing.T) {
	cfg := projectBriefSetup(t)
	rsSeed(t, cfg)
	dir := rsDir(t)
	t.Chdir(dir)
	ids := func() string {
		sc := filtersStructured(t, "search_memory", fmt.Sprintf(`{"query":%s,"limit":10}`, quoteJSON(rsQuery)))
		results, _ := sc["results"].([]any)
		got := filterResultIDs(t, results)
		return strings.Join(got, ",")
	}
	cliBefore, mcpBefore := run(t, "search", rsQuery), ids()
	for _, id := range []string{"mem_a", "mem_b", "mem_g", "mem_p"} {
		if !strings.Contains(mcpBefore, id) {
			t.Fatalf("a deliberate search must find %s: %s", id, mcpBefore)
		}
	}
	run(t, "project", "bind", "--scope", "project:a", "--owner-only", dir)
	if cliAfter := run(t, "search", rsQuery); cliAfter != cliBefore {
		t.Fatalf("mora search changed after a binding:\nbefore %s\nafter %s", cliBefore, cliAfter)
	}
	if mcpAfter := ids(); mcpAfter != mcpBefore {
		t.Fatalf("search_memory changed after a binding: %s vs %s", mcpBefore, mcpAfter)
	}
}

// rsSeedCrowd writes 50 strong project:other notes and one weak project:demo
// note for the same query, plus a strong global note outside the allowed set.
func rsSeedCrowd(t *testing.T, cfg Config) {
	t.Helper()
	for i := 0; i < 50; i++ {
		m := Memory{
			ID: fmt.Sprintf("other-%02d", i), Scope: "project:other", Type: "decision",
			Title: fmt.Sprintf("other-%02d", i), CreatedAt: "2026-06-01T00:00:00Z",
			Text: "quorvenal quorvenal quorvenal quorvenal crowd filler",
		}
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeMemory(cfg, Memory{
		ID: "demo-weak", Scope: "project:demo", Type: "decision", Title: "demo-weak",
		CreatedAt: "2026-06-02T00:00:00Z", Text: "a lone quorvenal mention",
	}); err != nil {
		t.Fatal(err)
	}
	mustRebuild(t, cfg)
}

func rsIDs(mems []Memory) []string {
	out := make([]string, len(mems))
	for i, m := range mems {
		out[i] = m.ID
	}
	return out
}

func rsDemoOnly(t *testing.T, path string, mems []Memory) {
	t.Helper()
	got := rsIDs(mems)
	if len(got) != 1 || got[0] != "demo-weak" {
		t.Fatalf("%s: want only demo-weak past 50 project:other matches at limit 2, got %v", path, got)
	}
}

func TestRecallScopePreRankProofStaticPath(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	rsSeedCrowd(t, cfg)
	f := searchFilters{Scopes: []string{"project:demo", "global"}}
	mems, err := hookRecallSearch(context.Background(), cfg, "quorvenal", "", 2, f)
	if err != nil {
		t.Fatal(err)
	}
	rsDemoOnly(t, "static", mems)
	// The unbound set drops project notes in the same pre-rank way.
	mems, err = hookRecallSearch(context.Background(), cfg, "quorvenal", "", 2, searchFilters{NoProjectScopes: true})
	if err != nil || len(mems) != 0 {
		t.Fatalf("unbound set returned %v, %v", rsIDs(mems), err)
	}
}

func TestRecallScopePreRankProofHybridPath(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	rsSeedCrowd(t, cfg)
	mems, err := hybridSearch(context.Background(), cfg, "quorvenal", "", 2, searchFilters{Scopes: []string{"project:demo", "global"}})
	if err != nil {
		t.Fatal(err)
	}
	rsDemoOnly(t, "hybrid", mems)
}

func TestRecallScopePreRankProofSegmentArm(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	seed := func(id, scope, body string) {
		t.Helper()
		sender := id + "@example.com"
		if err := writeMemory(cfg, Memory{
			ID: "gmail_thread/" + id, Scope: scope, Type: "email", Source: "gmail",
			Provider: "gmail", ProviderID: "thread/" + id, Title: id, CreatedAt: "2026-06-15T09:00:00Z",
			Text: gmailSegJoinBody([2]string{sender, body}),
			Meta: map[string]any{"from": []string{sender}, "messages": gmailSegMessages(commitmentMessageEvidence{
				MessageRef: "gmail_thread/" + id + "#msg-000", Sender: sender, At: "2026-06-15T09:00:00Z", BlockRefs: []string{"body"},
			})},
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 50; i++ {
		seed(fmt.Sprintf("other-%02d", i), "project:other", "zibbermark zibbermark zibbermark zibbermark")
	}
	seed("demo-weak", "project:demo", "one zibbermark among many other words here")
	mustRebuild(t, cfg)
	db, err := openIndexRO(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f := searchFilters{Scopes: []string{"project:demo", "global"}}
	ids, _, err := gmailSegmentQueryArmBounded(context.Background(), db, "zibbermark", "", 2, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "gmail_thread/demo-weak" {
		t.Fatalf("segment arm: want only the demo thread past 50 other-project threads at pool 2, got %v", ids)
	}
}

// The unfiltered brief shows an authored project note; the hook's unbound
// path leaves it out and keeps the global and personal ones.
func TestSessionStartDailyBriefLeavesOutProjectNotes(t *testing.T) {
	cfg := projectBriefSetup(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for _, n := range []struct{ id, scope string }{{"g", "global"}, {"p", "personal"}, {"a", "project:a"}} {
		pbSeed(t, cfg, "mem_"+n.id, n.scope, "decision", "Brief "+rsMark(n.id), "authored body "+n.id, 10,
			func(m *Memory) { m.Source = "manual" })
	}
	run(t, "index", "rebuild")

	plain, _, err := resolveBrief(cfg, now, briefOpts{})
	if err != nil || !strings.Contains(plain, rsMark("a")) {
		t.Fatalf("fixture: the unfiltered brief must show the project note (%v):\n%s", err, plain)
	}

	prevNow, prevBrief := hookNow, hookResolveBrief
	hookNow, hookResolveBrief = func() time.Time { return now }, resolveBrief
	defer func() { hookNow, hookResolveBrief = prevNow, prevBrief }()
	for _, cwd := range []string{rsDir(t), ""} {
		var out bytes.Buffer
		in := fmt.Sprintf(`{"source":"startup","cwd":%s}`, quoteJSON(cwd))
		if err := cmdHook(testCtx(t), []string{"session-start"}, &out, testStderr, strings.NewReader(in)); err != nil {
			t.Fatal(err)
		}
		got := decodeHookOutput(t, out.String()).HookSpecificOutput.AdditionalContext
		rsWant(t, got, []string{"g", "p"}, []string{"a"})
	}
}

// The rule is by scope, not by origin: a mail record in a project scope is
// left out of the unbound brief, and one in another scope stays.
func TestSessionStartDailyBriefLeavesOutProjectMail(t *testing.T) {
	cfg := projectBriefSetup(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for _, n := range []struct{ id, scope string }{{"mail-a", "project:a"}, {"mail-p", "personal"}} {
		if err := writeMemory(cfg, Memory{
			ID: n.id, Scope: n.scope, Type: "email", Source: "gmail", Provider: "gmail",
			ProviderID: "thread/" + n.id, Title: "Mail " + rsMark(n.id), CreatedAt: now.Add(-time.Hour).Format(time.RFC3339),
			Text: "a mail body",
		}); err != nil {
			t.Fatal(err)
		}
	}
	d, err := buildDigest(cfg, now, briefOpts{sinceHours: 24, excludeProjectScopes: true})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), rsMark("mail-a")) || !strings.Contains(string(b), rsMark("mail-p")) {
		t.Fatalf("project mail must be out and other mail in: %s", b)
	}
}

// A fresh cached brief can hold project records, so an unbound session start
// must not serve it.
func TestSessionStartDailyBriefNeverServesCachedBody(t *testing.T) {
	cfg := projectBriefSetup(t)
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	seedBriefFile(t, cfg, now.Format("2006-01-02"), "# Brief\nMARK_CACHED_PROJECT_A is in project:a\n")
	if body, generated, err := resolveBrief(cfg, now, briefOpts{}); err != nil || generated || !strings.Contains(body, "MARK_CACHED_PROJECT_A") {
		t.Fatalf("fixture: the plain brief must serve the cache (%v, %v): %s", generated, err, body)
	}
	prevNow, prevBrief := hookNow, hookResolveBrief
	hookNow, hookResolveBrief = func() time.Time { return now }, resolveBrief
	defer func() { hookNow, hookResolveBrief = prevNow, prevBrief }()
	var out bytes.Buffer
	if err := cmdHook(testCtx(t), []string{"session-start"}, &out, testStderr, strings.NewReader(fmt.Sprintf(`{"source":"startup","cwd":%s}`, quoteJSON(rsDir(t))))); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "MARK_CACHED_PROJECT_A") {
		t.Fatalf("the cached brief was served in an unbound folder: %s", out.String())
	}
}

func TestRecallScopeBindingOfCwdDecides(t *testing.T) {
	cfg := projectBriefSetup(t)
	rsSeed(t, cfg)
	work := rsDir(t)
	client := filepath.Join(work, "client")
	if err := os.MkdirAll(client, 0o755); err != nil {
		t.Fatal(err)
	}
	args := []string{"--scope", "project:a", "--project-dir", work}

	// Install-time scope only: project a and global, never personal.
	rsWant(t, rsRecall(t, work, args...), []string{"g", "a"}, []string{"b", "p"})

	// A nested binding to b wins over the installed scope a.
	run(t, "project", "bind", "--scope", "project:b", client)
	rsWant(t, rsRecall(t, client, args...), []string{"g", "b"}, []string{"a", "p"})

	// An owner-only parent does not give personal notes inside a plain nested binding.
	run(t, "project", "bind", "--scope", "project:a", "--owner-only", work)
	rsWant(t, rsRecall(t, work, args...), []string{"g", "a", "p"}, []string{"b"})
	rsWant(t, rsRecall(t, client, args...), []string{"g", "b"}, []string{"a", "p"})

	// Rebinding the directory takes effect without a reinstall.
	run(t, "project", "unbind", client)
	run(t, "project", "bind", "--scope", "project:b", work)
	rsWant(t, rsRecall(t, work, args...), []string{"g", "b"}, []string{"a", "p"})
}

func TestRecallScopeBindingsStoreReadFirst(t *testing.T) {
	cfg := projectBriefSetup(t)
	rsSeed(t, cfg)
	dir := rsDir(t)
	before := len(givenLinesOf(t, cfg))
	// Readable store: an empty or relative cwd gets the unbound rule.
	for _, cwd := range []string{"", "."} {
		rsWant(t, rsRecall(t, cwd), []string{"g", "p"}, []string{"a", "b"})
	}
	if len(givenLinesOf(t, cfg)) == before {
		t.Fatal("fixture: a readable store must recall and log")
	}
	if err := os.WriteFile(projectBindingsPath(cfg), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	before = len(givenLinesOf(t, cfg))
	for _, cwd := range []string{"", ".", dir} {
		if got := rsRecall(t, cwd); got != "" {
			t.Fatalf("cwd %q with an unreadable store wrote %q", cwd, got)
		}
	}
	if after := len(givenLinesOf(t, cfg)); after != before {
		t.Fatalf("recall logged %d lines on a bindings error", after-before)
	}
}

const rsHealthMarker = "PRIVATE_FOREIGN_ACCOUNT_QX"

func rsBreakGmail(t *testing.T, cfg Config) {
	t.Helper()
	enableSources(t, cfg, "gmail")
	seedSyncStatusFull(t, cfg, "gmail", &memory.SyncStatus{
		Source: "gmail", LastAttemptAt: time.Now().UTC().Format(time.RFC3339),
		LastError: rsHealthMarker + " /private/other-project",
	})
	if healthBannerFrom(healthOf(cfg, hookNow())) == "" {
		t.Fatal("fixture must have an unhealthy banner")
	}
}

func TestBoundFolderShowsOnlyFixedHealthLine(t *testing.T) {
	cfg := projectBriefSetup(t)
	rsSeed(t, cfg)
	dir := rsDir(t)
	rsBreakGmail(t, cfg)

	// Unbound: today's detailed banner.
	if got := rsRecall(t, dir); !strings.Contains(got, rsHealthMarker) {
		t.Fatalf("fixture: an unbound folder keeps the detailed banner: %s", got)
	}

	run(t, "project", "bind", "--scope", "project:a", dir)
	prevBrief := hookResolveBrief
	hookResolveBrief = resolveBrief
	defer func() { hookResolveBrief = prevBrief }()
	for name, got := range map[string]string{
		"recall":        rsRecall(t, dir),
		"recall-scope":  rsRecall(t, dir, "--scope", "project:a", "--project-dir", dir),
		"session-start": pbHook(t, dir),
	} {
		if strings.Contains(got, rsHealthMarker) || strings.Contains(got, "private/other-project") {
			t.Errorf("%s leaked a source error in a bound folder: %s", name, got)
		}
		if strings.HasPrefix(got, "{") {
			got = decodeHookOutput(t, got).HookSpecificOutput.AdditionalContext
		}
		if !strings.Contains(got, boundHealthLine) {
			t.Errorf("%s misses the fixed health line: %s", name, got)
		}
	}
}
