package mora

import (
	"context"
	"reflect"
	"testing"
)

func TestMCPResourceProjectCatalogIsExactSevenTools(t *testing.T) {
	ctx := context.WithValue(context.Background(), mcpProjectKey{}, mcpProject{scope: "project:resource", root: "/r"})
	ctx = withMCPWorkspace(ctx)
	defs := mcpSessionToolCatalog(ctx)
	got := make([]string, len(defs))
	for i := range defs {
		got[i] = defs[i].Name
	}
	want := []string{"search_memory", "read_memory", "context_memory", "write_memory", "brief", "discover_resources", "submit_resource"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("project catalog=%v want=%v", got, want)
	}
	if mcpProjectAllowsTool(ctx, "list_memory") {
		t.Fatal("project dispatch exposed hidden list_memory")
	}
}
