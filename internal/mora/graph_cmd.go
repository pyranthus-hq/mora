package mora

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// cmdGraph renders the entity graph visually in the terminal — the fast way to
// see (and debug) the shape of the graph the connectors built from real data.
// With a name it expands one entity: who it connects to, the relationship
// breakdown, and the evidence memories behind it.
//
//	mora graph                 # overview: top people + topics, with mention bars
//	mora graph "Sam"           # drill into one entity (connections + evidence)
//	mora graph --top 20        # widen the overview
//	mora graph --json          # structured output (entities, or the entity record)
func cmdGraph(ctx context.Context, args []string, stdout io.Writer) error {
	top := 12
	jsonOut := false
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json" || a == "-json":
			jsonOut = true
		case a == "--top" || a == "-top":
			if i+1 < len(args) {
				i++
				if n, err := strconv.Atoi(args[i]); err == nil && n > 0 {
					top = n
				}
			}
		case strings.HasPrefix(a, "--top="):
			if n, err := strconv.Atoi(strings.TrimPrefix(a, "--top=")); err == nil && n > 0 {
				top = n
			}
		default:
			positional = append(positional, a)
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	if len(positional) > 0 {
		return graphDetailView(ctx, cfg, stdout, strings.Join(positional, " "), jsonOut)
	}

	entities, err := graphListEntities(ctx, cfg)
	if err != nil {
		return err
	}
	if jsonOut {
		return emit(stdout, entities, true)
	}
	if len(entities) == 0 {
		fmt.Fprintln(stdout, "No entity graph yet. Connect a source (mora connect google / mora connect imessage), sync, then try again.")
		return nil
	}
	renderGraphOverview(stdout, entities, top)
	return nil
}

// renderGraphOverview prints the kinds of entities present, each kind's top
// members with a proportional bar per row. The People section ranks by salience
// (Phase 14 — fixes the bills/barbershop-over-friends inversion) and excludes
// service-kind identities (they stay searchable via list_entities/get_entity, just
// not surfaced here). All other sections keep ranking by mention Count, unchanged.
func renderGraphOverview(w io.Writer, entities []Entity, top int) {
	byKind := map[string][]Entity{}
	for _, e := range entities {
		// Service-kind person identities are excluded from the People overview
		// (D14-6) — render-time filter ONLY; graphListEntities still returns them so
		// search/get_entity resolve them. Other kinds pass through unchanged.
		if e.Kind == "service" || e.Kind == "org" || e.Kind == "repo" || e.Kind == "artifact" {
			continue
		}
		byKind[e.Kind] = append(byKind[e.Kind], e)
	}
	fmt.Fprintf(w, "\nEntity graph — %d entities across the vault\n", len(entities))

	sections := []struct{ kind, label string }{
		{"person", "People"},
		{"link", "Topics & links"},
		{"scope", "Scopes"},
		{"category", "Categories"},
		{"tag", "Tags"},
	}
	for _, s := range sections {
		group := byKind[s.kind]
		if len(group) == 0 {
			continue
		}
		if s.kind == "person" {
			renderPersonSection(w, s.label, group, top)
			continue
		}
		sort.SliceStable(group, func(i, j int) bool { return group[i].Count > group[j].Count })
		n := top
		if n > len(group) {
			n = len(group)
		}
		max := group[0].Count
		fmt.Fprintf(w, "\n%s (top %d of %d)\n", s.label, n, len(group))
		for _, e := range group[:n] {
			fmt.Fprintf(w, "  %-28s %-20s %d\n", graphTrunc(e.Name, 28), graphBar(e.Count, max, 20), e.Count)
		}
	}
	fmt.Fprintln(w, "\nExpand one:  mora graph \"<name>\"")
}

