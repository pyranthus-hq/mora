package mora

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Build info, injected at release time via -ldflags -X. Defaults keep `go run`
// and source builds honest about being unversioned.
var (
	BuildVersion = "dev"
	BuildCommit  = "none"
	BuildDate    = "unknown"
)

type Config struct {
	VaultDir  string
	ConfigDir string
	DataDir   string
	StateDir  string
	// Embedder is the durable embedder opt-in from config.toml (`embedder = "ollama"`).
	// It is the persistent way to turn on semantic retrieval for BOTH the CLI and the
	// MCP server (which the agent uses) without per-host env wiring. The MORA_EMBEDDER
	// env var, when SET, still wins (it is the CI-determinism + power-user override);
	// this is the fallback consulted only when the env is unset. Empty ⇒ static floor.
	Embedder string
	// ContextProfile is the durable quality/size knob from config.toml
	// (`context = "small"|"large"`; empty ⇒ default). It scales the DEFAULT
	// token budget of every budget-bounded surface (context_memory, digest,
	// brief, the persisted brief artifact) and the digest per-item snippet
	// length — small for lean agent windows, large for denser context. It
	// NEVER moves the 20k per-call ceiling, and an explicit max_tokens from
	// the caller always wins. Set via `mora config context <profile>`.
	ContextProfile string
	// fusionOv overrides the production RRF arm weights / k (retrieval tuning + the
	// TestEvalWeightSweep grid). nil ⇒ defaultFusion. Unexported and NOT loaded from
	// TOML — it is a code/eval seam, not a user knob.
	fusionOv *fusionParams
	// MMR opts the hybrid path into a greedy Maximal Marginal Relevance rerank of the
	// fused candidates (a diversity-aware reorder) before the top-k truncate. Durable,
	// from config.toml `mmr = true`; default false ⇒ fused order unchanged (the
	// production default path stays byte-identical). Only takes effect when the vector
	// arm is live (a semantic embedder), since MMR reranks on cosine. Mirrors Embedder;
	// there is deliberately no MORA_MMR env var (the eval forces via mmrOv, not env).
	MMR bool
	// mmrOv overrides the MMR params (the W2 regression gate + the unit tests). nil ⇒
	// derive from Config.MMR with defaultLambda. Unexported and NOT loaded from TOML —
	// a code/eval seam exactly like fusionOv, and the ONLY path that can set
	// mmrParams.force (so the user MMR bool can never run MMR under static-hash).
	mmrOv *mmrParams
}

type Memory struct {
	ID          string   `json:"id"`
	Scope       string   `json:"scope"`
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Tags        []string `json:"tags"`
	Source      string   `json:"source"`
	CreatedAt   string   `json:"created_at"`
	Path        string   `json:"path"`
	Text        string   `json:"text,omitempty"`
	Score       float64  `json:"score,omitempty"`
	Provider    string   `json:"provider,omitempty"`
	Account     string   `json:"account,omitempty"` // multi-account label; composes the "provider:account" instance key
	ProviderID  string   `json:"provider_id,omitempty"`
	ContentHash string   `json:"content_hash,omitempty"`
	LastSynced  string   `json:"last_synced,omitempty"`
	Truncated   bool     `json:"truncated,omitempty"`
	DeletedAt   string   `json:"deleted_at,omitempty"`
	// Owner attributes a result from a SHARED corpus (`mora share subscribe`)
	// with the subscriber-chosen subscription name. Never persisted to disk and
	// always empty for the user's own memories — omitempty keeps local-only
	// payloads byte-identical (the MCP budget gate depends on that).
	Owner string `json:"owner,omitempty"`
	// Meta is structured identity/frontmatter (participants, from/to, occurred_at),
	// persisted as one canonical JSON line (`meta: {...}`). Powers the entity graph;
	// the graph compiler reads it deterministically (no NER).
	Meta map[string]any `json:"meta,omitempty"`
}

type Source struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Scope     string   `json:"scope"`
	Path      string   `json:"path,omitempty"`
	Label     string   `json:"label,omitempty"`
	Calendar  string   `json:"calendar,omitempty"`
	FolderID  string   `json:"folder_id,omitempty"`
	SinceDays int      `json:"since_days,omitempty"` // 0 => default per type
	LabelIDs  []string `json:"label_ids,omitempty"`
	DocsOnly  bool     `json:"docs_only,omitempty"` // filesystem: docs/metadata only
	Account   string   `json:"account,omitempty"`   // google: account label for multi-mailbox (empty = the default/legacy account)
	Email     string   `json:"email,omitempty"`     // google: the signed-in address, stamped at connect — the same-account re-auth guard reads it
	Enabled   *bool    `json:"enabled,omitempty"`   // nil => legacy source, grandfather to true (D-12); *false => opt-in disabled (D-11)
	CreatedAt string   `json:"created_at"`

	// DenyContacts / DenyConversations scope iMessage ingest (IMSG-06/D-07/D-08).
	// Persisted on the imessage source row in sources.json (no new config file),
	// matching Phase 1's no-new-file precedent. Empty = include everyone.
	DenyContacts      []string `json:"deny_contacts,omitempty"`
	DenyConversations []string `json:"deny_conversations,omitempty"`
}

// connectorInfo is a static catalog entry describing a user-enableable connector
// type. NeedsAuth marks types that require an OAuth consent moment on enable.
//
// Phase-12 capability + section descriptor (consumed in internal/mora/connectors.go):
//   - Ingesting: true for connectors that persist memories + a SyncStatus, so
//     they belong in the delta-watermark three-state enumeration set (M-2). A
//     future live-passthrough (PostHog/Linear) or on-demand (GitHub) connector
//     would be Ingesting=false and is thus excluded by construction — it can
//     never read "unavailable — sync error".
//   - Rank / Label: the digest section ordering + human label (M-6). Lower Rank
//     leads (and survives budget truncation first); these move the formerly
//     hardcoded sourceDigestRank / digestSourceLabel switch DATA onto the
//     descriptor so an Nth connector is not silently truncated-first or rendered
//     as an ugly title-cased raw provider. connectorDisplay is the single reader.
type connectorInfo struct {
	Type        string
	DisplayName string
	NeedsAuth   bool
	Ingesting   bool
	Rank        int
	Label       string
	// Provider is the memory-side Provider this connector's mapper mints in
	// frontmatter when it differs from Type (applecalendar mints "applecal").
	// Empty means Provider == Type. The alias is applied at LOOKUP boundaries
	// only (providerToType / sourceInstanceKey in connectors.go) — on-disk
	// frontmatter is never rewritten to "fix" a mismatch.
	// TestConnectorProviderKeysReconcile enforces the round-trip for every
	// ingesting entry.
	Provider string
	// Upcoming marks connectors whose items are future-dated events: cold-start
	// courtesy windows look FORWARD (next 7d) instead of back. Capability DATA
	// here, never a provider-string heuristic in digest code (the old
	// HasPrefix(key, "calendar") silently missed applecalendar).
	Upcoming bool
}

