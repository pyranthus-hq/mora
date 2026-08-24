package mora

// mora_coreA_cover_test.go — coverage worker AREA=coreA. Exercises the top-level
// functions defined in the first half of mora.go (lines 1–3024): the pure helpers,
// config load/write, and the CLI command dispatch. Every test asserts on real
// behavior/output/error — never a bare "it ran".

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// coreADirsCfg returns a Config with every dir rooted under one temp dir — for the
// unit tests that call sources/config helpers directly without the CLI harness.
func coreADirsCfg(t *testing.T) Config {
	t.Helper()
	d := t.TempDir()
	return Config{
		VaultDir:  filepath.Join(d, "vault"),
		ConfigDir: filepath.Join(d, "config"),
		DataDir:   filepath.Join(d, "data"),
		StateDir:  filepath.Join(d, "state"),
	}
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestCoreA_Fusion(t *testing.T) {
	// Default path: no override => production defaultFusion.
	var c Config
	if got := configFusion(c); got != defaultFusion {
		t.Fatalf("fusion() default = %+v, want %+v", got, defaultFusion)
	}
	// Override path: the eval/test seam wins.
	ov := fusionParams{fts: 2, vec: 3, graph: 4, k: 5}
	c.SetFusionOverride(&ov)
	if got := configFusion(c); got != ov {
		t.Fatalf("fusion() override = %+v, want %+v", got, ov)
	}
}

func TestCoreA_FormatBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1 << 20, "1.0 MiB"},
		{1 << 30, "1.0 GiB"},
	}
	for _, tc := range cases {
		if got := formatBytes(tc.n); got != tc.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestCoreA_IsGoogleAuthError(t *testing.T) {
	if isGoogleAuthError(nil) {
		t.Error("nil error must not be a google-auth error")
	}
	for _, marker := range []string{"oauth", "token", "invalid_grant", "unauthorized", "401", "expired", "refresh"} {
		if !isGoogleAuthError(errString("failed: " + marker)) {
			t.Errorf("error containing %q should be a google-auth error", marker)
		}
	}
	if isGoogleAuthError(errString("disk full")) {
		t.Error("a plain non-auth error must not be classified as google-auth")
	}
}

func TestCoreA_ParseCSVList(t *testing.T) {
	if got := parseCSVList("a, b ,,c"); strings.Join(got, "|") != "a|b|c" {
		t.Errorf("parseCSVList trims + drops empties, got %v", got)
	}
	if got := parseCSVList(""); got != nil {
		t.Errorf("parseCSVList(\"\") = %v, want nil", got)
	}
	if got := parseCSVList("   "); got != nil {
		t.Errorf("parseCSVList(all blank) = %v, want nil", got)
	}
}

// errString is a tiny error whose message is exactly s.
type errString string

func (e errString) Error() string { return string(e) }

// ---------------------------------------------------------------------------
// Run dispatch: usage / version / unknown
// ---------------------------------------------------------------------------

func TestCoreA_RunDispatch(t *testing.T) {
	withTempHome(t)

	// No args prints usage, no error.
	if out := run(t); !strings.Contains(out, "USAGE:") {
		t.Fatalf("empty args should print usage, got:\n%s", out)
	}
	// help / -h / --help aliases print usage.
	for _, alias := range []string{"help", "-h", "--help"} {
		if out := run(t, alias); !strings.Contains(out, "mora init") {
			t.Fatalf("%q should print usage, got:\n%s", alias, out)
		}
	}
	// version / -v / --version print the build stanza.
	for _, alias := range []string{"version", "-v", "--version"} {
		out := run(t, alias)
		if !strings.Contains(out, "mora ") || !strings.Contains(out, "commit:") || !strings.Contains(out, "go:") {
			t.Fatalf("%q should print version stanza, got:\n%s", alias, out)
		}
	}
	// Unknown command errors.
	if _, err := runErr(t, "definitely-not-a-command"); err == nil {
		t.Fatal("unknown command should error")
	}
}

// ---------------------------------------------------------------------------
// config: load (all keys + read error), show, set, write-preserve
// ---------------------------------------------------------------------------

func TestCoreA_CmdConfigShowAndSet(t *testing.T) {
	withTempHome(t)

	// Show (no args).
	out := run(t, "config")
	for _, want := range []string{"vault_dir", "data_dir", "state_dir", "embedder", "context", "mmr"} {
		if !strings.Contains(out, want) {
			t.Fatalf("config show missing %q:\n%s", want, out)
		}
	}

	// mmr on with a non-ollama embedder prints the vector-only note.
	out = run(t, "config", "mmr", "on")
	if !strings.Contains(out, "mmr = on") || !strings.Contains(out, "note: MMR") {
		t.Fatalf("config mmr on should confirm + note; got:\n%s", out)
	}
	// context set prints the budget note.
	out = run(t, "config", "context", "large")
	if !strings.Contains(out, "context = large") || !strings.Contains(out, "default budget") {
		t.Fatalf("config context large should confirm + budget note; got:\n%s", out)
	}
	// embedder set (no error path).
	if out := run(t, "config", "embedder", "ollama"); !strings.Contains(out, "embedder = ollama") {
		t.Fatalf("config embedder ollama; got:\n%s", out)
	}
	// Reset-to-default variants exercise the drop-the-line writeConfig branch.
	run(t, "config", "mmr", "off")
	run(t, "config", "context", "default")
	run(t, "config", "embedder", "static")

	// Error branches.
	for _, args := range [][]string{
		{"config", "context", "huge"},
		{"config", "embedder", "faiss"},
		{"config", "mmr", "maybe"},
		{"config", "nonsense", "x"},
		{"config", "context"}, // len(args)==1 => usage error
	} {
		if _, err := runErr(t, args...); err == nil {
			t.Fatalf("%v should error", args)
		}
	}
}

func TestCoreA_WriteConfigPreservesForeignLines(t *testing.T) {
	withTempHome(t)
	dir := configDirFor(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	initial := strings.Join([]string{
		"# hand-written comment",
		"vault_dir = \"/first/vault\"",
		"vault_dir = \"/dup/vault\"", // duplicate owned key => collapsed
		"data_dir =",                 // empty dir value => preserved verbatim, never dropped
		"custom_key = \"keepme\"",    // foreign key => preserved
		"embedder = \"ollama\"",
	}, "\n")
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	// Any set triggers the read-modify-write path.
	run(t, "config", "context", "small")

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, "# hand-written comment") {
		t.Errorf("comment not preserved:\n%s", got)
	}
	if !strings.Contains(got, `custom_key = "keepme"`) {
		t.Errorf("foreign key not preserved:\n%s", got)
	}
	if strings.Count(got, "vault_dir = ") != 1 {
		t.Errorf("duplicate owned key not collapsed to one line:\n%s", got)
	}
	if !strings.Contains(got, "data_dir =") {
		t.Errorf("empty dir value must be preserved verbatim:\n%s", got)
	}
	if !strings.Contains(got, `context = "small"`) {
		t.Errorf("new owned key not appended:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// init: repoint (confirmed + declined) + scaffold idempotence
// ---------------------------------------------------------------------------

func TestCoreA_CmdInitRepointConfirmed(t *testing.T) {
	withTempHome(t)
	run(t, "init") // default vault + config on disk

	newVault := filepath.Join(t.TempDir(), "moved-vault")

	orig := confirmVaultRepointFn
	confirmVaultRepointFn = func(_ io.Reader, _ io.Writer, _, _ string) error { return nil } // confirm
	t.Cleanup(func() { confirmVaultRepointFn = orig })

	out := run(t, "init", "--vault", newVault)
	if !strings.Contains(out, "Mora initialized") {
		t.Fatalf("repointed init should complete; got:\n%s", out)
	}
	cfg := mustConfig(t)
	if filepath.Clean(cfg.VaultDir) != filepath.Clean(newVault) {
		t.Fatalf("config vault_dir not repointed: %s", cfg.VaultDir)
	}
	if _, err := os.Stat(markerPath(cfg)); err != nil {
		t.Fatalf("new vault should have a marker: %v", err)
	}
}

func TestCoreA_CmdInitRepointDeclined(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfgBefore := mustConfig(t)

	orig := confirmVaultRepointFn
	confirmVaultRepointFn = func(_ io.Reader, _ io.Writer, _, _ string) error { return errString("init cancelled") }
	t.Cleanup(func() { confirmVaultRepointFn = orig })

	newVault := filepath.Join(t.TempDir(), "rejected-vault")
	if _, err := runErr(t, "init", "--vault", newVault); err == nil {
		t.Fatal("declined repoint must return an error")
	}
	cfgAfter := mustConfig(t)
	if cfgAfter.VaultDir != cfgBefore.VaultDir {
		t.Fatalf("declined repoint must leave vault_dir unchanged: %s -> %s", cfgBefore.VaultDir, cfgAfter.VaultDir)
	}
}

func TestCoreA_ConfirmVaultRepointNonInteractive(t *testing.T) {
	// Non-*os.File stdin => refuse, with the manual-alternative message.
	var out bytes.Buffer
	err := confirmVaultRepoint(strings.NewReader(""), &out, "/old", "/new")
	if err == nil || !strings.Contains(err.Error(), "refusing to repoint") {
		t.Fatalf("non-interactive repoint must be refused, got %v", err)
	}
}

func TestCoreA_ScaffoldControlFilesIdempotent(t *testing.T) {
	cfg := coreADirsCfg(t)
	if err := os.MkdirAll(cfg.VaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := scaffoldControlFiles(cfg); err != nil {
		t.Fatalf("first scaffold: %v", err)
	}
	// Mutate one file, then re-run: existing files are skipped (not overwritten).
	live := filepath.Join(cfg.VaultDir, "live-tasks.md")
	if err := os.WriteFile(live, []byte("EDITED"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := scaffoldControlFiles(cfg); err != nil {
		t.Fatalf("second scaffold: %v", err)
	}
	b, _ := os.ReadFile(live)
	if string(b) != "EDITED" {
		t.Fatalf("scaffold must not overwrite an existing file, got %q", string(b))
	}
}

// ---------------------------------------------------------------------------
// write / read / list / search / delete / context / think
// ---------------------------------------------------------------------------

func TestCoreA_CmdWriteVariants(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	// Missing title/text => error.
	if _, err := runErr(t, "write", "--text", "body only"); err == nil {
		t.Fatal("write without --title must error")
	}
	// Positional args become the text.
	out := run(t, "write", "--title", "Positional", "the", "body", "words")
	if !strings.Contains(out, "Positional") {
		t.Fatalf("write with positional text should emit the memory; got:\n%s", out)
	}

	// Degraded-rebuild path: swap the vault marker so the post-write rebuild is
	// blocked on identity; the write still succeeds and warns.
	cfg := mustConfig(t)
	coreASwapVaultMarker(t, cfg, "v_someone_elses")
	out = run(t, "write", "--title", "Degraded", "--text", "still saved")
	if !strings.Contains(out, "warning: memory saved but the search index was not updated") {
		t.Fatalf("blocked rebuild must warn but still save; got:\n%s", out)
	}
	if !strings.Contains(out, "Degraded") {
		t.Fatalf("degraded write must still emit the memory; got:\n%s", out)
	}
}

// coreASwapVaultMarker overwrites the vault identity marker with a foreign id so the
// next rebuild trips decBlockIdentity.
func coreASwapVaultMarker(t *testing.T, cfg Config, id string) {
	t.Helper()
	m := vaultMarker{Schema: vaultMarkerSchema, VaultID: id, CreatedAt: nowRFC3339(), CreatedBy: "coreA-test"}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath(cfg), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCoreA_CmdReadListSearchDelete(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	writeOut := run(t, "write", "--scope", "project:acme", "--type", "decision",
		"--title", "Ship it", "--text", "we decided to ship the coreA cover", "--json")
	var m Memory
	if err := json.Unmarshal([]byte(writeOut), &m); err != nil {
		t.Fatalf("write --json: %v\n%s", err, writeOut)
	}

	// read <id> --json (flag after positional exercises flagsFirst).
	readOut := run(t, "read", m.ID, "--json")
	var got Memory
	if err := json.Unmarshal([]byte(readOut), &got); err != nil {
		t.Fatalf("read --json: %v\n%s", err, readOut)
	}
	if got.Title != "Ship it" {
		t.Fatalf("read returned wrong memory: %+v", got)
	}
	// read with no id and a missing id both error.
	if _, err := runErr(t, "read"); err == nil {
		t.Fatal("read with no id must error")
	}
	if _, err := runErr(t, "read", "no-such-id"); err == nil {
		t.Fatal("read of a missing id must error")
	}

	// list --json returns the memory.
	listOut := run(t, "list", "--scope", "project:acme", "--json")
	if !strings.Contains(listOut, "Ship it") {
		t.Fatalf("list should include the memory; got:\n%s", listOut)
	}

	// search help + no-query + real query.
	if out := run(t, "search", "--help"); !strings.Contains(out, "usage: mora search") {
		t.Fatalf("search --help; got:\n%s", out)
	}
	if _, err := runErr(t, "search"); err == nil {
		t.Fatal("search with no query must error")
	}
	if out := run(t, "search", "ship", "--scope", "project:acme", "--json"); !strings.Contains(out, "Ship it") {
		t.Fatalf("search should find the memory; got:\n%s", out)
	}

	// delete: no-yes refusal, missing id, then real delete.
	if _, err := runErr(t, "delete", m.ID); err == nil {
		t.Fatal("delete without --yes must refuse")
	}
	if _, err := runErr(t, "delete", "--yes"); err == nil {
		t.Fatal("delete with no id must error")
	}
	if _, err := runErr(t, "delete", "missing-id", "--yes"); err == nil {
		t.Fatal("delete of a missing id must error")
	}
	if out := run(t, "delete", m.ID, "--yes"); !strings.Contains(out, "deleted "+m.ID) {
		t.Fatalf("delete should confirm; got:\n%s", out)
	}
}

func TestCoreA_CmdContext(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "project:ctx", "--title", "Auth design",
		"--text", "the auth token flow uses OAuth loopback")

	// No query => listMemories path, plain text output.
	plain := run(t, "context", "--scope", "project:ctx", "--budget", "500")
	if strings.TrimSpace(plain) == "" {
		t.Fatal("context (no query) should emit text")
	}
	// With query => hybridSearch path, --json envelope.
	jsonOut := run(t, "context", "--scope", "project:ctx", "--query", "auth", "--json")
	var env struct {
		Context string   `json:"context"`
		Items   []Memory `json:"items"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &env); err != nil {
		t.Fatalf("context --json: %v\n%s", err, jsonOut)
	}
	if env.Context == "" {
		t.Fatalf("context --json should carry a context string; got:\n%s", jsonOut)
	}
}

func TestCoreA_CmdThink(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "project:think", "--title", "Launch date",
		"--text", "Sam decided the launch is the 14th")

	// Empty query => usage error.
	if _, err := runErr(t, "think"); err == nil {
		t.Fatal("think with no query must error")
	}
	// Human (printThink) path with flags.
	human := run(t, "think", "what did Sam decide", "--scope", "project:think", "--limit", "5")
	if !strings.Contains(human, "Evidence") {
		t.Fatalf("think should print an Evidence header; got:\n%s", human)
	}
	// JSON path.
	if out := run(t, "think", "launch", "--json"); !strings.Contains(out, "\"query\"") && !strings.Contains(out, "query") {
		t.Fatalf("think --json should emit a structured result; got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// index / tasks / brief / lint / backup
// ---------------------------------------------------------------------------

func TestCoreA_CmdIndex(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	var statusReceipt struct {
		Schema        string `json:"schema"`
		SchemaVersion int    `json:"schema_version"`
		Subcommand    string `json:"subcommand"`
		State         string `json:"state"`
	}
	if err := json.Unmarshal([]byte(run(t, "index", "--json")), &statusReceipt); err != nil {
		t.Fatalf("index --json must emit one receipt: %v", err)
	}
	if statusReceipt.Schema != "mora.index" || statusReceipt.SchemaVersion != 1 || statusReceipt.Subcommand != "status" || statusReceipt.State == "" {
		t.Fatalf("index --json receipt = %+v", statusReceipt)
	}
	// Bad usage.
	if _, err := runErr(t, "index"); err == nil {
		t.Fatal("index with no subcommand must error")
	}
	if _, err := runErr(t, "index", "wat"); err == nil {
		t.Fatal("index with a bad subcommand must error")
	}
	if _, err := runErr(t, "index", "--bogusflag"); err == nil {
		t.Fatal("index with an unknown flag must error")
	} else {
		var typed moraError
		if !errors.As(err, &typed) || typed.Code != errCodeUsageUnknownFlag {
			t.Fatalf("index unknown flag error = %v, want usage.unknown_flag", err)
		}
	}
	// rebuild + rebuild --force.
	if out := run(t, "index", "rebuild"); !strings.Contains(out, "indexed") {
		t.Fatalf("index rebuild; got:\n%s", out)
	}
	if out := run(t, "index", "rebuild", "--force"); !strings.Contains(out, "indexed") {
		t.Fatalf("index rebuild --force; got:\n%s", out)
	}
}

func TestCoreA_CmdTasks(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	if _, err := runErr(t, "tasks"); err == nil {
		t.Fatal("tasks with no subcommand must error")
	}
	if _, err := runErr(t, "tasks", "bogus"); err == nil {
		t.Fatal("tasks with a bad subcommand must error")
	}

	// add (name-first, flags after).
	if out := run(t, "tasks", "add", "Reply to Sam", "--pri", "P0"); !strings.Contains(out, "task added: Reply to Sam") {
		t.Fatalf("tasks add; got:\n%s", out)
	}
	// add again => exists.
	if out := run(t, "tasks", "add", "Reply to Sam"); !strings.Contains(out, "task exists") {
		t.Fatalf("re-add should report exists; got:\n%s", out)
	}
	// add with '|' rejected; add with empty name rejected.
	if _, err := runErr(t, "tasks", "add", "bad|name"); err == nil {
		t.Fatal("task name with '|' must error")
	}
	if _, err := runErr(t, "tasks", "add", "   "); err == nil {
		t.Fatal("empty task name must error")
	}

	// list (plain + json).
	if out := run(t, "tasks", "list"); !strings.Contains(out, "Reply to Sam") {
		t.Fatalf("tasks list; got:\n%s", out)
	}
	if out := run(t, "tasks", "list", "--json"); !strings.Contains(out, "Reply to Sam") {
		t.Fatalf("tasks list --json; got:\n%s", out)
	}

	// done: no name => error; unknown name => error; real done => confirm.
	if _, err := runErr(t, "tasks", "done"); err == nil {
		t.Fatal("tasks done with no name must error")
	}
	if _, err := runErr(t, "tasks", "done", "nonexistent task"); err == nil {
		t.Fatal("tasks done of an unknown task must error")
	}
	if out := run(t, "tasks", "done", "Reply to Sam"); !strings.Contains(out, "task done: Reply to Sam") {
		t.Fatalf("tasks done; got:\n%s", out)
	}

	// sync --write path.
	if out := run(t, "tasks", "sync", "--write"); !strings.Contains(out, "tasks added:") {
		t.Fatalf("tasks sync --write; got:\n%s", out)
	}
}

func TestCoreA_CmdBrief(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--scope", "project:brief", "--title", "Kickoff",
		"--text", "the kickoff notes for the coreA brief")

	// Plain brief (styled off-TTY = raw markdown).
	if out := run(t, "brief"); strings.TrimSpace(out) == "" {
		t.Fatal("brief should render a body")
	}
	// JSON result shape.
	jsonOut := run(t, "brief", "--json")
	var br briefResult
	if err := json.Unmarshal([]byte(jsonOut), &br); err != nil {
		t.Fatalf("brief --json: %v\n%s", err, jsonOut)
	}
	// Envelope (unfiltered) exercises briefDigest + its zero-item fallback window.
	if out := run(t, "brief", "--envelope"); strings.TrimSpace(out) == "" {
		t.Fatal("brief --envelope should render body + synthesis prompt")
	}
	// Filtered envelope exercises the filteredBriefDigest branch.
	if out := run(t, "brief", "--envelope", "--scope", "project:brief"); strings.TrimSpace(out) == "" {
		t.Fatal("brief --envelope --scope should render")
	}
	// Fresh regen.
	if out := run(t, "brief", "--fresh"); strings.TrimSpace(out) == "" {
		t.Fatal("brief --fresh should render")
	}
	// Unresolvable entity filter errors before rendering.
	if _, err := runErr(t, "brief", "--entity", "no-such-person-xyz"); err == nil {
		t.Fatal("brief --entity with no match must error")
	}
}

func TestCoreA_CmdLint(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	// Fresh init scaffolds every required file => lint ok.
	if out := run(t, "lint"); !strings.Contains(out, "lint ok") {
		t.Fatalf("lint on a fresh vault should be ok; got:\n%s", out)
	}
	// Remove a required file => lint reports the missing one.
	cfg := mustConfig(t)
	if err := os.Remove(filepath.Join(cfg.VaultDir, "heartbeat.md")); err != nil {
		t.Fatal(err)
	}
	if out := run(t, "lint"); !strings.Contains(out, "missing heartbeat.md") {
		t.Fatalf("lint should report the missing file; got:\n%s", out)
	}
}

func TestCoreA_CmdBackup(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	out := strings.TrimSpace(run(t, "backup"))
	if !strings.HasSuffix(out, ".tar.gz") {
		t.Fatalf("backup should print the archive path; got:\n%s", out)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		t.Fatalf("backup archive should exist and be non-empty: err=%v", err)
	}
}

// ---------------------------------------------------------------------------
// usage / disconnect
// ---------------------------------------------------------------------------

func TestCoreA_CmdUsageAndReport(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	// report with no log yet.
	if out := run(t, "usage", "report"); !strings.Contains(out, "no usage recorded") {
		t.Fatalf("usage report (empty); got:\n%s", out)
	}
	// Seed two events, then report.
	logUsage(cfg, usageEvent{Tool: "search_memory", Results: 0, Millis: 5})
	logUsage(cfg, usageEvent{Tool: "search_memory", Results: 3, Millis: 9})
	out := run(t, "usage", "report")
	if !strings.Contains(out, "total calls: 2") || !strings.Contains(out, "empty-result rate") || !strings.Contains(out, "latency p50") {
		t.Fatalf("usage report should summarize; got:\n%s", out)
	}
	// off writes the marker; on removes it; queries on/off toggles the marker.
	run(t, "usage", "off")
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "usage", "OFF")); err != nil {
		t.Fatalf("usage off should write the OFF marker: %v", err)
	}
	run(t, "usage", "on")
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "usage", "OFF")); !os.IsNotExist(err) {
		t.Fatalf("usage on should remove the OFF marker")
	}
	run(t, "usage", "queries", "on")
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "usage", "QUERIES")); err != nil {
		t.Fatalf("usage queries on should write the QUERIES marker: %v", err)
	}
	run(t, "usage", "queries", "off") // remove; also idempotent (no error if absent)
	run(t, "usage", "queries", "off")

	// Error branches.
	for _, args := range [][]string{
		{"usage"},
		{"usage", "bogus"},
		{"usage", "queries"},
		{"usage", "queries", "maybe"},
	} {
		if _, err := runErr(t, args...); err == nil {
			t.Fatalf("%v should error", args)
		}
	}
}

func TestCoreA_CmdDisconnect(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	// Wrong/absent subcommand errors.
	if _, err := runErr(t, "disconnect"); err == nil {
		t.Fatal("disconnect with no arg must error")
	}
	if _, err := runErr(t, "disconnect", "imessage"); err == nil {
		t.Fatal("disconnect of a non-google connector must error")
	}
	// google with no saved token still succeeds (nothing to revoke/remove).
	if out := run(t, "disconnect", "google"); !strings.Contains(out, "disconnected google") {
		t.Fatalf("disconnect google; got:\n%s", out)
	}
}
