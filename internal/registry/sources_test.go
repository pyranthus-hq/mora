package registry

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestLoadSourcesMissingAndGrandfatherEnabled(t *testing.T) {
	cfg := config.Config{ConfigDir: t.TempDir()}
	got, err := LoadSources(cfg)
	if err != nil || got != nil {
		t.Fatalf("missing=(%v,%v)", got, err)
	}
	body := `[{"name":"old","type":"gmail"},{"name":"off","type":"github","enabled":false}]`
	if err := os.WriteFile(filepath.Join(cfg.ConfigDir, "sources.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = LoadSources(cfg)
	if err != nil || len(got) != 2 {
		t.Fatalf("load=(%v,%v)", got, err)
	}
	if got[0].Enabled == nil || !*got[0].Enabled {
		t.Fatal("legacy enabled not grandfathered")
	}
	if got[1].Enabled == nil || *got[1].Enabled {
		t.Fatal("explicit false changed")
	}
}
func TestSaveSourcesExactBytesAndMode(t *testing.T) {
	cfg := config.Config{ConfigDir: filepath.Join(t.TempDir(), "nested")}
	enabled := true
	sources := []memory.Source{{Name: "mail", Type: "gmail", Enabled: &enabled}}
	if err := SaveSources(cfg, sources); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.ConfigDir, "sources.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "[\n  {\n    \"name\": \"mail\",\n    \"type\": \"gmail\",\n    \"scope\": \"\",\n    \"enabled\": true,\n    \"created_at\": \"\"\n  }\n]\n"
	if string(body) != want {
		t.Fatalf("body=%q want=%q", body, want)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%#o", st.Mode().Perm())
	}
	got, err := LoadSources(cfg)
	if err != nil || !reflect.DeepEqual(got, sources) {
		t.Fatalf("roundtrip=(%+v,%v)", got, err)
	}
}
func TestLoadSourcesCorruptAndOrEmpty(t *testing.T) {
	cfg := config.Config{ConfigDir: t.TempDir()}
	path := filepath.Join(cfg.ConfigDir, "sources.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSources(cfg); err == nil {
		t.Fatal("corrupt accepted")
	}
	if got := LoadSourcesOrEmpty(cfg); got != nil {
		t.Fatalf("fallback=%v", got)
	}
}
