package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

const givenEnvDir = "MORA_GIVEN_HELPER_DIR"

func readGivenRaw(t *testing.T, cfg Config) string {
	t.Helper()
	b, err := os.ReadFile(givenLogPath(cfg))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func givenLinesOf(t *testing.T, cfg Config) []givenLine {
	t.Helper()
	var out []givenLine
	for _, raw := range strings.Split(strings.TrimSpace(readGivenRaw(t, cfg)), "\n") {
		if raw == "" {
			continue
		}
		var g givenLine
		if err := json.Unmarshal([]byte(raw), &g); err != nil {
			t.Fatalf("bad given line %q: %v", raw, err)
		}
		out = append(out, g)
	}
	return out
}

func gc(g givenLine) int {
	if g.Count == nil {
		return -1
	}
	return *g.Count
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

func TestGivenSessionStartLogsExactEmittedIDs(t *testing.T) {
	cfg := projectBriefSetup(t)
	pbOwner(t, cfg, "mem_a", pbScope, "decision", "Use pnpm", "Always use pnpm.", 1)
	pbOwner(t, cfg, "mem_b", pbScope, "correction", "Tabs ban", "Never tabs.", 2)
	pbSeed(t, cfg, "mem_hidden", "personal", "decision", "Private", "Secret.", 3)
	dir := pbBind(t)
	got := pbHook(t, filepath.Join(dir, "sub"))
	_, wantIDs, err := buildProjectBrief(cfg, pbScope, dir, time.Now())
	if err != nil || len(wantIDs) != 2 {
		t.Fatalf("emitted ids %v, err %v", wantIDs, err)
	}
	lines := givenLinesOf(t, cfg)
	if len(lines) != 1 {
		t.Fatalf("want one line, got %d:\n%s", len(lines), readGivenRaw(t, cfg))
	}
	g := lines[0]
	if g.Surface != "hook-session-start" || g.Kind != "project-brief" || g.Caller != "claude-code" || g.Scope != pbScope ||
		g.Dir != dir || gc(g) != 2 || *g.More != 0 || g.Written == nil || !*g.Written || !reflect.DeepEqual(sortedCopy(g.IDs), sortedCopy(wantIDs)) {
		t.Fatalf("line = %+v, want ids %v", g, wantIDs)
	}
	if !strings.Contains(got, "Use pnpm") || !strings.Contains(got, "Tabs ban") || strings.Contains(got, "Private") {
		t.Fatalf("brief text:\n%s", got)
	}
	if _, err := time.Parse(time.RFC3339, g.At); err != nil || !strings.HasSuffix(g.At, "Z") {
		t.Fatalf("at = %q", g.At)
	}
	// An unbound folder with the daily brief logs a digest line: no notes.
	enableUnboundSessionBrief(t)
	defer stubHookBrief(t, "daily brief body")()
	pbHook(t, t.TempDir())
	lines = givenLinesOf(t, cfg)
	if len(lines) != 2 || lines[1].Kind != "daily-brief" || gc(lines[1]) != 0 || len(lines[1].IDs) != 0 || lines[1].Dir == "" {
		t.Fatalf("daily line = %+v", lines)
	}
	// A hook that prints nothing logs nothing.
	var out bytes.Buffer
	_ = cmdHook(testCtx(t), []string{"session-start"}, &out, testStderr, strings.NewReader(fmt.Sprintf(`{"source":"compact","cwd":%q}`, dir)))
	if len(givenLinesOf(t, cfg)) != 2 {
		t.Fatal("compact session start must log nothing")
	}
}

var recallIDRe = regexp.MustCompile(`id: (\S+)\]`)

func TestGivenRecallLogsOnlyPrintedIDs(t *testing.T) {
	cfg := seedRecallMemories(t, 6)
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cwdDir := t.TempDir()
	input := fmt.Sprintf(`{"prompt":"What did we decide about eelpout recall token alpha?","cwd":%q}`, cwdDir)
	if err := cmdHook(testCtx(t), []string{"recall"}, &out, testStderr, strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	var printed []string
	for _, m := range recallIDRe.FindAllStringSubmatch(decodeHookOutput(t, out.String()).HookSpecificOutput.AdditionalContext, -1) {
		printed = append(printed, m[1])
	}
	lines := givenLinesOf(t, cfg)
	if len(lines) != 1 || len(printed) != 3 {
		t.Fatalf("printed %v, lines %+v", printed, lines)
	}
	g := lines[0]
	if g.Surface != "hook-recall" || g.Kind != "recall" || gc(g) != 3 || *g.More != 0 || g.Dir != cwdDir || g.Written == nil || !*g.Written || !reflect.DeepEqual(g.IDs, printed) {
		t.Fatalf("line = %+v, printed %v (6 candidates existed)", g, printed)
	}
	// A threshold that cuts every candidate prints nothing and logs nothing.
	out.Reset()
	if err := cmdHook(testCtx(t), []string{"recall", "--threshold", "-999"}, &out, testStderr, strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 || len(givenLinesOf(t, cfg)) != 1 {
		t.Fatalf("cut recall must log nothing; out %q", out.String())
	}
}

func TestGivenMCPReadToolsLogIDsFromPayload(t *testing.T) {
	cfg := projectBriefSetup(t)
	for i, id := range []string{"mem_a", "mem_b", "mem_c"} {
		pbSeed(t, cfg, id, pbScope, "decision", "Title"+id, "Pnpm rule body GIVENZEBRA "+id, i+1)
	}
	dir := pbBind(t)
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	ctx := withAgentCaller(testCtx(t), "mcp-stdio/Claude Code")
	seeded := []string{"mem_a", "mem_b", "mem_c"}
	calls := []struct {
		tool    string
		args    map[string]any
		wantIDs []string // nil: count only
	}{
		{"search_memory", map[string]any{"query": "GIVENZEBRA pnpm", "scope": pbScope}, seeded},
		{"list_memory", map[string]any{"scope": pbScope}, seeded},
		{"read_memory", map[string]any{"id": "mem_b"}, []string{"mem_b"}},
		{"context_memory", map[string]any{"query": "GIVENZEBRA pnpm", "scope": pbScope}, nil},
		{"think", map[string]any{"query": "GIVENZEBRA pnpm", "scope": pbScope}, seeded},
	}
	for _, c := range calls {
		before := len(givenLinesOf(t, cfg))
		inv := invokeMCPTool(ctx, c.tool, c.args)
		if inv.err != nil {
			t.Fatalf("%s: %v", c.tool, inv.err)
		}
		lines := givenLinesOf(t, cfg)
		if len(lines) != before+1 {
			t.Fatalf("%s: want one new line, got %d -> %d", c.tool, before, len(lines))
		}
		g := lines[len(lines)-1]
		if g.Surface != "mcp:"+c.tool || g.Kind != c.tool || g.Caller != "claude-code" || g.Written != nil {
			t.Fatalf("%s: line = %+v", c.tool, g)
		}
		if c.wantIDs == nil {
			if len(g.IDs) != 0 || g.Count != nil || g.More != nil || g.CountKnown == nil || *g.CountKnown {
				t.Fatalf("%s: count-only tool logged ids %v", c.tool, g.IDs)
			}
			continue
		}
		if !reflect.DeepEqual(sortedCopy(g.IDs), sortedCopy(c.wantIDs)) || gc(g) != len(c.wantIDs) {
			t.Fatalf("%s: ids %v count %d, want %v", c.tool, g.IDs, gc(g), c.wantIDs)
		}
	}
	// search_memory: the ids are the ids in the returned payload.
	inv := invokeMCPTool(ctx, "search_memory", map[string]any{"query": "GIVENZEBRA", "scope": pbScope, "limit": 1})
	raw, _ := json.Marshal(inv.value)
	var payload struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	var inPayload []string
	for _, r := range payload.Results {
		inPayload = append(inPayload, r.ID)
	}
	lines := givenLinesOf(t, cfg)
	if g := lines[len(lines)-1]; len(inPayload) == 0 || !reflect.DeepEqual(g.IDs, inPayload) {
		t.Fatalf("search ids %v, payload %v", g.IDs, inPayload)
	}
	// brief with project_dir logs the emitted ids, scope and dir; without it, nothing.
	before := len(givenLinesOf(t, cfg))
	invokeMCPTool(ctx, "brief", map[string]any{})
	if len(givenLinesOf(t, cfg)) != before {
		t.Fatal("daily brief over MCP must log nothing")
	}
	pbOwner(t, cfg, "mem_rule", pbScope, "decision", "Rule", "Use pnpm.", 5)
	if inv := invokeMCPTool(ctx, "brief", map[string]any{"project_dir": dir}); inv.err != nil {
		t.Fatal(inv.err)
	}
	lines = givenLinesOf(t, cfg)
	_, emitted, err := buildProjectBrief(cfg, pbScope, dir, briefClock())
	if err != nil || len(emitted) == 0 {
		t.Fatalf("emitted %v, %v", emitted, err)
	}
	if g := lines[len(lines)-1]; len(lines) != before+1 || g.Surface != "mcp:brief" || g.Scope != pbScope || g.Dir != dir || !reflect.DeepEqual(g.IDs, emitted) {
		t.Fatalf("project brief line = %+v", g)
	}
	// Write tools, failed reads and empty results log nothing.
	before = len(lines)
	invokeMCPTool(ctx, "write_memory", map[string]any{"scope": pbScope, "type": "decision", "title": "GIVENTITLE", "text": "GIVENTEXT"})
	invokeMCPTool(ctx, "delete_memory", map[string]any{"id": "mem_c"})
	invokeMCPTool(ctx, "read_memory", map[string]any{"id": "mem_missing"})
	invokeMCPTool(ctx, "list_memory", map[string]any{"scope": "project:empty"})
	if got := len(givenLinesOf(t, cfg)); got != before {
		t.Fatalf("write, failed read or empty result logged %d lines", got-before)
	}
}

func TestGivenLineNeverHoldsPromptQueryTitleOrText(t *testing.T) {
	cfg := seedRecallMemories(t, 2)
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	pbSeed(t, cfg, "mem_marker", pbScope, "decision", "TITLEMARKERQX", "BODYMARKERQX eelpout recall token alpha", 4)
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	input := `{"prompt":"PROMPTMARKERQX eelpout recall token alpha please","cwd":"/work/x"}`
	if err := cmdHook(testCtx(t), []string{"recall"}, &out, testStderr, strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	ctx := withAgentCaller(testCtx(t), "mcp-stdio/test")
	for _, tool := range []string{"search_memory", "context_memory", "think"} {
		invokeMCPTool(ctx, tool, map[string]any{"query": "QUERYMARKERQX eelpout recall token alpha"})
	}
	invokeMCPTool(ctx, "read_memory", map[string]any{"id": "mem_marker"})
	invokeMCPTool(ctx, "list_memory", map[string]any{})
	raw := readGivenRaw(t, cfg)
	if len(givenLinesOf(t, cfg)) < 5 {
		t.Fatalf("expected several lines, got:\n%s", raw)
	}
	for _, marker := range []string{"PROMPTMARKERQX", "QUERYMARKERQX", "TITLEMARKERQX", "BODYMARKERQX", "Eelpout recall decision", "decided about"} {
		if strings.Contains(raw, marker) {
			t.Fatalf("log holds %q:\n%s", marker, raw)
		}
	}
}

func TestGivenLogFileMode(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := appendGiven(cfg, givenLine{Surface: "hook-recall", Kind: "recall", Caller: "claude-code"}, []string{"a"}, 1); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(givenLogPath(cfg))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, err %v", info.Mode().Perm(), err)
	}
}

func TestGivenHelperProcess(t *testing.T) {
	dir := os.Getenv(givenEnvDir)
	if dir == "" {
		t.Skip("helper process only")
	}
	n, _ := strconv.Atoi(os.Getenv("MORA_GIVEN_HELPER_N"))
	cfg := Config{StateDir: dir}
	for i := 0; i < 200; i++ {
		ids := []string{fmt.Sprintf("p%d-%d", n, i), "mem_filler_one", "mem_filler_two"}
		if err := appendGiven(cfg, givenLine{Surface: "mcp:search_memory", Kind: "search_memory", Caller: "helper", Dir: "/tmp/x"}, ids, 3); err != nil && runtime.GOOS != "windows" {
			t.Fatal(err) // on Windows a rotation can still skip a line
		}
	}
}

func TestGivenEightProcessesAppendWholeLines(t *testing.T) {
	dir := t.TempDir()
	var cmds []*exec.Cmd
	var outs []*bytes.Buffer
	for p := 0; p < 8; p++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestGivenHelperProcess$")
		cmd.Env = append(os.Environ(), givenEnvDir+"="+dir, "MORA_GIVEN_HELPER_N="+strconv.Itoa(p), "MORA_TEST_SUBPROCESS=1")
		buf := &bytes.Buffer{}
		cmd.Stdout, cmd.Stderr = buf, buf
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds, outs = append(cmds, cmd), append(outs, buf)
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper %d: %v\n%s", i, err, outs[i])
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, givenLogName))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != 1600 {
		t.Fatalf("got %d lines, want 1600", len(lines))
	}
	seen := map[string]bool{}
	for _, l := range lines {
		var g givenLine
		if err := json.Unmarshal([]byte(l), &g); err != nil || gc(g) != 3 || len(g.IDs) != 3 {
			t.Fatalf("torn line %q: %v", l, err)
		}
		seen[g.IDs[0]] = true
	}
	if len(seen) != 1600 {
		t.Fatalf("%d distinct lines, want 1600", len(seen))
	}
}

func TestGivenUnwritableLogChangesNoResult(t *testing.T) {
	cfg := projectBriefSetup(t)
	pbSeed(t, cfg, "mem_a", pbScope, "decision", "Rule", "Pnpm body", 1)
	dir := pbBind(t)
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	enableUnboundSessionBrief(t)
	defer stubHookBrief(t, "daily brief body")()
	hook := func() string {
		var out bytes.Buffer
		in := fmt.Sprintf(`{"source":"startup","cwd":%q}`, t.TempDir())
		if err := cmdHook(testCtx(t), []string{"session-start"}, &out, testStderr, strings.NewReader(in)); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	tool := func() string {
		inv := invokeMCPTool(testCtx(t), "list_memory", map[string]any{"scope": pbScope})
		if inv.err != nil {
			t.Fatal(inv.err)
		}
		ids, _, _ := givenFromMCP("list_memory", nil, inv)
		return strings.Join(ids, ",")
	}
	wantHook, wantTool := hook(), tool()
	if err := os.Remove(givenLogPath(cfg)); err != nil {
		t.Fatal(err)
	}
	// A directory where the file should be makes every append fail.
	if err := os.Mkdir(givenLogPath(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := hook(); got != wantHook || !strings.Contains(got, "daily brief body") {
		t.Fatalf("hook output changed:\n%q\nvs\n%q", got, wantHook)
	}
	if got := tool(); got != wantTool || got == "" {
		t.Fatalf("tool result changed: %q vs %q", got, wantTool)
	}
	_ = dir
}

func TestGivenLogRotatesAtTwoMegabytes(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	path := givenLogPath(cfg)
	if err := os.WriteFile(path+".1", []byte("older\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("x"), givenLogMaxBytes+1)
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendGiven(cfg, givenLine{Surface: "hook-recall", Kind: "recall", Caller: "claude-code"}, []string{"a"}, 1); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(path + ".1")
	if err != nil || len(old) != len(big) {
		t.Fatalf("given.log.1 size %d, err %v", len(old), err)
	}
	if lines := givenLinesOf(t, Config{StateDir: cfg.StateDir}); len(lines) != 1 {
		t.Fatalf("new log has %d lines", len(lines))
	}
	// A log at or below the limit keeps growing in place.
	if err := appendGiven(cfg, givenLine{Surface: "hook-recall", Kind: "recall", Caller: "claude-code"}, []string{"b"}, 1); err != nil {
		t.Fatal(err)
	}
	if lines := givenLinesOf(t, cfg); len(lines) != 2 {
		t.Fatalf("log has %d lines", len(lines))
	}
}

func TestGivenLineIsBounded(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	var ids []string
	for i := 0; i < 50; i++ {
		ids = append(ids, strings.Repeat("i", 60)+strconv.Itoa(i))
	}
	g := givenLine{Surface: "hook-recall", Kind: "recall", Caller: "claude-code", Dir: "/" + strings.Repeat("d", 3000)}
	if err := appendGiven(cfg, g, ids, 50); err != nil {
		t.Fatal(err)
	}
	b := []byte(readGivenRaw(t, cfg))
	if len(b) > givenLineMax {
		t.Fatalf("line is %d bytes", len(b))
	}
	got := givenLinesOf(t, cfg)[0]
	if gc(got) != 50 || *got.More != 50-len(got.IDs) || len(got.IDs) > givenMaxIDs {
		t.Fatalf("line = %+v", got)
	}
}

func TestGivenCommandNewestFirstAndEmpty(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if got := run(t, "given"); strings.TrimSpace(got) != "Nothing given yet." {
		t.Fatalf("empty = %q", got)
	}
	if got := run(t, "given", "--json"); strings.TrimSpace(got) != "[]" {
		t.Fatalf("empty json = %q", got)
	}
	prev := hookNow
	t.Cleanup(func() { hookNow = prev })
	for i, surface := range []string{"hook-session-start", "hook-recall", "mcp:search_memory"} {
		hookNow = func() time.Time { return time.Date(2026, 11, 10, 10, i, 0, 0, time.UTC) }
		ids := make([]string, 0, 22)
		for j := 0; j < 22; j++ {
			ids = append(ids, fmt.Sprintf("mem_%d_%d", i, j))
		}
		if err := appendGiven(cfg, givenLine{Surface: surface, Kind: "k", Caller: "claude-code", Dir: "/work/demo"}, ids, 22); err != nil {
			t.Fatal(err)
		}
	}
	out := strings.Split(strings.TrimSpace(run(t, "given")), "\n")
	if len(out) != 3 || !strings.HasPrefix(out[0], "2026-11-10T10:02:00Z  mcp:search_memory  claude-code  /work/demo  22 notes  ids: mem_2_0, ") ||
		!strings.Contains(out[0], "(+2 more)") || !strings.HasPrefix(out[2], "2026-11-10T10:00:00Z  hook-session-start") {
		t.Fatalf("plain output:\n%s", strings.Join(out, "\n"))
	}
	if got := strings.Split(strings.TrimSpace(run(t, "given", "--limit", "1")), "\n"); len(got) != 1 || !strings.Contains(got[0], "mcp:search_memory") {
		t.Fatalf("limit 1 = %v", got)
	}
	var arr []givenLine
	if err := json.Unmarshal([]byte(run(t, "given", "--json", "--limit", "2")), &arr); err != nil || len(arr) != 2 ||
		arr[0].Surface != "mcp:search_memory" || arr[1].Surface != "hook-recall" {
		t.Fatalf("json = %+v, err %v", arr, err)
	}
	for _, bad := range [][]string{{"given", "--limit", "0"}, {"given", "extra"}, {"given", "--nope"}} {
		if err := Run(testCtx(t), bad, &bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader("")); err == nil {
			t.Fatalf("%v should fail", bad)
		}
	}
}

func TestGivenDoctorLastGiven(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	has := func() (map[string]any, bool) {
		out, _ := runErr(t, "doctor", "--json")
		i := strings.Index(out, "{")
		if i < 0 {
			t.Fatalf("no JSON in doctor output:\n%s", out)
		}
		var rep map[string]any
		if err := json.Unmarshal([]byte(out[i:]), &rep); err != nil {
			t.Fatal(err)
		}
		v, ok := rep["last_given"].(map[string]any)
		return v, ok
	}
	if _, ok := has(); ok {
		t.Fatal("last_given must be absent before any hand-over")
	}
	if err := appendGiven(cfg, givenLine{Surface: "hook-recall", Kind: "recall", Caller: "claude-code"}, []string{"a", "b"}, 2); err != nil {
		t.Fatal(err)
	}
	v, ok := has()
	if !ok || v["surface"] != "hook-recall" || v["count"] != float64(2) || v["at"] == "" || len(v) != 3 {
		t.Fatalf("last_given = %v, present %v", v, ok)
	}
}

func TestGivenCallerNames(t *testing.T) {
	for in, want := range map[string]string{
		"mcp-stdio/Claude Code": "claude-code",
		"mcp-stdio/cursor":      "cursor",
		"mcp-stdio":             "unknown",
		"mcp":                   "unknown",
		"":                      "unknown",
		"mcp-stdio/<>\"'; \n":   "-",
		"\u00e9\u00e8\u4e2d":    "unknown",
		"http":                  "http",
		strings.Repeat("a", 90): strings.Repeat("a", 40),
	} {
		if got := givenCaller(in); got != want {
			t.Errorf("givenCaller(%q) = %q, want %q", in, got, want)
		}
	}
	if got := givenCaller("Weird/Name:One"); !regexp.MustCompile(`^[a-z0-9._-]+$`).MatchString(got) {
		t.Errorf("caller %q has odd characters", got)
	}
}

func TestGivenFreeTextDirNeverReachesLog(t *testing.T) {
	cfg := seedRecallMemories(t, 3)
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	bad := "/nonexistent/DIRMARKERQX"
	var out bytes.Buffer
	in := fmt.Sprintf(`{"prompt":"What did we decide about eelpout recall token alpha?","cwd":%q}`, bad)
	if err := cmdHook(testCtx(t), []string{"recall"}, &out, testStderr, strings.NewReader(in)); err != nil || out.Len() == 0 {
		t.Fatalf("recall: %v %q", err, out.String())
	}
	enableUnboundSessionBrief(t)
	defer stubHookBrief(t, "daily brief body")()
	for _, cwd := range []string{bad, "relative/DIRMARKERQX", "DIRMARKERQX"} {
		_ = cmdHook(testCtx(t), []string{"session-start"}, &out, testStderr, strings.NewReader(fmt.Sprintf(`{"cwd":%q}`, cwd)))
	}
	ctx := testCtx(t)
	invokeMCPTool(ctx, "search_memory", map[string]any{"query": "eelpout", "project_dir": bad})
	invokeMCPTool(ctx, "list_memory", map[string]any{"project_dir": bad})
	invokeMCPTool(ctx, "brief", map[string]any{"project_dir": bad})
	raw := readGivenRaw(t, cfg)
	if len(givenLinesOf(t, cfg)) < 5 || strings.Contains(raw, "DIRMARKERQX") {
		t.Fatalf("log:\n%s", raw)
	}
	for _, g := range givenLinesOf(t, cfg) {
		if g.Dir != "" {
			t.Fatalf("dir %q logged for a call with no real folder", g.Dir)
		}
	}
}

func TestGivenStatusWordsAndUnknownCount(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	hookDir := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(appendGiven(cfg, givenLine{Surface: "hook-recall", Kind: "recall", Caller: "claude-code", Dir: hookDir, Written: boolPtr(true)}, []string{"a"}, 1))
	must(appendGiven(cfg, givenLine{Surface: "hook-recall", Kind: "recall", Caller: "claude-code", Written: boolPtr(false)}, []string{"b"}, 1))
	must(appendGiven(cfg, givenLine{Surface: "mcp:context_memory", Kind: "context_memory", Caller: "cursor"}, nil, -1))
	out := run(t, "given")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || !strings.HasSuffix(lines[0], "count not known  ids: none  handed to transport") ||
		!strings.HasSuffix(lines[1], "1 notes  ids: b  send failed") || !strings.HasSuffix(lines[2], "1 notes  ids: a  sent") {
		t.Fatalf("output:\n%s", out)
	}
	raw := readGivenRaw(t, cfg)
	last := strings.Split(strings.TrimSpace(raw), "\n")[2]
	if strings.Contains(last, `"count"`) || strings.Contains(last, `"more"`) || strings.Contains(last, `"written"`) || !strings.Contains(last, `"count_known":false`) {
		t.Fatalf("context line: %s", last)
	}
	if v := lastGiven(cfg); v == nil || v.Count != nil || v.Surface != "mcp:context_memory" {
		t.Fatalf("last_given = %+v", v)
	}
}

func TestGivenFourProcessesAcrossRotationLoseNothing(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{StateDir: dir}
	// Pre-fill just under the limit so the 800 appends cross it once.
	filler, _ := json.Marshal(givenLine{At: "2026-11-10T00:00:00Z", Surface: "hook-recall", Kind: "recall", Caller: "x", Dir: strings.Repeat("f", 200)})
	filler = append(filler, '\n')
	prefill := (givenLogMaxBytes - 60*1024) / len(filler)
	if err := os.WriteFile(givenLogPath(cfg), bytes.Repeat(filler, prefill), 0o600); err != nil {
		t.Fatal(err)
	}
	var cmds []*exec.Cmd
	var outs []*bytes.Buffer
	for p := 0; p < 4; p++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestGivenHelperProcess$")
		cmd.Env = append(os.Environ(), givenEnvDir+"="+dir, "MORA_GIVEN_HELPER_N="+strconv.Itoa(p), "MORA_TEST_SUBPROCESS=1")
		buf := &bytes.Buffer{}
		cmd.Stdout, cmd.Stderr = buf, buf
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds, outs = append(cmds, cmd), append(outs, buf)
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("helper %d: %v\n%s", i, err, outs[i])
		}
	}
	total := 0
	complete := true
	for _, name := range []string{givenLogPath(cfg), givenLogPath(cfg) + ".1"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("%s: %v (rotation did not happen?)", name, err)
		}
		total += bytes.Count(b, []byte("\n"))
		for _, l := range bytes.Split(bytes.TrimSuffix(b, []byte("\n")), []byte("\n")) {
			if !json.Valid(l) {
				complete = false
			}
		}
	}
	if runtime.GOOS == "windows" {
		if !complete || total < (prefill+800)*95/100 {
			t.Fatalf("lines = %d of %d, complete JSON = %v", total, prefill+800, complete)
		}
		return
	}
	if total != prefill+800 || !complete {
		t.Fatalf("lines = %d, want %d (complete %v)", total, prefill+800, complete)
	}
}