// renderPersonSection ranks + bars the People overview by salience (Phase 14, SC#2).
// Sort key: Salience desc, then the EXISTING deterministic tie-break (Count desc,
// then Name, then evidence-id join) so two renders are byte-identical even on equal
// micros. The bar magnitude is salience (max = top person's Salience); the printed
// numeric column shows the mention Count (the human-legible signal — raw micros are
// an opaque internal sort key, not a readable number), while the SORT/bar use the
// int64 micros.
func renderPersonSection(w io.Writer, label string, group []Entity, top int) {
	sort.SliceStable(group, func(i, j int) bool {
		if group[i].Salience != group[j].Salience {
			return group[i].Salience > group[j].Salience
		}
		if group[i].Count != group[j].Count {
			return group[i].Count > group[j].Count
		}
		if group[i].Name != group[j].Name {
			return group[i].Name < group[j].Name
		}
		return strings.Join(group[i].MemoryIDs, ",") < strings.Join(group[j].MemoryIDs, ",")
	})
	n := top
	if n > len(group) {
		n = len(group)
	}
	max := group[0].Salience
	fmt.Fprintf(w, "\n%s (top %d of %d)\n", label, n, len(group))
	for _, e := range group[:n] {
		fmt.Fprintf(w, "  %-28s %-20s %d\n", graphTrunc(e.Name, 28), graphBar(int(e.Salience), int(max), 20), e.Count)
	}
}

// graphDetailView expands a single entity: co-occurring people, the relationship
// breakdown by edge type, and the evidence memories — all graph-backed.
func graphDetailView(ctx context.Context, cfg Config, w io.Writer, name string, jsonOut bool) error {
	data, err := graphGetEntity(ctx, cfg, name)
	if err != nil {
		return err
	}
	if jsonOut {
		return emit(w, data, true)
	}
	if found, _ := data["found"].(bool); !found {
		fmt.Fprintf(w, "No entity named %q in the graph. Run `mora graph` to see what's there.\n", name)
		return nil
	}

	disp, _ := data["display_name"].(string)
	kind, _ := data["kind"].(string)
	count, _ := data["count"].(int)
	degree, _ := data["degree"].(int)
	fmt.Fprintf(w, "\n%s  (%s)  — %d memories · degree %d\n", disp, kind, count, degree)

	if ns, ok := data["neighbors"].([]map[string]any); ok && len(ns) > 0 {
		fmt.Fprintln(w, "\nConnected people (co-occur in the same threads/events)")
		for _, nb := range ns {
			if d, _ := nb["display_name"].(string); d != "" {
				fmt.Fprintf(w, "  • %s\n", d)
			}
		}
	}

	if es, ok := data["edges"].([]map[string]any); ok && len(es) > 0 {
		tally := map[string]int{}
		for _, e := range es {
			r, _ := e["rel"].(string)
			tally[r]++
		}
		rels := make([]string, 0, len(tally))
		for r := range tally {
			rels = append(rels, r)
		}
		sort.Slice(rels, func(i, j int) bool {
			if tally[rels[i]] != tally[rels[j]] {
				return tally[rels[i]] > tally[rels[j]]
			}
			return rels[i] < rels[j]
		})
		fmt.Fprintln(w, "\nRelationships")
		for _, r := range rels {
			fmt.Fprintf(w, "  %-18s %d\n", r, tally[r])
		}
	}

	if mems, ok := data["memories"].([]Memory); ok && len(mems) > 0 {
		fmt.Fprintln(w, "\nEvidence")
		const limit = 8
		for i, m := range mems {
			if i >= limit {
				fmt.Fprintf(w, "  … and %d more\n", len(mems)-limit)
				break
			}
			fmt.Fprintf(w, "  [%s] %s\n", m.ID, m.Title)
		}
	}
	return nil
}

// graphBar renders a proportional unicode bar of `width` cells for count/max.
func graphBar(count, max, width int) string {
	if max <= 0 || width <= 0 {
		return ""
	}
	n := count * width / max
	if n < 1 && count > 0 {
		n = 1
	}
	return strings.Repeat("█", n)
}

// graphTrunc clamps s to n runes, ellipsizing if needed.
func graphTrunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
