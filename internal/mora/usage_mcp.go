package mora

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

// mcpUsageTrace carries content-free measurement from one tools/call handler to
// the envelope boundary. The pointer is request-local in context, so concurrent
// MCP/HTTP calls never share mutable measurement state.
type mcpUsageTrace struct {
	event           usageEvent
	configMillis    int64
	retrievalMillis *int64
	assemblyMillis  *int64
}

type mcpUsageTraceKey struct{}

// mcpToolInvocation is the native result plus the measurement state needed to
// finish one event only after the final CallToolResult has been assembled and
// serialized. callMCPTool still returns the native value to its internal/HTTP
// callers; handleMCP returns invocation.result() directly to the MCP client.
type mcpToolInvocation struct {
	cfg      Config
	started  time.Time
	trace    *mcpUsageTrace
	value    any
	err      error
	loggable bool
}

func invokeMCPTool(ctx context.Context, name string, args map[string]any) mcpToolInvocation {
	started := time.Now()
	configStarted := time.Now()
	cfg, err := loadConfigFor(ctx)
	trace := &mcpUsageTrace{configMillis: time.Since(configStarted).Milliseconds()}
	inv := mcpToolInvocation{cfg: cfg, started: started, trace: trace, err: err}
	if err != nil {
		// A failed config load does not establish a trustworthy state directory.
		// Return the error envelope without guessing where an event belongs.
		return inv
	}
	def, ok := mcpToolIndex[name]
	if !ok {
		inv.err = unknownMCPToolError(name)
		return inv
	}
	inv.loggable = true
	traced := context.WithValue(ctx, mcpUsageTraceKey{}, trace)
	policyHandled := false
	action, policyErr := mcppkg.MutationAction(configMCPWritePolicy(cfg), name)
	switch action {
	case mcppkg.ActionRefuse:
		inv.err = policyErr
		policyHandled = true
	case mcppkg.ActionPropose:
		inv.value, inv.err = stageMCPWriteProposal(cfg, args)
		policyHandled = true
	}
	if !policyHandled {
		inv.value, inv.err = def.Handler(traced, cfg, args)
	}
	if trace.event.Tool == "" {
		// Some handlers (including mutations) have no tool-specific structural
		// counts. They still get one content-free event and honest envelope size.
		results := 0
		if inv.err == nil {
			results = 1
		}
		trace.event = usageEvent{Tool: name, Results: results}
	}
	return inv
}

func unknownMCPToolError(name string) error {
	// Kept separate so the dispatcher and measurement wrapper have one error
	// spelling without copying a raw unknown name into the usage log.
	return fmt.Errorf("unknown tool %q", name)
}

// result assembles the real CallToolResult, serializes that exact map once for
// output_bytes, then appends the event. The returned map is the same map that
// handleMCP places in jsonRPCResponse.Result; measurement never reshapes it.
func (inv mcpToolInvocation) result() map[string]any {
	envelopeStarted := time.Now()
	result := toCallToolResult(inv.value, inv.err)
	b, _ := json.Marshal(result)
	envelopeMillis := time.Since(envelopeStarted).Milliseconds()
	if !inv.loggable {
		return result
	}
	event := inv.trace.event
	event.Millis = time.Since(inv.started).Milliseconds()
	event.OutputBytes = len(b)
	event.Phases = &usagePhaseTimings{
		ConfigMillis:    inv.trace.configMillis,
		RetrievalMillis: inv.trace.retrievalMillis,
		AssemblyMillis:  inv.trace.assemblyMillis,
		EnvelopeMillis:  envelopeMillis,
	}
	logUsage(inv.cfg, event)
	return result
}

// recordMCPUsage defers a handler's structural metadata to the final envelope
// boundary. Direct helper-level calls (outside invokeMCPTool) retain the legacy
// immediate logging behavior; real MCP and HTTP dispatch always carry a trace.
func recordMCPUsage(ctx context.Context, cfg Config, event usageEvent) {
	if trace, ok := ctx.Value(mcpUsageTraceKey{}).(*mcpUsageTrace); ok {
		trace.event = event
		return
	}
	logUsage(cfg, event)
}

func recordMCPPhases(ctx context.Context, retrieval, assembly time.Duration) {
	trace, ok := ctx.Value(mcpUsageTraceKey{}).(*mcpUsageTrace)
	if !ok {
		return
	}
	retrievalMillis := retrieval.Milliseconds()
	assemblyMillis := assembly.Milliseconds()
	trace.retrievalMillis = &retrievalMillis
	trace.assemblyMillis = &assemblyMillis
}

// readUsageMode exposes only an allowlisted structural label. Presence of the
// evidence_ref arg wins over match-bounded knobs because the integrated E lane
// uses that mode. Any unrecognized arg/mode maps to "other" instead of copying
// a future mode name (which may itself contain sensitive user input).
func readUsageMode(args map[string]any) string {
	for key := range args {
		switch key {
		case "id", "match", "max_tokens", "occurrence", "evidence_ref", "mode":
		default:
			return "other"
		}
	}
	if raw, ok := args["mode"]; ok {
		mode, _ := raw.(string)
		label := allowlistedReadUsageMode(mode)
		if label == "other" {
			return label
		}
	}
	if _, ok := args["evidence_ref"]; ok {
		return "evidence_ref"
	}
	if boundedReadRequested(args) {
		return "match"
	}
	if raw, ok := args["mode"].(string); ok {
		return allowlistedReadUsageMode(raw)
	}
	return "full"
}

func allowlistedReadUsageMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "full":
		return "full"
	case "match", "bounded":
		return "match"
	case "evidence_ref":
		return "evidence_ref"
	default:
		return "other"
	}
}

func readUsageEvent(args map[string]any, result map[string]any, found bool) usageEvent {
	truncated := false
	matchCount := 0
	budgetRequested := intArg(args, "max_tokens", 0)
	budgetUsed := 0
	if found {
		budgetUsed = estimateTokensUsed(len(readUsageText(result)))
	}
	if receipt, ok := result["receipt"].(boundedReadReceipt); ok {
		truncated = receipt.Truncated
		matchCount = receipt.MatchCount
	}
	return usageEvent{
		Tool:            "read_memory",
		Results:         boolInt(found),
		Mode:            readUsageMode(args),
		Truncated:       &truncated,
		MatchCount:      &matchCount,
		BudgetRequested: &budgetRequested,
		BudgetUsed:      &budgetUsed,
	}
}

func readUsageText(result map[string]any) string {
	switch memory := result["memory"].(type) {
	case Memory:
		return memory.Text
	case map[string]any:
		text, _ := memory["text"].(string)
		return text
	default:
		return ""
	}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
