package mora

// mora_coreA_cover2_test.go — coreA coverage worker, part 2. Covers the connector
// registry, guided-setup seams, sync/ingest/reingest commands, doctor, and the
// source-mutation helpers (mora.go lines ~1900–3024). Direct-call unit tests hit
// the pure helpers; the CLI harness drives the command dispatch.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/google"
	"github.com/pyranthus-hq/mora/internal/memory"
	"golang.org/x/oauth2"
)

// ---------------------------------------------------------------------------
// source-mutation helpers (unit)
// ---------------------------------------------------------------------------

func TestCoreA_SetSourceHelpers(t *testing.T) {
	cfg := coreADirsCfg(t)
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Seed a two-account google registry.
	seed := []Source{
		{Name: "gmail", Type: "gmail", Scope: "personal", Account: "", Enabled: genericutil.Ptr(false), CreatedAt: nowRFC3339()},
		{Name: "calendar", Type: "calendar", Scope: "personal", Account: "", Enabled: genericutil.Ptr(false), CreatedAt: nowRFC3339()},
	}
	if err := saveSources(cfg, seed); err != nil {
		t.Fatal(err)
	}

	// setSourceEnabledByName: flips exactly one row; errors on a missing name.
	if err := setSourceEnabledByName(cfg, "gmail", true); err != nil {
		t.Fatal(err)
	}
	if err := setSourceEnabledByName(cfg, "no-such", true); err == nil {
		t.Fatal("setSourceEnabledByName must error on a missing name")
	}
	// setSourceSinceDaysByName: sets the window on one row; errors on missing.
	if err := setSourceSinceDaysByName(cfg, "gmail", 365); err != nil {
		t.Fatal(err)
	}
	if err := setSourceSinceDaysByName(cfg, "no-such", 30); err == nil {
		t.Fatal("setSourceSinceDaysByName must error on a missing name")
	}
	// setSourceSinceDays: sets the window on every row of a type (no-op if absent).
	if err := setSourceSinceDays(cfg, "calendar", 10); err != nil {
		t.Fatal(err)
	}
	if err := setSourceSinceDays(cfg, "imessage", 10); err != nil {
		t.Fatal(err) // no matching row => harmless save, no error
	}
	// setSourceEmailByAccount: stamps the email on the account's rows.
	if err := setSourceEmailByAccount(cfg, "", "me@example.com"); err != nil {
		t.Fatal(err)
	}

	got, err := loadSources(cfg)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Source{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if !byName["gmail"].IsEnabled() {
		t.Error("gmail should be enabled by name")
	}
	if byName["calendar"].IsEnabled() {
		t.Error("calendar must NOT have been flipped by the gmail-name call")
	}
	if byName["gmail"].SinceDays != 365 {
		t.Errorf("gmail since_days = %d, want 365", byName["gmail"].SinceDays)
	}
	if byName["calendar"].SinceDays != 10 {
		t.Errorf("calendar since_days = %d, want 10", byName["calendar"].SinceDays)
	}
	if byName["gmail"].Email != "me@example.com" || byName["calendar"].Email != "me@example.com" {
		t.Errorf("email not stamped on both account rows: %+v", byName)
	}
}

func TestCoreA_SetSourceEnabledCreatesRow(t *testing.T) {
	cfg := coreADirsCfg(t)
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// No source row yet => setSourceEnabled mints one carrying the consent bit
	// for types whose bare row is meaningful (implicit local store).
	if err := setSourceEnabled(cfg, "imessage", true); err != nil {
		t.Fatal(err)
	}
	got, _ := loadSources(cfg)
	if len(got) != 1 || got[0].Type != "imessage" || !got[0].IsEnabled() {
		t.Fatalf("setSourceEnabled should create an enabled imessage row, got %+v", got)
	}
	// filesystem is the exception: a row without a path can never ingest, so
	// setSourceEnabled must NOT mint one (and disable never mints for any type —
	// absence already means disabled).
	if err := setSourceEnabled(cfg, "filesystem", true); err != nil {
		t.Fatal(err)
	}
	if err := setSourceEnabled(cfg, "gmail", false); err != nil {
		t.Fatal(err)
	}
	got, _ = loadSources(cfg)
	if len(got) != 1 {
		t.Fatalf("no phantom rows should be minted (filesystem enable / gmail disable), got %+v", got)
	}
}

func TestCoreA_SetIMessageDenyList(t *testing.T) {
	cfg := coreADirsCfg(t)
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// No imessage row yet => creates one with the deny fields.
	if err := setIMessageDenyList(cfg, []string{"+15551234567"}, []string{"Family"}); err != nil {
		t.Fatal(err)
	}
	got, _ := loadSources(cfg)
	if len(got) != 1 || got[0].Type != "imessage" {
		t.Fatalf("setIMessageDenyList should create an imessage row, got %+v", got)
	}
	if strings.Join(got[0].DenyContacts, ",") != "+15551234567" || strings.Join(got[0].DenyConversations, ",") != "Family" {
		t.Fatalf("deny fields not persisted: %+v", got[0])
	}
	// Existing row => updates in place (found branch).
	if err := setIMessageDenyList(cfg, []string{"a@b.com"}, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = loadSources(cfg)
	if len(got) != 1 || strings.Join(got[0].DenyContacts, ",") != "a@b.com" {
		t.Fatalf("deny-list update should replace in place, got %+v", got)
	}
}

func TestCoreA_LoadSourcesOrEmptyCorrupt(t *testing.T) {
	cfg := coreADirsCfg(t)
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.ConfigDir, "sources.json"), []byte("not json {{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadSourcesOrEmpty(cfg); got != nil {
		t.Fatalf("loadSourcesOrEmpty on corrupt file must collapse to nil, got %v", got)
	}
}

func TestCoreA_GoogleSetupStep(t *testing.T) {
	// No google types selected => passthrough, not skipped.
	if rem, msg, skipped := googleSetupStep([]string{"imessage", "filesystem"}); skipped || msg != "" || len(rem) != 2 {
		t.Fatalf("no-google selection should pass through, got rem=%v msg=%q skipped=%v", rem, msg, skipped)
	}

	// gmail selected + creds are the placeholder => skip google, keep the rest.
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "") // force embedded placeholder
	rem, msg, skipped := googleSetupStep([]string{"gmail", "imessage"})
	if !skipped || msg == "" || containsType(rem, "gmail") {
		t.Fatalf("placeholder creds should skip google, got rem=%v skipped=%v", rem, skipped)
	}

	// gmail selected + real (non-placeholder) creds => NOT skipped.
	credPath := filepath.Join(t.TempDir(), "client.json")
	cred := `{"installed":{"client_id":"fake-id.apps.googleusercontent.com","client_secret":"s","auth_uri":"https://a","token_uri":"https://t"}}`
	if err := os.WriteFile(credPath, []byte(cred), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MORA_GOOGLE_CREDENTIALS", credPath)
	if !google.IsConfigured() {
		t.Skip("google creds unexpectedly not configured; environment-dependent")
	}
	if rem, msg, skipped := googleSetupStep([]string{"gmail"}); skipped || msg != "" || len(rem) != 1 {
		t.Fatalf("configured creds should NOT skip google, got rem=%v skipped=%v", rem, skipped)
	}
}

// ---------------------------------------------------------------------------
// enable / disable / applySetupSelection / backfill seams
// ---------------------------------------------------------------------------

func TestCoreA_EnableConnectorVariants(t *testing.T) {
	asDarwinOnWindows(t) // exercise the imessage/applecalendar enable FLOW; the Windows refusal is covered elsewhere
	// Unknown type is rejected.
	cfg := coreADirsCfg(t)
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	stdin := strings.NewReader("") // non-TTY

	if err := enableConnector(context.Background(), cfg, "nope", &out, testStderr, stdin); err == nil {
		t.Fatal("enableConnector must reject an unknown type")
	}

	// filesystem with NO configured folder: guidance, no phantom row, no error.
	out.Reset()
	if err := enableConnector(context.Background(), cfg, "filesystem", &out, testStderr, stdin); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "mora connect filesystem") {
		t.Fatalf("enable filesystem without a folder should guide to `mora connect filesystem`; got:\n%s", out.String())
	}

	// filesystem with a configured folder: flips the row, success line.
	if err := addSource(cfg, []string{"filesystem", "--name", "docs", "--path", t.TempDir()}, io.Discard); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := enableConnector(context.Background(), cfg, "filesystem", &out, testStderr, stdin); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "enabled filesystem") {
		t.Fatalf("enable filesystem; got:\n%s", out.String())
	}

	// imessage: no login, Full-Disk-Access guidance.
	out.Reset()
	if err := enableConnector(context.Background(), cfg, "imessage", &out, testStderr, stdin); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "enabled imessage") {
		t.Fatalf("enable imessage; got:\n%s", out.String())
	}

	// applecalendar: same no-login gate.
	out.Reset()
	if err := enableConnector(context.Background(), cfg, "applecalendar", &out, testStderr, stdin); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "enabled applecalendar") {
		t.Fatalf("enable applecalendar; got:\n%s", out.String())
	}

	// gmail (NeedsAuth) non-interactive with NO saved token => notes that it
	// needs authorization, still flips the bit. Plan 01-03 routed that note to
	// stderr, so the success line and the advisory land on different streams.
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")
	out.Reset()
	var authNote bytes.Buffer
	if err := enableConnector(context.Background(), cfg, "gmail", &out, &authNote, stdin); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(authNote.String(), "needs Google authorization") {
		t.Fatalf("enable gmail (no token, non-TTY) should note the auth gap on stderr; got:\n%s", authNote.String())
	}

	// gmail with a SAVED token => reuse branch, no auth prompt.
	if err := google.SaveToken(googleTokenPath(cfg), &oauth2.Token{AccessToken: "x", RefreshToken: "y", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := enableConnector(context.Background(), cfg, "gmail", &out, testStderr, stdin); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Reusing your saved Google sign-in") {
		t.Fatalf("enable gmail with a saved token should reuse it; got:\n%s", out.String())
	}
}

