package mora

import (
	"context"
	"strings"
	"testing"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

func noopMCPHandler(context.Context, Config, map[string]any) (any, error) { return nil, nil }
func TestBindMCPToolsRequiresExactHandlerAlignment(t *testing.T) {
	catalog := []mcppkg.ToolDefinition{{Name: "one"}}
	got := bindMCPTools(catalog, map[string]func(context.Context, Config, map[string]any) (any, error){"one": noopMCPHandler})
	if len(got) != 1 || got[0].Name != "one" || got[0].Handler == nil {
		t.Fatalf("got=%+v", got)
	}
	cases := []struct {
		name     string
		catalog  []mcppkg.ToolDefinition
		handlers map[string]func(context.Context, Config, map[string]any) (any, error)
		want     string
	}{{"missing", catalog, nil, "missing MCP tool handler: one"}, {"extra", nil, map[string]func(context.Context, Config, map[string]any) (any, error){"extra": noopMCPHandler}, "MCP tool handler has no metadata: extra"}, {"duplicate", []mcppkg.ToolDefinition{{Name: "one"}, {Name: "one"}}, map[string]func(context.Context, Config, map[string]any) (any, error){"one": noopMCPHandler}, "duplicate MCP tool metadata: one"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				got := recover()
				if got == nil || !strings.Contains(got.(string), tc.want) {
					t.Fatalf("panic=%v", got)
				}
			}()
			bindMCPTools(tc.catalog, tc.handlers)
		})
	}
}
