package mora

import "testing"

func findEntity(es []Entity, kind, name string) *Entity {
	for i := range es {
		if es[i].Kind == kind && es[i].Name == name {
			return &es[i]
		}
	}
	return nil
}

func TestExtractEntities(t *testing.T) {
	mems := []Memory{
		{ID: "m1", Scope: "project:demo", Title: "Kickoff with [[Neil]]", Tags: []string{"pilot"},
			Text: "Talked to [[Neil]] about the plan.\n- [Decision] adopt MCP"},
		{ID: "m2", Scope: "project:demo", Title: "Follow-up", Tags: []string{"pilot", "urgent"},
			Text: "[[Neil]] confirmed. Loop in [[Marcus]]."},
		{ID: "m3", Scope: "personal", Title: "Note", Text: "no entities here"},
	}

	es := extractEntities(mems)

	// scope: project:demo referenced by m1+m2 (count 2), personal by m3 (count 1)
	if e := findEntity(es, "scope", "project:demo"); e == nil || e.Count != 2 {
		t.Fatalf("scope project:demo: %+v", e)
	}
	if e := findEntity(es, "scope", "personal"); e == nil || e.Count != 1 {
		t.Fatalf("scope personal: %+v", e)
	}

	// tag: pilot in m1+m2 (count 2), urgent in m2 (count 1)
	if e := findEntity(es, "tag", "pilot"); e == nil || e.Count != 2 {
		t.Fatalf("tag pilot: %+v", e)
	}
	if e := findEntity(es, "tag", "urgent"); e == nil || e.Count != 1 {
		t.Fatalf("tag urgent: %+v", e)
	}

	// link: [[Neil]] appears in m1 (title+body) and m2 -> distinct memories = 2 (not 3)
	neil := findEntity(es, "link", "Neil")
	if neil == nil || neil.Count != 2 {
		t.Fatalf("link Neil should be count 2 (distinct memories): %+v", neil)
	}
	if len(neil.MemoryIDs) != 2 {
		t.Fatalf("link Neil MemoryIDs = %v, want [m1 m2]", neil.MemoryIDs)
	}
	if e := findEntity(es, "link", "Marcus"); e == nil || e.Count != 1 {
		t.Fatalf("link Marcus: %+v", e)
	}

	// category: [Decision] from m1
	if e := findEntity(es, "category", "Decision"); e == nil || e.Count != 1 {
		t.Fatalf("category Decision: %+v", e)
	}
}

func TestExtractEntitiesIgnoresCheckboxes(t *testing.T) {
	mems := []Memory{
		{ID: "t1", Scope: "personal", Text: "- [ ] todo item\n- [x] done item\n- [X] also done"},
	}
	es := extractEntities(mems)
	for _, e := range es {
		if e.Kind == "category" {
			t.Fatalf("checkbox markers must not become categories, got %q", e.Name)
		}
	}
}

func TestExtractEntitiesIgnoresMarkdownLinkBullets(t *testing.T) {
	// "- [Title](url)" is a Markdown link list item, NOT a "- [Category]" tag.
	mems := []Memory{
		{ID: "ml", Scope: "personal", Text: "- [Anthos](anthos.md) — the audit sprint\n- [RealCategory] keep this one"},
	}
	es := extractEntities(mems)
	if e := findEntity(es, "category", "Anthos"); e != nil {
		t.Fatalf("Markdown link [Anthos](url) must not be a category, got %+v", e)
	}
	if e := findEntity(es, "category", "RealCategory"); e == nil {
		t.Fatalf("a genuine - [RealCategory] line should still be a category")
	}
}

func TestExtractEntitiesSortedByCountDesc(t *testing.T) {
	mems := []Memory{
		{ID: "a", Scope: "project:big"},
		{ID: "b", Scope: "project:big"},
		{ID: "c", Scope: "project:big"},
		{ID: "d", Scope: "project:small"},
	}
	es := extractEntities(mems)
	// first scope entity must be the higher-count one
	var firstScope *Entity
	for i := range es {
		if es[i].Kind == "scope" {
			firstScope = &es[i]
			break
		}
	}
	if firstScope == nil || firstScope.Name != "project:big" {
		t.Fatalf("entities not sorted by count desc: %+v", es)
	}
}

func TestExtractEntitiesEmpty(t *testing.T) {
	if es := extractEntities(nil); len(es) != 0 {
		t.Fatalf("nil memories should yield no entities, got %v", es)
	}
}
