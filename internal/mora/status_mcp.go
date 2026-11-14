package mora

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"unicode/utf8"
)

// statusMCP reports the current local stdio default/project-bound surface. It
// is a startup-cost diagnostic, not an authenticated profile or live-client measurement.
type statusMCP struct {
	Surface      string     `json:"surface"`
	Caveat       string     `json:"caveat"`
	ToolCount    int        `json:"tool_count"`
	Instructions statusCost `json:"instructions"`
	Catalog      statusCost `json:"catalog"`
	Combined     statusCost `json:"combined"`
}

type statusCost struct {
	Characters      int `json:"characters"`
	Bytes           int `json:"bytes"`
	EstimatedTokens int `json:"estimated_tokens"`
}

func (c statusCost) text() string {
	return strconv.Itoa(c.Characters) + " chars/" + strconv.Itoa(c.Bytes) + " bytes/~" + strconv.Itoa(c.EstimatedTokens) + " tokens"
}

// statusMCPReport drives the same request handler as stdio initialize and
// tools/list. It never starts a server or invokes a tool.
func statusMCPReport(ctx context.Context, cwd string) statusMCP {
	ctx = withMCPServeProject(ctx, cwd, io.Discard)
	initialize := handleMCP(ctx, jsonRPCRequest{Method: "initialize"})
	tools := handleMCP(ctx, jsonRPCRequest{Method: "tools/list"})

	instructions, _ := initialize.Result.(map[string]any)["instructions"].(string)
	catalog, _ := tools.Result.(map[string]any)
	toolList, _ := catalog["tools"].([]map[string]any)
	encoded, _ := json.Marshal(catalog)
	return statusMCP{
		Surface:      "current local stdio default/project-bound MCP-only surface",
		Caveat:       "Hooks, rules, and recall are excluded; client token counts are estimated, not an authenticated profile or live-client measurement.",
		ToolCount:    len(toolList),
		Instructions: statusCostFor([]byte(instructions)),
		Catalog:      statusCostFor(encoded),
		Combined:     statusCostFor(append([]byte(instructions), encoded...)),
	}
}

func statusCostFor(b []byte) statusCost {
	return statusCost{Characters: utf8.RuneCount(b), Bytes: len(b), EstimatedTokens: (utf8.RuneCount(b) + 3) / 4}
}
