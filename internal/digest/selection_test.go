package digest

import (
	"reflect"
	"testing"
	"time"
)

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func sel(id string, sal int64, at time.Time) Selection {
	return Selection{ID: id, Title: id, Salience: sal, At: at}
}
func ordered(in []Selection, cap int, upcoming bool) []string {
	order, _ := OrderSelections(in, cap, upcoming)
	out := make([]string, len(order))
	for i, j := range order {
		out[i] = in[j].ID
	}
	return out
}
func TestCapRecencySalienceLeadsSection(t *testing.T) {
	older, newer := at(t, "2026-06-01T00:00:00Z"), at(t, "2026-06-08T00:00:00Z")
	got := ordered([]Selection{sel("recent_noise", 100, newer), sel("salient_old", 900000, older)}, 8, false)
	if !reflect.DeepEqual(got, []string{"salient_old", "recent_noise"}) {
		t.Fatalf("got=%v", got)
	}
}
func TestCapRecencySalienceSurvivesCap(t *testing.T) {
	base := at(t, "2026-06-01T00:00:00Z")
	in := []Selection{sel("recent_a", 10, base.Add(48*time.Hour)), sel("recent_b", 20, base.Add(24*time.Hour)), sel("salient_old", 900000, base)}
	order, more := OrderSelections(in, 2, false)
	if more != 1 || in[order[0]].ID != "salient_old" || in[order[1]].ID != "recent_b" {
		t.Fatalf("order=%v more=%d", order, more)
	}
}
func TestCapRecencyZeroSalienceSinksToBottom(t *testing.T) {
	base := at(t, "2026-06-01T00:00:00Z")
	got := ordered([]Selection{sel("zero_old", 0, base), sel("zero_new", 0, base.Add(24*time.Hour)), sel("human", 500000, base.Add(-72*time.Hour))}, 8, false)
	if !reflect.DeepEqual(got, []string{"human", "zero_new", "zero_old"}) {
		t.Fatalf("got=%v", got)
	}
}
func TestCapRecencyEqualSalienceRecencyTieBreak(t *testing.T) {
	t0, t1 := at(t, "2026-06-01T00:00:00Z"), at(t, "2026-06-02T00:00:00Z")
	got := ordered([]Selection{sel("b_same_instant", 500, t0), sel("a_same_instant", 500, t0), sel("recent", 500, t1)}, 8, false)
	if !reflect.DeepEqual(got, []string{"recent", "a_same_instant", "b_same_instant"}) {
		t.Fatalf("got=%v", got)
	}
	up := ordered([]Selection{sel("later", 1, t1), sel("near", 1, t0)}, 8, true)
	if !reflect.DeepEqual(up, []string{"near", "later"}) {
		t.Fatalf("up=%v", up)
	}
}
func TestCapRecencyDeterministic(t *testing.T) {
	base := at(t, "2026-06-01T00:00:00Z")
	build := func() []Selection {
		return []Selection{sel("c", 100, base), sel("a", 100, base), sel("b", 900000, base.Add(time.Hour)), sel("d", 0, base.Add(2*time.Hour))}
	}
	a, b := ordered(build(), 8, false), ordered(build(), 8, false)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("a=%v b=%v", a, b)
	}
}
func TestCollapseLowSignal(t *testing.T) {
	in := []Selection{{ID: "h1"}, {ID: "h2"}, {ID: "s1", LowSignal: true}, {ID: "s2", LowSignal: true}, {ID: "s3", LowSignal: true}, {ID: "s4", LowSignal: true}}
	kept, n := CollapseLowSignal(in, 2)
	if n != 2 || !reflect.DeepEqual(kept, []int{0, 1, 2, 3}) {
		t.Fatalf("kept=%v n=%d", kept, n)
	}
	all, n := CollapseLowSignal([]Selection{{ID: "a"}, {ID: "b"}}, 2)
	if n != 0 || len(all) != 2 {
		t.Fatal("all signal")
	}
	noise, n := CollapseLowSignal([]Selection{{LowSignal: true}, {LowSignal: true}, {LowSignal: true}}, 2)
	if len(noise) != 2 || n != 1 {
		t.Fatal("noise")
	}
}
func TestCollapseRecurringSeriesPreservesUpdatedInstance(t *testing.T) {
	now := at(t, "2026-06-12T12:00:00Z")
	in := []Selection{{ID: "a", Title: "a", Change: "new", Series: "s1", At: now.Add(6 * time.Hour)}, {ID: "b", Title: "b", Change: "new", Series: "s1", At: now.Add(12 * time.Hour)}, {ID: "c", Title: "c", Change: "new", Series: "s1", At: now.Add(18 * time.Hour)}, {ID: "u", Title: "u", Change: "updated", Series: "s1", At: now.Add(3 * time.Hour)}}
	out := CollapseRecurring(in, now)
	if len(out) != 2 {
		t.Fatalf("out=%+v", out)
	}
	var updated bool
	for _, v := range out {
		if in[v.OriginalIndex].ID == "u" {
			updated = true
			if len(v.Members) > 1 {
				t.Fatal("updated absorbed")
			}
		} else if len(v.Members) != 3 || v.Title != "a (×3 through Jun 13)" {
			t.Fatalf("collapsed=%+v", v)
		}
	}
	if !updated {
		t.Fatal("updated missing")
	}
}
func TestSelectionParentMembersAndTextWindows(t *testing.T) {
	in := []Selection{{ID: "a", LowSignal: true}, {ID: "b", LowSignal: true}, {ID: "c", LowSignal: true}}
	kept, members, count := SplitDisplayLowSignal(in, map[string][]string{"a": {"a", "a2"}}, 2)
	if !reflect.DeepEqual(kept, []int{0, 1}) || !reflect.DeepEqual(members["a"], []string{"a", "a2"}) || !reflect.DeepEqual(count, []string{"c"}) {
		t.Fatalf("kept=%v members=%v count=%v", kept, members, count)
	}
	if SnippetTail(" a   long phrase ", 6) != "…phrase" || SnippetTail("short", 9) != "short" {
		t.Fatal("tail")
	}
	now := at(t, "2026-06-01T00:00:00Z")
	if !InColdStartWindow(now.Add(7*24*time.Hour), now, true, 7) || InColdStartWindow(now.Add(-time.Second), now, true, 7) || !InColdStartWindow(now.Add(-7*24*time.Hour), now, false, 7) {
		t.Fatal("window")
	}
}
func TestRecurringRepresentativePastFutureAndFastPaths(t *testing.T) {
	now := at(t, "2026-06-10T00:00:00Z")
	if !BetterSeriesRepresentative(now.Add(time.Hour), now.Add(-time.Hour), now) || !BetterSeriesRepresentative(now.Add(time.Hour), now.Add(2*time.Hour), now) || !BetterSeriesRepresentative(now.Add(-time.Hour), now.Add(-2*time.Hour), now) {
		t.Fatal("representative")
	}
	single := []Selection{{ID: "x", Title: "x", Series: "one", At: now}}
	if got := CollapseRecurring(single, now); len(got) != 1 || got[0].OriginalIndex != 0 {
		t.Fatalf("single=%+v", got)
	}
	plain := []Selection{{ID: "x", Title: "x"}}
	if got := CollapseRecurring(plain, now); len(got) != 1 {
		t.Fatal("fast")
	}
}
