package mora

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// ---------------------------------------------------------------------------
// isValidAccountLabel — path-safe by construction (lowercase, digits, hyphen).
// ---------------------------------------------------------------------------

func TestCoreB_UtilIsValidAccountLabel(t *testing.T) {
	valid := []string{"work", "abc123", "a-b-c", "0", "z", "9", "----", "gmail-2"}
	for _, s := range valid {
		if !isValidAccountLabel(s) {
			t.Errorf("isValidAccountLabel(%q) = false, want true", s)
		}
	}
	// empty, uppercase, underscore, dot, space, slash, unicode → invalid
	invalid := []string{"", "Work", "WORK", "a_b", "a.b", "a b", "a/b", "café", "A", "work!", "tab\t"}
	for _, s := range invalid {
		if isValidAccountLabel(s) {
			t.Errorf("isValidAccountLabel(%q) = true, want false", s)
		}
	}
}

// ---------------------------------------------------------------------------
// sourceFreshlySynced — reads LastSuccessAt from the source's SyncStatus.
// ---------------------------------------------------------------------------

func coreBUtilGmailSource() Source { return Source{Name: "gmail", Type: "gmail"} }

func TestCoreB_UtilSourceFreshlySynced(t *testing.T) {
	cfg := testCfg(t)
	s := coreBUtilGmailSource()
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	path := syncStatusPathFor(cfg, s)

	// (1) Never synced: no status file on disk → false.
	if sourceFreshlySynced(cfg, s, time.Hour, now) {
		t.Fatal("no status file should read as NOT freshly synced")
	}

	// (2) Synced 5m ago, window 1h → fresh.
	if err := memory.SaveStatus(path, &memory.SyncStatus{
		Source:        s.Name,
		LastSuccessAt: now.Add(-5 * time.Minute).Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	if !sourceFreshlySynced(cfg, s, time.Hour, now) {
		t.Fatal("synced 5m ago within a 1h window must be fresh")
	}

	// (3) Synced 2h ago, window 1h → stale.
	if err := memory.SaveStatus(path, &memory.SyncStatus{
		LastSuccessAt: now.Add(-2 * time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	if sourceFreshlySynced(cfg, s, time.Hour, now) {
		t.Fatal("synced 2h ago is outside a 1h window → stale")
	}

	// (4) Status present but LastSuccessAt empty (attempted, never succeeded) → false.
	if err := memory.SaveStatus(path, &memory.SyncStatus{LastAttemptAt: now.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if sourceFreshlySynced(cfg, s, time.Hour, now) {
		t.Fatal("empty LastSuccessAt must read as NOT freshly synced")
	}

	// (5) Malformed timestamp → time.Parse fails → false.
	if err := memory.SaveStatus(path, &memory.SyncStatus{LastSuccessAt: "not-a-timestamp"}); err != nil {
		t.Fatal(err)
	}
	if sourceFreshlySynced(cfg, s, time.Hour, now) {
		t.Fatal("unparseable LastSuccessAt must read as NOT freshly synced")
	}

	// (6) LoadStatus error (invalid JSON on disk) → false.
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sourceFreshlySynced(cfg, s, time.Hour, now) {
		t.Fatal("corrupt status JSON must read as NOT freshly synced (LoadStatus error)")
	}
}

// ---------------------------------------------------------------------------
// expandHome — only a leading "~/" is expanded.
// ---------------------------------------------------------------------------

func TestCoreB_UtilExpandHome(t *testing.T) {
	withTempHome(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}

	if got := expandHome("~/x/y"); got != filepath.Join(home, "x", "y") {
		t.Fatalf("expandHome(~/x/y) = %q, want %q", got, filepath.Join(home, "x", "y"))
	}
	if got := expandHome("~/x/y"); !strings.HasPrefix(got, home) {
		t.Fatalf("expanded path %q must be rooted at HOME %q", got, home)
	}
	// Unchanged: absolute, relative, bare ~, and ~user (no "~/" prefix).
	for _, p := range []string{"/abs/path", "rel/path", "~", "~otheruser", "./x", "a~/b"} {
		if got := expandHome(p); got != p {
			t.Errorf("expandHome(%q) = %q, want unchanged", p, got)
		}
	}
}

// ---------------------------------------------------------------------------
// parseSearchArgs — every flag/error branch.
// ---------------------------------------------------------------------------

func TestCoreB_UtilParseSearchArgs(t *testing.T) {
	// Defaults + bare-word accumulation + both flag spellings, interleaved.
	scope, limit, jsonOut, query, err := parseSearchArgs([]string{
		"hello", "--json", "world", "--scope", "personal", "--limit", "5", "more",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if scope != "personal" || limit != 5 || !jsonOut {
		t.Fatalf("got scope=%q limit=%d json=%v", scope, limit, jsonOut)
	}
	if strings.Join(query, " ") != "hello world more" {
		t.Fatalf("query = %v, want [hello world more]", query)
	}

	// = spellings.
	scope, limit, jsonOut, query, err = parseSearchArgs([]string{"--scope=project:x", "--limit=42"})
	if err != nil {
		t.Fatal(err)
	}
	if scope != "project:x" || limit != 42 || jsonOut || len(query) != 0 {
		t.Fatalf("got scope=%q limit=%d json=%v query=%v", scope, limit, jsonOut, query)
	}

	// Pure defaults.
	scope, limit, jsonOut, query, err = parseSearchArgs(nil)
	if err != nil || scope != "" || limit != 10 || jsonOut || query != nil {
		t.Fatalf("defaults wrong: scope=%q limit=%d json=%v query=%v err=%v", scope, limit, jsonOut, query, err)
	}

	// --scope with no value.
	if _, _, _, _, err := parseSearchArgs([]string{"--scope"}); err == nil || err.Error() != "--scope requires value" {
		t.Fatalf("--scope alone err = %v, want \"--scope requires value\"", err)
	}
	// --limit with no value.
	if _, _, _, _, err := parseSearchArgs([]string{"--limit"}); err == nil || err.Error() != "--limit requires value" {
		t.Fatalf("--limit alone err = %v, want \"--limit requires value\"", err)
	}
	// --limit non-integer (space form).
	if _, _, _, _, err := parseSearchArgs([]string{"--limit", "abc"}); err == nil || !strings.Contains(err.Error(), "invalid syntax") {
		t.Fatalf("--limit abc err = %v, want invalid syntax", err)
	}
	// --limit=non-integer (= form).
	if _, _, _, _, err := parseSearchArgs([]string{"--limit=xyz"}); err == nil || !strings.Contains(err.Error(), "invalid syntax") {
		t.Fatalf("--limit=xyz err = %v, want invalid syntax", err)
	}
}

// ---------------------------------------------------------------------------
// atomicWrite — content + mode + MkdirAll-fail branch.
// ---------------------------------------------------------------------------

func TestCoreB_UtilAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deep", "f.txt")
	body := []byte("hello atomic\n")
	if err := atomicWrite(path, body, 0o640); err != nil {
		t.Fatalf("atomicWrite: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("content = %q, want %q", got, body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	assertPermUnix(t, info.Mode(), 0o640)

	// MkdirAll-fail: make the parent a regular FILE, then write to file/child.
	fileAsDir := filepath.Join(dir, "iamafile")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(filepath.Join(fileAsDir, "child.txt"), []byte("no"), 0o644); err == nil {
		t.Fatal("atomicWrite into a path whose parent is a file must error")
	}
}

// ---------------------------------------------------------------------------
// appendFile — appends, creates parent, MkdirAll-fail branch.
// ---------------------------------------------------------------------------

func TestCoreB_UtilAppendFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "log.jsonl")
	if err := appendFile(path, "line-a\n"); err != nil {
		t.Fatalf("appendFile 1: %v", err)
	}
	if err := appendFile(path, "line-b\n"); err != nil {
		t.Fatalf("appendFile 2: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "line-a\nline-b\n" {
		t.Fatalf("appended content = %q, want two concatenated lines", got)
	}

	// MkdirAll-fail: parent is a file.
	fileAsDir := filepath.Join(dir, "afile")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendFile(filepath.Join(fileAsDir, "child.log"), "z\n"); err == nil {
		t.Fatal("appendFile whose parent is a file must error")
	}
}

// ---------------------------------------------------------------------------
// queryLoggingEnabled — env + marker gates.
// ---------------------------------------------------------------------------

func TestCoreB_UtilQueryLoggingEnabled(t *testing.T) {
	cfg := testCfg(t)
	t.Setenv("MORA_LOG_QUERIES", "")

	if queryLoggingEnabled(cfg) {
		t.Fatal("query logging must default OFF")
	}

	t.Setenv("MORA_LOG_QUERIES", "1")
	if !queryLoggingEnabled(cfg) {
		t.Fatal("MORA_LOG_QUERIES=1 must enable query logging")
	}
	t.Setenv("MORA_LOG_QUERIES", "")

	// Marker file also enables it.
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "usage"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "usage", "QUERIES"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !queryLoggingEnabled(cfg) {
		t.Fatal("usage/QUERIES marker must enable query logging")
	}
}

func coreBUtilReadEvent(t *testing.T, cfg Config) usageEvent {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cfg.StateDir, "usage", "events.jsonl"))
	if err != nil {
		t.Fatalf("usage log missing: %v", err)
	}
	line := strings.TrimSpace(string(raw))
	var e usageEvent
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		t.Fatalf("decode usage event %q: %v", line, err)
	}
	return e
}

func coreBUtilEventsExist(cfg Config) bool {
	_, err := os.Stat(filepath.Join(cfg.StateDir, "usage", "events.jsonl"))
	return err == nil
}

// logUsage default: query STRIPPED, metadata retained, TS stamped.
func TestCoreB_UtilLogUsageStripsQueryByDefault(t *testing.T) {
	cfg := testCfg(t)
	t.Setenv("DO_NOT_TRACK", "")
	t.Setenv("MORA_LOG_QUERIES", "")

	logUsage(cfg, usageEvent{Tool: "search_memory", Query: "secret merger", Scope: "personal", Results: 3, Millis: 7})
	e := coreBUtilReadEvent(t, cfg)
	if e.Query != "" {
		t.Fatalf("query must be stripped by default, got %q", e.Query)
	}
	if e.Tool != "search_memory" || e.Scope != "personal" || e.Results != 3 || e.Millis != 7 {
		t.Fatalf("metadata not preserved: %+v", e)
	}
	if e.TS == "" {
		t.Fatal("logUsage must stamp a TS")
	}
	if _, err := time.Parse(time.RFC3339, e.TS); err != nil {
		t.Fatalf("TS %q is not RFC3339: %v", e.TS, err)
	}
}

// logUsage retains the query when opted in via env.
func TestCoreB_UtilLogUsageRetainsQueryWithEnv(t *testing.T) {
	cfg := testCfg(t)
	t.Setenv("DO_NOT_TRACK", "")
	t.Setenv("MORA_LOG_QUERIES", "1")

	logUsage(cfg, usageEvent{Tool: "search_memory", Query: "keepme", Results: 1})
	if e := coreBUtilReadEvent(t, cfg); e.Query != "keepme" {
		t.Fatalf("query should be retained with MORA_LOG_QUERIES=1, got %q", e.Query)
	}
}

// logUsage retains the query when opted in via the QUERIES marker.
func TestCoreB_UtilLogUsageRetainsQueryWithMarker(t *testing.T) {
	cfg := testCfg(t)
	t.Setenv("DO_NOT_TRACK", "")
	t.Setenv("MORA_LOG_QUERIES", "")
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "usage"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "usage", "QUERIES"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	logUsage(cfg, usageEvent{Tool: "graph", Query: "Neil", Results: 2})
	if e := coreBUtilReadEvent(t, cfg); e.Query != "Neil" {
		t.Fatalf("query should be retained with usage/QUERIES marker, got %q", e.Query)
	}
}

// logUsage writes NOTHING when disabled by DO_NOT_TRACK.
func TestCoreB_UtilLogUsageDisabledDoNotTrack(t *testing.T) {
	cfg := testCfg(t)
	t.Setenv("DO_NOT_TRACK", "1")
	logUsage(cfg, usageEvent{Tool: "search_memory", Query: "nope"})
	if coreBUtilEventsExist(cfg) {
		t.Fatal("DO_NOT_TRACK=1 must write no usage events")
	}
}

// logUsage writes NOTHING when disabled by the usage/OFF marker.
func TestCoreB_UtilLogUsageDisabledOffMarker(t *testing.T) {
	cfg := testCfg(t)
	t.Setenv("DO_NOT_TRACK", "")
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "usage"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.StateDir, "usage", "OFF"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	logUsage(cfg, usageEvent{Tool: "search_memory"})
	if coreBUtilEventsExist(cfg) {
		t.Fatal("usage/OFF marker must write no usage events")
	}
}

// ---------------------------------------------------------------------------
// emit — JSON path + human off-path for Memory / []Memory / []catalogRow / default.
// ---------------------------------------------------------------------------

func TestCoreB_UtilEmitJSON(t *testing.T) {
	var buf bytes.Buffer
	src := map[string]any{"a": "b", "n": float64(2)}
	if err := emit(&buf, src, true); err != nil {
		t.Fatalf("emit json: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "\n  \"a\"") { // indented two spaces
		t.Fatalf("expected indented JSON, got %q", out)
	}
	var back map[string]any
	if err := json.Unmarshal([]byte(out), &back); err != nil {
		t.Fatalf("emit JSON must round-trip: %v\n%s", err, out)
	}
	if back["a"] != "b" || back["n"] != float64(2) {
		t.Fatalf("round-trip mismatch: %v", back)
	}
}

func TestCoreB_UtilEmitJSONError(t *testing.T) {
	var buf bytes.Buffer
	// A channel is not JSON-marshalable → emit returns the marshal error.
	if err := emit(&buf, make(chan int), true); err == nil {
		t.Fatal("emit of an unmarshalable value must return an error")
	}
	if buf.Len() != 0 {
		t.Fatalf("nothing should be written on marshal error, got %q", buf.String())
	}
}

func TestCoreB_UtilEmitMemory(t *testing.T) {
	var buf bytes.Buffer
	m := Memory{ID: "mem_1", Scope: "personal", Title: "Hello World"}
	if err := emit(&buf, m, false); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "mem_1\tpersonal\tHello World\n" {
		t.Fatalf("single Memory line = %q", got)
	}
}

func TestCoreB_UtilEmitMemorySlice(t *testing.T) {
	var buf bytes.Buffer
	ms := []Memory{
		{ID: "mem_a", Scope: "global", Title: "First"},
		{ID: "mem_b", Scope: "project:x", Title: "Second"},
	}
	if err := emit(&buf, ms, false); err != nil {
		t.Fatal(err)
	}
	want := "mem_a\tglobal\tFirst\nmem_b\tproject:x\tSecond\n"
	if got := buf.String(); got != want {
		t.Fatalf("[]Memory output = %q, want %q", got, want)
	}
}

func TestCoreB_UtilEmitCatalogRows(t *testing.T) {
	var buf bytes.Buffer
	rows := []catalogRow{
		{Type: "gmail", Name: "work", Enabled: true},
		{Type: "calendar", Name: "home", Enabled: false},
	}
	if err := emit(&buf, rows, false); err != nil {
		t.Fatal(err)
	}
	want := "gmail\twork\tenabled\ncalendar\thome\tdisabled\n"
	if got := buf.String(); got != want {
		t.Fatalf("[]catalogRow output = %q, want %q", got, want)
	}
}

func TestCoreB_UtilEmitDefault(t *testing.T) {
	var buf bytes.Buffer
	if err := emit(&buf, "just a string", false); err != nil {
		t.Fatal(err)
	}
	if got := buf.String(); got != "just a string\n" {
		t.Fatalf("default emit = %q, want %q", got, "just a string\n")
	}
}

// ---------------------------------------------------------------------------
// tarGz — round-trip a tree, then the os.Create error branch.
// ---------------------------------------------------------------------------

func TestCoreB_UtilTarGz(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("AAA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "b.txt"), []byte("BBB"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(base, "out.tar.gz")
	if err := tarGz(out, root); err != nil {
		t.Fatalf("tarGz: %v", err)
	}

	entries := coreBUtilReadTarGz(t, out)
	if entries["root/a.txt"] != "AAA" {
		t.Fatalf("root/a.txt = %q, want AAA (entries: %v)", entries["root/a.txt"], entries)
	}
	if entries["root/sub/b.txt"] != "BBB" {
		t.Fatalf("root/sub/b.txt = %q, want BBB (entries: %v)", entries["root/sub/b.txt"], entries)
	}
	// Directories are skipped (info.IsDir short-circuit) — only files recorded.
	if len(entries) != 2 {
		t.Fatalf("expected exactly 2 file entries, got %d: %v", len(entries), entries)
	}

	// os.Create error: output in a nonexistent directory.
	if err := tarGz(filepath.Join(base, "nope", "deep", "x.tar.gz"), root); err == nil {
		t.Fatal("tarGz to a path in a nonexistent dir must error")
	}
}

func coreBUtilReadTarGz(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		if _, err := io.Copy(&b, tr); err != nil {
			t.Fatal(err)
		}
		out[filepath.ToSlash(hdr.Name)] = b.String()
	}
	return out
}

// ---------------------------------------------------------------------------
// schedulePlistFor — known/unknown job, RunAtLoad diff, env snapshot.
// ---------------------------------------------------------------------------

func TestCoreB_UtilSchedulePlistFor(t *testing.T) {
	cfg := testCfg(t)
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")
	t.Setenv("MORA_CONFIG_DIR", "")

	// Unknown job → ("", false).
	if plist, ok := schedulePlistFor(cfg, "does-not-exist"); ok || plist != "" {
		t.Fatalf("unknown job = (%q,%v), want (\"\",false)", plist, ok)
	}

	// Known job: label + program args + RunAtLoad + schedule fragment.
	plist, ok := schedulePlistFor(cfg, "index-hourly")
	if !ok {
		t.Fatal("index-hourly must be a known job")
	}
	for _, want := range []string{
		"<string>com.mora.index-hourly</string>",
		"<string>index</string><string>rebuild</string>",
		"<key>RunAtLoad</key><true/>",
		"<key>StartInterval</key><integer>3600</integer>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("index-hourly plist missing %q:\n%s", want, plist)
		}
	}
	// No env vars set → no EnvironmentVariables dict.
	if strings.Contains(plist, "EnvironmentVariables") {
		t.Fatalf("plist must omit EnvironmentVariables when no env set:\n%s", plist)
	}

	// pulse-daily drops RunAtLoad and uses a calendar interval.
	pulse, ok := schedulePlistFor(cfg, "pulse-daily")
	if !ok {
		t.Fatal("pulse-daily must be a known job")
	}
	if strings.Contains(pulse, "RunAtLoad") {
		t.Fatalf("pulse-daily must NOT set RunAtLoad:\n%s", pulse)
	}
	if !strings.Contains(pulse, "StartCalendarInterval") {
		t.Fatalf("pulse-daily must use StartCalendarInterval:\n%s", pulse)
	}

	// Env snapshot: both PATHs embedded into an EnvironmentVariables dict.
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "/creds/g.json")
	t.Setenv("MORA_CONFIG_DIR", "/scratch/mora")
	withEnv, _ := schedulePlistFor(cfg, "index-hourly")
	if !strings.Contains(withEnv, "<key>EnvironmentVariables</key>") {
		t.Fatalf("plist missing EnvironmentVariables dict:\n%s", withEnv)
	}
	if !strings.Contains(withEnv, "<key>MORA_GOOGLE_CREDENTIALS</key><string>/creds/g.json</string>") {
		t.Fatalf("plist missing creds env:\n%s", withEnv)
	}
	if !strings.Contains(withEnv, "<key>MORA_CONFIG_DIR</key><string>/scratch/mora</string>") {
		t.Fatalf("plist missing config-dir env:\n%s", withEnv)
	}
}

// ---------------------------------------------------------------------------
// installSchedule + listSchedules (launchd assertions are darwin-only).
// ---------------------------------------------------------------------------

func TestCoreB_UtilInstallAndListSchedule(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("installSchedule shells out to schtasks on windows (#56)")
	}
	withTempHome(t)
	cfg := testCfg(t)

	// Unknown job → error (any OS).
	var errBuf bytes.Buffer
	if err := installSchedule(&errBuf, cfg, "bogus"); err == nil || !strings.Contains(err.Error(), `unknown job "bogus"`) {
		t.Fatalf("installSchedule(bogus) err = %v, want unknown job", err)
	}

	// Known job: darwin writes the plist; linux prints cron/systemd guidance.
	var out bytes.Buffer
	if err := installSchedule(&out, cfg, "index-hourly"); err != nil {
		t.Fatalf("installSchedule: %v", err)
	}
	if runtime.GOOS != "darwin" {
		if !strings.Contains(out.String(), "launchd unavailable") && !strings.Contains(out.String(), "no launchd") {
			t.Fatalf("missing cron guidance on %s, got %q", runtime.GOOS, out.String())
		}
		return
	}
	if !strings.Contains(out.String(), "installed launchd job com.mora.index-hourly") {
		t.Fatalf("missing confirmation message, got %q", out.String())
	}
	plistPath := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", "com.mora.index-hourly.plist")
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("plist not written to %s: %v", plistPath, err)
	}

	// listSchedules surfaces the installed job's basename.
	var listBuf bytes.Buffer
	if err := listSchedules(&listBuf, cfg); err != nil {
		t.Fatalf("listSchedules: %v", err)
	}
	if !strings.Contains(listBuf.String(), "com.mora.index-hourly.plist") {
		t.Fatalf("listSchedules missing installed job, got %q", listBuf.String())
	}
}

// installSchedule surfaces the atomicWrite error when the LaunchAgents dir
// cannot be created (HOME points at a regular file → MkdirAll fails).
func TestCoreB_UtilInstallScheduleWriteError(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("exercises the darwin launchd atomicWrite error path; other OSes never touch LaunchAgents")
	}
	homeFile := filepath.Join(t.TempDir(), "home-as-file")
	if err := os.WriteFile(homeFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	setTestHome(t, homeFile)
	t.Setenv("MORA_CONFIG_DIR", "")
	cfg := testCfg(t)
	var out bytes.Buffer
	if err := installSchedule(&out, cfg, "index-hourly"); err == nil {
		t.Fatal("installSchedule must error when LaunchAgents dir cannot be created")
	}
}

// ---------------------------------------------------------------------------
// launchdSchedule — per-job calendar/interval fragment.
// ---------------------------------------------------------------------------

func TestCoreB_UtilLaunchdSchedule(t *testing.T) {
	cases := map[string]string{
		"index-hourly":  "<key>StartInterval</key><integer>3600</integer>",
		"ingest-hourly": "<key>StartInterval</key><integer>3600</integer>",
		"pulse-daily":   "<key>StartCalendarInterval</key><dict><key>Hour</key><integer>8</integer><key>Minute</key><integer>0</integer></dict>",
		"backup-daily":  "<key>StartCalendarInterval</key><dict><key>Hour</key><integer>2</integer><key>Minute</key><integer>0</integer></dict>",
		"git-daily":     "<key>StartCalendarInterval</key><dict><key>Hour</key><integer>3</integer><key>Minute</key><integer>0</integer></dict>",
		"lint-weekly":   "<key>StartCalendarInterval</key><dict><key>Weekday</key><integer>0</integer><key>Hour</key><integer>9</integer><key>Minute</key><integer>0</integer></dict>",
		"unknown-job":   "<key>StartInterval</key><integer>3600</integer>", // default
	}
	for job, want := range cases {
		if got := launchdSchedule(job); got != want {
			t.Errorf("launchdSchedule(%q) = %q, want %q", job, got, want)
		}
	}
	// The calendar jobs must differ from the interval jobs.
	if launchdSchedule("pulse-daily") == launchdSchedule("index-hourly") {
		t.Fatal("pulse-daily and index-hourly schedules must differ")
	}
	if launchdSchedule("backup-daily") == launchdSchedule("git-daily") {
		t.Fatal("backup-daily and git-daily schedules must differ (different Hour)")
	}
}

// ---------------------------------------------------------------------------
// extractDocxText — happy path + tab/br/cr + error branches.
// ---------------------------------------------------------------------------

func coreBUtilWriteZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCoreB_UtilExtractDocxText(t *testing.T) {
	dir := t.TempDir()
	const nsHead = `<?xml version="1.0" encoding="UTF-8"?>` +
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>`
	const nsTail = `</w:body></w:document>`

	// Happy path: two paragraphs → "Hello\nWorld".
	docx := filepath.Join(dir, "ok.docx")
	coreBUtilWriteZip(t, docx, map[string]string{
		"[Content_Types].xml": `<?xml version="1.0"?><Types/>`,
		"word/document.xml":   nsHead + `<w:p><w:r><w:t>Hello</w:t></w:r></w:p><w:p><w:r><w:t>World</w:t></w:r></w:p>` + nsTail,
	})
	got, err := extractDocxText(docx)
	if err != nil {
		t.Fatalf("extractDocxText: %v", err)
	}
	if got != "Hello\nWorld" {
		t.Fatalf("docx text = %q, want %q", got, "Hello\nWorld")
	}

	// tab / br / cr control runs.
	docx2 := filepath.Join(dir, "ctrl.docx")
	coreBUtilWriteZip(t, docx2, map[string]string{
		"word/document.xml": nsHead + `<w:p><w:t>A</w:t><w:tab/><w:t>B</w:t><w:br/><w:cr/><w:t>C</w:t></w:p>` + nsTail,
	})
	got2, err := extractDocxText(docx2)
	if err != nil {
		t.Fatalf("extractDocxText ctrl: %v", err)
	}
	if got2 != "A\tB\n\nC" {
		t.Fatalf("ctrl docx text = %q, want %q", got2, "A\tB\n\nC")
	}

	// Error: not a zip at all.
	notzip := filepath.Join(dir, "plain.docx")
	if err := os.WriteFile(notzip, []byte("this is not a zip file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := extractDocxText(notzip); err == nil {
		t.Fatal("a non-zip file must error")
	}

	// Error: malformed XML in document.xml → decoder returns a non-EOF error.
	badxml := filepath.Join(dir, "bad.docx")
	coreBUtilWriteZip(t, badxml, map[string]string{
		"word/document.xml": nsHead + `<w:p><w:t>oops</w:notclosed>`,
	})
	if _, err := extractDocxText(badxml); err == nil {
		t.Fatal("malformed document.xml must return a decode error")
	}

	// Error: a valid zip that lacks word/document.xml.
	nodoc := filepath.Join(dir, "nodoc.docx")
	coreBUtilWriteZip(t, nodoc, map[string]string{"other.xml": "<x/>"})
	_, err = extractDocxText(nodoc)
	if err == nil || !strings.Contains(err.Error(), "no word/document.xml") {
		t.Fatalf("zip without document.xml err = %v, want 'no word/document.xml'", err)
	}
}