func TestCoreA_DisableConnector(t *testing.T) {
	cfg := coreADirsCfg(t)
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	// Unknown type rejected.
	if err := disableConnector(cfg, "nope", &out); err == nil {
		t.Fatal("disableConnector must reject an unknown type")
	}
	// Enable then disable.
	if err := setSourceEnabled(cfg, "filesystem", true); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := disableConnector(cfg, "filesystem", &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "filesystem disabled") {
		t.Fatalf("disable filesystem; got:\n%s", out.String())
	}
	got, _ := loadSources(cfg)
	for _, s := range got {
		if s.Type == "filesystem" && s.IsEnabled() {
			t.Fatal("filesystem should be disabled after disableConnector")
		}
	}
}

func TestCoreA_ApplySetupSelection(t *testing.T) {
	asDarwinOnWindows(t)
	cfg := coreADirsCfg(t)
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	stdin := strings.NewReader("")

	// filesystem needs a configured folder for enable to have a row to flip.
	if err := addSource(cfg, []string{"filesystem", "--name", "docs", "--path", t.TempDir()}, io.Discard); err != nil {
		t.Fatal(err)
	}

	// doBackfill=false: enable only, ZERO ingest.
	if err := applySetupSelection(context.Background(), cfg, []string{"imessage", "filesystem"}, false, &out, testStderr, stdin); err != nil {
		t.Fatal(err)
	}
	got, _ := loadSources(cfg)
	enabled := map[string]bool{}
	for _, s := range got {
		if s.IsEnabled() {
			enabled[s.Type] = true
		}
	}
	if !enabled["imessage"] || !enabled["filesystem"] {
		t.Fatalf("applySetupSelection(false) should enable selected connectors, got %+v", got)
	}

	// doBackfill=true with no google sources => backfill runs, reports 0.
	out.Reset()
	if err := applySetupSelection(context.Background(), cfg, []string{"filesystem"}, true, &out, testStderr, stdin); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "backfilled 0 item(s)") {
		t.Fatalf("applySetupSelection(true) should report a backfill count; got:\n%s", out.String())
	}
}

