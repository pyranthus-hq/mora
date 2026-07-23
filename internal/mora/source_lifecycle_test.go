package mora

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// source_lifecycle_test.go pins the coherent connector/source lifecycle for the
// per-source connector type (filesystem):
//
//   (a) `connectors enable filesystem` never fabricates a pathless source row —
//       a filesystem source without a folder is meaningless, breaks every later
//       `ingest run --all`/`reingest`, and raises a permanent red "never synced"
//       health banner on a vault that holds no data at all.
//   (b) `sources add` inherits the type's consent state, so add-after-enable is
//       not a dead end (there is no `sources enable` command).
//   (c) `ingest run --source <typo>` errors instead of "ingested 0 item(s)".
//   (d) the post-enable hint names a command that fits the connector type.

// (a) On a fresh install with no configured folder, `connectors enable
// filesystem` must not mint a phantom {Name:"filesystem", Path:""} row. It
// prints guidance (pointing at `mora connect filesystem <path>`) and leaves the
// registry untouched, so `ingest run --all` stays clean and no red health
// banner appears on a healthy, empty vault.
func TestEnableFilesystemDoesNotFabricatePathlessSource(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	out, err := runErr(t, "connectors", "enable", "filesystem")
	if err != nil {
		// Guidance, not an error: the setup menu enables selected connectors in a
		// loop, and a hard error here would abort the remaining connectors.
		t.Fatalf("enable filesystem with no configured folder should guide, not error: %v\n%s", err, out)
	}
	if !strings.Contains(out, "mora connect filesystem") {
		t.Fatalf("enable filesystem with no configured folder should point at `mora connect filesystem <path>`; got:\n%s", out)
	}
	if strings.Contains(out, "enabled filesystem") {
		t.Fatalf("nothing was enabled — output must not claim success; got:\n%s", out)
	}

	cfg := mustConfig(t)
	sources, err := loadSources(cfg)
	if err != nil {
		t.Fatalf("loadSources: %v", err)
	}
	for _, s := range sources {
		if s.Type == "filesystem" {
			t.Fatalf("phantom filesystem source fabricated by enable: %+v", s)
		}
	}

	// The fresh vault stays fully healthy afterwards.
	if out, err := runErr(t, "ingest", "run", "--all"); err != nil {
		t.Fatalf("ingest run --all must stay clean after a folderless enable: %v\n%s", err, out)
	}
	if b := healthBanner(cfg, time.Now()); b != "" {
		t.Fatalf("healthy fresh vault must show no red health banner, got: %q", b)
	}
}

// (a) With a configured folder present, `connectors enable filesystem` flips
// the real row — and only the real row (no extra pathless sibling).
func TestEnableFilesystemFlipsConfiguredSources(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	dir := t.TempDir()
	run(t, "sources", "add", "filesystem", "--name", "docs", "--path", dir)

	out := run(t, "connectors", "enable", "filesystem")
	if !strings.Contains(out, "enabled filesystem") {
		t.Fatalf("enable filesystem with a configured source should succeed; got:\n%s", out)
	}

	cfg := mustConfig(t)
	sources, err := loadSources(cfg)
	if err != nil {
		t.Fatalf("loadSources: %v", err)
	}
	var docsEnabled bool
	for _, s := range sources {
		if s.Type != "filesystem" {
			continue
		}
		if s.Name != "docs" {
			t.Fatalf("unexpected extra filesystem row (phantom): %+v", s)
		}
		docsEnabled = s.IsEnabled()
	}
	if !docsEnabled {
		t.Fatalf("docs source should be enabled after `connectors enable filesystem`; got %+v", sources)
	}
}

