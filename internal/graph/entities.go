package graph

import (
	"github.com/pyranthus-hq/mora/internal/memory"
	"regexp"
	"sort"
	"strings"
)

var (
	wikilinkRe = regexp.MustCompile(`\[\[([^\[\]]+)\]\]`)
	categoryRe = regexp.MustCompile(`(?m)^\s*-\s*\[([^\[\]]+)\]`)
)

type structuralEntity struct {
	Name, Kind string
	Count      int
	MemoryIDs  []string
}

func extractEntities(mems []memory.Memory) []structuralEntity {
	type key struct{ kind, name string }
	seen := map[key]map[string]bool{}
	add := func(kind, name, id string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		k := key{kind, name}
		if seen[k] == nil {
			seen[k] = map[string]bool{}
		}
		seen[k][id] = true
	}
	for _, m := range mems {
		add("scope", m.Scope, m.ID)
		for _, t := range m.Tags {
			add("tag", t, m.ID)
		}
		hay := m.Title + "\n" + m.Text
		for _, mm := range wikilinkRe.FindAllStringSubmatch(hay, -1) {
			add("link", mm[1], m.ID)
		}
		for _, loc := range categoryRe.FindAllStringSubmatchIndex(m.Text, -1) {
			name := m.Text[loc[2]:loc[3]]
			if isCheckboxMarker(name) {
				continue
			}
			if loc[1] < len(m.Text) && m.Text[loc[1]] == '(' {
				continue
			}
			add("category", name, m.ID)
		}
	}
	out := make([]structuralEntity, 0, len(seen))
	for k, ids := range seen {
		idList := make([]string, 0, len(ids))
		for id := range ids {
			idList = append(idList, id)
		}
		sort.Strings(idList)
		out = append(out, structuralEntity{Name: k.name, Kind: k.kind, Count: len(idList), MemoryIDs: idList})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out
}
func isCheckboxMarker(s string) bool {
	switch strings.TrimSpace(s) {
	case "", "x", "X":
		return true
	}
	return false
}
