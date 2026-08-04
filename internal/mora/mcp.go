package mora

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"
)

func cmdMCP(ctx context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) error {
	if len(args) == 1 && args[0] == "serve" {
		return serveMCP(ctx, stdout, stdin)
	}
	if len(args) >= 2 && args[0] == "proposals" {
		return cmdMCPProposals(ctx, args[1:], stdout)
	}
	return errors.New("usage: mora mcp serve | mora mcp proposals <list|approve ID|reject ID>")
}
func serveMCP(ctx context.Context, stdout io.Writer, stdin io.Reader) error {
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 64*1024), mcpMaxRequestBytes)
	for scanner.Scan() {
		var req jsonRPCRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		// JSON-RPC notifications carry no "id" and MUST NOT be answered. The
		// post-initialize `notifications/initialized` is the common one; replying
		// to it (with a stray -32601 frame) makes strict MCP clients — notably
		// Antigravity's official go-sdk — abort the session and drop every tool
		// ("tools/list: invalid request"). Lenient clients (Claude Code, Codex)
		// tolerate the stray frame, which is why this hid. Ignore notifications.
		if req.ID == nil {
			continue
		}
		resp := handleMCP(ctx, req)
		b, _ := json.Marshal(resp)
		fmt.Fprintln(stdout, string(b))
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return fmt.Errorf("MCP request line exceeded the %d-byte cap: %w", mcpMaxRequestBytes, err)
		}
		return err
	}
	return nil
}

// contextDefaultTokens resolves the ContextProfile to the default token budget
// used when a caller passes no max_tokens: small=3000, default=6000,
// large=12000. Unknown values fall back to the default (never zero).
func (c Config) contextDefaultTokens() int {
	switch c.ContextProfile {
	case "small":
		return defaultContextTokens / 2
	case "large":
		return defaultContextTokens * 2
	default:
		return defaultContextTokens
	}
}

// contextMaxTokens resolves the ContextProfile to the per-call max_tokens
// ceiling: small/default keep the 20k guardrail (one tool result must not
// dominate a normal agent window); large opts into 50k — the user choosing
// "large" is explicitly trading window headroom for denser single-call context.
func (c Config) contextMaxTokens() int {
	if c.ContextProfile == "large" {
		return largeContextMaxTokens
	}
	return maxContextTokens
}

// digestSnippetChars resolves the ContextProfile to the digest per-item
// snippet length: small=120, default=200 (digestSnippetLen), large=400. The
// large profile exists precisely so conversation tails (the user's own
// replies) survive the clip — see digestItemFor.
func (c Config) digestSnippetChars() int {
	switch c.ContextProfile {
	case "small":
		return 120
	case "large":
		return 400
	default:
		return digestSnippetLen
	}
}

// budgetUnitTokens is stamped on every budget-bounded MCP/CLI JSON envelope so
// consumers can verify the knob unit (#69).
const budgetUnitTokens = "tokens"

// resolveContextBudgetTokens clamps a requested token budget and returns both
// the token count and its char equivalent. Non-positive requests fall back to the
// profile default; over-ceiling requests clamp to contextMaxTokens. Clamping
// happens BEFORE the *charsPerToken conversion so an arbitrarily large max_tokens
// cannot overflow.
func resolveContextBudgetTokens(cfg Config, maxTokens int) (tokens int, charBudget int) {
	if maxTokens <= 0 {
		maxTokens = cfg.contextDefaultTokens()
	}
	if ceiling := cfg.contextMaxTokens(); maxTokens > ceiling {
		maxTokens = ceiling
	}
	return maxTokens, maxTokens * charsPerToken
}

// resolveContextBudget converts a requested token budget (the context_memory
// max_tokens arg) into a character budget for buildContext.
func resolveContextBudget(cfg Config, maxTokens int) int {
	_, chars := resolveContextBudgetTokens(cfg, maxTokens)
	return chars
}

// estimateTokensUsed converts a byte count to the codebase's token heuristic.
func estimateTokensUsed(bytes int) int {
	if bytes <= 0 {
		return 0
	}
	return (bytes + charsPerToken - 1) / charsPerToken
}

// mcpEntityBudgetChars converts max_tokens into a compact dossier byte budget,
// accounting for the CallToolResult envelope inflation (same divisor as digest).
func mcpEntityBudgetChars(cfg Config, maxTokens int) int {
	return resolveContextBudget(cfg, maxTokens) / mcpDigestEnvelopeDivisor
}

// mcpDigestBudgetChars converts a requested max_tokens into a COMPACT-payload byte
// budget for digestMCPPayload such that the doubled+indented envelope stays under
// the token ceiling. resolveContextBudget clamps the request to [6000,20000]
// tokens; we divide by the envelope inflation factor so the on-the-wire envelope
// (what the T0 gate measures) respects the ceiling while the knob still scales
// (a 20k request yields a strictly larger compact budget than the 6k default).
func mcpDigestBudgetChars(cfg Config, maxTokens int) int {
	return resolveContextBudget(cfg, maxTokens) / mcpDigestEnvelopeDivisor
}
func handleMCP(ctx context.Context, req jsonRPCRequest) jsonRPCResponse {
	resp := jsonRPCResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var instructions string
		if cfg, err := loadConfig(); err == nil {
			instructions = mcpInstructionsFor(cfg.mcpWritePolicy())
		} else {
			instructions = "Mora could not load its configuration, so tools are unavailable and no mutation will be attempted. Fix config.toml and reconnect."
		}
		resp.Result = map[string]any{"protocolVersion": "2024-11-05", "serverInfo": map[string]string{"name": "mora", "version": BuildVersion}, "capabilities": map[string]any{"tools": map[string]any{}}, "instructions": instructions}
	case "tools/list":
		tools := make([]map[string]any, 0, len(mcpToolRegistry))
		for _, def := range mcpToolRegistry {
			tools = append(tools, mcpTool(def.Name, def.Description, def.Params...))
		}
		resp.Result = map[string]any{"tools": tools}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		resp.Result = invokeMCPTool(ctx, p.Name, p.Arguments).result()
	default:
		resp.Error = map[string]any{"code": -32601, "message": "method not found"}
	}
	return resp
}

