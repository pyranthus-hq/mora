package mcp

// ToolDefinition is public tool metadata independent of application handlers.
type ToolDefinition struct {
	Name        string
	Description string
	Params      []Param
}

var toolCatalog = []ToolDefinition{
	{
		Name: "write_memory", Description: "Write a durable memory to the vault",
		Params: []Param{
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
	},
	{
		Name: "read_memory", Description: "Read a single memory by its id",
		Params: []Param{
			{"id", "string", "The memory id (as returned by search_memory/list_memory)", true},
			{"match", "string", "Optional literal phrase to center a bounded excerpt on (omit for the full body)", false},
			{"max_tokens", "integer", "Optional excerpt budget in tokens for bounded reads (default ~800)", false},
			{"occurrence", "integer", "Optional 1-indexed match occurrence to center the excerpt on (default 1)", false},
			{"evidence_ref", "string", "Optional Gmail or iMessage evidence ref (from search_memory's evidence.evidence_ref) to read ONLY that message's derived segment, bounded, with a receipt naming its sender/time and iMessage direction; a ref that does not belong to this memory id is rejected", false},
		},
	},
	{
		Name: "search_memory", Description: "Search the vault for the most relevant memories (hybrid semantic+keyword when Ollama embeddings are enabled, full-text otherwise)",
		Params: []Param{
			{"query", "string", "Search query (words are OR-matched against the index)", true},
			{"scope", "string", `Optional scope filter, e.g. "project:acme"`, false},
			{"limit", "integer", "Max results to return (default 8)", false},
			{"confidence", "boolean", "Opt-in: return confidence with ranking scores, direct answer coverage, freshness, and missing/unhealthy sources (default false)", false},
			{"source", "string", `Filter to one connector: "imessage", "gmail", "calendar", "applecalendar", "github", or an account instance like "gmail:work" ("gmail" spans all gmail accounts). Applied BEFORE ranking in every retrieval arm. An unrecognized value is a tool error.`, false},
			{"since_hours", "integer", "Only memories created in the last N hours (must be a positive integer). Applied BEFORE ranking in every retrieval arm.", false},
		},
	},
	{
		Name: "calendar_events", Description: "List calendar events whose start falls in an exact date range. Use this for date, day, and week questions instead of keyword search.",
		Params: []Param{
			{"start", "string", "Required inclusive boundary: YYYY-MM-DD or RFC3339", true},
			{"end", "string", "Required exclusive boundary: YYYY-MM-DD or RFC3339", true},
			{"timezone", "string", "Optional IANA timezone for date-only boundaries (default local timezone)", false},
			{"source", "string", `Optional calendar source: "calendar" or "applecalendar"`, false},
			{"limit", "integer", "Max events to return (default 50, max 200)", false},
		},
	},
	{
		Name: "list_memory", Description: "Browse the memories Mora wrote most recently, newest first. Ordered by `indexed_at` (when Mora recorded the memory), never by event time, so a future calendar event cannot lead the list. Each row splits the timestamps `created_at` conflated: `event_start` (when a calendar event happens), `source_created_at` (when the source object was created at its provider), and `indexed_at`; a field Mora cannot derive honestly is omitted rather than filled in",
		Params: []Param{
			{"scope", "string", "Optional scope filter", false},
			{"limit", "integer", "Max memories to return (default 10)", false},
		},
	},
	{
		Name: "delete_memory", Description: "Delete a memory by its id",
		Params: []Param{{"id", "string", "The memory id to delete", true}},
	},
	{
		Name: "context_memory", Description: "Assemble one dense, budget-bounded context block for a query (or a session-start briefing when no query is given)",
		Params: []Param{
			{"query", "string", "Topic to assemble context for; omit for a recency briefing", false},
			{"scope", "string", "Optional scope filter", false},
			{"max_tokens", "integer", "Approximate token budget for the response (default ~6000, max ~20000)", false},
			{"source", "string", `Filter to one connector: "imessage", "gmail", "calendar", "applecalendar", "github", or an account instance like "gmail:work" ("gmail" spans all gmail accounts). Applied BEFORE ranking in every retrieval arm, including the no-query recency fallback. An unrecognized value is a tool error.`, false},
			{"since_hours", "integer", "Only memories created in the last N hours (must be a positive integer). Applied BEFORE ranking in every retrieval arm, including the no-query recency fallback.", false},
		},
	},
	{
		Name: "think", Description: "Synthesis envelope for a question: cited evidence + a deterministic 'what the vault does NOT know' gap analysis + a prompt to compose a cited answer",
		Params: []Param{
			{"query", "string", "The question to synthesize an answer for", true},
			{"scope", "string", "Optional scope filter", false},
			{"limit", "integer", "Max evidence memories to gather (default 8)", false},
			{"confidence", "boolean", "Opt-in: return confidence with ranking scores, direct answer coverage, freshness, and missing/unhealthy sources (default false)", false},
		},
	},
	{
		Name: "list_entities", Description: "List the entities (people, scopes, tags, [[links]], categories) referenced across memory, with counts, ranked by salience",
		Params: []Param{
			{"kind", "string", `Optional kind filter: "person", "service", "scope", "tag", "link", or "category"`, false},
			{"limit", "integer", "Max entities to return, ranked by salience (default 150)", false},
		},
	},
	{
		Name: "get_entity", Description: "Get a budget-bounded, fully-cited dossier for a named entity (merged identities, typed neighbors, top evidence by salience)",
		Params: []Param{
			{"name", "string", "The entity name (person, tag, scope, or [[link]]) to fetch", true},
			{"max_tokens", "integer", "Approximate token budget for the dossier (default ~6000, max ~20000)", false},
		},
	},
	{
		Name: "digest", Description: "Assemble a daily cross-source digest (recent emails, texts, calendar items, and stale open tasks), grouped by source, cited, and budget-bounded; opt into `envelope` to also get a synthesis_prompt for composing a grounded, cited brief",
		Params: []Param{
			{"since_hours", "integer", "Look-back window in hours (default 24)", false},
			{"source", "string", `Filter to one connector: "imessage", "gmail", "calendar", "applecalendar", "github", or an account instance like "gmail:work" ("gmail" spans all gmail accounts). Use with since_hours for asks like "my texts from the past week" — without it, earlier-ranked sources can consume the byte budget`, false},
			{"max_tokens", "integer", "Approximate token budget for the digest (default ~6000, max ~20000)", false},
			{"envelope", "boolean", "Opt-in: also return a synthesis_prompt instructing the agent to write a grounded, cited brief over the digest items (default false; Mora makes no model call)", false},
			{"entity", "string", `Filter to memories referencing one person (display name or email/handle, e.g. "Riya" or "riya@example.com"). A no-match or ambiguous name returns an error rather than an empty digest. Preview-only.`, false},
			{"scope", "string", `Filter to one memory scope/namespace, e.g. "project:acme". Preview-only.`, false},
			{"since_days", "integer", "Additional look-back: only memories created in the last N days (negative is treated as no filter). Preview-only.", false},
		},
	},
	{
		Name: "brief", Description: "Return the latest what-changed/what-matters brief for session start — the same budgeted, cited, source-grouped daily brief as `digest`, resolved to the freshest available; call this FIRST at the start of a session. Opt into `envelope` for a synthesis_prompt to compose a grounded, cited brief.",
		Params: []Param{
			{"max_tokens", "integer", "Approximate token budget for the brief (default ~6000, max ~20000)", false},
			{"envelope", "boolean", "Opt-in: also return a synthesis_prompt for composing a grounded, cited brief over the items (default false; Mora makes no model call)", false},
			{"entity", "string", `Filter the brief to memories referencing one person (display name or email/handle). A no-match or ambiguous name returns an error. Preview-only.`, false},
			{"scope", "string", `Filter the brief to one memory scope/namespace, e.g. "project:acme". Preview-only.`, false},
			{"since_days", "integer", "Additional look-back: only memories created in the last N days (negative = no filter). Preview-only.", false},
		},
	},
	{
		Name: "meeting_prep", Description: "Assemble the same fully-cited unfinished-business brief as `mora brief --event-id`: user-owned obligations, unresolved threads, staleness guards, and material shared context. Every evidence line carries memory_id, channel/source, and date. Local, deterministic, and model-free.",
		Params: []Param{
			{"event_id", "string", "Calendar memory id to brief; omit to use the next (or in-progress) event", false},
			{"at", "string", "RFC3339 as-of time for reproducible assembly (default now)", false},
			{"name", "string", `Optional attendee name/email/handle: prep the next meeting WITH this person (falls back to the next meeting if they have none). Omit for the next meeting on the calendar.`, false},
			{"limit", "integer", "Max actionable cited lines per attendee (default 8)", false},
			{"max_tokens", "integer", "Approximate token budget for the pack (default ~6000, max ~20000)", false},
		},
	},
}

// ToolCatalog returns the canonical ordered public MCP tool catalog.
func ToolCatalog() []ToolDefinition {
	out := make([]ToolDefinition, len(toolCatalog))
	copy(out, toolCatalog)
	for i := range out {
		out[i].Params = append([]Param(nil), toolCatalog[i].Params...)
	}
	return out
}

// RenderTools renders the catalog as strict MCP tool descriptors in declared order.
func RenderTools(defs []ToolDefinition) []map[string]any {
	tools := make([]map[string]any, 0, len(defs))
	for _, def := range defs {
		tools = append(tools, Tool(def.Name, def.Description, def.Params...))
	}
	return tools
}
