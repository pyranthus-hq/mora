package mora

import (
	"context"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// signWriteArgs returns write_memory arguments carrying the writer's
// signature. An explicit, non-empty agent argument wins (a named bot running
// inside a generic client); otherwise the session's MCP clientInfo.name is
// used. The caller's map is never mutated. With neither, the arguments are
// returned unchanged and the memory stays unsigned (read back as "unknown").
func signWriteArgs(ctx context.Context, args map[string]any) map[string]any {
	if explicit, ok := args["agent"]; ok {
		if s, isString := explicit.(string); !isString || s != "" {
			// A non-string is left for MemoryFromArgs to reject.
			return args
		}
	}
	agent := memory.AgentFromClientName(mcppkg.ClientName(ctx))
	if agent == "" {
		return args
	}
	signed := make(map[string]any, len(args)+1)
	for k, v := range args {
		signed[k] = v
	}
	signed["agent"] = agent
	return signed
}

// memoryAgentUnknown is memory.AgentUnknown for files where a local variable
// named memory shadows the package.
const memoryAgentUnknown = memory.AgentUnknown

// signedBy is the human-output suffix naming an authored memory's signature,
// "" for connector records and shared-corpus rows (whose owner already
// attributes them).
func signedBy(sty styler, m Memory) string {
	if m.Owner != "" {
		return ""
	}
	agent := memory.DisplayAgent(m)
	if agent == "" {
		return ""
	}
	return "\t" + sty.dim("by "+agent)
}