// toCallToolResult wraps a tool's native return value in a spec-compliant MCP
// CallToolResult. The payload is JSON-encoded into a text content block (the
// shape strict clients like Codex read — a bare []Memory/map is rejected as an
// "unexpected response type") and, when object-shaped, mirrored into
// structuredContent for clients that consume machine-readable output. A tool
// error is returned as isError:true content rather than a JSON-RPC error, so the
// calling agent's tool loop stays alive and can react to the message.
func toCallToolResult(v any, err error) map[string]any {
	if err != nil {
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": err.Error()}},
			"isError": true,
		}
	}
	text, mErr := json.MarshalIndent(v, "", "  ")
	if mErr != nil {
		text = []byte(fmt.Sprintf("%v", v))
	}
	res := map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(text)}},
		"isError": false,
	}
	// structuredContent must be a JSON object per the MCP spec; only attach it
	// when the marshaled value is object-shaped. Tools that return arrays still
	// carry the full payload via the text block above.
	if len(text) > 0 && text[0] == '{' {
		res["structuredContent"] = v
	}
	return res
}

// mcpToolDef is one MCP tool's registry entry — the SINGLE source both
// tools/list (via mcpTool) and callMCPTool derive from (C3 ▸R2: MCP had THREE
// hand-lists before this — the tools/list literal, the callMCPTool switch, and
// httpCallAllowed — so a tool could silently ship in one and not another). It
// also doubles as the completeness registry TestEverySurfaceCarriesHealth
// enumerates: every entry here is driven through its real dispatcher
// (callMCPTool), never a helper-level unit test.
type mcpToolDef struct {
	Name        string
	Description string
	Params      []mcpParam
	Handler     func(ctx context.Context, cfg Config, args map[string]any) (any, error)
}

