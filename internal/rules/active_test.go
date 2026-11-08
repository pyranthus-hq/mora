package rules

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestLoadActiveMissing(t *testing.T) {
	set, present, err := LoadActive(filepath.Join(t.TempDir(), "active.json"))
	if present || err != nil || len(set.Rules) != 0 {
		t.Fatalf("missing file: set=%v present=%v err=%v", set, present, err)
	}
}

func TestLoadActiveRejectsBadFiles(t *testing.T) {
	rule := ActiveRule{ID: "r", Hash: "sha256:x", Mode: ModeWarn, Tool: "Bash", Match: "x", Message: "m"}
	many := ActiveSet{Schema: ActiveSchema, Rules: make([]ActiveRule, MaxActiveRules+1)}
	for i := range many.Rules {
		many.Rules[i] = rule
	}
	manyBody, _ := json.Marshal(many)
	big := `{"schema":"` + ActiveSchema + `","rules":[],"pad":"` + strings.Repeat("x", 257<<10) + `"}`
	cases := map[string]string{
		"empty":         "",
		"garbage":       "not json {",
		"wrong schema":  `{"schema":"other","rules":[]}`,
		"trailing data": `{"schema":"` + ActiveSchema + `","rules":[]} {}`,
		"257 KiB":       big,
		"201 rules":     string(manyBody),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "active.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			set, present, err := LoadActive(path)
			if !present || err == nil || len(set.Rules) != 0 || set.Version != "" {
				t.Fatalf("set=%d rules present=%v err=%v", len(set.Rules), present, err)
			}
		})
	}
}

func TestLoadActiveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules", "active.json")
	want := ActiveSet{CompiledAt: "2026-11-05T02:10:00Z", Rules: []ActiveRule{
		{ID: "a", Hash: "sha256:1", Mode: ModeWarn, Holdout: true, Tool: "Bash", Match: `^cd && x`, Message: "m1"},
		{ID: "b", Hash: "sha256:2", Mode: ModeShadow, Tool: "Bash", Match: "y", Exclude: "e", Unless: "u", Context: ContextSubagent, Message: "m2"},
	}}
	if err := WriteActive(path, want); err != nil {
		t.Fatal(err)
	}
	// Windows reports 0666 for any writable file.
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("active.json mode: %v %v", fi, err)
		}
	}
	got, present, err := LoadActive(path)
	if !present || err != nil {
		t.Fatalf("present=%v err=%v", present, err)
	}
	if !strings.HasPrefix(got.Version, "mora-active:") || len(got.Version) != len("mora-active:")+12 {
		t.Fatalf("Version = %q", got.Version)
	}
	want.Schema, want.Version = ActiveSchema, got.Version
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, want)
	}
	if got.Rules[1].Spec() != (Spec{Version: 1, Tool: "Bash", Match: "y", Exclude: "e", Unless: "u", Context: ContextSubagent}) {
		t.Fatalf("Spec() = %+v", got.Rules[1].Spec())
	}
}

// The hook tests and the manual check seed this fixture. It must stay the
// compound-cd template with its real hash.
func TestActiveFixtureMatchesTemplate(t *testing.T) {
	set, present, err := LoadActive(filepath.Join("testdata", "active_compound_cd_holdout.json"))
	if !present || err != nil || len(set.Rules) != 1 {
		t.Fatalf("fixture: present=%v err=%v rules=%d", present, err, len(set.Rules))
	}
	r := compoundCD(t)
	got := set.Rules[0]
	if got.ID != r.ID || got.Hash != Hash(r) || got.Spec() != r.Spec || got.Message != r.Message || got.Mode != ModeWarn || !got.Holdout {
		t.Fatalf("fixture drifted from the template: %+v", got)
	}
}