func TestCoreA_BackfillEnabledGoogleFailure(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	// An enabled gmail source with NO token => ingestSource fails; the loop must
	// keep going, rebuild, and surface an aggregate error (never swallow).
	enableSources(t, cfg, "gmail")
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")
	var out bytes.Buffer
	total, err := backfillEnabledGoogle(context.Background(), cfg, &out)
	if err == nil {
		t.Fatal("a failing google backfill must return a non-nil aggregate error")
	}
	if total != 0 {
		t.Fatalf("no items should have been ingested, got %d", total)
	}
	if !strings.Contains(out.String(), "sync incomplete") {
		t.Fatalf("a failing source should warn; got:\n%s", out.String())
	}
}

// ---------------------------------------------------------------------------
// connectors / schedule / sources dispatch
// ---------------------------------------------------------------------------

func TestCoreA_CmdConnectors(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	if _, err := runErr(t, "connectors"); err == nil {
		t.Fatal("connectors with no subcommand must error")
	}
	if _, err := runErr(t, "connectors", "bogus"); err == nil {
		t.Fatal("connectors with a bad subcommand must error")
	}
	// list (plain + json).
	if out := run(t, "connectors", "list"); !strings.Contains(out, "gmail") {
		t.Fatalf("connectors list; got:\n%s", out)
	}
	listJSON := run(t, "connectors", "list", "--json")
	// Plan 01-07: the rows move under `connectors` inside the schema envelope.
	var listDoc struct {
		Connectors []catalogRow `json:"connectors"`
	}
	if err := json.Unmarshal([]byte(listJSON), &listDoc); err != nil {
		t.Fatalf("connectors list --json: %v\n%s", err, listJSON)
	}
	rows := listDoc.Connectors
	if len(rows) == 0 {
		t.Fatal("connectors list --json should return the catalog")
	}
	// enable/disable arg-count guards.
	if _, err := runErr(t, "connectors", "enable"); err == nil {
		t.Fatal("connectors enable with no type must error")
	}
	if _, err := runErr(t, "connectors", "disable"); err == nil {
		t.Fatal("connectors disable with no type must error")
	}
	// enable then disable a no-auth connector (needs a configured folder first —
	// a folderless `enable filesystem` is a guided no-op, never a phantom row).
	run(t, "sources", "add", "filesystem", "--name", "docs", "--path", t.TempDir())
	if out := run(t, "connectors", "enable", "filesystem"); !strings.Contains(out, "enabled filesystem") {
		t.Fatalf("connectors enable filesystem; got:\n%s", out)
	}
	if out := run(t, "connectors", "disable", "filesystem"); !strings.Contains(out, "filesystem disabled") {
		t.Fatalf("connectors disable filesystem; got:\n%s", out)
	}
	// setup on a non-TTY prints the hint and returns (never blocks).
	if out := run(t, "connectors", "setup"); !strings.Contains(out, "Non-interactive terminal") {
		t.Fatalf("connectors setup (non-TTY) should print the skip hint; got:\n%s", out)
	}
}