// mcpToolRegistry is the derived source of truth for every MCP tool. Order is
// preserved in tools/list for readability; callMCPTool derives its dispatch map
// from this slice (mcpToolIndex), so a tool added here needs no second edit.
var mcpToolRegistry = []mcpToolDef{
	{
		Name: "write_memory", Description: "Write a durable memory to the vault",
		Params: []mcpParam{
			{"title", "string", "Short human-readable title for the memory", true},
			{"text", "string", "The memory body (Markdown allowed)", true},
			{"scope", "string", `Scope/namespace, e.g. "global" or "project:acme" (default "global")`, false},
			{"type", "string", `Memory type: insight|fact|decision|task (default "insight")`, false},
			{"source", "string", `Origin label (default "mcp")`, false},
			{"as_of", "string", "Decision validity instant (RFC3339; decision memories only)", false},
			{"durability", "string", "Decision durability: provisional|working|standing", false},
			{"flip_conditions", "string", "Semicolon-separated conditions that would reverse the decision", false},
			{"review_by", "string", "Optional decision review deadline (RFC3339)", false},
		},
		Handler: mcpWriteMemory,
	},
	{
		Name: "read_memory", Description: "Read a single memory by its id",
		Params: []mcpParam{
			{"id", "string", "The memory id (as returned by search_memory/list_memory)", true},
			{"match", "string", "Optional literal phrase to center a bounded excerpt on (omit for the full body)", false},
			{"max_tokens", "integer", "Optional excerpt budget in tokens for bounded reads (default ~800)", false},
			{"occurrence", "integer", "Optional 1-indexed match occurrence to center the excerpt on (default 1)", false},
			{"evidence_ref", "string", "Optional Gmail or iMessage evidence ref (from search_memory's evidence.evidence_ref) to read ONLY that message's derived segment, bounded, with a receipt naming its sender/time and iMessage direction; a ref that does not belong to this memory id is rejected", false},
		},
		Handler: mcpReadMemory,
	},
	{
		Name: "search_memory", Description: "Search the vault for the most relevant memories (hybrid semantic+keyword when Ollama embeddings are enabled, full-text otherwise)",
		Params: []mcpParam{
			{"query", "string", "Search query (words are OR-matched against the index)", true},
			{"scope", "string", `Optional scope filter, e.g. "project:acme"`, false},
			{"limit", "integer", "Max results to return (default 8)", false},
			{"confidence", "boolean", "Opt-in: also return a compact confidence envelope (strength, score rollup, freshest source, missing/unhealthy sources) derived from this call's own results (default false)", false},
			{"source", "string", `Filter to one connector: "imessage", "gmail", "calendar", "applecalendar", "github", or an account instance like "gmail:work" ("gmail" spans all gmail accounts). Applied BEFORE ranking in every retrieval arm. An unrecognized value is a tool error.`, false},
			{"since_hours", "integer", "Only memories created in the last N hours (must be a positive integer). Applied BEFORE ranking in every retrieval arm.", false},
		},
		Handler: mcpSearchMemory,
	},
	{
		Name: "list_memory", Description: "Browse the memories Mora wrote most recently, newest first. Ordered by `indexed_at` (when Mora recorded the memory), never by event time, so a future calendar event cannot lead the list. Each row splits the timestamps `created_at` conflated: `event_start` (when a calendar event happens), `source_created_at` (when the source object was created at its provider), and `indexed_at`; a field Mora cannot derive honestly is omitted rather than filled in",
		Params: []mcpParam{
			{"scope", "string", "Optional scope filter", false},
			{"limit", "integer", "Max memories to return (default 10)", false},
		},
		Handler: mcpListMemory,
	},
	{
		Name: "delete_memory", Description: "Delete a memory by its id",
		Params:  []mcpParam{{"id", "string", "The memory id to delete", true}},
		Handler: mcpDeleteMemory,
	},
	{
		Name: "context_memory", Description: "Assemble one dense, budget-bounded context block for a query (or a session-start briefing when no query is given)",
		Params: []mcpParam{
			{"query", "string", "Topic to assemble context for; omit for a recency briefing", false},
			{"scope", "string", "Optional scope filter", false},
			{"max_tokens", "integer", "Approximate token budget for the response (default ~6000, max ~20000)", false},
			{"source", "string", `Filter to one connector: "imessage", "gmail", "calendar", "applecalendar", "github", or an account instance like "gmail:work" ("gmail" spans all gmail accounts). Applied BEFORE ranking in every retrieval arm, including the no-query recency fallback. An unrecognized value is a tool error.`, false},
			{"since_hours", "integer", "Only memories created in the last N hours (must be a positive integer). Applied BEFORE ranking in every retrieval arm, including the no-query recency fallback.", false},
		},
		Handler: mcpContextMemory,
	},
	{
		Name: "think", Description: "Synthesis envelope for a question: cited evidence + a deterministic 'what the vault does NOT know' gap analysis + a prompt to compose a cited answer",
		Params: []mcpParam{
			{"query", "string", "The question to synthesize an answer for", true},
			{"scope", "string", "Optional scope filter", false},
			{"limit", "integer", "Max evidence memories to gather (default 8)", false},
			{"confidence", "boolean", "Opt-in: also return a compact confidence envelope (strength, score rollup, freshest source, missing/unhealthy sources) derived from this call's own evidence (default false)", false},
		},
		Handler: mcpThink,
	},
	{
		Name: "list_entities", Description: "List the entities (people, scopes, tags, [[links]], categories) referenced across memory, with counts, ranked by salience",
		Params: []mcpParam{
			{"kind", "string", `Optional kind filter: "person", "service", "scope", "tag", "link", or "category"`, false},
			{"limit", "integer", "Max entities to return, ranked by salience (default 150)", false},
		},
		Handler: mcpListEntities,
	},
	{
		Name: "get_entity", Description: "Get a budget-bounded, fully-cited dossier for a named entity (merged identities, typed neighbors, top evidence by salience)",
		Params: []mcpParam{
			{"name", "string", "The entity name (person, tag, scope, or [[link]]) to fetch", true},
			{"max_tokens", "integer", "Approximate token budget for the dossier (default ~6000, max ~20000)", false},
		},
		Handler: mcpGetEntity,
	},
	{
		Name: "digest", Description: "Assemble a daily cross-source digest (recent emails, texts, calendar items, and stale open tasks), grouped by source, cited, and budget-bounded; opt into `envelope` to also get a synthesis_prompt for composing a grounded, cited brief",
		Params: []mcpParam{
			{"since_hours", "integer", "Look-back window in hours (default 24)", false},
			{"source", "string", `Filter to one connector: "imessage", "gmail", "calendar", "applecalendar", "github", or an account instance like "gmail:work" ("gmail" spans all gmail accounts). Use with since_hours for asks like "my texts from the past week" — without it, earlier-ranked sources can consume the byte budget`, false},
			{"max_tokens", "integer", "Approximate token budget for the digest (default ~6000, max ~20000)", false},
			{"envelope", "boolean", "Opt-in: also return a synthesis_prompt instructing the agent to write a grounded, cited brief over the digest items (default false; Mora makes no model call)", false},
			{"entity", "string", `Filter to memories referencing one person (display name or email/handle, e.g. "Riya" or "riya@example.com"). A no-match or ambiguous name returns an error rather than an empty digest. Preview-only.`, false},
			{"scope", "string", `Filter to one memory scope/namespace, e.g. "project:acme". Preview-only.`, false},
			{"since_days", "integer", "Additional look-back: only memories created in the last N days (negative is treated as no filter). Preview-only.", false},
		},
		Handler: mcpDigest,
	},
	{
		Name: "brief", Description: "Return the latest what-changed/what-matters brief for session start — the same budgeted, cited, source-grouped daily brief as `digest`, resolved to the freshest available; call this FIRST at the start of a session. Opt into `envelope` for a synthesis_prompt to compose a grounded, cited brief.",
		Params: []mcpParam{
			{"max_tokens", "integer", "Approximate token budget for the brief (default ~6000, max ~20000)", false},
			{"envelope", "boolean", "Opt-in: also return a synthesis_prompt for composing a grounded, cited brief over the items (default false; Mora makes no model call)", false},
			{"entity", "string", `Filter the brief to memories referencing one person (display name or email/handle). A no-match or ambiguous name returns an error. Preview-only.`, false},
			{"scope", "string", `Filter the brief to one memory scope/namespace, e.g. "project:acme". Preview-only.`, false},
			{"since_days", "integer", "Additional look-back: only memories created in the last N days (negative = no filter). Preview-only.", false},
		},
		Handler: mcpBrief,
	},
	{
		Name: "meeting_prep", Description: "Assemble the same fully-cited unfinished-business brief as `mora brief --event-id`: user-owned obligations, unresolved threads, staleness guards, and material shared context. Every evidence line carries memory_id, channel/source, and date. Local, deterministic, and model-free.",
		Params: []mcpParam{
			{"event_id", "string", "Calendar memory id to brief; omit to use the next (or in-progress) event", false},
			{"at", "string", "RFC3339 as-of time for reproducible assembly (default now)", false},
			{"name", "string", `Optional attendee name/email/handle: prep the next meeting WITH this person (falls back to the next meeting if they have none). Omit for the next meeting on the calendar.`, false},
			{"limit", "integer", "Max actionable cited lines per attendee (default 8)", false},
			{"max_tokens", "integer", "Approximate token budget for the pack (default ~6000, max ~20000)", false},
		},
		Handler: mcpMeetingPrep,
	},
}

// mcpToolIndex is the name->def lookup callMCPTool dispatches through, built
// once from mcpToolRegistry.
var mcpToolIndex = buildMCPToolIndex(mcpToolRegistry)

func buildMCPToolIndex(defs []mcpToolDef) map[string]mcpToolDef {
	idx := make(map[string]mcpToolDef, len(defs))
	for _, d := range defs {
		idx[d.Name] = d
	}
	return idx
}

// mcpToolNames returns every registered tool name, sorted — the completeness
// registry TestEverySurfaceCarriesHealth walks (C3 ▸R2).
func mcpToolNames() []string {
	names := make([]string, 0, len(mcpToolRegistry))
	for _, d := range mcpToolRegistry {
		names = append(names, d.Name)
	}
	sort.Strings(names)
	return names
}

