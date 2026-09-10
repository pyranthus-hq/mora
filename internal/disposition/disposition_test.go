package disposition

import (
	"github.com/pyranthus-hq/mora/internal/memory"
	"reflect"
	"testing"
	"time"
)

func TestProjectExplicitLatestScopedAndDeterministic(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	target := memory.Memory{ID: "target", Scope: "personal", CreatedAt: "2026-09-01T00:00:00Z"}
	correction := func(id, value, at string) memory.Memory {
		return memory.Memory{ID: id, Type: "correction", Scope: "personal", CreatedAt: at, Meta: map[string]any{"target": "target", "disposition": value}}
	}
	old := correction("old", "not-context", "2026-09-02T00:00:00Z")
	latest := correction("latest", "keep", "2026-09-03T00:00:00Z")
	tie := correction("zz", "done", "2026-09-03T01:00:00+01:00")
	foreign := correction("foreign", "outdated", "2026-09-04T00:00:00Z")
	foreign.Scope = "work"
	forged := correction("forged", "outdated", "2026-09-05T00:00:00Z")
	forged.Provider = "gmail"
	sourced := correction("sourced", "outdated", "2026-09-05T00:00:00Z")
	sourced.Source = "gmail"
	note := correction("note", "", "2026-09-06T00:00:00Z")
	future := correction("future", "outdated", "2026-09-11T00:00:00Z")
	shared := correction("shared", "outdated", "2026-09-07T00:00:00Z")
	shared.Owner = "friend"
	deleted := correction("deleted", "outdated", "2026-09-07T00:00:00Z")
	deleted.DeletedAt = "2026-09-08T00:00:00Z"
	wrongType := correction("wrong-type", "outdated", "2026-09-07T00:00:00Z")
	wrongType.Type = "insight"
	malformed := correction("malformed", "outdated", "not-a-time")
	early := correction("early", "outdated", "2026-08-01T00:00:00Z")
	records := []memory.Memory{target, old, latest, tie, foreign, forged, sourced, note, future, shared, deleted, wrongType, malformed, early}
	want := map[Key]memory.Disposition{{"personal", "target"}: {Value: "done", CorrectionID: "zz", At: tie.CreatedAt}}
	if got := Project(records, now); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v want %#v", got, want)
	}
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}
	if got := Project(records, now); !reflect.DeepEqual(got, want) {
		t.Fatalf("order changed result: %#v", got)
	}
	if target.DeletedAt != "" {
		t.Fatal("annotation mutated target")
	}
}

func TestProjectRequiresVisibleSameScopeTarget(t *testing.T) {
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	c := memory.Memory{ID: "correction", Scope: "a", Type: "correction", CreatedAt: "2026-09-03T00:00:00Z", Meta: map[string]any{"target": "target", "disposition": "keep"}}
	for _, target := range []memory.Memory{
		{ID: "other", Scope: "a", CreatedAt: "2026-09-01T00:00:00Z"},
		{ID: "target", Scope: "b", CreatedAt: "2026-09-01T00:00:00Z"},
		{ID: "target", Scope: "a", CreatedAt: "2026-09-01T00:00:00Z", Owner: "friend"},
		{ID: "target", Scope: "a", CreatedAt: "2026-09-01T00:00:00Z", DeletedAt: "2026-09-02T00:00:00Z"},
		{ID: "target", Scope: "a", CreatedAt: "invalid"},
	} {
		if got := Project([]memory.Memory{target, c}, now); len(got) != 0 {
			t.Fatalf("adopted invalid target %+v: %+v", target, got)
		}
	}
}

func TestFieldAndTargetValidation(t *testing.T) {
	for _, v := range []string{"not-context", "keep", "done", "outdated"} {
		if err := ValidateFields("target", v); err != nil {
			t.Fatal(err)
		}
		if err := ValidateFields("", v); err == nil {
			t.Fatal("untargeted disposition accepted")
		}
	}
	if err := ValidateFields("target", ""); err != nil {
		t.Fatal("target-only note", err)
	}
	if err := ValidateFields("", ""); err != nil {
		t.Fatal("legacy write", err)
	}
	for _, pair := range [][2]string{{"target", "closed"}, {"target", " keep"}, {" target ", "keep"}} {
		if ValidateFields(pair[0], pair[1]) == nil {
			t.Fatal("invalid fields accepted", pair)
		}
	}
	c := memory.Memory{Scope: "a", Meta: map[string]any{"target": "target"}}
	target := memory.Memory{ID: "target", Scope: "a"}
	if err := ValidateTarget(c, target); err != nil {
		t.Fatal(err)
	}
	target.Scope = "b"
	if ValidateTarget(c, target) == nil {
		t.Fatal("cross-scope target accepted")
	}
}