func TestCoreA_CmdSchedule(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	if _, err := runErr(t, "schedule"); err == nil {
		t.Fatal("schedule with no subcommand must error")
	}
	if _, err := runErr(t, "schedule", "bogus"); err == nil {
		t.Fatal("schedule with a bad subcommand must error")
	}
	if _, err := runErr(t, "schedule", "install"); err == nil {
		t.Fatal("schedule install with no job name must error")
	}
	if _, err := runErr(t, "schedule", "install", "not-a-job"); err == nil {
		t.Fatal("schedule install of an unknown job must error")
	}
	// install a known job. On darwin this writes a plist under the temp HOME's
	// LaunchAgents dir and bootstraps it via the (stubbed) launchctl runner; off
	// darwin it prints cron guidance. Either way it must not error.
	withScheduleRunner(t, nil)
	if _, err := runErr(t, "schedule", "install", "pulse-daily"); err != nil {
		t.Fatalf("schedule install pulse-daily should not error: %v", err)
	}
	// list runs cleanly (on darwin it now lists the installed plist).
	out, err := runErr(t, "schedule", "list")
	if err != nil {
		t.Fatalf("schedule list should not error: %v", err)
	}
	if runtime.GOOS == "darwin" && !strings.Contains(out, "com.mora.pulse-daily.plist") {
		t.Fatalf("schedule list should show the installed job; got:\n%s", out)
	}
}