func callMCPTool(ctx context.Context, name string, args map[string]any) (any, error) {
	inv := invokeMCPTool(ctx, name, args)
	// Internal and loopback-HTTP callers consume the native value rather than a
	// CallToolResult. Assemble the identical envelope once for local measurement
	// so every registered dispatcher call still emits one output_bytes event.
	_ = inv.result()
	return inv.value, inv.err
}

// mcpWriteClock is the single logical clock for one write_memory call. Tests
// pin it to prove byte/token ceilings against a deterministic real MCP
// mutation; production leaves it as time.Now. Index bookkeeping retains its
// separate indexClock because that clock owns durable pending/index stamps.
var mcpWriteClock = time.Now

func mcpWriteMemory(ctx context.Context, cfg Config, args map[string]any) (any, error) {
	now := mcpWriteClock()
	m, err := mcpMemoryFromArgs(args, now)
	if err != nil {
		return nil, err
	}
	// Create-exclusive publish: concurrent write_memory calls that mint the
	// same id never clobber each other (os.Link fails EEXIST → re-mint). This
	// is the server's most concurrent write path — N agents writing at once.
	// createMemory sets m.ID and m.Path.
	var op pendingOp
	if m, op, err = createMemory(ctx, cfg, m); err != nil {
		return nil, err
	}
	m = decorateDecision(m, now)
	// The vault write succeeded (vault is truth; the index is a derived
	// cache). Reflect just this one memory into the index (O(1)) instead of a
	// full vault rebuild, so concurrent write_memory calls don't serialize
	// whole-vault rebuilds. A failed index update must SURFACE — as a degraded
	// SUCCESS, never an isError result: signaling failure for a write that stuck
	// invites the client to retry, and each retry mints a fresh server-side
	// ID (N retries = N duplicate memories). The structured result keeps
	// the saved memory + its ID so the client has nothing to re-send.
	// (delete_memory below is the deliberate asymmetry: its retry is
	// harmless, and serving deleted content warrants the loud error.)
	//
	// health (C1/C3): write_memory is a mutation, not a read — but the caller
	// still needs to know the vault it just wrote into is otherwise healthy
	// (or not), so both the plain-success and the degraded-success shape carry
	// the SAME bounded envelope under the "memory" key (Open Q1 shape).
	if rerr := indexUpsert(ctx, cfg, m); rerr != nil {
		// Degraded SUCCESS — the op REMAINS so the index reads dirty (A2), now
		// backed by durable state instead of one lost warning string.
		return map[string]any{
			"memory":      m,
			"index_stale": true,
			"warning":     fmt.Sprintf("memory %s saved, but the search index could not be updated: %v — run `mora index rebuild` (do NOT retry the write; it is saved)", m.ID, rerr),
			"health":      compactHealthOf(cfg, now),
		}, nil
	}
	_ = unmarkIndexDirty(cfg, op.OpID) // the committed upsert covers this write
	return map[string]any{"memory": m, "health": compactHealthOf(cfg, now)}, nil
}

// mcpMemoryFromArgs validates and shapes a write_memory request without
// mutating the vault. The propose policy uses the same validation before it
// persists a pending candidate, so approval cannot reveal a second, looser
// interpretation of the request.
func mcpMemoryFromArgs(args map[string]any, now time.Time) (Memory, error) {
	m := Memory{Scope: strArg(args, "scope", "global"), Type: strArg(args, "type", "insight"), Title: strArg(args, "title", ""), Text: strArg(args, "text", ""), Source: strArg(args, "source", "mcp"), CreatedAt: now.Format(time.RFC3339)}
	if m.Title == "" || m.Text == "" {
		return Memory{}, errors.New("title and text required")
	}
	if m.Type == "decision" {
		m.Decision = decisionValidityFromFlags(
			m.CreatedAt,
			strArg(args, "as_of", ""),
			strArg(args, "durability", ""),
			strArg(args, "flip_conditions", ""),
			strArg(args, "review_by", ""),
		)
	} else if strArg(args, "as_of", "") != "" || strArg(args, "durability", "") != "" ||
		strArg(args, "flip_conditions", "") != "" || strArg(args, "review_by", "") != "" {
		return Memory{}, errors.New("decision validity fields require type=decision")
	}
	return m, nil
}

func mcpReadMemory(ctx context.Context, cfg Config, args map[string]any) (any, error) {
	retrievalStarted := time.Now()
	m, err := findMemory(cfg, strArg(args, "id", ""))
	if err != nil {
		// Same read-only shared-corpus fallback as `mora read`: search
		// returns shared ids with 240-rune snippets, so read_memory is the
		// documented expansion path for them too.
		if sm, ok := findSharedMemory(cfg, strArg(args, "id", "")); ok {
			// Issue #243, P2-1 — evidence_ref is derived ONLY from the LOCAL
			// vault's own gmail_segments index; a shared memory has no such
			// projection to narrow into. Fail closed explicitly rather than
			// silently ignoring evidence_ref and returning the shared
			// memory's untouched full body.
			if strArg(args, "evidence_ref", "") != "" {
				retrieval := time.Since(retrievalStarted)
				recordMCPPhases(ctx, retrieval, 0)
				recordMCPUsage(ctx, cfg, readUsageEvent(args, nil, false))
				return nil, fmt.Errorf("evidence_ref is not supported for memory %q resolved via the shared-corpus fallback — evidence segments are derived from the local vault's own index only", strArg(args, "id", ""))
			}
			retrieval := time.Since(retrievalStarted)
			assemblyStarted := time.Now()
			result := mcpReadMemoryResult(cfg, sm, args)
			assembly := time.Since(assemblyStarted)
			recordMCPPhases(ctx, retrieval, assembly)
			recordMCPUsage(ctx, cfg, readUsageEvent(args, result, true))
			return result, nil
		}
		recordMCPPhases(ctx, time.Since(retrievalStarted), 0)
		recordMCPUsage(ctx, cfg, readUsageEvent(args, nil, false))
		return nil, err
	}
	// Issue #243 — evidence_ref narrows the read target to ONE derived Gmail
	// segment (DQ6, section 2). The segment DB lookup belongs to #245's
	// retrieval phase alongside findMemory; only bounded-read/receipt shaping
	// belongs to assembly.
	evidenceRef := strArg(args, "evidence_ref", "")
	var evidenceSegment gmailSegmentRow
	if evidenceRef != "" {
		var refErr error
		evidenceSegment, refErr = lookupReadMemoryEvidenceRef(ctx, cfg, m, evidenceRef)
		if refErr != nil {
			recordMCPPhases(ctx, time.Since(retrievalStarted), 0)
			recordMCPUsage(ctx, cfg, readUsageEvent(args, nil, false))
			return nil, refErr
		}
	}
	retrieval := time.Since(retrievalStarted)
	assemblyStarted := time.Now()
	var result map[string]any
	if evidenceRef != "" {
		result = shapeReadMemoryEvidenceRef(cfg, m, evidenceSegment, args)
	} else {
		result = mcpReadMemoryResult(cfg, m, args)
	}
	assembly := time.Since(assemblyStarted)
	recordMCPPhases(ctx, retrieval, assembly)
	recordMCPUsage(ctx, cfg, readUsageEvent(args, result, true))
	return result, nil
}

