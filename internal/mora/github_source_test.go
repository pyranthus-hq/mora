package mora

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/githubissues"
)

func TestGitHubSnapshotIsImmutableAndProvenanceAddressed(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	payload := githubissues.Payload{
		Snapshot: githubissues.Snapshot{
			Repository: "pyranthus-hq/mora",
			Number:     255,
			UpdatedAt:  "2026-08-01T12:34:56Z",
		},
		Bytes: []byte(`{"repository":"pyranthus-hq/mora","issue_number":255,"state":"open"}`),
	}
	if err := writeGitHubSnapshot(cfg, payload); err != nil {
		t.Fatal(err)
	}
	// A retry with the same source update must not overwrite the first immutable
	// receipt, even if the caller presents different bytes.
	changed := payload
	changed.Bytes = []byte(`{"must_not":"replace the source receipt"}`)
	if err := writeGitHubSnapshot(cfg, changed); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.StateDir, "source-evidence", "github", "pyranthus-hq", "mora", "255", "2026-08-01T12-34-56Z.json")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload.Bytes)+"\n" {
		t.Fatalf("immutable snapshot changed: %s", got)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("snapshot count = %d, want one idempotent receipt", len(entries))
	}
}

func TestGitHubStatusPathAndCatalogContract(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	got := syncStatusPathFor(cfg, Source{Name: "github", Type: "github"})
	if want := filepath.Join(cfg.StateDir, "sync", "github-github.json"); got != want {
		t.Fatalf("status path = %q, want %q", got, want)
	}
	c, ok := lookupCatalog("github")
	if !ok || !c.Ingesting || c.DisplayName != "GitHub Issues" {
		t.Fatalf("github catalog entry = %+v, %v", c, ok)
	}
	var out bytes.Buffer
	printUsage(&out)
	help := out.String()
	if !strings.Contains(help, "mora connect github") || !strings.Contains(help, "mora sync github") {
		t.Fatalf("top-level help omits GitHub issue source:\n%s", help)
	}
}
