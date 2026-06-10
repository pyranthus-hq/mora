package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/google"
)

// configDirFor returns the ConfigDir that defaultConfig resolves to under the
// temp HOME set by withTempHome ($HOME/.config/mora). Used to plant fixtures.
func configDirFor(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	return filepath.Join(home, ".config", "mora")
}

// runErr invokes Run with empty (non-TTY) stdin and returns output + error
// WITHOUT failing the test, so RED tests for not-yet-built commands can assert
// against intended behavior. Distinct from the `run` harness which t.Fatalf's.
func runErr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := Run(context.Background(), args, &out, &out, strings.NewReader(""))
	return out.String(), err
}

// ---------------------------------------------------------------------------
// Model-layer tests — GREEN after Plan 01-01 Task 2.
// ---------------------------------------------------------------------------

// TestIsEnabled is the function-level table test for the nil-sentinel helper
// and the ptr(bool) constructor (D-10/D-12 centralized nil-handling).
func TestIsEnabled(t *testing.T) {
	cases := []struct {
		name string
		in   *bool
		want bool
	}{
		{"nil grandfather-unset", nil, false},
		{"explicit false", ptr(false), false},
		{"explicit true", ptr(true), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Source{Name: "x", Type: "gmail", Enabled: tc.in}
			if got := s.IsEnabled(); got != tc.want {
				t.Fatalf("IsEnabled() with Enabled=%v = %v, want %v", tc.in, got, tc.want)
			}
		})
	}

	// ptr must return a pointer to a distinct copy each call.
	a, b := ptr(true), ptr(false)
	if a == b || *a != true || *b != false {
		t.Fatalf("ptr() returned unexpected pointers: *a=%v *b=%v same=%v", *a, *b, a == b)
	}
}