// mcpReadMemoryResult shapes the read_memory response. The parameter-free
// path (no match/max_tokens/occurrence) stays byte-identical to pre-#242
// behavior: exactly {"memory","health"}, memory.text the full untouched
// body. Any of the three #242 knobs being present opts into bounded mode
// (read_bounded.go), which replaces memory.text with a centred excerpt and
// adds the sibling "receipt" key — never a second "excerpt" field, so every
// caller keeps reading the body from memory.text.
func mcpReadMemoryResult(cfg Config, m Memory, args map[string]any) map[string]any {
	if !boundedReadRequested(args) {
		return map[string]any{"memory": m, "health": compactHealthOf(cfg, time.Now())}
	}
	shaped, receipt := applyBoundedRead(m, args)
	return map[string]any{"memory": shaped, "health": compactHealthOf(cfg, time.Now()), "receipt": receipt}
}

func mcpSearchMemory(ctx context.Context, cfg Config, args map[string]any) (any, error) {
	start := time.Now()
	// #241: since_hours reads briefClock() — the SAME test-injectable clock var
	// digest/brief already use for their own since-days/since-hours cutoffs
	// (mora.go) — never a bare time.Now(), so a single call sees one
	// consistent, pinnable clock across every retrieval arm.
	now := briefClock()
	query := strArg(args, "query", "")
	scope := strArg(args, "scope", "")
	limit := intArg(args, "limit", mcpSearchDefaultLimit)
	// #241: optional trusted-source/time-window filters, validated up front and
	// fail-closed (never a silent no-filter) on an unrecognized source or a
	// since_hours that is not a positive integer.
	filters, ferr := parseSearchFilters(args, now)
	if ferr != nil {
		return nil, ferr
	}
	// #238: defaultSearchForMCP (not the plain defaultSearch every other call
	// site uses) so the confidence envelope below can consume the ACTUAL
	// routing decision + retrieval trace this call already computed, instead
	// of re-probing chooseEmbedderFor or re-running the whole hybrid pipeline
	// a second time.
	retrievalStarted := time.Now()
	sr, err := defaultSearchForMCP(ctx, cfg, query, scope, limit, filters)
	retrieval := time.Since(retrievalStarted)
	res := sr.Results
	recordMCPUsage(ctx, cfg, usageEvent{Tool: "search_memory", Query: query, Scope: scope, Results: len(res), Millis: time.Since(start).Milliseconds()})
	if err != nil {
		recordMCPPhases(ctx, retrieval, 0)
		return nil, err
	}
	assemblyStarted := time.Now()
	// Honest-snapshot contract on the primary query surface: every search
	// answer carries the per-source last_synced map (same shape as
	// context_memory's), so the agent can qualify answers with data age
	// instead of presenting a stale vault as live.
	//
	// Aggregate byte budget (B2): snippetMemories caps each row, but a large
	// `limit` could still ship an array that dominates the MCP window — so
	// trim the total to searchMemoryResultsBudgetBytes on whole-Memory
	// boundaries, and report the cut honestly (results_truncated) rather than
	// silently dropping matches.
	budgeted, dropped := budgetSearchResults(snippetMemories(res, query), searchMemoryResultsBudgetBytes)
	// freshness (Gate 1) stays as a documented deprecated alias for one
	// release alongside the new typed `health` (C1/C4, Open Q1) — health.index
	// is what freshness never had: a dirty/failed INDEX distinct from a stale
	// SOURCE.
	//
	// #241: when a source filter is active, health is scoped the SAME way
	// confidence's missing_sources is below — a source the caller explicitly
	// excluded must not drag the always-present health banner into
	// degraded/unhealthy (compactHealthFiltered, search_filters.go). Reuses
	// the SAME `now` captured once above (briefClock()) rather than a fresh
	// time.Now() — one consistent instant for every filter-related signal
	// this call computes.
	health := compactHealthOf(cfg, now)
	if filters.Source != "" {
		health = compactHealthFiltered(cfg, now, filters)
	}
	out := map[string]any{"results": budgeted, "freshness": sourceFreshness(cfg), "health": health}
	if dropped > 0 {
		out["results_truncated"] = dropped
	}
	// Packet H5: surface any subscription that is degraded/failed/never so an
	// unhealthy share is visible (never silently dropped). Additive key on the
	// same envelope PR 2 introduces — neither reshapes the other's field.
	if su := sharesUnhealthy(cfg, time.Now()); len(su) > 0 {
		out["shares_unhealthy"] = su
	}
	// Issue #238: opt-in confidence envelope, per-call boolean arg mirroring
	// digest/brief's `envelope` gate. OFF (default/omitted/false) leaves the
	// payload byte-identical to today's shape (confidence_contract_test.go's
	// TestConfidenceSearchMemoryKnobOffByteIdentical). Scoped over `budgeted`
	// — the actual RETURNED set — per the frozen contract.
	if boolArg(args, "confidence", false) {
		conf := searchConfidence(ctx, cfg, budgeted, sr.ScoreFused, sr.Local, sr.Trace, query, now)
		// #241/#238 interaction: a source excluded by an active source filter
		// is a caller choice, not a coverage gap — recompute missing_sources/
		// health_impact over the filter-narrowed population (confidence.go's
		// confidenceSourceGaps/searchConfidence are UNTOUCHED; this overwrites
		// the two fields after the fact, search_filters.go's filteredMissingSources).
		// SAME captured `now` as above.
		if filters.Source != "" {
			conf.MissingSources, conf.HealthImpact = filteredMissingSources(cfg, now, filters)
		}
		out["confidence"] = conf
	}
	// #241: the "filters" receipt appears ONLY when at least one filter was
	// actually supplied — omitted params stay byte-identical to pre-#241
	// output (filters_contract_test.go's ByteIdenticalWhenOmitted pins).
	if r := filters.receipt(); r != nil {
		out["filters"] = r
	}
	// #241 acceptance: "Health/confidence output distinguishes
	// excluded_by_filter from unavailable/unhealthy sources" — an explicit
	// top-level marker, present only when the source filter actually excludes
	// an enabled source (search_filters.go's excludedByFilterSources).
	if filters.Source != "" {
		if excl := excludedByFilterSources(cfg, now, filters); len(excl) > 0 {
			out["excluded_by_filter"] = excl
		}
	}
	recordMCPPhases(ctx, retrieval, time.Since(assemblyStarted))
	return out, nil
}

