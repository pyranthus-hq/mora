package registry

import (
	"errors"
	"reflect"
	"testing"

	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func boolp(v bool) *bool { return &v }
func TestCatalogAliasesAndPlatformFiltering(t *testing.T) {
	apple, ok := Lookup("applecalendar")
	if !ok || apple.Provider != "applecal" || !apple.Upcoming {
		t.Fatalf("apple=%+v,%v", apple, ok)
	}
	if _, ok := Lookup("unknown"); ok {
		t.Fatal("unknown catalog hit")
	}
	if !MacOSOnly("imessage") || !MacOSOnly("applecalendar") || MacOSOnly("gmail") {
		t.Fatal("platform capability mismatch")
	}
	windows := CatalogForGOOS("windows")
	for _, c := range windows {
		if MacOSOnly(c.Type) {
			t.Fatalf("macOS-only %q on windows", c.Type)
		}
	}
	all := CatalogForGOOS("linux")
	if !reflect.DeepEqual(all, Entries()) {
		t.Fatal("non-windows catalog changed")
	}
	all[0].Type = "mutated"
	if _, ok := Lookup("gmail"); !ok {
		t.Fatal("Entries exposed mutable catalog")
	}
}
func TestInstanceIdentityAndProviderAlias(t *testing.T) {
	m := memory.Memory{Provider: "applecal", Account: "work"}
	if got, ok := SourceInstanceKey(m); !ok || got != "applecalendar:work" {
		t.Fatalf("key=(%q,%v)", got, ok)
	}
	if _, ok := SourceInstanceKey(memory.Memory{}); ok {
		t.Fatal("empty provider accepted")
	}
	if got := ProviderToType("future"); got != "future" {
		t.Fatalf("unknown=%q", got)
	}
	if got := InstanceKeyForSource(memory.Source{Type: "gmail", Account: "work"}); got != "gmail:work" {
		t.Fatalf("source=%q", got)
	}
}
func TestIngestingConnectorsFiltersDeduplicatesAndSorts(t *testing.T) {
	sources := []memory.Source{{Type: "gmail", Account: "z", Enabled: boolp(true)}, {Type: "calendar", Enabled: boolp(true)}, {Type: "gmail", Account: "z", Enabled: boolp(true)}, {Type: "github", Enabled: boolp(false)}, {Type: "unknown", Enabled: boolp(true)}}
	got, err := IngestingConnectors(config.Config{}, func(config.Config) ([]memory.Source, error) { return sources, nil })
	want := []string{"calendar", "gmail:z"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v err=%v", got, err)
	}
	sentinel := errors.New("load")
	if _, err = IngestingConnectors(config.Config{}, func(config.Config) ([]memory.Source, error) { return nil, sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("error=%v", err)
	}
}
func TestDisplayUpcomingAndLabels(t *testing.T) {
	cases := []struct {
		key   string
		rank  int
		label string
		up    bool
	}{{"calendar", 0, "Calendar", true}, {"gmail:work", 2, "Emails (work)", false}, {"applecalendar:home", 0, "Calendar (Apple) (home)", true}, {"notion", UnknownRank, "Notion", false}, {"", UnknownRank, "Other", false}}
	for _, tc := range cases {
		rank, label := Display(tc.key)
		if rank != tc.rank || label != tc.label || Upcoming(tc.key) != tc.up {
			t.Errorf("%q=(%d,%q,%v)", tc.key, rank, label, Upcoming(tc.key))
		}
	}
}
