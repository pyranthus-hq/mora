package mora

// health_registry.go — Packet C3 (Gate 2 PR 2, HEALTH-02 completion/HEALTH-10
// exposure): the derived health-surface registry.
//
// There was no production registry for any of the three transports before this
// packet: HTTP routes were imperative mux.HandleFunc calls (no pattern list Go's
// ServeMux exposes), the CLI is a bare `switch` (no runtime enumeration is
// possible over a switch), and MCP had THREE independent hand-lists (tools/list's
// literal, the callMCPTool switch, and httpCallAllowed) that could silently
// drift apart. Two of the three transports now have a MECHANICALLY derived
// single source of truth:
//
//   - MCP: mcpToolRegistry (mcp.go) — both tools/list and callMCPTool's dispatch
//     derive from the SAME []mcpToolDef slice.
//   - HTTP: internal/loopbackhttp owns one route registry used for both its
//     ServeMux and the []httpRoute metadata enumerated through the Mora adapter.
//
// The CLI cannot be derived the same way — a Go `switch` has no runtime
// reflection — so cliHealthSurfaces below is an EXPLICIT list, hand-verified
// against mora.go's dispatch and the packet's acceptance box (the "search,
// context, think, list, read, graph, entities, tasks, brief, pulse, doctor, and
// sync status" set). Every entry across all three lists is driven through its
// REAL dispatcher by TestEverySurfaceCarriesHealth — no helper-level unit test
// stands in for this (that was the exact hole the mutation experiment found: 5
// call sites bypassed the OLD ad-hoc lists, 1,674 tests green).

// mcpHealthExemptTools is empty: every registered MCP tool carries the compact
// health envelope (either directly under a "health" key, or via
// MeetingBrief.Health for meeting_prep) — there is no exempt tool.
var mcpHealthExemptTools = map[string]bool{}

// httpHealthExemptRoutes are HTTP routes that do NOT need to carry typed health
// or render the banner:
//   - GET /healthz is the LIVENESS probe itself — it reports Health.State (see
//     handleHealthz), but per C3 it is "health-reporting, banner-exempt," not
//     name-matched into the rendered-banner/typed-payload completeness set.
//   - GET /{$} is the static landing page (embeds the bearer token for the
//     browser bridge) — it renders no product data.
//   - POST /call is a generic passthrough router to an arbitrary allowed tool
//     name, not itself a distinct product surface — the tool it dispatches to
//     (search_memory, think, …) is covered individually via its own named
//     route or the MCP registry.
var httpHealthExemptRoutes = map[string]bool{
	"GET /healthz": true,
	"GET /{$}":     true,
	"POST /call":   true,
}

// cliHealthSurfaces is the CLI verb argv set HEALTH-02/C3 requires, each
// driven through the real Run([...]) dispatcher and asserted to render the
// banner as their first content line. Argv chosen so every command takes its
// TEXT (non --json) branch, where the banner is rendered — CLI `--json` output
// is out of scope for this packet's envelope break (see printHealthBannerLine).
//
// `doctor` is required too (C3) but is checked SEPARATELY in
// TestEverySurfaceCarriesHealth: it predates this packet with its OWN typed
// per-check ok/warn convention (doctor --json's Sources/Index arrays; Gate 1's
// TestSixDayFreezeSurfacesWithin24h already pins doctor --json/--strict/
// --pulse), which is the "exposes typed health in payload" half of C3's
// contract rather than the rendered-banner half — it does not ALSO render the
// 🔴 banner line in plain-text mode, and does not need to.
var cliHealthSurfaces = [][]string{
	{"search", "lorem"},
	{"context", "--query", "lorem"},
	{"think", "lorem"},
	{"list"},
	{"read", "read-target"},
	{"graph"},
	{"entities"},
	{"tasks", "list"},
	{"brief"},
	{"pulse", "--digest"}, // bare `pulse` prints nothing to stdout unless --digest/--write
	{"sync", "status"},
}