func mcpListMemory(ctx context.Context, cfg Config, args map[string]any) (any, error) {
	start := time.Now()
	retrievalStarted := time.Now()
	res, err := listMemories(cfg, strArg(args, "scope", ""), intArg(args, "limit", 10))
	retrieval := time.Since(retrievalStarted)
	recordMCPUsage(ctx, cfg, usageEvent{Tool: "list_memory", Scope: strArg(args, "scope", ""), Results: len(res), Millis: time.Since(start).Milliseconds()})
	if err != nil {
		recordMCPPhases(ctx, retrieval, 0)
		return nil, err
	}
	assemblyStarted := time.Now()
	// decorateBrowseRecency runs BEFORE snippetMemories, which drops Meta — the
	// source of event_start/source_created_at (#218).
	budgeted, dropped := budgetSearchResults(snippetMemories(decorateBrowseRecency(res), ""), searchMemoryResultsBudgetBytes)
	out := map[string]any{"memories": budgeted, "health": compactHealthOf(cfg, time.Now())}
	if dropped > 0 {
		out["memories_truncated"] = dropped
	}
	recordMCPPhases(ctx, retrieval, time.Since(assemblyStarted))
	return out, nil
}

func mcpContextMemory(ctx context.Context, cfg Config, args map[string]any) (any, error) {
	start := time.Now()
	// #241: same briefClock() seam as mcpSearchMemory — see its comment.
	now := briefClock()
	scope := strArg(args, "scope", "")
	query := strArg(args, "query", "")
	tokenBudget, charBudget := resolveContextBudgetTokens(cfg, intArg(args, "max_tokens", 0))
	filters, ferr := parseSearchFilters(args, now)
	if ferr != nil {
		return nil, ferr
	}
	retrievalStarted := time.Now()
	var items []Memory
	var err error
	if query != "" {
		items, err = hybridSearch(ctx, cfg, query, scope, 10, filters)
	} else {
		items, err = listMemories(cfg, scope, 10, filters)
	}
	retrieval := time.Since(retrievalStarted)
	if err != nil {
		recordMCPPhases(ctx, retrieval, 0)
		return nil, err
	}
	assemblyStarted := time.Now()
	text := buildContext(cfg, items, charBudget, query != "")
	recordMCPUsage(ctx, cfg, usageEvent{Tool: "context_memory", Query: query, Scope: scope, Results: len(items), Millis: time.Since(start).Milliseconds()})
	used := estimateTokensUsed(len(text))
	// #241: health scoped the same way as mcpSearchMemory's — an excluded
	// source must not drag the always-present health banner down. Reuses the
	// SAME `now` captured once above (briefClock()), not a fresh time.Now().
	health := compactHealthOf(cfg, now)
	if filters.Source != "" {
		health = compactHealthFiltered(cfg, now, filters)
	}
	out := map[string]any{
		"context":     text,
		"freshness":   sourceFreshness(cfg),
		"budget_unit": budgetUnitTokens,
		"budget":      tokenBudget,
		"used":        used,
		"health":      health,
	}
	if r := filters.receipt(); r != nil {
		out["filters"] = r
	}
	// #241 acceptance: explicit excluded_by_filter marker — see
	// mcpSearchMemory's identical block for the full rationale.
	if filters.Source != "" {
		if excl := excludedByFilterSources(cfg, now, filters); len(excl) > 0 {
			out["excluded_by_filter"] = excl
		}
	}
	// Issue #237, round-3 amendment: an OPTIONAL top-level "corroborating" array,
	// a sibling of "context" — the SAME compact four-key ref shape search_memory
	// and think use, collecting every cluster head's folded members across
	// `items`. Present ONLY when clustering actually folded a member (not an
	// always-there-possibly-empty key), so a zero-cluster query stays byte-
	// identical to pre-#237 output — no "corroborating" key anywhere.
	var corroborating []CorroboratingRef
	for _, m := range items {
		corroborating = append(corroborating, m.Corroborating...)
	}
	if len(corroborating) > 0 {
		out["corroborating"] = corroborating
	}
	recordMCPPhases(ctx, retrieval, time.Since(assemblyStarted))
	return out, nil
}