func TestCoreA_CmdSources(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	if _, err := runErr(t, "sources"); err == nil {
		t.Fatal("sources with no subcommand must error")
	}
	if _, err := runErr(t, "sources", "bogus"); err == nil {
		t.Fatal("sources with a bad subcommand must error")
	}
	// add a filesystem source, then list shows it.
	dir := t.TempDir()
	if out := run(t, "sources", "add", "filesystem", "--name", "docs", "--path", dir, "--scope", "personal"); strings.TrimSpace(out) == "" {
		t.Fatalf("sources add should confirm; got:\n%s", out)
	}
	listOut := run(t, "sources", "list")
	if !strings.Contains(listOut, "docs") {
		t.Fatalf("sources list should include the added source; got:\n%s", listOut)
	}
}

// ---------------------------------------------------------------------------
// ingest / sync / reingest / connect
// ---------------------------------------------------------------------------

func TestCoreA_CmdIngest(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	// Bad usage.
	if _, err := runErr(t, "ingest"); err == nil {
		t.Fatal("ingest with no subcommand must error")
	}
	if _, err := runErr(t, "ingest", "walk"); err == nil {
		t.Fatal("ingest with a non-run subcommand must error")
	}

	// A named DISABLED source errors before any ingest.
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{{Name: "off", Type: "filesystem", Scope: "personal", Enabled: genericutil.Ptr(false), CreatedAt: nowRFC3339()}}); err != nil {
		t.Fatal(err)
	}
	if _, err := runErr(t, "ingest", "run", "--source", "off"); err == nil {
		t.Fatal("ingest of a disabled named source must error")
	}

	// Named-source failure surfaces after the final rebuild (ingestSourceFn seam).
	origFn := ingestSourceFn
	t.Cleanup(func() { ingestSourceFn = origFn })
	ingestSourceFn = func(_ Config, s Source, _ io.Writer) (sourceIngestResult, error) {
		if s.Name == "boom" {
			return sourceIngestResult{}, errString("kaboom")
		}
		return sourceIngestResult{Examined: 2, Materialized: 2}, nil
	}
	if err := saveSources(cfg, []Source{
		{Name: "boom", Type: "filesystem", Scope: "personal", Enabled: genericutil.Ptr(true), CreatedAt: nowRFC3339()},
		{Name: "ok", Type: "filesystem", Scope: "personal", Enabled: genericutil.Ptr(true), CreatedAt: nowRFC3339()},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runErr(t, "ingest", "run", "--source", "boom"); err == nil {
		t.Fatal("a failing named source must surface its error")
	}
	// --all: one broken source warns and keeps going. A usable partial result
	// succeeds; callers inspect the aggregate receipt for the failed source.
	out, err := runErr(t, "ingest", "run", "--all")
	if err != nil {
		t.Fatalf("ingest --all with a usable partial result: %v", err)
	}
	if !strings.Contains(out, "sync incomplete") {
		t.Fatalf("ingest --all should warn about the broken source; got:\n%s", out)
	}
}

func TestCoreA_CmdSync(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	// help.
	if out := run(t, "sync", "--help"); !strings.Contains(out, "usage: mora sync") {
		t.Fatalf("sync --help; got:\n%s", out)
	}
	// status with nothing synced.
	if out := run(t, "sync", "status"); !strings.Contains(out, "no sources synced yet") {
		t.Fatalf("sync status (empty); got:\n%s", out)
	}
	// status with a healthy + a failed + a stale (no-error) entry. C3 ▸R2:
	// `mora sync status` now classifies through the SAME worst-first
	// precedence as sourceHealthFor (never > failed > stale > fresh), so an
	// old success WITH an error reads FAILED (an active error outranks mere
	// age) and only an old success with NO error reads STALE — the old flat-
	// 48h/LastSynced check collapsed both into one undifferentiated "STALE".
	seedSyncStatus(t, cfg, "gmail", time.Now().Add(-1*time.Hour))
	old := time.Now().Add(-72 * time.Hour).UTC().Format(time.RFC3339)
	seedSyncStatusFull(t, cfg, "imessage", &memory.SyncStatus{
		Source:        "imessage",
		LastSynced:    old,
		LastAttemptAt: old,
		LastSuccessAt: old,
		ItemCount:     4,
		ErrorCount:    2,
	})
	seedSyncStatusFull(t, cfg, "filesystem", &memory.SyncStatus{
		Source:        "filesystem",
		LastSynced:    old,
		LastAttemptAt: old,
		LastSuccessAt: old,
		ItemCount:     1,
	})
	out := run(t, "sync", "status")
	if !strings.Contains(out, "gmail") || !strings.Contains(out, "imessage") || !strings.Contains(out, "filesystem") {
		t.Fatalf("sync status should list all three sources; got:\n%s", out)
	}
	if !strings.Contains(out, "(FAILED)") {
		t.Fatalf("sync status should mark the errored >48h source FAILED (not merely STALE); got:\n%s", out)
	}
	if !strings.Contains(out, "(STALE)") {
		t.Fatalf("sync status should mark the error-free >48h source STALE; got:\n%s", out)
	}
	var receipt struct {
		Schema        string `json:"schema"`
		SchemaVersion int    `json:"schema_version"`
		Sources       []struct {
			Source    string `json:"source"`
			State     string `json:"state"`
			LastError string `json:"last_error"`
		} `json:"sources"`
	}
	if err := json.Unmarshal([]byte(run(t, "sync", "status", "--json")), &receipt); err != nil {
		t.Fatalf("sync status --json must emit one receipt: %v", err)
	}
	if receipt.Schema != "mora.sync.status" || receipt.SchemaVersion != 1 || len(receipt.Sources) != 3 {
		t.Fatalf("sync status --json receipt = %+v", receipt)
	}
	for i, source := range receipt.Sources {
		if source.State != healthFresh && source.State != healthStale && source.State != healthFailed && source.State != healthNever {
			t.Fatalf("source state = %q, want established health vocabulary", source.State)
		}
		if i > 0 && receipt.Sources[i-1].Source > source.Source {
			t.Fatalf("sources must be sorted by source: %+v", receipt.Sources)
		}
	}
	if receipt.Sources[2].Source != "imessage" || receipt.Sources[2].State != healthFailed {
		t.Fatalf("sync status receipt must retain the persisted health state: %+v", receipt.Sources)
	}

	// Provider backfills with no enabled sources => 0 items, no error.
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")
	if out := run(t, "sync", "google"); !strings.Contains(out, "synced 0 item(s)") {
		t.Fatalf("sync google (empty); got:\n%s", out)
	}
	if out := run(t, "sync", "imessage"); !strings.Contains(out, "synced 0 item(s)") {
		t.Fatalf("sync imessage (empty); got:\n%s", out)
	}
	if out := run(t, "sync", "applecalendar"); !strings.Contains(out, "synced 0 item(s)") {
		t.Fatalf("sync applecalendar (empty); got:\n%s", out)
	}
}

func TestCoreA_CmdReingest(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	// help + unknown flag.
	if out := run(t, "reingest", "--help"); !strings.Contains(out, "usage: mora reingest") {
		t.Fatalf("reingest --help; got:\n%s", out)
	}
	if _, err := runErr(t, "reingest", "--nope"); err == nil {
		t.Fatal("reingest with an unknown flag must error")
	}
	// No enabled sources => 0 items.
	if out := run(t, "reingest"); !strings.Contains(out, "reingested 0 item(s)") {
		t.Fatalf("reingest (empty); got:\n%s", out)
	}

	// --full over enabled gmail + imessage sources (no token/db) exercises the
	// lookback switch + resumable-failure aggregation.
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{
		{Name: "gmail", Type: "gmail", Scope: "personal", Enabled: genericutil.Ptr(true), CreatedAt: nowRFC3339()},
		{Name: "imessage", Type: "imessage", Scope: "personal", Enabled: genericutil.Ptr(true), CreatedAt: nowRFC3339()},
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")
	out, _ := runErr(t, "reingest", "--full")
	if !strings.Contains(out, "full lookback") && !strings.Contains(out, "reingest incomplete") {
		t.Fatalf("reingest --full should render the full-lookback suffix or a resumable warning; got:\n%s", out)
	}
}

func TestCoreA_CmdConnectRouting(t *testing.T) {
	asDarwinOnWindows(t)
	withTempHome(t)
	run(t, "init")
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "") // force the placeholder path (never opens a browser)

	// No/invalid args => usage error.
	if _, err := runErr(t, "connect"); err == nil {
		t.Fatal("connect with no arg must error")
	}
	// Invalid account label is rejected BEFORE any network call.
	if _, err := runErr(t, "connect", "google", "--account", "Bad Label"); err == nil {
		t.Fatal("connect google with an invalid account label must error")
	}
	// Placeholder creds => ResolveOAuthConfig fails fast; no browser is opened.
	if _, err := runErr(t, "connect", "google"); err == nil {
		t.Fatal("connect google with placeholder creds must error before the loopback")
	}
	// imessage route: enables the source, prints readiness, returns (no block).
	out, _ := runErr(t, "connect", "imessage")
	if !strings.Contains(out, "enabled imessage") {
		t.Fatalf("connect imessage should route to the imessage flow; got:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// doctor / google-auth recency / imessage readiness / printThink / briefDigest
// ---------------------------------------------------------------------------

func TestCoreA_CmdDoctor(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--title", "Doc", "--text", "doctor body")
	cfg := mustConfig(t)

	// JSON healthy report.
	jsonOut := run(t, "doctor", "--json")
	var rep doctorReport
	if err := json.Unmarshal([]byte(jsonOut), &rep); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, jsonOut)
	}
	if !rep.Healthy || len(rep.Checks) == 0 {
		t.Fatalf("fresh init should be healthy with checks; got %+v", rep)
	}
	// Text output.
	if out := run(t, "doctor"); !strings.Contains(out, "vault") || !strings.Contains(out, "storage") {
		t.Fatalf("doctor text output; got:\n%s", out)
	}
	// --strict on a healthy vault is nil.
	if _, err := runErr(t, "doctor", "--strict"); err != nil {
		t.Fatalf("doctor --strict on a healthy vault should pass, got %v", err)
	}

	// git-sync disclosure branch.
	if err := os.MkdirAll(filepath.Join(cfg.VaultDir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if out := run(t, "doctor"); !strings.Contains(out, "git-sync is configured") {
		t.Fatalf("doctor should disclose git-sync; got:\n%s", out)
	}

	// A recorded rebuild block is surfaced in text + JSON.
	if err := writeBlockRecord(cfg, decBlockIdentity, cfg.VaultDir, 3, 4); err != nil {
		t.Fatal(err)
	}
	if out := run(t, "doctor"); !strings.Contains(out, "index_rebuild BLOCKED") {
		t.Fatalf("doctor should surface a rebuild block; got:\n%s", out)
	}
	blockJSON := run(t, "doctor", "--json")
	if !strings.Contains(blockJSON, "rebuild_block") {
		t.Fatalf("doctor --json should include the rebuild block; got:\n%s", blockJSON)
	}

	// Unhealthy: drop the index db => index_db check fails => --strict errors.
	if err := os.Remove(dbPath(cfg)); err != nil {
		t.Fatal(err)
	}
	if _, err := runErr(t, "doctor", "--strict"); err == nil {
		t.Fatal("doctor --strict must error when a critical check fails")
	}
	if _, err := runErr(t, "doctor", "--json", "--strict"); err == nil {
		t.Fatal("doctor --json --strict must also error when unhealthy")
	}
}

func TestCoreA_PrintGoogleAuthRecency(t *testing.T) {
	cfg := coreADirsCfg(t)
	// No tokens dir yet => silent (early return, no output).
	var out bytes.Buffer
	printGoogleAuthRecency(cfg, &out, time.Now())
	if out.String() != "" {
		t.Fatalf("no tokens dir should produce no output, got:\n%s", out.String())
	}

	// Two accounts: one with a recorded auth, one without.
	tokenDir := filepath.Join(cfg.ConfigDir, "tokens")
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"google.json", "google-work.json"} {
		if err := os.WriteFile(filepath.Join(tokenDir, name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A subdir and a non-json file must be skipped by the enumerator.
	if err := os.MkdirAll(filepath.Join(tokenDir, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tokenDir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Record an auth for "google" only.
	if err := google.RecordAuth(tokenDir, "google", time.Now().Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	printGoogleAuthRecency(cfg, &out, time.Now())
	got := out.String()
	if !strings.Contains(got, "google auth (google): last authed") {
		t.Fatalf("recency should report the recorded auth; got:\n%s", got)
	}
	if !strings.Contains(got, "google auth (google-work): no recorded auth yet") {
		t.Fatalf("recency should report the un-authed account; got:\n%s", got)
	}
}

func TestCoreA_PrintIMessageReadiness(t *testing.T) {
	if runtime.GOOS != "darwin" {
		// Off darwin only the skip line is reachable.
		var out bytes.Buffer
		if printIMessageReadiness(&out, false) {
			t.Fatal("non-darwin readiness must be false")
		}
		if !strings.Contains(out.String(), "only runs on macOS") {
			t.Fatalf("non-darwin readiness should say macOS-only; got:\n%s", out.String())
		}
		return
	}

	// darwin: no chat.db present => "No Messages database found".
	t.Run("no_db", func(t *testing.T) {
		withTempHome(t)
		var out bytes.Buffer
		if printIMessageReadiness(&out, false) {
			t.Fatal("no chat.db must be not-ready")
		}
		if !strings.Contains(out.String(), "No Messages database found") {
			t.Fatalf("missing chat.db message; got:\n%s", out.String())
		}
	})

	// darwin: an unreadable (non-sqlite) chat.db => FDA-denied guidance.
	t.Run("fda_denied", func(t *testing.T) {
		withTempHome(t)
		coreAMkChatDB(t, []byte("not a sqlite file"))
		var out bytes.Buffer
		if printIMessageReadiness(&out, true) { // setupVariant => "then mora sync imessage"
			t.Fatal("an unreadable chat.db must be not-ready")
		}
		if !strings.Contains(out.String(), "Full Disk Access") || !strings.Contains(out.String(), "mora sync imessage") {
			t.Fatalf("FDA-denied guidance (setup variant); got:\n%s", out.String())
		}
	})

	// darwin: a real (empty) sqlite chat.db reads clean => ready.
	t.Run("ready", func(t *testing.T) {
		withTempHome(t)
		coreAMakeEmptySQLiteChatDB(t)
		var out bytes.Buffer
		if !printIMessageReadiness(&out, false) {
			t.Fatalf("a readable chat.db must be ready; got:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "ready to sync") {
			t.Fatalf("ready message; got:\n%s", out.String())
		}
	})
}

// coreAMkChatDB writes raw bytes to $HOME/Library/Messages/chat.db.
func coreAMkChatDB(t *testing.T, body []byte) string {
	t.Helper()
	p := chatDBPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// coreAMakeEmptySQLiteChatDB materializes a valid (empty) sqlite database at the
// chat.db path so ProbeReadable's sqlite_master query succeeds.
func coreAMakeEmptySQLiteChatDB(t *testing.T) {
	t.Helper()
	p := chatDBPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE probe(x)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCoreA_PrintThinkGaps(t *testing.T) {
	// No-gaps path.
	var out bytes.Buffer
	printThink(&out, ThinkResult{
		Query:    "q1",
		Evidence: []ThinkEvidence{{StableID: "id1", Title: "T1", Snippet: "snip"}},
	})
	if !strings.Contains(out.String(), "Gaps: none detected") {
		t.Fatalf("empty gaps should print 'none detected'; got:\n%s", out.String())
	}
	// Gaps present: each bucket is rendered.
	out.Reset()
	printThink(&out, ThinkResult{
		Query: "q2",
		Gaps: ThinkGaps{
			Stale:            []string{"stale-note"},
			SparseEvidence:   []string{"sparse-note"},
			SourceCoverage:   []string{"source-note"},
			TemporalState:    []string{"state-note"},
			ThinCoverage:     []string{"thin-note"},
			CoverageHoles:    []string{"hole-note"},
			RetrievalCaveats: []string{"retrieval-note"},
		},
	})
	got := out.String()
	for _, want := range []string{"does NOT know", "stale-note", "sparse-note", "source-note", "state-note", "thin-note", "hole-note", "retrieval-note"} {
		if !strings.Contains(got, want) {
			t.Fatalf("gaps output missing %q; got:\n%s", want, got)
		}
	}
}

func TestCoreA_BriefDigestFallbackWindow(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	// An empty vault surfaces zero delta items, forcing the fallback-window rebuild.
	d, err := briefDigest(cfg, time.Now(), 0)
	if err != nil {
		t.Fatalf("briefDigest: %v", err)
	}
	_ = d // the point is the fallback path executed without error
}