// connectorCatalog is the static, exhaustive catalog of user-enableable connector
// types (D-01 static catalog, D-02 per-type granularity). gmail and calendar are
// separate, independently-toggled rows even though they share one Google OAuth.
// gdrive is intentionally OMITTED (D-03): it is a no-op stub, not user-enableable.
//
// Ingesting/Rank/Label carry the Phase-12 capability + section descriptor (read
// by connectorDisplay / ingestingConnectors in connectors.go). Ranks/labels
// preserve the legacy digest intent: calendar=(0,"Calendar"), imessage=(1,
// "Texts"), gmail=(2,"Emails"); filesystem gets a real rank (3) + clean label
// ("Files") rather than the old default-rank-3 / title-cased fallback.
var connectorCatalog = []connectorInfo{
	{Type: "gmail", DisplayName: "Gmail", NeedsAuth: true, Ingesting: true, Rank: 2, Label: "Emails"},
	{Type: "calendar", DisplayName: "Google Calendar", NeedsAuth: true, Ingesting: true, Rank: 0, Label: "Calendar", Upcoming: true},
	{Type: "filesystem", DisplayName: "Filesystem", NeedsAuth: false, Ingesting: true, Rank: 3, Label: "Files"},
	// iMessage: default-disabled, no OAuth — the real gate is macOS Full Disk
	// Access (surfaced by `mora doctor`), not a login (D-11, Surface 1).
	{Type: "imessage", DisplayName: "iMessage", NeedsAuth: false, Ingesting: true, Rank: 1, Label: "Texts"},
	// Apple Calendar: same gate story as iMessage (local store + Full Disk
	// Access, no login). Rank ties with Google Calendar break on the key, so
	// both calendar sections lead the digest together. Its mapper mints
	// Provider "applecal" (internal/applecal), so the entry carries the alias —
	// without it, applecal memories never reconcile with this instance and
	// silently vanish from the delta brief.
	{Type: "applecalendar", DisplayName: "Apple Calendar", NeedsAuth: false, Ingesting: true, Rank: 0, Label: "Calendar (Apple)", Provider: "applecal", Upcoming: true},
}

// catalogRow is the per-type view emitted by `connectors list`. Enabled joins the
// static catalog against the user's sources.json consent state.
type catalogRow struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Enabled   bool   `json:"enabled"`
	NeedsAuth bool   `json:"needs_auth"`
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Result  any    `json:"result,omitempty"`
	Error   any    `json:"error,omitempty"`
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) error {
	if len(args) == 0 {
		printUsage(stdout)
		return nil
	}
	cmd := args[0]
	switch cmd {
	case "init":
		return cmdInit(ctx, args[1:], stdout, stdin)
	case "write":
		return cmdWrite(ctx, args[1:], stdout)
	case "read":
		return cmdRead(ctx, args[1:], stdout)
	case "list":
		return cmdList(ctx, args[1:], stdout)
	case "search":
		return cmdSearch(ctx, args[1:], stdout)
	case "entities":
		return cmdEntities(ctx, args[1:], stdout)
	case "graph":
		return cmdGraph(ctx, args[1:], stdout)
	case "delete":
		return cmdDelete(ctx, args[1:], stdout)
	case "context":
		return cmdContext(ctx, args[1:], stdout)
	case "index":
		return cmdIndex(ctx, args[1:], stdout, stdin)
	case "tasks":
		return cmdTasks(ctx, args[1:], stdout)
	case "pulse":
		return cmdPulse(ctx, args[1:], stdout)
	case "lint":
		return cmdLint(ctx, args[1:], stdout)
	case "backup":
		return cmdBackup(ctx, args[1:], stdout)
	case "doctor":
		return cmdDoctor(ctx, args[1:], stdout)
	case "config":
		return cmdConfig(args[1:], stdout)
	case "schedule":
		return cmdSchedule(ctx, args[1:], stdout)
	case "sources":
		return cmdSources(ctx, args[1:], stdout)
	case "connectors":
		return cmdConnectors(ctx, args[1:], stdout, stdin)
	case "ingest":
		return cmdIngest(ctx, args[1:], stdout)
	case "connect":
		return cmdConnect(ctx, args[1:], stdout)
	case "sync":
		return cmdSync(ctx, args[1:], stdout)
	case "share":
		return cmdShare(ctx, args[1:], stdout, stdin)
	case "reingest":
		return cmdReingest(ctx, args[1:], stdout)
	case "think":
		return cmdThink(ctx, args[1:], stdout)
	case "brief":
		return cmdBrief(ctx, args[1:], stdout)
	case "prep":
		return cmdPrep(ctx, args[1:], stdout)
	case "usage":
		return cmdUsage(ctx, args[1:], stdout)
	case "disconnect":
		return cmdDisconnect(ctx, args[1:], stdout)
	case "mcp":
		return cmdMCP(ctx, args[1:], stdout, stderr, stdin)
	case "serve":
		return cmdServe(ctx, args[1:], stdout)
	case "hook":
		return cmdHook(ctx, args[1:], stdout, stdin)
	case "loop":
		return cmdLoop(ctx, args[1:], stdout)
	case "upgrade":
		return cmdUpgrade(ctx, args[1:], stdout)
	case "version", "--version", "-v":
		return cmdVersion(stdout)
	case "help", "-h", "--help":
		printUsage(stdout)
		return nil
	default:
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func cmdVersion(w io.Writer) error {
	fmt.Fprintf(w, "mora %s\n", BuildVersion)
	fmt.Fprintf(w, "  commit: %s\n", BuildCommit)
	fmt.Fprintf(w, "  built:  %s\n", BuildDate)
	fmt.Fprintf(w, "  go:     %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return nil
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `mora — local memory utility

USAGE:
  mora init --vault ~/vault/mora
  mora write --scope project:acme --type decision --title "OAuth" --text "..."
  mora search "OAuth status" --scope project:acme --json
  mora entities                    # the people/projects/topics across your memory
  mora entities "Sam" --json       # what's known about one entity
  mora graph                       # visual map of the entity graph (top people + topics)
  mora graph "Sam"                 # expand one entity: connections, edges, evidence
  mora read <id> --json
  mora list --scope project:acme --json
  mora delete <id> --yes
  mora context --scope project:acme --query "auth" --budget 6000 --json
  mora think "what did Sam decide about the launch" --json   # cited evidence + gap analysis
  mora brief                       # the latest what-changed/what-matters brief (session-start default; local-only)
  mora brief --envelope --json     # add a synthesis prompt / emit structured {generated, body}
  mora index rebuild
  mora share init acme --scope project:acme --recipient age1... --remote <PRIVATE git URL>   # publish a scope, always encrypted
  mora share push acme             # preview exactly what leaves, then publish
  mora share subscribe neil --remote <URL>   # read someone's share beside your vault (never merged into it)
  mora tasks sync --write
  mora tasks add "Reply to Sam about the launch" --pri P0   # capture an open loop (name first, then flags)
  mora tasks list --json                                    # the current live tasks
  mora tasks done "Set up Mora"    # mark a live task complete so it stops resurfacing as stale
  mora pulse --write --digest
  mora sources add filesystem --name docs --path ~/Documents --scope personal
  mora ingest run --source docs
  mora schedule install pulse-daily
  mora config context large        # context profile: small | default | large (budget + snippet density)
  mora config mmr on               # diversity-aware rerank of hybrid results (needs embedder=ollama)
  mora connectors list|enable <type>|disable <type>
  mora connect google              # sign in with Google in your browser, then backfill Gmail + Calendar (last 90 days)
  mora connect google --since-days 365   # widen the gmail backfill window
  mora connect imessage            # macOS: enable iMessage, check Full Disk Access, then backfill
  mora connect imessage --since-days 365   # widen the iMessage backlog window (negative = all-time)
  mora connect filesystem ~/Documents      # add + enable + index a folder in one step (one-shot of: sources add + ingest run)
  mora sync status
  mora sync google
  mora sync imessage               # macOS: read local Messages (read-only) into memories
  mora reingest [--full]           # re-fetch + rewrite memories with latest metadata, rebuild graph
  mora usage report
  mora usage off|on
  mora disconnect google
  mora mcp serve
  mora serve http                  # loopback HTTP for sandboxed AI browsers (Aside); token in ~/.config/mora/http.json
  mora serve http install          # run it as an auto-restarting background service (launchd/systemd); also: uninstall|status
  mora hook install|uninstall|status
  mora upgrade                     # self-update to the latest release (brew installs: brew upgrade)
  mora version`)
}

// confirmVaultRepointFn is the repoint-confirmation gate cmdInit calls. It is a
// package var (defaulting to confirmVaultRepoint) only so tests can drive the
// confirmed and declined branches end-to-end — confirmVaultRepoint refuses
// non-interactively by design, so the real user-facing repoint flow is otherwise
// untestable without a TTY. Production never reassigns it.
var confirmVaultRepointFn = confirmVaultRepoint

// flagsFirst reorders args so flag tokens precede positionals, so `mora read <id>
// --json` works like `mora read --json <id>` (Go's flag package otherwise stops
// parsing at the first positional). Safe ONLY for commands whose flags are all
// boolean (read/delete) — a value-taking flag's value would be misread as a positional.
func flagsFirst(args []string) []string {
	var flags, pos []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
		} else {
			pos = append(pos, a)
		}
	}
	return append(flags, pos...)
}