// (a, review finding 1) `connectors disable filesystem` on a row-less registry
// must not mint a disabled pathless row — a later `connectors enable
// filesystem` would see that row and resurrect the phantom (enable it with no
// path), putting the hourly `ingest run --all` right back into the failure the
// enable-side guard exists to prevent. And enable must never activate a
// pathless legacy row even when one is present.
func TestDisableThenEnableFilesystemDoesNotResurrectPhantom(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	run(t, "connectors", "disable", "filesystem")
	sources, err := loadSources(cfg)
	if err != nil {
		t.Fatalf("loadSources: %v", err)
	}
	for _, s := range sources {
		if s.Type == "filesystem" {
			t.Fatalf("disable of an absent type must not mint a row: %+v", s)
		}
	}

	out, err := runErr(t, "connectors", "enable", "filesystem")
	if err != nil {
		t.Fatalf("enable after disable should guide, not error: %v\n%s", err, out)
	}
	if !strings.Contains(out, "mora connect filesystem") {
		t.Fatalf("enable after disable should still guide to `mora connect filesystem`; got:\n%s", out)
	}
	sources, _ = loadSources(cfg)
	for _, s := range sources {
		if s.Type == "filesystem" {
			t.Fatalf("disable→enable resurrected a phantom row: %+v", s)
		}
	}

	// A legacy pathless row (older binaries minted one) must never be
	// (re)activated by enable — it can only fail the walk.
	if err := saveSources(cfg, []Source{{Name: "filesystem", Type: "filesystem", Scope: "personal", Enabled: ptr(false), CreatedAt: time.Now().Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	out = run(t, "connectors", "enable", "filesystem")
	if !strings.Contains(out, "mora connect filesystem") {
		t.Fatalf("enable with only a pathless legacy row should guide; got:\n%s", out)
	}
	sources, _ = loadSources(cfg)
	for _, s := range sources {
		if s.Type == "filesystem" && s.IsEnabled() {
			t.Fatalf("enable activated a pathless legacy row: %+v", s)
		}
	}
}

// (a, review finding 2) `mora connect filesystem <path>` — the repair command
// the pathless-source error recommends — must actually repair the registry:
// legacy pathless filesystem rows are dropped while the healthy row is added,
// so the hourly `ingest run --all` comes back clean.
func TestConnectFilesystemRemovesLegacyPathlessRow(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "filesystem") // enabled pathless legacy phantom

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte("repair me"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := run(t, "connect", "filesystem", dir, "--name", "docs")
	if !strings.Contains(out, "Enabled filesystem") {
		t.Fatalf("connect filesystem should succeed; got:\n%s", out)
	}

	sources, err := loadSources(cfg)
	if err != nil {
		t.Fatalf("loadSources: %v", err)
	}
	for _, s := range sources {
		if s.Type == "filesystem" && s.Path == "" {
			t.Fatalf("legacy pathless row survived connect: %+v", s)
		}
	}
	if out, err := runErr(t, "ingest", "run", "--all"); err != nil {
		t.Fatalf("ingest --all must be clean after the connect repair: %v\n%s", err, out)
	}
}

// (b) `sources add` inherits the connector type's consent state. Before this,
// a source added AFTER `connectors enable filesystem` landed disabled, and
// `ingest run --source <name>` dead-ended on "run `mora connectors enable
// filesystem` first" — which had ALREADY been run, and there is no
// `sources enable` command. Consent is granted per TYPE (D-02: `connectors
// enable` flips every row of the type), so a row added under an
// already-consented type starts enabled; on a never-enabled type it still
// defaults disabled (D-11 unchanged).
func TestSourcesAddInheritsTypeConsent(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	// Grant type-level consent via the documented flow: add, then enable.
	run(t, "sources", "add", "filesystem", "--name", "docs", "--path", t.TempDir())
	run(t, "connectors", "enable", "filesystem")

	// A source added while the type is enabled must start enabled.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte("hello notes"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, "sources", "add", "filesystem", "--name", "notes", "--path", dir)

	sources, err := loadSources(cfg)
	if err != nil {
		t.Fatalf("loadSources: %v", err)
	}
	byName := map[string]Source{}
	for _, s := range sources {
		byName[s.Name] = s
	}
	if !byName["notes"].IsEnabled() {
		t.Fatalf("source added under an enabled type must inherit enabled; got %+v", byName["notes"])
	}
	if !byName["docs"].IsEnabled() {
		t.Fatalf("existing enabled source must stay enabled; got %+v", byName["docs"])
	}

	// The add → ingest flow now completes with no dead end.
	out, err := runErr(t, "ingest", "run", "--source", "notes")
	if err != nil {
		t.Fatalf("ingest run --source notes should work right after add: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ingested 1 item(s)") {
		t.Fatalf("expected the notes file to ingest; got:\n%s", out)
	}
}

// (d) The post-enable hint must fit the connector type. Enabling filesystem
// used to print the hardcoded "Pull data with `mora sync google`" — a Google
// command for a folder connector. filesystem now hints its own pull commands;
// gmail/calendar keep the google hint (which was correct for them all along).
func TestEnableHintMatchesConnectorType(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "sources", "add", "filesystem", "--name", "docs", "--path", t.TempDir())

	out := run(t, "connectors", "enable", "filesystem")
	if strings.Contains(out, "mora sync google") {
		t.Fatalf("enabling filesystem must not hint the google sync; got:\n%s", out)
	}
	if !strings.Contains(out, "mora sync filesystem") {
		t.Fatalf("enabling filesystem should hint `mora sync filesystem`; got:\n%s", out)
	}

	// gmail (non-TTY, no token: flips the bit + auth note) keeps the google hint.
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")
	out = run(t, "connectors", "enable", "gmail")
	if !strings.Contains(out, "mora sync google") {
		t.Fatalf("enabling gmail should hint `mora sync google`; got:\n%s", out)
	}
}

// (c) `ingest run --source <name>` with a name that matches NO configured
// source must error (exit 1) naming the typo and pointing at `mora sources
// list` — not print "ingested 0 item(s)" and exit 0, which made a typo look
// like a successful (empty) run.
func TestIngestUnknownSourceErrors(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	out, err := runErr(t, "ingest", "run", "--source", "nonexistent")
	if err == nil {
		t.Fatalf("ingest run --source nonexistent must error; got:\n%s", out)
	}
	if !strings.Contains(err.Error(), `"nonexistent"`) || !strings.Contains(err.Error(), "mora sources list") {
		t.Fatalf("unknown-source error should name the source and point at `mora sources list`; got: %v", err)
	}
	if strings.Contains(out, "ingested 0 item(s)") {
		t.Fatalf("a typo'd source must not report a successful empty run; got:\n%s", out)
	}

	// Neither --source nor --all is a usage error, not a silent empty run.
	out, err = runErr(t, "ingest", "run")
	if err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("ingest run with no --source/--all should be a usage error; got err=%v out:\n%s", err, out)
	}
}

// (a) A legacy install may already carry a pathless filesystem row (fabricated
// by an older binary). Ingesting it must fail with an error that names the
// source and the fix — never the bare `lstat : no such file or directory`.
func TestIngestPathlessFilesystemSourceErrorsClearly(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "filesystem") // Name==Type, Path=="" — the legacy phantom shape

	out, err := runErr(t, "ingest", "run", "--source", "filesystem")
	if err == nil {
		t.Fatalf("ingesting a pathless filesystem source must error; got:\n%s", out)
	}
	if !strings.Contains(err.Error(), "no path") || !strings.Contains(err.Error(), "mora connect filesystem") {
		t.Fatalf("pathless-source error should name the problem and the fix; got: %v", err)
	}
}
