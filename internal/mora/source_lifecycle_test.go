package mora

import (
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
