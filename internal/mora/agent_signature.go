package mora

import (
	"context"
	"fmt"
	"strings"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// signWriteArgs returns write_memory arguments carrying the writer's
// self-reported agent label. The first that applies wins:
//  1. an explicit, non-blank agent argument (a named bot running inside a
//     generic client);
//  2. the session's MCP clientInfo.name;
//  3. the default a trusted in-process caller put in ctx (loopback HTTP
//     writes are labeled "http").
//
// A caller may not claim one of Mora's reserved labels (cli, mora, http,
// companion, unknown), by argument or by client name: at the approval step
// "(by cli)" reads as the owner. A blank agent argument counts as absent. The
// caller's map is never mutated. With none of the three, the arguments are
// returned unchanged and the memory stays unsigned (read back as "unknown").
func signWriteArgs(ctx context.Context, args map[string]any) (map[string]any, error) {
	if explicit, ok := args["agent"]; ok {
		s, isString := explicit.(string)
		if !isString {
			// A non-string is left for MemoryFromArgs to reject.
			return args, nil
		}
		if strings.TrimSpace(s) != "" {
			agent, err := memory.NormalizeAgent(s)
			if err != nil {
				// MemoryFromArgs reports the exact problem.
				return args, nil
			}
			if memory.IsReservedAgent(agent) {
				return nil, fmt.Errorf("agent %q is reserved for Mora's own writers; name the agent itself, such as \"claude-code\" or a bot name", agent)
			}
			return withAgent(args, agent), nil
		}
	}
	agent := memory.AgentFromClientName(mcppkg.ClientName(ctx))
	if memory.IsReservedAgent(agent) {
		agent = ""
	}
	if agent == "" {
		agent = mcppkg.DefaultAgent(ctx)
	}
	if agent == "" {
		return args, nil
	}
	return withAgent(args, agent), nil
}

// withAgent returns a copy of args with agent set.
func withAgent(args map[string]any, agent string) map[string]any {
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