func mcpThink(ctx context.Context, cfg Config, args map[string]any) (any, error) {
	start := time.Now()
	query := strArg(args, "query", "")
	res, err := buildThink(ctx, cfg, query, strArg(args, "scope", ""), intArg(args, "limit", 8), time.Now())
	recordMCPUsage(ctx, cfg, usageEvent{Tool: "think", Query: query, Scope: strArg(args, "scope", ""), Results: len(res.Evidence), Millis: time.Since(start).Milliseconds()})
	out := map[string]any{"think": res, "health": compactHealthOf(cfg, time.Now())}
	// Issue #238: opt-in confidence envelope (see mcpSearchMemory's identical
	// gate). Skipped on error so a failed buildThink never fabricates a
	// confidence reading over an incomplete/zero-value result.
	if err == nil && boolArg(args, "confidence", false) {
		out["confidence"] = thinkConfidence(res, cfg, time.Now())
	}
	return out, err
}

func mcpListEntities(ctx context.Context, cfg Config, args map[string]any) (any, error) {
	start := time.Now()
	ents, err := entitiesForMCP(ctx, cfg, strArg(args, "kind", ""), intArg(args, "limit", mcpListEntitiesDefaultLimit))
	recordMCPUsage(ctx, cfg, usageEvent{Tool: "list_entities", Results: len(ents), Millis: time.Since(start).Milliseconds()})
	return map[string]any{"entities": ents, "health": compactHealthOf(cfg, time.Now())}, err
}

func mcpGetEntity(ctx context.Context, cfg Config, args map[string]any) (any, error) {
	start := time.Now()
	res, err := entityDossierForMCP(ctx, cfg, strArg(args, "name", ""), intArg(args, "max_tokens", 0))
	recordMCPUsage(ctx, cfg, usageEvent{Tool: "get_entity", Query: strArg(args, "name", ""), Millis: time.Since(start).Milliseconds()})
	return map[string]any{"entity": res, "health": compactHealthOf(cfg, time.Now())}, err
}

func mcpDigest(ctx context.Context, cfg Config, args map[string]any) (any, error) {
	start := time.Now()
	// MCP digest is preview by construction (no advance arg exists — D-02). An
	// explicit since_hours selects the plain-window path (SC#2); 0 => DELTA mode.
	//
	// D-05 knob-alive: we surface a GENEROUS per-source cap (mcpDigestMaxItems)
	// so the byte BUDGET — not the human-brief cap (digestDefaultCap=8) — is the
	// real limiter. That is what makes max_tokens actually scale the content: a
	// 20k request can surface more items than the 6k default. The MCP path is
	// always preview (advance=false), so raising the cap has no watermark effect
	// (no snapshot is written, no items are marked-read).
	opts := briefOpts{
		sinceHours:   intArg(args, "since_hours", 0),
		perSourceCap: mcpDigestMaxItems,
		source:       strArg(args, "source", ""),
		scope:        strArg(args, "scope", ""),
		sinceDays:    clampSinceDays(intArg(args, "since_days", 0)),
	}
	if name := strArg(args, "entity", ""); name != "" {
		idSet, rerr := resolveEntityFilter(ctx, cfg, name)
		if rerr != nil {
			return nil, rerr
		}
		opts.entityIDSet = idSet
	}
	// Read the clock through the briefClock seam (defaults to time.Now in
	// production) so the digest tool is clock-pinnable in tests, like the brief
	// tool — keeps the SC#4 byte-identical gate deterministic instead of
	// straddling a one-second boundary between two generations.
	d, derr := buildDigest(cfg, briefClock(), opts)
	if derr != nil {
		return nil, derr
	}
	// Ship ONE budgeted payload — the typed-delta sections + the derived
	// source_states, scaled by max_tokens (default ~6k, max 20k). NO `digest`
	// render string beside the sections (that doubling — clipped render PLUS the
	// full unclipped sections — is the bug we fix here). The CLI keeps the render
	// path (renderDigest); the agent reads the structured payload directly.
	budgetChars := mcpDigestBudgetChars(cfg, intArg(args, "max_tokens", 0))
	recordMCPUsage(ctx, cfg, usageEvent{Tool: "digest", Results: len(d.Sections), Millis: time.Since(start).Milliseconds()})
	// Opt-in envelope (15-02, D15-3): when `envelope` is true, return the
	// DigestEnvelope — the SAME budgeted base payload PLUS a synthesis_prompt
	// built from those budgeted sections (model-free: Mora attaches a STRING the
	// agent runs, NO sampling/model call — SC#2). When false/absent (the safe
	// default), return the EXACT digestMCPPayload map as today: byte-identical,
	// no synthesis_prompt key, so the T0 gate + the plain digest tests are
	// unregressed (T-15-04).
	if boolArg(args, "envelope", false) {
		return budgetEnvelopePayload(cfg, d, budgetChars), nil
	}
	return digestMCPPayload(cfg, d, budgetChars), nil
}

func mcpBrief(ctx context.Context, cfg Config, args map[string]any) (any, error) {
	// The single tool call an MCP agent makes at session start (D16-3/SC#2). It
	// returns the SAME ONE budgeted payload as `digest`, resolved to the freshest
	// brief: briefDigest builds the DELTA preview and falls back to the fixed 24h
	// window when the delta is empty (the resolveBrief generate semantics) so an
	// agent ALWAYS gets context. LOCAL + read-only by construction — briefDigest
	// forces advance:false on every build, so the tool NEVER syncs and NEVER
	// advances the Phase-12 watermark (D16-2/SC#4, zero egress). The verbatim-file
	// path is the human CLI's; the agent reads the STRUCTURED, budgeted projection
	// (like the digest tool), not a render string.
	start := time.Now()
	var err error
	// A filtered brief uses the filter-aware factory (same delta→24h-window
	// fallback as the human resolveBrief); the unfiltered default stays briefDigest
	// so the payload is byte-identical (T0 gate + plain-brief tests unregressed).
	bopts := briefOpts{
		perSourceCap: mcpDigestMaxItems,
		scope:        strArg(args, "scope", ""),
		sinceDays:    clampSinceDays(intArg(args, "since_days", 0)),
	}
	if name := strArg(args, "entity", ""); name != "" {
		idSet, rerr := resolveEntityFilter(ctx, cfg, name)
		if rerr != nil {
			return nil, rerr
		}
		bopts.entityIDSet = idSet
	}
	var d Digest
	if bopts.filtered() {
		d, err = filteredBriefDigest(cfg, briefClock(), bopts)
	} else {
		d, err = briefDigest(cfg, briefClock(), mcpDigestMaxItems)
	}
	if err != nil {
		return nil, err
	}
	budgetChars := mcpDigestBudgetChars(cfg, intArg(args, "max_tokens", 0))
	recordMCPUsage(ctx, cfg, usageEvent{Tool: "brief", Results: len(d.Sections), Millis: time.Since(start).Milliseconds()})
	// Reuse the Phase-15 budget machinery VERBATIM (additive — the digest case and
	// its helpers are untouched): envelope-gated synthesis_prompt, max_tokens
	// budget, T0-safe by construction (16-03 adds the gate row). Model-free: the
	// synthesis_prompt is a STRING the agent runs with its own model — no sampling.
	if boolArg(args, "envelope", false) {
		return budgetEnvelopePayload(cfg, d, budgetChars), nil
	}
	return digestMCPPayload(cfg, d, budgetChars), nil
}