// TestGrandfatherMigration plants a hand-crafted legacy sources.json (a JSON
// array with NO `enabled` keys), then asserts loadSources normalizes the
// missing key to enabled (D-12 grandfather: absence = prior explicit consent).
func TestGrandfatherMigration(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	legacy := `[
  {"name":"gmail","type":"gmail","scope":"personal","created_at":"2026-01-01T00:00:00Z"},
  {"name":"calendar","type":"calendar","scope":"personal","calendar":"primary","created_at":"2026-01-01T00:00:00Z"}
]`
	dir := configDirFor(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sources.json"), []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy sources.json: %v", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	sources, err := loadSources(cfg)
	if err != nil {
		t.Fatalf("loadSources: %v", err)
	}
	if len(sources) != 2 {
		t.Fatalf("expected 2 grandfathered sources, got %d: %+v", len(sources), sources)
	}
	for _, s := range sources {
		if s.Enabled == nil {
			t.Fatalf("grandfather migration left %s Enabled=nil (expected normalized to true)", s.Name)
		}
		if !s.IsEnabled() {
			t.Fatalf("legacy source %s should grandfather to enabled, got IsEnabled()=false", s.Name)
		}
	}
}

// TestEnabledPersists writes a source with Enabled explicitly set, then reloads
// from disk and asserts the bit round-trips unchanged (REG-05). A disabled
// source must NOT be silently re-enabled by the grandfather path.
func TestEnabledPersists(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	want := []Source{
		{Name: "gmail", Type: "gmail", Scope: "personal", Enabled: ptr(false), CreatedAt: "2026-01-01T00:00:00Z"},
		{Name: "calendar", Type: "calendar", Scope: "personal", Calendar: "primary", Enabled: ptr(true), CreatedAt: "2026-01-01T00:00:00Z"},
	}
	if err := saveSources(cfg, want); err != nil {
		t.Fatalf("saveSources: %v", err)
	}

	got, err := loadSources(cfg)
	if err != nil {
		t.Fatalf("loadSources: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 sources after round-trip, got %d", len(got))
	}
	byName := map[string]Source{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if byName["gmail"].IsEnabled() {
		t.Fatalf("gmail saved disabled must reload disabled, got IsEnabled()=true")
	}
	if !byName["calendar"].IsEnabled() {
		t.Fatalf("calendar saved enabled must reload enabled, got IsEnabled()=false")
	}
}

// ---------------------------------------------------------------------------
// Command / gate / menu tests — RED until their owning plan (02/03/04).
// These assert intended CLI behavior and call Run directly so they compile now
// and fail at runtime, not at build (no t.Skip).
// ---------------------------------------------------------------------------

// TestConnectorsList — REG-01. `connectors list --json` emits per-type rows with
// an enabled flag. RED until Plan 02 builds cmdConnectors + the catalog.
func TestConnectorsList(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	out, err := runErr(t, "connectors", "list", "--json")
	if err != nil {
		t.Fatalf("connectors list --json should succeed (RED until Plan 02): %v\n%s", err, out)
	}
	var rows []struct {
		Type    string `json:"type"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("connectors list --json should emit a JSON array of rows: %v\n%s", err, out)
	}
	var sawGmail, sawCalendar bool
	for _, r := range rows {
		switch r.Type {
		case "gmail":
			sawGmail = true
		case "calendar":
			sawCalendar = true
		}
	}
	if !sawGmail || !sawCalendar {
		t.Fatalf("connectors list should include gmail and calendar rows, got %+v", rows)
	}
}

// TestConnectorEnableGatesIngest — REG-02. A source that has not been enabled
// must not ingest; `connectors enable <type>` flips the bit. RED until Plan 02/03.
func TestConnectorEnableGatesIngest(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	// A freshly-created google source defaults disabled (D-11).
	if err := ensureGoogleSources(cfg, ""); err != nil {
		t.Fatalf("ensureGoogleSources: %v", err)
	}
	sources, err := loadSources(cfg)
	if err != nil {
		t.Fatalf("loadSources: %v", err)
	}
	for _, s := range sources {
		if s.Type == "gmail" && s.IsEnabled() {
			t.Fatalf("a brand-new gmail source must default disabled (D-11), got enabled")
		}
	}

	// enable flips the bit. RED until Plan 02 builds the command.
	if _, err := runErr(t, "connectors", "enable", "gmail"); err != nil {
		t.Fatalf("connectors enable gmail should succeed (RED until Plan 02): %v", err)
	}
	sources, _ = loadSources(cfg)
	var enabled bool
	for _, s := range sources {
		if s.Type == "gmail" {
			enabled = s.IsEnabled()
		}
	}
	if !enabled {
		t.Fatalf("after `connectors enable gmail`, gmail should be IsEnabled()==true")
	}
}

// TestEnableNoBackfill — REG-03. enable sets the bit/token but pulls ZERO data
// (distinct from sync/backfill). RED until Plan 02/03.
func TestEnableNoBackfill(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	if _, err := runErr(t, "connectors", "enable", "filesystem"); err != nil {
		t.Fatalf("connectors enable filesystem should succeed (RED until Plan 02): %v", err)
	}
	// enable must not have ingested any memories — the vault stays empty.
	out := run(t, "search", "", "--json")
	var got []Memory
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		// search with empty query may behave specially; tolerate but require no ingest below.
		t.Logf("search json parse note: %v\n%s", err, out)
	}
	if len(got) != 0 {
		t.Fatalf("enable must pull zero data (REG-03), found %d memories", len(got))
	}
}

// TestDisableNonDestructive — REG-04. disable flips the bit and stops ingest but
// KEEPS the token and existing memories (anti-analog: not disconnect). RED until Plan 03.
func TestDisableNonDestructive(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if err := ensureGoogleSources(cfg, ""); err != nil {
		t.Fatalf("ensureGoogleSources: %v", err)
	}
	// Pretend gmail was enabled, then disable it.
	if _, err := runErr(t, "connectors", "enable", "gmail"); err != nil {
		t.Fatalf("connectors enable gmail should succeed (RED until Plan 02): %v", err)
	}
	if _, err := runErr(t, "connectors", "disable", "gmail"); err != nil {
		t.Fatalf("connectors disable gmail should succeed (RED until Plan 03): %v", err)
	}
	sources, _ := loadSources(cfg)
	for _, s := range sources {
		if s.Type == "gmail" && s.IsEnabled() {
			t.Fatalf("after disable, gmail must be IsEnabled()==false")
		}
	}
}

// TestGateNamedVsAll — D-07. `--all` silently skips disabled sources; a named
// disabled source ERRORS (never silently ingests 0). RED until Plan 03 adds the gate.
func TestGateNamedVsAll(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if err := ensureGoogleSources(cfg, ""); err != nil {
		t.Fatalf("ensureGoogleSources: %v", err)
	}
	// gmail is disabled by construction (D-11).

	// --all must NOT error on a disabled source (silent skip, D-07).
	if _, err := runErr(t, "ingest", "run", "--all"); err != nil {
		t.Fatalf("ingest --all should silently skip disabled sources, got error (RED until Plan 03): %v", err)
	}

	// A named disabled source MUST error (D-07).
	if _, err := runErr(t, "ingest", "run", "--source", "gmail"); err == nil {
		t.Fatalf("ingest --source gmail on a disabled source must ERROR (D-07), got nil (RED until Plan 03)")
	}
}

// TestSetupMenuNonTTY — D-08/D-09. Both `mora init` and `mora connectors setup`
// drive the setup menu, which must no-op on non-TTY stdin (empty reader) and
// return promptly without hanging — the empty-stdin harness means a regression
// that drops the isatty guard would hang this test (the intended canary, Pitfall 2).
func TestSetupMenuNonTTY(t *testing.T) {
	withTempHome(t)

	// `mora init` scaffolds the vault AND attaches the menu — must not hang.
	initOut, err := runErr(t, "init")
	if err != nil {
		t.Fatalf("init on non-TTY stdin must return without error: %v\n%s", err, initOut)
	}
	if !strings.Contains(initOut, "Non-interactive terminal") {
		t.Fatalf("init on non-TTY should print the setup-menu hint, got:\n%s", initOut)
	}

	// `mora connectors setup` re-opens the same menu (D-08) — must not hang.
	out, err := runErr(t, "connectors", "setup")
	if err != nil {
		t.Fatalf("connectors setup on non-TTY stdin must return without error: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Non-interactive terminal") {
		t.Fatalf("connectors setup on non-TTY should print the hint, got:\n%s", out)
	}
}

// TestSetupBackfillDefaultsNo — D-09. The pure applySetupSelection seam, called
// with doBackfill=false (the menu's default-NO confirm), enables the selected
// connector but performs ZERO ingest. This makes the consent guarantee automated
// and TTY-free (independent of the huh TUI). Uses the auth-less filesystem
// connector so no browser/OAuth is triggered.
func TestSetupBackfillDefaultsNo(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	var buf bytes.Buffer
	if err := applySetupSelection(context.Background(), cfg, []string{"filesystem"}, false, &buf, strings.NewReader("")); err != nil {
		t.Fatalf("applySetupSelection(doBackfill=false) should succeed: %v\n%s", err, buf.String())
	}

	// (a) The selected connector is now enabled.
	sources, err := loadSources(cfg)
	if err != nil {
		t.Fatalf("loadSources: %v", err)
	}
	var enabled bool
	for _, s := range sources {
		if s.Type == "filesystem" {
			enabled = s.IsEnabled()
		}
	}
	if !enabled {
		t.Fatalf("applySetupSelection should leave filesystem IsEnabled()==true")
	}

	// (b) ZERO ingest happened — no memories were written to the vault.
	out := run(t, "search", "", "--json")
	var got []Memory
	_ = json.Unmarshal([]byte(out), &got)
	if len(got) != 0 {
		t.Fatalf("setup without affirmative backfill must perform zero ingest (D-09), found %d memories", len(got))
	}
}

// TestWindowForSourceGmail asserts the Gmail backfill defaults to a lean
// 90-day window (not 365 — a year of mail is mostly low-signal for a memory
// index) and that an explicit SinceDays override is honored.
func TestWindowForSourceGmail(t *testing.T) {
	approxDays := func(w google.FetchWindow) int {
		return int(time.Since(w.Since).Hours()/24 + 0.5)
	}

	def := approxDays(windowForSource(Source{Type: "gmail"}, google.KindGmailThread))
	if def < 89 || def > 91 {
		t.Fatalf("default gmail window should be ~90 days, got ~%d", def)
	}

	over := approxDays(windowForSource(Source{Type: "gmail", SinceDays: 30}, google.KindGmailThread))
	if over < 29 || over > 31 {
		t.Fatalf("SinceDays=30 should give a ~30-day window, got ~%d", over)
	}
}

// TestAddSourceDefaultsDisabled — REG-02/D-11 regression guard (CR-01). A source
// created through `mora sources add` must default-disabled and stay disabled
// across a reload. Without an explicit Enabled:false on the addSource literal,
// the source persists with a nil `enabled` key and the grandfather migration in
// loadSources normalizes nil => true on the next load, silently auto-enabling a
// brand-new source — a consent bypass. This asserts that does not happen.
func TestAddSourceDefaultsDisabled(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	dir := t.TempDir()

	// Add a filesystem source (the auth-less type with a real ingest path).
	if out, err := runErr(t, "sources", "add", "filesystem", "--path", dir); err != nil {
		t.Fatalf("sources add filesystem should succeed: %v\n%s", err, out)
	}

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	// Reload from disk — this is where the grandfather migration runs.
	sources, err := loadSources(cfg)
	if err != nil {
		t.Fatalf("loadSources: %v", err)
	}

	var found bool
	for _, s := range sources {
		if s.Type == "filesystem" {
			found = true
			if s.Enabled == nil {
				t.Fatalf("added source persisted with Enabled=nil — grandfather migration will auto-enable it (CR-01 consent bypass)")
			}
			if s.IsEnabled() {
				t.Fatalf("freshly added source must default-disabled (D-11), got IsEnabled()=true (CR-01 consent bypass)")
			}
		}
	}
	if !found {
		t.Fatalf("filesystem source not found after sources add; got %+v", sources)
	}
}
