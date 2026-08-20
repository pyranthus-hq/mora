package digest

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type Selection struct {
	ID, Title, Change, Series string
	At                        time.Time
	Salience                  int64
	LowSignal                 bool
}
type CollapsedSelection struct {
	OriginalIndex int
	Title         string
	Salience      int64
	Members       []string
}

func CollapseRecurring(in []Selection, now time.Time) []CollapsedSelection {
	collapsible := func(v Selection) bool { return v.Series != "" && v.Change != "updated" }
	groups := map[string][]int{}
	var order []string
	for i, v := range in {
		if !collapsible(v) {
			continue
		}
		if _, ok := groups[v.Series]; !ok {
			order = append(order, v.Series)
		}
		groups[v.Series] = append(groups[v.Series], i)
	}
	if len(groups) == 0 {
		out := make([]CollapsedSelection, len(in))
		for i, v := range in {
			out[i] = CollapsedSelection{OriginalIndex: i, Title: v.Title, Salience: v.Salience}
		}
		return out
	}
	out := make([]CollapsedSelection, 0, len(in))
	for i, v := range in {
		if !collapsible(v) || len(groups[v.Series]) == 1 {
			out = append(out, CollapsedSelection{OriginalIndex: i, Title: v.Title, Salience: v.Salience})
		}
	}
	for _, sid := range order {
		idxs := groups[sid]
		if len(idxs) < 2 {
			continue
		}
		rep := idxs[0]
		var last time.Time
		var bestSal int64
		members := make([]string, 0, len(idxs))
		for _, k := range idxs {
			members = append(members, in[k].ID)
			if in[k].At.After(last) {
				last = in[k].At
			}
			if in[k].Salience > bestSal {
				bestSal = in[k].Salience
			}
			if BetterSeriesRepresentative(in[k].At, in[rep].At, now) {
				rep = k
			}
		}
		out = append(out, CollapsedSelection{OriginalIndex: rep, Title: fmt.Sprintf("%s (×%d through %s)", in[rep].Title, len(idxs), last.UTC().Format("Jan 2")), Salience: bestSal, Members: members})
	}
	return out
}
func BetterSeriesRepresentative(a, b, now time.Time) bool {
	af, bf := !a.Before(now), !b.Before(now)
	if af != bf {
		return af
	}
	if af {
		return a.Before(b)
	}
	return a.After(b)
}
func OrderSelections(in []Selection, cap int, upcoming bool) (order []int, more int) {
	order = make([]int, len(in))
	for i := range in {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := in[order[i]], in[order[j]]
		if a.Salience != b.Salience {
			return a.Salience > b.Salience
		}
		if !a.At.Equal(b.At) {
			if upcoming {
				return a.At.Before(b.At)
			}
			return a.At.After(b.At)
		}
		return a.ID < b.ID
	})
	if len(order) > cap {
		more = len(order) - cap
		order = order[:cap]
	}
	return order, more
}
func CollapseLowSignal(in []Selection, floor int) (kept []int, collapsed int) {
	n := 0
	for i, v := range in {
		if v.LowSignal {
			if n >= floor {
				collapsed++
				continue
			}
			n++
		}
		kept = append(kept, i)
	}
	return kept, collapsed
}
func SplitDisplayLowSignal(in []Selection, memberOf map[string][]string, floor int) (kept []int, lineMembers map[string][]string, countOnly []string) {
	lineMembers = map[string][]string{}
	n := 0
	for i, v := range in {
		members := memberOf[v.ID]
		if len(members) == 0 {
			members = []string{v.ID}
		}
		if v.LowSignal {
			if n >= floor {
				countOnly = append(countOnly, members...)
				continue
			}
			n++
		}
		kept = append(kept, i)
		lineMembers[v.ID] = members
	}
	return kept, lineMembers, countOnly
}
func SnippetTail(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	return "…" + strings.TrimSpace(string(r[len(r)-n:]))
}
func InColdStartWindow(at, now time.Time, isCalendar bool, days int) bool {
	if isCalendar {
		end := now.Add(time.Duration(days) * 24 * time.Hour)
		return !at.Before(now) && !at.After(end)
	}
	start := now.Add(-time.Duration(days) * 24 * time.Hour)
	return !at.Before(start)
}
