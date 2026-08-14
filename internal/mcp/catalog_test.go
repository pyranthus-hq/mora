package mcp

import (
	"reflect"
	"testing"
)

func TestToolCatalogCanonicalOrderAndCompleteness(t *testing.T) {
	defs := ToolCatalog()
	want := []string{"write_memory", "read_memory", "search_memory", "calendar_events", "list_memory", "delete_memory", "context_memory", "think", "list_entities", "get_entity", "digest", "brief", "meeting_prep"}
	if len(defs) != len(want) {
		t.Fatalf("len=%d", len(defs))
	}
	seen := map[string]bool{}
	for i, def := range defs {
		if def.Name != want[i] {
			t.Errorf("[%d]=%q", i, def.Name)
		}
		if seen[def.Name] {
			t.Errorf("duplicate %q", def.Name)
		}
		seen[def.Name] = true
		if def.Description == "" {
			t.Errorf("%s empty description", def.Name)
		}
	}
}
func TestToolCatalogReturnsDeepCopy(t *testing.T) {
	a := ToolCatalog()
	a[0].Name = "changed"
	a[0].Params[0].Name = "changed"
	b := ToolCatalog()
	if b[0].Name != "write_memory" || b[0].Params[0].Name != "title" {
		t.Fatalf("catalog mutated: %+v", b[0])
	}
}
func TestRenderToolsPreservesOrderAndStrictSchemas(t *testing.T) {
	defs := []ToolDefinition{{Name: "first", Description: "one", Params: []Param{{Name: "required", Type: "string", Desc: "r", Required: true}, {Name: "optional", Type: "integer", Desc: "o"}}}, {Name: "second", Description: "two"}}
	got := RenderTools(defs)
	want := []map[string]any{{"name": "first", "description": "one", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"required": map[string]any{"type": "string", "description": "r"}, "optional": map[string]any{"type": "integer", "description": "o"}}, "additionalProperties": false, "required": []string{"required"}}}, {"name": "second", "description": "two", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%#v", got)
	}
}