// briefResult is the small typed object `mora brief --json` emits — byte-clean
// (ANSI never reaches --json), mirroring resolveBrief's (body, generated) return.
type briefResult struct {
	Generated bool   `json:"generated"`
	Body      string `json:"body"`
}

// briefDigest factors the resolveBrief GENERATE semantics so the CLI envelope
// path and the MCP `brief` tool share ONE definition of "build a non-empty brief
// digest": a DELTA preview first (advance forced false — strictly read-only,
// never mutates the Phase-12 watermark), and when the delta surfaces ZERO items
// (the scheduled --advance job already consumed today's delta) a re-build in the
// fixed briefFallbackWindowHours WINDOW so a session-start brief is never useless.
// Both builds pass advance:false, so neither syncs nor advances the watermark
// (D16-2 / SC#4). It is the Digest sibling of resolveBrief's rendered-string path.
func briefDigest(cfg Config, now time.Time, perSourceCap int) (Digest, error) {
	d, err := buildDigest(cfg, now, briefOpts{advance: false, perSourceCap: perSourceCap})
	if err != nil {
		return Digest{}, err
	}
	if briefSurfacedItemCount(d) == 0 {
		d, err = buildDigest(cfg, now, briefOpts{advance: false, sinceHours: briefFallbackWindowHours, perSourceCap: perSourceCap})
		if err != nil {
			return Digest{}, err
		}
	}
	return d, nil
}

// briefClock is the wall clock the session-start brief surfaces (the `mora brief`
// CLI and the MCP `brief` tool) resolve freshness against. It is a var so the
// CLI/MCP wiring tests can pin "now" to the SAME fixed instant they date a seeded
// brief file with — otherwise a file dated on a fixed day silently goes stale in
// real time and the surface REGENERATES instead of reading it verbatim (the
// 2026-06-08-pinned brief tests that failed once the calendar rolled past it).
// Production never reassigns it; the resolver itself already takes now as a param.
var briefClock = time.Now

