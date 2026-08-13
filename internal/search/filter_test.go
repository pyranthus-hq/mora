package search

import (
	"math"
	"reflect"
	"testing"
	"time"
)

func TestFilterCore(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	zero := Filter{}
	if zero.Active() || zero.Receipt() != nil || zero.NormalizedSource() != "" {
		t.Fatalf("zero filter not inert: %+v", zero)
	}
	f := Filter{Source: " gmail:work ", SourceFamily: "gmail", SourceInstance: "work", SinceHours: 24, Now: now}
	if !f.Active() || f.NormalizedSource() != "gmail:work" {
		t.Fatalf("filter=%+v", f)
	}
	if got := f.Receipt(); !reflect.DeepEqual(got, map[string]any{"source": " gmail:work ", "since_hours": 24}) {
		t.Fatalf("receipt=%v", got)
	}
	clause, args := f.SQLPredicate()
	if clause != " AND m.provider = ? AND m.account = ? AND m.created_at_unix >= ?" || !reflect.DeepEqual(args, []any{"gmail", "work", now.Add(-24 * time.Hour).Unix()}) {
		t.Fatalf("predicate=(%q,%v)", clause, args)
	}
}
func TestOneAndCreatedAtUnix(t *testing.T) {
	if One(nil).Active() {
		t.Fatal("nil filter must be inert")
	}
	f := Filter{Source: "gmail"}
	if One([]Filter{f}).Source != "gmail" {
		t.Fatal("One lost first filter")
	}
	if got := CreatedAtUnix("2026-08-13T12:00:00Z"); got != 1786622400 {
		t.Fatalf("unix=%d", got)
	}
	if got := CreatedAtUnix("bad"); got != math.MinInt64 {
		t.Fatalf("bad timestamp=%d", got)
	}
}