func mcpMeetingPrep(ctx context.Context, cfg Config, args map[string]any) (any, error) {
	start := time.Now()
	eventID := strArg(args, "event_id", "")
	name := strArg(args, "name", "")
	if eventID != "" && name != "" {
		return nil, errors.New("meeting_prep event_id cannot be combined with name")
	}
	at := prepClock()
	if raw := strArg(args, "at", ""); raw != "" {
		parsed, perr := time.Parse(time.RFC3339, raw)
		if perr != nil {
			return nil, fmt.Errorf("invalid at %q (want RFC3339): %w", raw, perr)
		}
		at = parsed
	}
	var filter map[string]bool
	if name != "" {
		idSet, rerr := resolveEntityFilter(ctx, cfg, name)
		if rerr != nil {
			return nil, rerr
		}
		filter = idSet
	}
	var brief MeetingBrief
	var err error
	if eventID != "" {
		brief, err = buildEventMeetingBrief(
			ctx, cfg, eventID, at,
			intArg(args, "max_tokens", 0),
			intArg(args, "limit", meetingBriefDefaultPerGuest),
		)
	} else {
		brief, err = buildNextMeetingBrief(
			ctx, cfg, at, filter,
			intArg(args, "max_tokens", 0),
			intArg(args, "limit", meetingBriefDefaultPerGuest),
		)
	}
	if err != nil {
		return nil, humanizeIndexBusy(err)
	}
	if verr := brief.validate(); verr != nil {
		return nil, fmt.Errorf("refusing uncited meeting_prep payload: %w", verr)
	}
	recordMCPUsage(ctx, cfg, usageEvent{Tool: "meeting_prep", Results: meetingBriefLineCount(brief), Millis: time.Since(start).Milliseconds()})
	return brief, nil
}

func mcpDeleteMemory(ctx context.Context, cfg Config, args map[string]any) (any, error) {
	id := strArg(args, "id", "")
	m, err := findMemory(cfg, id)
	if err != nil {
		return nil, err
	}
	// Mark the delete BEFORE removing the file (A5 row 5). A rebuild that fails
	// after the file is gone would keep serving the deleted content; the pending
	// op both reddens the index and suppresses the id on every read path (B4),
	// so serving-deleted-content is impossible even while the rebuild is broken.
	op, merr := markIndexDirty(ctx, cfg, pendingOp{Kind: opKindDelete, Path: m.Path, MemoryID: m.ID})
	if merr != nil {
		return nil, merr
	}
	if err := os.Remove(m.Path); err != nil {
		_ = unmarkIndexDirty(cfg, op.OpID)
		return nil, err
	}
	// A failed rebuild after a delete is worse than after a write: search
	// keeps SERVING the deleted content as if it still existed. Delete is the
	// privacy path, so it rebuilds in Allow mode (mirrors cmdDelete): a
	// last-memory delete (newCount==0) must still drop the deleted row from the
	// index — the Enforce decBlockEmpty guard would roll it back and keep
	// serving it. Allow is safe here because no write is being committed over a
	// populated vault; the destructive intent is the user's own delete.
	if _, rerr := rebuildIndexWithPolicy(ctx, cfg, policyAllow); rerr != nil {
		return nil, fmt.Errorf("memory %s deleted, but the search index could not be updated and may still serve it: %w — run `mora index rebuild`", id, rerr)
	}
	return map[string]any{"deleted": id, "health": compactHealthOf(cfg, time.Now())}, nil
}

// mcpParam describes one tool argument for the JSON Schema published in
// tools/list. Agents (Codex, Claude Code) read this to learn exactly what to
// pass — without it the tools sit unused (the pilot's "commands aren't useful
// directly" report).
type mcpParam struct {
	Name     string
	Type     string // JSON Schema type: "string" | "integer"
	Desc     string
	Required bool
}

// mcpTool builds a tools/list entry with a precise inputSchema. additionalProperties
// is false so strict clients (Codex) know the arg set is closed; tools with no
// params still publish an explicit empty object schema rather than the old
// catch-all that gave agents zero guidance.
func mcpTool(name, desc string, params ...mcpParam) map[string]any {
	properties := map[string]any{}
	var required []string
	for _, p := range params {
		properties[p.Name] = map[string]any{"type": p.Type, "description": p.Desc}
		if p.Required {
			required = append(required, p.Name)
		}
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return map[string]any{"name": name, "description": desc, "inputSchema": schema}
}
func strArg(args map[string]any, key, def string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return def
}
func intArg(args map[string]any, key string, def int) int {
	if v, ok := args[key].(float64); ok {
		return int(v)
	}
	return def
}

// boolArg reads an MCP tool arg as a bool. MCP arguments arrive as map[string]any
// from json.Unmarshal, so we accept a native JSON bool directly and ALSO a
// "true"/"false" string from a lenient client; anything else (absent, a number, a
// malformed string) falls back to def. This mirrors strArg/intArg's defensive
// type-switch — an untrusted/absent value never crashes and never silently flips
// the safe default (the opt-in envelope arg must default OFF, 15-02 T-15-04).
func boolArg(args map[string]any, key string, def bool) bool {
	switch v := args[key].(type) {
	case bool:
		return v
	case string:
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
