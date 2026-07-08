package mora

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

func cmdMCP(ctx context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) error {
	if len(args) != 1 || args[0] != "serve" {
		return errors.New("usage: mora mcp serve")
	}
	return serveMCP(ctx, stdout, stdin)
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

// resolveContextBudget converts a requested token budget (the context_memory
// max_tokens arg) into a character budget for buildContext. A non-positive
// request falls back to the profile default (contextDefaultTokens); an
// over-ceiling request is clamped to maxContextTokens. The token count is
// clamped BEFORE the *charsPerToken conversion so an arbitrarily large
// max_tokens cannot overflow.
func resolveContextBudget(cfg Config, maxTokens int) int {
	if maxTokens <= 0 {
		maxTokens = cfg.contextDefaultTokens()
	}
	if ceiling := cfg.contextMaxTokens(); maxTokens > ceiling {
		maxTokens = ceiling
	}
	return maxTokens * charsPerToken
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
		resp.Result = map[string]any{"protocolVersion": "2024-11-05", "serverInfo": map[string]string{"name": "mora", "version": BuildVersion}, "capabilities": map[string]any{"tools": map[string]any{}}, "instructions": mcpInstructions}
	case "tools/list":
		resp.Result = map[string]any{"tools": []map[string]any{
			mcpTool("write_memory", "Write a durable memory to the vault",
				mcpParam{"title", "string", "Short human-readable title for the memory", true},
				mcpParam{"text", "string", "The memory body (Markdown allowed)", true},
				mcpParam{"scope", "string", `Scope/namespace, e.g. "global" or "project:acme" (default "global")`, false},
				mcpParam{"type", "string", `Memory type: insight|fact|decision|task (default "insight")`, false},
				mcpParam{"source", "string", `Origin label (default "mcp")`, false},
			),
			mcpTool("read_memory", "Read a single memory by its id",
				mcpParam{"id", "string", "The memory id (as returned by search_memory/list_memory)", true},
			),
			mcpTool("search_memory", "Search the vault for the most relevant memories (hybrid semantic+keyword when Ollama embeddings are enabled, full-text otherwise)",
				mcpParam{"query", "string", "Search query (words are OR-matched against the index)", true},
				mcpParam{"scope", "string", `Optional scope filter, e.g. "project:acme"`, false},
				mcpParam{"limit", "integer", "Max results to return (default 8)", false},
			),
			mcpTool("list_memory", "List the most recent memories, newest first",
				mcpParam{"scope", "string", "Optional scope filter", false},
				mcpParam{"limit", "integer", "Max memories to return (default 10)", false},
			),
			mcpTool("delete_memory", "Delete a memory by its id",
				mcpParam{"id", "string", "The memory id to delete", true},
			),
			mcpTool("context_memory", "Assemble one dense, budget-bounded context block for a query (or a session-start briefing when no query is given)",
				mcpParam{"query", "string", "Topic to assemble context for; omit for a recency briefing", false},
				mcpParam{"scope", "string", "Optional scope filter", false},
				mcpParam{"max_tokens", "integer", "Approximate token budget for the response (default ~6000, max ~20000)", false},
			),
			mcpTool("think", "Synthesis envelope for a question: cited evidence + a deterministic 'what the vault does NOT know' gap analysis + a prompt to compose a cited answer",
				mcpParam{"query", "string", "The question to synthesize an answer for", true},
				mcpParam{"scope", "string", "Optional scope filter", false},
				mcpParam{"limit", "integer", "Max evidence memories to gather (default 8)", false},
			),
			mcpTool("list_entities", "List the entities (people, scopes, tags, [[links]], categories) referenced across memory, with counts, ranked by salience",
				mcpParam{"kind", "string", `Optional kind filter: "person", "service", "scope", "tag", "link", or "category"`, false},
				mcpParam{"limit", "integer", "Max entities to return, ranked by salience (default 200)", false}),
			mcpTool("get_entity", "Get the memories that reference a named entity",
				mcpParam{"name", "string", "The entity name (person, tag, scope, or [[link]]) to fetch", true},
			),
			mcpTool("digest", "Assemble a daily cross-source digest (recent emails, texts, calendar items, and stale open tasks), grouped by source, cited, and budget-bounded; opt into `envelope` to also get a synthesis_prompt for composing a grounded, cited brief",
				mcpParam{"since_hours", "integer", "Look-back window in hours (default 24)", false},
				mcpParam{"source", "string", `Filter to one connector: "imessage", "gmail", "calendar", "applecalendar", or an account instance like "gmail:work" ("gmail" spans all gmail accounts). Use with since_hours for asks like "my texts from the past week" — without it, earlier-ranked sources can consume the byte budget`, false},
				mcpParam{"max_tokens", "integer", "Approximate token budget for the digest (default ~6000, max ~20000)", false},
				mcpParam{"envelope", "boolean", "Opt-in: also return a synthesis_prompt instructing the agent to write a grounded, cited brief over the digest items (default false; Mora makes no model call)", false},
				mcpParam{"entity", "string", `Filter to memories referencing one person (display name or email/handle, e.g. "Riya" or "riya@example.com"). A no-match or ambiguous name returns an error rather than an empty digest. Preview-only.`, false},
				mcpParam{"scope", "string", `Filter to one memory scope/namespace, e.g. "project:acme". Preview-only.`, false},
				mcpParam{"since_days", "integer", "Additional look-back: only memories created in the last N days (negative is treated as no filter). Preview-only.", false},
			),
			mcpTool("brief", "Return the latest what-changed/what-matters brief for session start — the same budgeted, cited, source-grouped daily brief as `digest`, resolved to the freshest available; call this FIRST at the start of a session. Opt into `envelope` for a synthesis_prompt to compose a grounded, cited brief.",
				mcpParam{"max_tokens", "integer", "Approximate token budget for the brief (default ~6000, max ~20000)", false},
				mcpParam{"envelope", "boolean", "Opt-in: also return a synthesis_prompt for composing a grounded, cited brief over the items (default false; Mora makes no model call)", false},
				mcpParam{"entity", "string", `Filter the brief to memories referencing one person (display name or email/handle). A no-match or ambiguous name returns an error. Preview-only.`, false},
				mcpParam{"scope", "string", `Filter the brief to one memory scope/namespace, e.g. "project:acme". Preview-only.`, false},
				mcpParam{"since_days", "integer", "Additional look-back: only memories created in the last N days (negative = no filter). Preview-only.", false},
			),
			mcpTool("meeting_prep", "Assemble a CITED prep pack for the user's next (or in-progress) calendar event, optionally with one attendee by name: the event, recent emails/texts/events with each attendee, a deterministic 'what the vault does NOT know' gap analysis, and a model-free synthesis_prompt to compose the prep. Local + read-only; never advances the watermark; Mora makes NO model call and never invents decisions or open questions.",
				mcpParam{"name", "string", `Optional attendee name/email/handle: prep the next meeting WITH this person (falls back to the next meeting if they have none). Omit for the next meeting on the calendar.`, false},
				mcpParam{"limit", "integer", "Max evidence memories per attendee (default 8)", false},
				mcpParam{"max_tokens", "integer", "Approximate token budget for the pack (default ~6000, max ~20000)", false},
			),
		}}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		resp.Result = toCallToolResult(callMCPTool(ctx, p.Name, p.Arguments))
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
func callMCPTool(ctx context.Context, name string, args map[string]any) (any, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	switch name {
	case "write_memory":
		m := Memory{Scope: strArg(args, "scope", "global"), Type: strArg(args, "type", "insight"), Title: strArg(args, "title", ""), Text: strArg(args, "text", ""), Source: strArg(args, "source", "mcp"), CreatedAt: time.Now().Format(time.RFC3339)}
		if m.Title == "" || m.Text == "" {
			return nil, errors.New("title and text required")
		}
		// Create-exclusive publish: concurrent write_memory calls that mint the
		// same id never clobber each other (os.Link fails EEXIST → re-mint). This
		// is the server's most concurrent write path — N agents writing at once.
		// createMemory sets m.ID and m.Path.
		var err error
		if m, err = createMemory(cfg, m); err != nil {
			return nil, err
		}
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
		if rerr := indexUpsert(ctx, cfg, m); rerr != nil {
			return map[string]any{
				"memory":      m,
				"index_stale": true,
				"warning":     fmt.Sprintf("memory %s saved, but the search index could not be updated: %v — run `mora index rebuild` (do NOT retry the write; it is saved)", m.ID, rerr),
			}, nil
		}
		return m, nil
	case "read_memory":
		m, err := findMemory(cfg, strArg(args, "id", ""))
		if err != nil {
			// Same read-only shared-corpus fallback as `mora read`: search
			// returns shared ids with 240-rune snippets, so read_memory is the
			// documented expansion path for them too.
			if sm, ok := findSharedMemory(cfg, strArg(args, "id", "")); ok {
				return sm, nil
			}
			return nil, err
		}
		return m, nil
	case "search_memory":
		start := time.Now()
		query := strArg(args, "query", "")
		res, err := defaultSearch(ctx, cfg, query, strArg(args, "scope", ""), intArg(args, "limit", mcpSearchDefaultLimit))
		logUsage(cfg, usageEvent{Tool: "search_memory", Query: query, Scope: strArg(args, "scope", ""), Results: len(res), Millis: time.Since(start).Milliseconds()})
		if err != nil {
			return nil, err
		}
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
		out := map[string]any{"results": budgeted, "freshness": sourceFreshness(cfg)}
		if dropped > 0 {
			out["results_truncated"] = dropped
		}
		return out, nil
	case "list_memory":
		start := time.Now()
		res, err := listMemories(cfg, strArg(args, "scope", ""), intArg(args, "limit", 10))
		logUsage(cfg, usageEvent{Tool: "list_memory", Scope: strArg(args, "scope", ""), Results: len(res), Millis: time.Since(start).Milliseconds()})
		return res, err
	case "context_memory":
		start := time.Now()
		scope := strArg(args, "scope", "")
		query := strArg(args, "query", "")
		budget := resolveContextBudget(cfg, intArg(args, "max_tokens", 0))
		var items []Memory
		if query != "" {
			items, err = hybridSearch(ctx, cfg, query, scope, 10)
		} else {
			items, err = listMemories(cfg, scope, 10)
		}
		if err != nil {
			return nil, err
		}
		text := buildContext(cfg, items, budget, query != "")
		logUsage(cfg, usageEvent{Tool: "context_memory", Query: query, Scope: scope, Results: len(items), Millis: time.Since(start).Milliseconds()})
		return map[string]any{"context": text, "freshness": sourceFreshness(cfg)}, nil
	case "think":
		start := time.Now()
		query := strArg(args, "query", "")
		res, err := buildThink(ctx, cfg, query, strArg(args, "scope", ""), intArg(args, "limit", 8), time.Now())
		logUsage(cfg, usageEvent{Tool: "think", Query: query, Scope: strArg(args, "scope", ""), Results: len(res.Evidence), Millis: time.Since(start).Milliseconds()})
		return res, err
	case "list_entities":
		start := time.Now()
		ents, err := entitiesForMCP(ctx, cfg, strArg(args, "kind", ""), intArg(args, "limit", 200))
		logUsage(cfg, usageEvent{Tool: "list_entities", Results: len(ents), Millis: time.Since(start).Milliseconds()})
		return ents, err
	case "get_entity":
		start := time.Now()
		res, err := entityMemoriesForMCP(ctx, cfg, strArg(args, "name", ""))
		logUsage(cfg, usageEvent{Tool: "get_entity", Query: strArg(args, "name", ""), Millis: time.Since(start).Milliseconds()})
		return res, err
	case "digest":
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
		logUsage(cfg, usageEvent{Tool: "digest", Results: len(d.Sections), Millis: time.Since(start).Milliseconds()})
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
	case "brief":
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
		var derr error
		if bopts.filtered() {
			d, derr = filteredBriefDigest(cfg, briefClock(), bopts)
		} else {
			d, derr = briefDigest(cfg, briefClock(), mcpDigestMaxItems)
		}
		if derr != nil {
			return nil, derr
		}
		budgetChars := mcpDigestBudgetChars(cfg, intArg(args, "max_tokens", 0))
		logUsage(cfg, usageEvent{Tool: "brief", Results: len(d.Sections), Millis: time.Since(start).Milliseconds()})
		// Reuse the Phase-15 budget machinery VERBATIM (additive — the digest case and
		// its helpers are untouched): envelope-gated synthesis_prompt, max_tokens
		// budget, T0-safe by construction (16-03 adds the gate row). Model-free: the
		// synthesis_prompt is a STRING the agent runs with its own model — no sampling.
		if boolArg(args, "envelope", false) {
			return budgetEnvelopePayload(cfg, d, budgetChars), nil
		}
		return digestMCPPayload(cfg, d, budgetChars), nil
	case "meeting_prep":
		start := time.Now()
		name := strArg(args, "name", "")
		var filter map[string]bool
		if name != "" {
			idSet, rerr := resolveEntityFilter(ctx, cfg, name)
			if rerr != nil {
				return nil, rerr
			}
			filter = idSet
		}
		mp, err := buildMeetingPrep(ctx, cfg, prepClock(), name, filter, intArg(args, "limit", mcpSearchDefaultLimit))
		if err != nil {
			return nil, humanizeIndexBusy(err)
		}
		budgetChars := mcpDigestBudgetChars(cfg, intArg(args, "max_tokens", 0))
		logUsage(cfg, usageEvent{Tool: "meeting_prep", Results: len(mp.Evidence), Millis: time.Since(start).Milliseconds()})
		return meetingPrepMCPPayload(mp, budgetChars), nil
	case "delete_memory":
		id := strArg(args, "id", "")
		m, err := findMemory(cfg, id)
		if err != nil {
			return nil, err
		}
		if err := os.Remove(m.Path); err != nil {
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
		return map[string]any{"deleted": id}, nil
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
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
