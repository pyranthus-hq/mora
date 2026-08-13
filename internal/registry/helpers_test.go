package registry

import (
	"reflect"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestFilesystemAndTypeHelpers(t *testing.T) {
	sources := []memory.Source{{Type: "filesystem"}, {Type: "filesystem", Path: "/vault/docs"}}
	if !HasConfiguredFilesystemSource(sources) || HasConfiguredFilesystemSource(sources[:1]) {
		t.Fatal("filesystem configuration mismatch")
	}
	types := []string{"gmail", "calendar", "gmail"}
	if !ContainsType(types, "calendar") || ContainsType(types, "github") {
		t.Fatal("contains mismatch")
	}
	got := WithoutTypes(types, "gmail")
	if !reflect.DeepEqual(got, []string{"calendar"}) {
		t.Fatalf("without=%v", got)
	}
	if !reflect.DeepEqual(types, []string{"gmail", "calendar", "gmail"}) {
		t.Fatalf("input mutated=%v", types)
	}
}
func TestParseCSVListPreservesOrderAndDuplicates(t *testing.T) {
	got := ParseCSVList(" gmail, ,calendar,gmail ")
	want := []string{"gmail", "calendar", "gmail"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v", got)
	}
	if got := ParseCSVList(""); got != nil {
		t.Fatalf("empty=%v", got)
	}
}
func TestAccountLabelsAndSourceNames(t *testing.T) {
	for _, valid := range []string{"work", "a1", "work-mail"} {
		if !ValidAccountLabel(valid) {
			t.Errorf("valid %q rejected", valid)
		}
	}
	for _, bad := range []string{"", "Work", "work_mail", "mail@example"} {
		if ValidAccountLabel(bad) {
			t.Errorf("bad %q accepted", bad)
		}
	}
	g, c := GoogleSourceNames("")
	if g != "gmail" || c != "calendar" {
		t.Fatalf("default=(%q,%q)", g, c)
	}
	g, c = GoogleSourceNames("work")
	if g != "gmail-work" || c != "calendar-work" {
		t.Fatalf("labeled=(%q,%q)", g, c)
	}
}
func TestGoogleAccountForEmail(t *testing.T) {
	sources := []memory.Source{{Type: "filesystem", Email: "x@example.com", Account: "wrong"}, {Type: "gmail", Email: "User@Example.com", Account: "work"}, {Type: "calendar", Email: "other@example.com", Account: "other"}}
	if got, ok := GoogleAccountForEmail(sources, "user@example.COM"); !ok || got != "work" {
		t.Fatalf("lookup=(%q,%v)", got, ok)
	}
	if _, ok := GoogleAccountForEmail(sources, ""); ok {
		t.Fatal("empty email found")
	}
	if _, ok := GoogleAccountForEmail(sources, "missing@example.com"); ok {
		t.Fatal("missing found")
	}
}
