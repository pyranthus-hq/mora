package memoryfile

import (
	"path/filepath"
	"testing"

	"github.com/pyranthus-hq/mora/internal/config"
)

func TestValidateWriteScope(t *testing.T) {
	for _, scope := range []string{".", "..", "../../outside", "..:..:outside", "project:..:..:..:outside", "/absolute", "project:", ":global", "team//demo", "team/:demo", `team\demo`, "a\x00b", "a\tb", "a\nb", "a\u2028b", "a::b", "a//b", ":a", "a:", "/a", "a\u202eb", "a\u200bb"} {
		if err := ValidateWriteScope(scope); err == nil {
			t.Errorf("accepted %q", scope)
		}
	}
	cfg := config.Config{VaultDir: filepath.Join(t.TempDir(), "vault")}
	for _, scope := range []string{"", "global", "personal", "project:demo", "team/demo", "project:my-app.v2"} {
		if err := ValidateWriteScope(scope); err != nil {
			t.Errorf("%q: %v", scope, err)
		}
		got, err := Path(cfg, Document{ID: "synthetic", Scope: scope})
		want := filepath.Join(MemoriesRoot(cfg), filepath.FromSlash(replaceScope(scope)), "synthetic.md")
		if err != nil || got != want {
			t.Errorf("Path(%q) = %q, %v; want %q", scope, got, err, want)
		}
	}
}

func replaceScope(scope string) string {
	// Independent expected layout for the colon examples above.
	switch scope {
	case "project:demo":
		return "project/demo"
	case "project:my-app.v2":
		return "project/my-app.v2"
	default:
		return scope
	}
}

func TestPathRejectsDestinationOutsideMemoriesRoot(t *testing.T) {
	cfg := config.Config{VaultDir: filepath.Join(t.TempDir(), "vault")}
	for _, m := range []Document{
		{Scope: "../../outside", ID: "synthetic"},
		{Scope: "global", ID: "../../outside"},
		{Scope: "global", ID: "../../../vault-sibling/outside"},
	} {
		if path, err := Path(cfg, m); err == nil || path != "" {
			t.Errorf("Path(%+v) = %q, %v", m, path, err)
		}
	}
}

func TestPathIDContainment(t *testing.T) {
	cfg := config.Config{VaultDir: t.TempDir()}
	for _, id := range []string{"", ".", "..", "../other/id", "nested/../../other/id", "/absolute", `a\b`, "a\x00b"} {
		if path, err := Path(cfg, Document{ID: id, Scope: "global"}); err == nil || path != "" {
			t.Errorf("accepted id %q: %q, %v", id, path, err)
		}
	}
	for _, id := range []string{"mem_20261022_120000_0123abcd", "retained_0123456789abcdef", "mem_0123456789abcdef0123456789abcdef", "src_0123456789abcdef", OSSafeBase("gmail_thread_synthetic"), "calendar_event/synthetic", "gmail_thread/synthetic"} {
		if _, err := Path(cfg, Document{ID: id, Scope: "global"}); err != nil {
			t.Errorf("legitimate id %q: %v", id, err)
		}
	}
}