// cmdBrief is the `mora brief` session-start command (SC#1): it prints the LOCAL
// latest-or-generated brief via resolveBrief (D16-1) — zero network, never
// advances the watermark (D16-2/SC#4). Flags mirror the other surfaces: --json
// emits the typed briefResult (byte-clean), --envelope appends the model-free
// synthesis_prompt after the body.
//
// Styling rule (the load-bearing correctness point): resolveBrief returns a
// VERBATIM persisted file when generated==false (read straight off disk) and a
// freshly RENDERED digest when generated==true. We apply styleDigestTTY ONLY to
// the freshly-generated body — re-skinning a persisted file would double-process
// it. styleDigestTTY is byte-identical off-TTY, so piped/redirected output and
// the test harness see raw Markdown either way; the skin only appears on a real
// terminal for a freshly-generated brief.
func cmdBrief(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("brief", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "emit a byte-clean JSON result")
	envelope := fs.Bool("envelope", false, "append a model-free synthesis prompt")
	entity := fs.String("entity", "", "filter to memories referencing one person (name or email/handle); preview-only")
	scope := fs.String("scope", "", "filter to one memory scope/namespace (e.g. project:acme); preview-only")
	sinceDays := fs.Int("since-days", 0, "only memories created in the last N days; preview-only (negative = no filter)")
	fresh := fs.Bool("fresh", false, "regenerate from the live vault now, bypassing today's cached brief (read-only; never advances the watermark)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	now := briefClock()

	// A filtered brief is preview-only and BYPASSES the persisted cache (§3); the
	// entity is resolved eagerly here so buildDigest stays DB-free, with a hard
	// error on no-match/ambiguity rather than a silently-empty brief.
	opts := briefOpts{scope: *scope, sinceDays: clampSinceDays(*sinceDays), forceRegen: *fresh}
	if *entity != "" {
		idSet, rerr := resolveEntityFilter(ctx, cfg, *entity)
		if rerr != nil {
			return rerr
		}
		opts.entityIDSet = idSet
	}

	body, generated, err := resolveBrief(cfg, now, opts)
	if err != nil {
		return err
	}
	logUsage(cfg, usageEvent{Tool: "brief"})
	if *jsonOut {
		// Byte-clean structured result; --envelope has no effect on --json (the
		// envelope is a human-stdout addition, like pulse --digest --envelope).
		return emit(stdout, briefResult{Generated: generated, Body: body}, *jsonOut)
	}
	if generated {
		// Freshly generated: apply the TTY skin (off-TTY this is a no-op, byte-clean).
		body = styleDigestTTY(body, newStyler(stdout, false))
	}
	fmt.Fprintln(stdout, body)
	if *envelope {
		// Build the brief digest the SAME way the body was generated so the
		// synthesis_prompt cites the SAME items. A filtered brief uses the
		// filter-aware factory; the unfiltered envelope is byte-unchanged.
		// Model-free + read-only: only digestSynthesisPrompt runs (no model/network,
		// no watermark mutation — both builders force advance:false).
		var d Digest
		var derr error
		if opts.filtered() {
			d, derr = filteredBriefDigest(cfg, now, opts)
		} else {
			d, derr = briefDigest(cfg, now, 0)
		}
		if derr != nil {
			return derr
		}
		fmt.Fprintln(stdout, digestSynthesisPrompt(d.Urgent, d.Sections, buildSourceStates(cfg, d)))
	}
	return nil
}

func cmdPulse(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("pulse", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	write := fs.Bool("write", false, "write")
	digest := fs.Bool("digest", false, "digest")
	// --since-hours (SC#2): the explicit ad-hoc window. A positive value renders
	// the plain last-N-hours brief and NEVER advances the watermark (the window
	// path is watermark-independent by construction).
	sinceHours := fs.Int("since-hours", 0, "render the explicit last-N-hours window (ad-hoc; never advances the watermark)")
	// --advance (D-02/SC#4): default-OFF on every surface. The watermark advances
	// ONLY when this is passed — the scheduled pulse-daily job is the sole caller.
	// An explicit --since-hours run never advances regardless.
	advance := fs.Bool("advance", false, "commit the delta watermark (the only surface that advances it; default preview)")
	// --sync (D13-4): refresh enabled sources BEFORE building the digest, so the
	// brief reflects current data. Default OFF — ad-hoc pulse never touches the
	// network; the scheduled pulse-daily job is the sole caller that opts in. A sync
	// error is NEVER fatal (honest-but-don't-abort): the failed source surfaces as
	// stale/unavailable via the existing three-state, a partial honest brief beats
	// no brief.
	syncFirst := fs.Bool("sync", false, "refresh enabled sources before building the digest (sync-first; the scheduled job sets this)")
	// --brief-file (D13-5): persist the rendered digest as a dated vault artifact
	// (briefs/<date>-brief.md). Default OFF for ad-hoc pulse; the scheduled job sets
	// it. A persist error is non-fatal — the brief still prints.
	briefFile := fs.Bool("brief-file", false, "persist the digest as a dated vault artifact (briefs/<date>-brief.md); the scheduled job sets this")
	// --notify (D13-5): post a best-effort macOS toast pointing at the persisted
	// brief. Default OFF; only fires when a brief was actually persisted; the toast
	// itself is GOOS/env-gated + best-effort in notify.go.
	notify := fs.Bool("notify", false, "post a macOS notification pointing at the persisted brief (the scheduled job sets this)")
	// --envelope (15-02/SC#1): preview-only, read-only opt-in. When set, AFTER the
	// existing rendered brief prints, append the synthesis_prompt (the grounded,
	// cited instruction the agent runs with its OWN model — Mora makes no model
	// call, SC#2). Default OFF: plain `pulse --digest` stdout is byte-unchanged. It
	// NEVER touches briefOpts/--advance/the watermark and does NOT gate the
	// brief-file/notify artifact — it is an interactive stdout addition only.
	envelope := fs.Bool("envelope", false, "also emit a synthesis_prompt for the agent to compose a grounded, cited brief over the digest items (default off; Mora makes no model call)")
	// --source: preview-only per-connector rundown ("just my texts this week").
	// Family or instance key (imessage | gmail | gmail:work | …). Never combined
	// with --advance (a filtered advance would mark unseen sources read).
	srcFilter := fs.String("source", "", "filter the digest to one connector (imessage|gmail|calendar|applecalendar or gmail:<account>); preview-only")
	// --entity/--scope/--since-days: the three preview-only content filters (same as
	// `mora brief`), orthogonal to --source. Never combined with --advance (the
	// hoisted buildDeltaDigest guard rejects a filtered advance).
	entityFilter := fs.String("entity", "", "filter the digest to memories referencing one person (name or email/handle); preview-only")
	scopeFilter := fs.String("scope", "", "filter the digest to one memory scope/namespace (e.g. project:acme); preview-only")
	sinceDays := fs.Int("since-days", 0, "additional look-back: only memories created in the last N days; preview-only (negative = no filter)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// One clock: compute now ONCE and thread the SAME value into buildDigest and the
	// persist/notify step (Task 2) so the digest, the dated artifact path, and any
	// watermark all agree on the logical day (D13-3, determinism).
	now := time.Now()
	added, err := syncTasks(cfg, *write)
	if err != nil {
		return err
	}
	stale, err := staleTasks(cfg, 3)
	if err != nil {
		return err
	}
	line := fmt.Sprintf("- [%s] pulse | tasks_added:%d | stale:%d\n", time.Now().Format(time.RFC3339), added, len(stale))
	if *write {
		if err := appendFile(filepath.Join(cfg.VaultDir, "log.md"), line); err != nil {
			return err
		}
	}
	if *digest {
		// Map the flags to briefOpts (D-02/SC#2/SC#4):
		//   - sinceHours>0 selects the explicit ad-hoc window — it NEVER advances
		//     the watermark regardless of --advance (the window path is watermark-
		//     independent), so we force advance=false there.
		//   - otherwise DELTA mode (the scheduled default); advance is opt-in via
		//     --advance, the ONLY surface that commits the watermark.
		opts := briefOpts{sinceHours: *sinceHours, source: *srcFilter, scope: *scopeFilter, sinceDays: clampSinceDays(*sinceDays)}
		if *entityFilter != "" {
			idSet, rerr := resolveEntityFilter(ctx, cfg, *entityFilter)
			if rerr != nil {
				return rerr
			}
			opts.entityIDSet = idSet
		}
		if *sinceHours <= 0 {
			opts.advance = *advance
			// Sync-first (D13-4): in DELTA mode only, when --sync is set, refresh the
			// enabled sources BEFORE building so the digest reflects current data. We
			// run the SAME backfills `mora sync` runs, in cmdSync's order (google then
			// imessage). Their errors are CAPTURED but NEVER returned — the backfill
			// already records the failure into SyncStatus, where classifyState renders
			// the source as stale/unavailable in the brief. Aborting here would defeat
			// the point: a partial honest brief beats no brief (T-13-09/T-13-12). An
			// explicit --since-hours window is ad-hoc and is intentionally not synced.
			if *syncFirst {
				if _, gerr := backfillGoogleFn(ctx, cfg, stdout); gerr != nil {
					warnf(stdout, "google sync incomplete; the brief reflects last good data (run `mora sync status`): %v", gerr)
				}
				if _, ierr := backfillIMessageFn(ctx, cfg, stdout); ierr != nil {
					warnf(stdout, "imessage sync incomplete; the brief reflects last good data (run `mora sync status`): %v", ierr)
				}
			}
		}
		// The --advance path (the scheduled pulse-daily job) runs the whole
		// build→budget→persist→commit as ONE locked transaction in advanceBrief, so the
		// watermark advances ONLY over items that survive the byte budget and rendered
		// into the persisted brief — never the pre-truncation cap (issue #62 defect 1).
		// Every preview/window surface stays the pure buildDigest read path.
		var d Digest
		renderBudget := defaultContextTokens * charsPerToken
		artifactPath := ""
		persisted := false
		if opts.advance {
			// Budget stdout, the artifact, and the commit at the SAME persist budget so
			// all three agree on exactly which items were shown.
			budgetChars := cfg.contextDefaultTokens() * charsPerToken
			bd, path, aerr := advanceBrief(cfg, now, opts, budgetChars, *briefFile)
			if aerr != nil {
				return aerr
			}
			d, renderBudget, artifactPath, persisted = bd, budgetChars, path, *briefFile
		} else {
			bd, derr := buildDigest(cfg, now, opts)
			if derr != nil {
				return derr
			}
			d = bd
		}
		// renderDigest is the data path (also what the MCP `digest` tool returns,
		// byte-identical). styleDigestTTY is a TTY-only presentation skin on top;
		// pipes, redirects, and the MCP transport get the raw Markdown unchanged. When
		// d is already budgeted (the --advance path) renderDigest is idempotent.
		out := renderDigest(d, renderBudget)
		out = styleDigestTTY(out, newStyler(stdout, false))
		fmt.Fprintln(stdout, out)
		// --envelope (15-02): preview-only append AFTER the brief — the prompt cites
		// the SAME rendered items (d.Sections). Model-free: digestSynthesisPrompt is a
		// pure string builder — Mora makes no model/network call (SC#2).
		if *envelope {
			fmt.Fprintln(stdout, digestSynthesisPrompt(d.Urgent, d.Sections, buildSourceStates(cfg, d)))
		}
		// Persist the PREVIEW path's artifact here (the --advance path already persisted
		// under the lock). A write error is non-fatal for a preview — the brief already
		// printed; a partial honest brief beats no brief (T-13-12).
		if *briefFile && !persisted {
			path, werr := writeBriefArtifact(cfg, d, now)
			if werr != nil {
				warnf(stdout, "could not persist the brief artifact: %v", werr)
			} else {
				artifactPath = path
			}
		}
		// Notify is the LAST step and best-effort: only fires when a brief was actually
		// persisted (we have a path to point at); notifyBriefFn is GOOS/env-gated and
		// swallows its own error. When the brief has an Urgent shelf, enrich the toast
		// with its top item so the deadline is visible without opening the brief (#62).
		if *briefFile && *notify && artifactPath != "" {
			var top *urgentNote
			if len(d.Urgent) > 0 {
				top = &urgentNote{subtitle: d.Urgent[0].Title, body: d.Urgent[0].Snippet}
			}
			_ = notifyBriefFn(artifactPath, top)
		}
	}
	return nil
}

func resolveReal(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

func plural(n int, unit string) string {
	if n == 1 {
		return unit
	}
	return unit + "s"
}

// backfillEnabledGoogle re-runs the gated google backfill (the `mora sync google`
// route) for every ENABLED gmail/calendar source, then rebuilds the index. It is
// the single backfill seam shared by cmdSync and applySetupSelection so the setup
// menu never reimplements ingest logic (Plan 04 / D-09). Disabled sources are
// silently skipped (D-07). Returns the item count and a non-nil error if any
// source failed (freshness is the product's value — never swallow sync errors).
// backfillGoogleFn / backfillIMessageFn are the injectable sync-first seams the
// scheduled pulse-daily job (and `pulse --sync`) refresh enabled sources through
// — the SAME backfills `mora sync` runs. They default to the production functions;
// tests swap them (t.Cleanup-restore, never t.Parallel) to assert sync-first
// ordering and honest, non-aborting errors WITHOUT real network. cmdPulse calls
// these vars, not the functions directly, so the seam is the only test hook.
var (
	backfillGoogleFn   = backfillEnabledGoogle
	backfillIMessageFn = backfillEnabledIMessage
)

// notifyBriefFn is the injectable notification seam cmdPulse posts the toast
// through (D13-5 / SC#3). It defaults to notifyBriefDefault (the 13-02 production
// entry point that wires osascriptRunner + runtime.GOOS and is GOOS/env-gated +
// best-effort). Tests swap it (t.Cleanup-restore, never t.Parallel) to assert the
// notify routing WITHOUT spawning a real osascript.
var notifyBriefFn = notifyBriefDefault

// ingestSourceFn is the injectable seam cmdIngest routes per-source ingest
// through (defaults to ingestSource); tests swap it (t.Cleanup-restore, never
// t.Parallel) to assert --all's warn-and-continue ordering without real
// connectors.
var ingestSourceFn = ingestSource

// reingestFullDays is the Gmail lookback used by `reingest --full` — ~100 years,
// effectively all-time (windowForSource computes Since = now - days; Gmail does not
// treat a negative SinceDays as all-time, so a large positive value is used).
const reingestFullDays = 36500

// connectFreshWindow is how recently a source must have cleanly synced for the
// connect path to skip its re-pull (run `mora sync google` to force).
const connectFreshWindow = time.Hour

// maxCreateAttempts bounds createMemory's re-mint loop. A collision needs two
// writers to mint the same second-granularity timestamp AND the same 4 random
// bytes (~1 in 2^32 per pair), and each retry mints fresh entropy, so exhausting
// this many attempts is astronomically improbable; the bound is a liveness
// backstop, not an expected path.
const maxCreateAttempts = 8

// newIDFn is the id-minting seam (house pattern, cf. confirmVaultRepointFn /
// ingestSourceFn). Production uses newID; tests override it to force a collision
// and exercise createMemory's re-mint retry deterministically.
var newIDFn = newID

// createMemory publishes a BRAND-NEW user memory with a collision-proof,
// create-exclusive publish. It mints an id, renders the memory, and atomicCreate()s
// it — os.Link, which fails EEXIST rather than clobbering an existing memory
// (atomicWrite's os.Rename would overwrite, silently losing the loser of a
// same-id race). On the astronomically-unlikely id collision it re-mints and
// retries, bounded by maxCreateAttempts. Returns the memory with its final
// (winning) id and Path set, ready for indexUpsert and the response.
//
// Use ONLY for new user memories (cmdWrite, MCP write_memory). Connector re-writes
// go through writeMappedMemory → atomicWrite (an existing provider memory is
// re-rendered onto its own stable path, where an idempotent overwrite is correct,
// not a collision); createMemory's anti-clobber applies specifically to freshly
// minted, non-deterministic ids.
func createMemory(cfg Config, m Memory) (Memory, error) {
	for attempt := 0; attempt < maxCreateAttempts; attempt++ {
		m.ID = newIDFn()
		body, err := renderMemory(m)
		if err != nil {
			return Memory{}, err
		}
		path := memoryPath(cfg, m)
		if err := atomicCreate(path, body, 0o644); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue // id collision: re-mint and retry
			}
			return Memory{}, err
		}
		m.Path = path
		return m, nil
	}
	return Memory{}, fmt.Errorf("create memory: could not mint a unique id after %d attempts", maxCreateAttempts)
}

// indexSchemaVersion stamps index.db (PRAGMA user_version) with the schema this
// binary writes. Bump it whenever rebuildIndex's shape changes meaning (a new
// table or column, vector layout, salience semantics). Read paths refuse a
// mismatched index with an actionable error instead of degrading silently —
// a binary swapped across a schema change otherwise serves missing columns or
// zeroed salience (the live Phase-14 failure). 1 = the first stamped schema;
// every pre-stamp index reads as 0 and asks for one rebuild.
const indexSchemaVersion = 2

// indexAutoHeal reports whether a version-stale index may be rebuilt inline at
// read time. True on the static-hash floor, where a rebuild is seconds — the
// same self-healing as rebuild-on-missing, and what saves every distributed
// user's FIRST upgrade across the stamp's introduction (their old binary's
// `upgrade` predates the post-upgrade rebuild hook, and Homebrew swaps bypass
// it entirely). False under a semantic embedder: a full re-embed takes minutes
// and must not stall an innocent MCP tool call — those users get the
// actionable error instead. Package var so tests can pin both branches.
var indexAutoHeal = func(cfg Config) bool { return !embedderIsSemantic(chooseEmbedderFor(cfg)) }

type rebuildPolicy int

const (
	policyEnforce rebuildPolicy = iota // default: block dangerous rebuilds
	policyAllow                        // --force: commit even if guard would block
)

var errRebuildBlocked = errors.New("rebuild blocked")

// listRebuildFiles resolves the vault's memory files for an index rebuild. It is
// a package var (defaulting to allMemoryFiles) purely as a test seam: rebuild
// must LIST the vault after it holds the write lock, and a test swaps this to
// assert the immediate transaction is already open when the listing runs (see
// TestRebuildListsVaultInsideWriteLock). Only rebuildIndexWithPolicy routes
// through it; every other allMemoryFiles caller stays direct.
var listRebuildFiles = allMemoryFiles

const (
	// mcpSearchDefaultLimit is search_memory's default result count. Bumped 5→8:
	// the T2 live eval showed gold docs landing at FTS ranks #5–#7, just outside
	// the old top-5 window. Safe only because the MCP path now snippets bodies
	// (snippetMemories) — 8 full bodies would blow the MCP token budget.
	mcpSearchDefaultLimit = 8
	// searchSnippetLen caps each search_memory result body for the token-budgeted
	// MCP surface (full bodies blew the T0 ceiling). Agents fetch full text via
	// read_memory by id. Matches think's thinkSnippetLen.
	searchSnippetLen = 240
	// searchMemoryResultsBudgetBytes caps the AGGREGATE byte size of the
	// search_memory results array. snippetMemories already bounds each row
	// (searchSnippetLen), but nothing bounded the TOTAL — a large `limit` arg
	// could still blow the MCP window. Derived to hold search_memory's 8000-token
	// envelope ceiling (mora_mcp_budget_test.go): toCallToolResult ships the
	// payload ~2.4× (indented text block + structuredContent mirror), so the
	// pre-envelope results must stay near a third of the ceiling —
	// 8000·charsPerToken/2.4 ≈ 13K, floored to 11K for headroom over the freshness
	// map + JSON frame. budgetSearchResults cuts on whole-Memory boundaries.
	searchMemoryResultsBudgetBytes = 11000
)

var p0Re = regexp.MustCompile(`^(\d+\.|-)\s+\*\*([^*]+)\*\*`)

// terminalTaskStatuses are the Status (col 4) values that close a task. A row in
// any of these states is finished work and must never resurface as "stale"
// (issue #19), regardless of its Last-touched date.
var terminalTaskStatuses = map[string]bool{
	"done":      true,
	"completed": true,
	"cancelled": true,
	"canceled":  true,
	"wontfix":   true,
}

// LiveTask is one row of live-tasks.md (the 8-column task table).
type LiveTask struct {
	Task        string `json:"task"`
	Domain      string `json:"domain"`
	Owner       string `json:"owner"`
	Pri         string `json:"pri"`
	Status      string `json:"status"`
	Blocker     string `json:"blocker"`
	Horizon     string `json:"horizon"`
	LastTouched string `json:"last_touched"`
}

// mcpMaxRequestBytes caps one JSON-RPC request line. bufio.Scanner's 64KB
// default is too small for real tool calls (a write_memory body or a think
// query with pasted context), and overflowing it doesn't drop the request —
// it kills the whole server mid-session. 4MB is far above any legitimate
// call yet still bounds a runaway client.
const mcpMaxRequestBytes = 4 << 20

// mcpInstructions is server-level usage guidance returned in the MCP initialize
// response. Clients (Claude Code, Codex) inject it into the model's context, so
// it is how a fresh agent learns Mora exists and when to reach for it — without
// it the tools sit unused and the agent keeps starting cold. Keep it tight and
// imperative.
const mcpInstructions = `Mora is the user's persistent, local memory across sessions — you do NOT start cold. Before answering anything about the user's past work, people, projects, meetings, decisions, or commitments, call search_memory (or context_memory at the start of a task) and answer from what you retrieve. "I don't have that context" is usually a bug: search first. Call brief at the START of a session for the latest what-changed / what-matters brief (the same daily cross-source briefing, resolved to the freshest available) before doing anything else. Use list_memory to browse recent memories, list_entities/get_entity to explore the people-and-topics graph, digest for a daily cross-source briefing (recent emails, texts, calendar, and open tasks), and think for a cited synthesis with an explicit "what the vault does NOT know" gap analysis. Write durable facts and decisions back with write_memory as they emerge — you do not need to ask permission. Always prefer the user's own memories over assumptions, cite what you recalled, surface stale or missing context honestly, and never invent a memory you did not retrieve.`

// MCP context_memory budget. Agents speak tokens (Neil's pilot asked for a
// ~20k-token per-call ceiling — the 2k-char default was too sparse to be useful
// directly), so the public knob is max_tokens. The engine stays char-based:
// buildContext truncates in runes and a pure-Go tokenizer would be a dependency
// we don't need for a guardrail, so we approximate ~charsPerToken chars/token.
const (
	charsPerToken        = 4     // rough English heuristic; budget guardrail, not exact accounting
	defaultContextTokens = 6000  // denser than the old ~4k-token default
	maxContextTokens     = 20000 // Neil's ceiling; one tool result must not dominate the window
	// largeContextMaxTokens is the raised one-call ceiling the "large" context
	// profile opts into (contextMaxTokens) — an explicit user trade of agent
	// window headroom for denser context.
	largeContextMaxTokens = 50000
)

// mcpDigestEnvelopeDivisor budgets the COMPACT digest payload so the full
// CallToolResult ENVELOPE stays under the requested token ceiling. The envelope
// carries the payload twice (an INDENTED text block + the structuredContent
// mirror — the generic toCallToolResult shape shared by every object tool, a
// separate concern from D-05's digest-internal doubling) and MarshalIndent
// inflates nested arrays, so the envelope runs ~3× the compact payload. We budget
// the compact sections to budgetChars/divisor so digest_max lands under 20000 and
// digest_default under its 6000-token budget with headroom. It is a guardrail
// constant, not exact accounting (the codebase's whole budget unit is approximate).
const mcpDigestEnvelopeDivisor = 3

// mcpDigestMaxItems is the GENEROUS per-source cap the MCP digest surfaces so the
// byte budget (not the human-brief cap of digestDefaultCap=8) governs how many
// items ship — that is what lets max_tokens actually scale the payload (D-05
// knob-alive). It bounds the work (no truly-unbounded section) while being large
// enough that the 20k budget can hold strictly more than the 6k default. The MCP
// path is always preview, so this never affects the watermark.
const mcpDigestMaxItems = 500

// scheduleCommands maps each scheduled job to its `mora <args>` command line.
//
// pulse-daily is the ONLY caller that passes --advance (D-02/SC#4): it is the
// single surface that commits the delta watermark. The 08:00 calendar interval
// (launchdSchedule) plus the dropped RunAtLoad (see schedulePlistFor) make it a
// once-daily commit — a reboot/login no longer re-consumes the morning delta.
//
// Phase 13 (D13-5) makes pulse-daily the sole caller that opts into the
// sync-first + persist + notify trio: --sync refreshes enabled sources before the
// build (honest, non-aborting), --brief-file persists the dated vault artifact,
// and --notify posts the gated macOS toast. --write/--digest/--advance are
// preserved verbatim; --advance remains the ONLY watermark-commit surface (D-02).
var scheduleCommands = map[string]string{
	"pulse-daily":   "pulse --write --digest --advance --sync --brief-file --notify",
	"index-hourly":  "index rebuild",
	"backup-daily":  "backup",
	"lint-weekly":   "lint",
	"ingest-hourly": "ingest run --all",
	"git-daily":     "sync git",
}

type scheduleCommandRunner func(name string, args ...string) ([]byte, error)

var runScheduleCommand scheduleCommandRunner = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// renameReplaceWithRetry publishes tmp onto path via os.Rename, replacing any
// existing target. On POSIX this is a single atomic rename(2) and the loop runs
// exactly once. On Windows, os.Rename maps to MoveFileEx(MOVEFILE_REPLACE_
// EXISTING): replacing an existing target requires deleting it, so concurrent
// writers racing to rename onto the SAME target transiently fail with
// ERROR_ACCESS_DENIED / ERROR_SHARING_VIOLATION. Retry those with JITTERED, capped
// backoff — deterministic backoff makes racing writers retry in lockstep and keep
// colliding, so the jitter is what lets them de-correlate — up to a deadline. Only
// the error path pays this; a permanent error (or any non-Windows error) surfaces
// on the first attempt.
func renameReplaceWithRetry(tmp, path string) error {
	var deadline time.Time
	for attempt := 0; ; attempt++ {
		rerr := os.Rename(tmp, path)
		if rerr == nil {
			return nil
		}
		if !renameReplaceRetryable(rerr) {
			return rerr
		}
		if deadline.IsZero() {
			deadline = time.Now().Add(5 * time.Second)
		} else if !time.Now().Before(deadline) {
			return rerr
		}
		capMs := 1 << min(attempt, 5) // backoff ceiling grows 1,2,4,8,16,32,32… ms
		time.Sleep(time.Duration(1+mrand.IntN(capMs)) * time.Millisecond)
	}
}

// linkPublish is the create-exclusive publish primitive (defaults to os.Link).
// Seam: tests override it to simulate a filesystem that refuses hard links and so
// exercise atomicCreate's fallback path on an ordinary filesystem.
var linkPublish = os.Link

// atomicCreate publishes body at path with a CREATE-EXCLUSIVE guarantee: unlike
// atomicWrite (whose final os.Rename REPLACES any existing target, last-writer-
// wins), atomicCreate never clobbers an existing file — a second writer racing
// onto the SAME path fails with os.ErrExist so exactly one wins and the caller
// (createMemory) re-mints a fresh id.
//
// PRIMARY path: os.Link the staged temp onto path. Link is BOTH create-exclusive
// (fails EEXIST, never replaces) AND content-atomic (the published name appears
// fully formed), so there is no torn-read window. This mirrors loop.go's proven
// publishLockFile; on Windows os.Link maps to CreateHardLinkW, which likewise
// fails ERROR_ALREADY_EXISTS on a present target.
//
// FALLBACK path: some filesystems (exFAT/FAT32 USB sticks, some SMB/NFS mounts)
// do not support hard links, so os.Link returns EPERM/ENOTSUP/EOPNOTSUPP (POSIX)
// or ERROR_NOT_SUPPORTED (Windows) — never os.ErrExist. vault_dir is
// user-configurable, so a hard failure here would regress `mora write` / MCP
// write_memory below where the old atomicWrite (plain os.Rename) worked. On that
// (and only that) error class we preserve the no-clobber guarantee WITHOUT a hard
// link: (1) claim the path with os.OpenFile(O_CREATE|O_EXCL) — an atomic
// create-exclusive that fails EEXIST if a racer or a colliding id already owns it,
// so we surface os.ErrExist exactly like the link path; then (2) rename our staged
// temp onto our OWN claimed placeholder. The rename is safe from clobber because
// every same-path racer already lost at the O_EXCL claim, so it can only replace
// our own empty placeholder, never a rival's memory — and it keeps content
// atomicity (no torn frontmatter). TRADEOFF, documented honestly: between (1) and
// (2) a concurrent reader can observe an EMPTY placeholder file. That degrades
// gracefully — parseMemory returns "missing frontmatter" on it and every
// index/list/find caller (rebuildIndex, findMemory, listMemories, digest,
// meetingprep, graph, share) skips a parse error with `continue`, so the
// placeholder is ignored (never a crash) and picked up once the rename lands. Only
// no-hardlink filesystems ever reach this branch; POSIX/NTFS keep the pure-link
// path with no such window.
//
// A link error that is NEITHER os.ErrExist NOR the link-unsupported class is a
// real fault and surfaces as-is — never masked as a collision or silently routed
// through the slower fallback.
func atomicCreate(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Stage through a unique temp in the target dir (same filesystem, so the link
	// is a cheap same-inode operation), never a fixed name, so concurrent creators
	// never share or truncate each other's in-flight temp.
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// Drop the temp name whether we publish it or fail; a no-op leftover name once
	// the link/rename publishes the file under its real name.
	defer os.Remove(tmp)
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// CreateTemp opens at 0600; raise to the caller's requested mode before publish.
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}

	// PRIMARY: create-exclusive hard-link publish (POSIX + NTFS).
	err = linkPublish(tmp, path)
	if err == nil {
		return nil
	}
	if errors.Is(err, os.ErrExist) {
		return err // genuine id collision → caller re-mints
	}
	if !linkUnsupported(err) {
		return err // a real filesystem/IO error → surface, don't mask
	}

	// FALLBACK for filesystems without hard links (see doc above).
	claim, cerr := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if cerr != nil {
		return cerr // EEXIST here wraps os.ErrExist → caller re-mints; else the real error
	}
	// Close our placeholder handle BEFORE the rename: on Windows MoveFileEx must
	// delete the destination to replace it, and our own still-open handle would make
	// every retry hit a sharing violation.
	if closeErr := claim.Close(); closeErr != nil {
		return errors.Join(closeErr, os.Remove(path))
	}
	// Move the staged body onto our own placeholder. renameReplaceWithRetry carries
	// the Windows MoveFileEx jittered retry; on failure drop the empty placeholder so
	// a failed write leaves nothing behind — and surface (join) any cleanup error so
	// a leaked placeholder is never silently swallowed.
	if rerr := renameReplaceWithRetry(tmp, path); rerr != nil {
		return errors.Join(rerr, os.Remove(path))
	}
	return nil
}

// usageEvent records a single MCP tool invocation for local analytics.
type usageEvent struct {
	TS      string `json:"ts"`
	Tool    string `json:"tool"`
	Query   string `json:"query,omitempty"` // stripped by default; retained only when query logging is opted in; never sent off-machine
	Scope   string `json:"scope,omitempty"`
	Results int    `json:"results"`
	Millis  int64  `json:"millis"`
}

// randRead is the entropy seam (defaults to crypto/rand.Read). Tests override it
// to simulate an unavailable OS CSPRNG and exercise newID's fallback branch.
var randRead = rand.Read

// warnRandFallback surfaces (once per mint) that newID fell back off crypto/rand.
// Seam: tests replace it to observe that the fallback is not silent, without
// capturing the real os.Stderr.
var warnRandFallback = func() {
	fmt.Fprintln(os.Stderr, "warn: crypto/rand unavailable; deriving memory id suffix from math/rand (still unique, but not cryptographically random)")
}

// ftsStopwords are content-free English function words. They carry near-zero
// discriminative signal but, when OR-joined into the FTS MATCH, balloon the
// candidate pool and let documents that match several common words (while
// missing the rare, meaningful terms) rank competitively — "OR-dilution".
// Deliberately conservative: only true function words, NO question-content or
// borderline-topical words (actually/most/now/latest/plan/…) which can be
// discriminative in a real query.
var ftsStopwords = map[string]bool{
	"a": true, "about": true, "am": true, "an": true, "and": true, "are": true,
	"as": true, "at": true, "be": true, "been": true, "being": true, "but": true,
	"by": true, "can": true, "could": true, "did": true, "do": true, "does": true,
	"doing": true, "for": true, "from": true, "had": true, "has": true, "have": true,
	"how": true, "i": true, "if": true, "in": true, "into": true, "is": true,
	"it": true, "its": true, "me": true, "my": true, "of": true, "on": true,
	"or": true, "our": true, "so": true, "that": true, "the": true, "their": true,
	"them": true, "then": true, "there": true, "these": true, "they": true,
	"this": true, "to": true, "was": true, "we": true, "were": true, "what": true,
	"when": true, "which": true, "who": true, "will": true, "with": true,
	"would": true, "you": true, "your": true,
}

// docxMaxDecompressed caps the decompressed bytes read from word/document.xml. A
// .docx is a ZIP, so a few-KB file can decompress to gigabytes (a zip bomb); the
// LimitReader stops well before that. Past the cap the XML is truncated and the
// decoder errors — extractDocxText returns that error and the file is skipped.
const docxMaxDecompressed = 8 << 20 // 8 MiB
