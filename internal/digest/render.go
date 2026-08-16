// Package digest owns deterministic digest presentation and structural budgets.
package digest

import (
	"fmt"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"strings"
)

type Atom struct{ Kind, Value string }
type Citation struct{ Role, MemoryID, CommitmentID string }
type Obligation struct {
	CommitmentID, Summary                                      string
	Owner                                                      Atom
	Direction, CounterpartyLabel, DueAt, Lifecycle, ClosureRef string
	Citations                                                  []Citation
}
type Item struct {
	ID, Title, Source, CreatedAt, Snippet, Change              string
	Obligations                                                []Obligation
	Owner                                                      Atom
	Direction, CounterpartyLabel, DueAt, Lifecycle, ClosureRef string
}
type Section struct {
	Source, Label, State string
	Items                []Item
	MoreCount            int
	Truncated            bool
	ElidedByBudget       int
}
type Model struct {
	Generated                      string
	SinceHours                     int
	Urgent                         []Item
	UrgentMore                     int
	Sections                       []Section
	Freshness                      map[string]string
	StaleTasks                     []string
	EmptyExplanation, HealthBanner string
}

func Header(d Model) string {
	if d.SinceHours > 0 {
		return fmt.Sprintf("# Mora digest — %s (last %dh)\n", d.Generated, d.SinceHours)
	}
	return fmt.Sprintf("# Mora digest — %s (since last brief)\n", d.Generated)
}
func Freshness(d Model) string {
	if len(d.Freshness) == 0 {
		return ""
	}
	keys := make([]string, 0, len(d.Freshness))
	for k := range d.Freshness {
		keys = append(keys, k)
	}
	sortStrings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+" "+d.Freshness[k])
	}
	return fmt.Sprintf("Fresh as of: %s\n", strings.Join(parts, " · "))
}
func sortStrings(v []string) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
func SectionHeading(s Section) string { return "\n## " + Heading(s) + "\n" }
func ArtifactLine(it Item) string {
	return fmt.Sprintf("- %s%s — %s (id: %s)\n", ChangePrefix(it.Change), it.Title, it.Snippet, it.ID)
}
func CounterpartyLabel(label string) string {
	if label = strings.TrimSpace(label); label != "" {
		return " · counterparty=" + label
	}
	return ""
}
func ObligationRow(o Obligation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  obligation: commitment_id=%s · owner=%s:%s · direction=%s%s · due=%s · lifecycle=%s · closure=%s · summary=%s\n", o.CommitmentID, o.Owner.Kind, o.Owner.Value, o.Direction, CounterpartyLabel(o.CounterpartyLabel), o.DueAt, o.Lifecycle, o.ClosureRef, strings.Join(strings.Fields(o.Summary), " "))
	for _, c := range o.Citations {
		fmt.Fprintf(&b, "    citation: role=%s · memory_id=%s · commitment_id=%s\n", c.Role, c.MemoryID, c.CommitmentID)
	}
	return b.String()
}
func ItemLine(it Item) string {
	line := ArtifactLine(it)
	if len(it.Obligations) > 0 {
		var b strings.Builder
		b.WriteString(line)
		for _, o := range it.Obligations {
			b.WriteString(ObligationRow(o))
		}
		return b.String()
	}
	if it.Direction == "" {
		return line
	}
	return line + fmt.Sprintf("  obligation: owner=%s:%s · direction=%s%s · due=%s · lifecycle=%s · closure=%s\n", it.Owner.Kind, it.Owner.Value, it.Direction, CounterpartyLabel(it.CounterpartyLabel), it.DueAt, it.Lifecycle, it.ClosureRef)
}
func MoreLine(n int) string { return fmt.Sprintf("- +%d more since last brief\n", n) }
func StaleTasks(d Model) string {
	if len(d.StaleTasks) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n## Open tasks (%d stale)\n", len(d.StaleTasks))
	for _, t := range d.StaleTasks {
		fmt.Fprintf(&b, "- %s\n", t)
	}
	return b.String()
}
func UrgentHeading(n int) string { return fmt.Sprintf("\n## ⚠ Urgent (%d)\n", n) }
func UrgentShelf(d Model) string {
	if len(d.Urgent) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(UrgentHeading(len(d.Urgent)))
	for _, it := range d.Urgent {
		b.WriteString(ItemLine(it))
	}
	if d.UrgentMore > 0 {
		fmt.Fprintf(&b, "- +%d more urgent\n", d.UrgentMore)
	}
	return b.String()
}
func RenderBody(d Model) string {
	var b strings.Builder
	b.WriteString(Header(d))
	if d.HealthBanner != "" {
		b.WriteString(d.HealthBanner)
		b.WriteByte('\n')
	}
	b.WriteString(Freshness(d))
	b.WriteString(UrgentShelf(d))
	for _, s := range d.Sections {
		b.WriteString(SectionHeading(s))
		for _, it := range s.Items {
			b.WriteString(ItemLine(it))
		}
		if s.MoreCount > 0 {
			b.WriteString(MoreLine(s.MoreCount))
		}
	}
	b.WriteString(StaleTasks(d))
	return b.String()
}
func Render(d Model, budget int) string { return genericutil.TruncateRunes(RenderBody(d), budget) }
func Heading(s Section) string {
	switch s.State {
	case "no changes since last brief":
		return s.Label + " — no changes since last brief"
	case "stale":
		return s.Label + " — stale (no recent sync)"
	case "unavailable":
		return s.Label + " — unavailable (sync error)"
	case "baseline":
		return fmt.Sprintf("%s — baseline (%d)", s.Label, len(s.Items)+s.MoreCount)
	default:
		return fmt.Sprintf("%s (%d)", s.Label, len(s.Items)+s.MoreCount)
	}
}
func ChangePrefix(change string) string {
	switch change {
	case "new":
		return "[new] "
	case "updated":
		return "[updated] "
	default:
		return ""
	}
}
func surfaced(d Model) int {
	n := len(d.Urgent)
	for _, s := range d.Sections {
		n += len(s.Items)
	}
	return n
}
func shell(s Section) Section {
	return Section{Source: s.Source, Label: s.Label, State: s.State, Items: []Item{}, MoreCount: s.MoreCount + len(s.Items), Truncated: true, ElidedByBudget: s.ElidedByBudget + len(s.Items)}
}
func Budget(d Model, budget, defaultBudget, sourceFloor int) (Model, map[string]bool) {
	survived := map[string]bool{}
	if budget <= 0 {
		budget = defaultBudget
	}
	out := d
	frame := len(Header(d)) + len(d.HealthBanner)
	if d.HealthBanner != "" {
		frame++
	}
	frame += len(Freshness(d)) + len(StaleTasks(d))
	if len(d.Urgent) > 0 {
		used := frame + len(UrgentHeading(len(d.Urgent)))
		fit := 0
		for _, it := range d.Urgent {
			c := len(ItemLine(it))
			if used+c > budget {
				break
			}
			used += c
			survived[it.ID] = true
			fit++
		}
		if fit < len(d.Urgent) {
			out.Urgent = append([]Item(nil), d.Urgent[:fit]...)
			out.UrgentMore = d.UrgentMore + len(d.Urgent) - fit
		}
	}
	reserve := frame + len(UrgentShelf(out))
	for _, s := range d.Sections {
		reserve += len(SectionHeading(s)) + len(MoreLine(len(s.Items)+s.MoreCount))
	}
	remaining := budget - reserve
	if remaining < 0 {
		remaining = 0
	}
	kept := make([]int, len(d.Sections))
	used := 0
	add := func(i, j int) bool {
		it := d.Sections[i].Items[j]
		cost := len(ItemLine(it))
		if used+cost > remaining {
			return false
		}
		used += cost
		kept[i]++
		survived[it.ID] = true
		return true
	}
	for i := range d.Sections {
		for j := 0; j < len(d.Sections[i].Items) && j < sourceFloor; j++ {
			if !add(i, j) {
				break
			}
		}
	}
	for i := range d.Sections {
		for j := kept[i]; j < len(d.Sections[i].Items); j++ {
			if !add(i, j) {
				break
			}
		}
	}
	before := surfaced(d)
	out.Sections = make([]Section, 0, len(d.Sections))
	for i, s := range d.Sections {
		switch {
		case len(s.Items) == 0:
			ns := s
			if ns.Items == nil {
				ns.Items = []Item{}
			}
			out.Sections = append(out.Sections, ns)
		case kept[i] == 0:
			out.Sections = append(out.Sections, shell(s))
		default:
			ns := Section{Source: s.Source, Label: s.Label, State: s.State, MoreCount: s.MoreCount, Truncated: s.Truncated, ElidedByBudget: s.ElidedByBudget}
			ns.Items = append([]Item{}, s.Items[:kept[i]]...)
			if dropped := len(s.Items) - kept[i]; dropped > 0 {
				ns.MoreCount += dropped
				ns.Truncated = true
				ns.ElidedByBudget += dropped
			}
			out.Sections = append(out.Sections, ns)
		}
	}
	if out.EmptyExplanation == "" && before > 0 && surfaced(out) == 0 {
		out.EmptyExplanation = "all items elided by token budget"
	}
	return out, survived
}
