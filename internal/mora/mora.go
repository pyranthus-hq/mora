package mora

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/huh"
	"github.com/mattn/go-isatty"
	"github.com/pyranthus-hq/mora/internal/applecal"
	"github.com/pyranthus-hq/mora/internal/google"
	"github.com/pyranthus-hq/mora/internal/imessage"
	"github.com/pyranthus-hq/mora/internal/memory"
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
}

// fusion returns the active RRF fusion params: the per-Config override when set,
// else the production default. Single source of truth for search + eval.
func (c Config) fusion() fusionParams {
	if c.fusionOv != nil {
		return *c.fusionOv
	}
	return defaultFusion
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

// IsEnabled centralizes the nil-sentinel handling for Source.Enabled so no
// caller dereferences the raw pointer. nil (legacy/unset) is normalized to true
// by loadSources before callers ever see it; an explicit *false stays disabled (D-12).
func (s Source) IsEnabled() bool { return s.Enabled != nil && *s.Enabled }

// ptr returns a pointer to b. Used to set Source.Enabled on freshly-constructed
// literals — leaving Enabled nil would grandfather to true on next load (D-11).
func ptr(b bool) *bool { return &b }

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

// isInteractive reports whether r is a real terminal (character device). It uses
// only the stdlib: in production stdin is *os.File (os.Stdin) and we check for
// ModeCharDevice; in tests/pipes stdin is a strings.Reader or a redirected file,
// so this returns false. This keeps interactive consent/menus from blocking on a
// non-TTY without adding a go-isatty dependency (deferred to Plan 04).
func isInteractive(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// lookupCatalog returns the catalog entry for ctype. The bool is false for any
// type not in the static catalog — callers MUST reject unknown types with an
// error (D-03 / ASVS V5), never silently no-op.
func lookupCatalog(ctype string) (connectorInfo, bool) {
	for _, c := range connectorCatalog {
		if c.Type == ctype {
			return c, true
		}
	}
	return connectorInfo{}, false
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
		return cmdIndex(ctx, args[1:], stdout)
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
  mora context --scope project:acme --query "auth" --budget 2000 --json
  mora think "what did Sam decide about the launch" --json   # cited evidence + gap analysis
  mora brief                       # the latest what-changed/what-matters brief (session-start default; local-only)
  mora brief --envelope --json     # add a synthesis prompt / emit structured {generated, body}
  mora index rebuild
  mora tasks sync --write
  mora tasks add "Reply to Sam about the launch" --pri P0   # capture an open loop (name first, then flags)
  mora tasks list --json                                    # the current live tasks
  mora tasks done "Set up Mora"    # mark a live task complete so it stops resurfacing as stale
  mora pulse --write --digest
  mora sources add filesystem --name docs --path ~/Documents --scope personal
  mora ingest run --source docs
  mora schedule install pulse-daily
  mora config context large        # context profile: small | default | large (budget + snippet density)
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
  mora upgrade                     # self-update to the latest release (brew installs: brew upgrade)
  mora version`)
}

func defaultConfig() Config {
	// MORA_CONFIG_DIR points an entire invocation at an ISOLATED install
	// (scripts, launchd jobs, demos, tests): config, vault, derived index, and
	// watermark state ALL default under the override. Re-rooting only the
	// config dir was not enough — a scratch `init` then rebuilt (wiped) the
	// LIVE ~/.local/share index.db and shared the live watermark state, the
	// exact incident class this env var exists to prevent. A config.toml
	// inside the override still wins for any dir it names (loadConfig
	// overlays).
	if dir := os.Getenv("MORA_CONFIG_DIR"); dir != "" {
		return Config{
			VaultDir:  filepath.Join(dir, "vault"),
			ConfigDir: dir,
			DataDir:   filepath.Join(dir, "data"),
			StateDir:  filepath.Join(dir, "state"),
		}
	}
	home, _ := os.UserHomeDir()
	return Config{
		VaultDir:  filepath.Join(home, "vault", "mora"),
		ConfigDir: filepath.Join(home, ".config", "mora"),
		DataDir:   filepath.Join(home, ".local", "share", "mora"),
		StateDir:  filepath.Join(home, ".local", "state", "mora"),
	}
}

// parseConfigValue extracts a config value from the raw right-hand side of a
// `key = value` line. A quoted value parses via strconv.Unquote (escapes
// honored) and anything after the closing quote — an inline comment — is
// ignored; the old strip-outer-quotes approach loaded `"/x" # note` as the
// garbage path `/x" # note`, which the read-modify-write writeConfig then
// persisted back, orphaning the real vault. Hand-editing config.toml is a
// path our own refusal messages recommend, so it must parse exactly. An
// unquoted value cuts at the first '#'.
func parseConfigValue(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, `"`) {
		for i := 1; i < len(raw); i++ {
			switch raw[i] {
			case '\\':
				i++ // skip the escaped byte
			case '"':
				if v, err := strconv.Unquote(raw[:i+1]); err == nil {
					return v
				}
				return strings.Trim(raw[:i+1], `"`)
			}
		}
		return strings.Trim(raw, `"`) // unterminated quote: legacy lenient read
	}
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSpace(raw)
}

func loadConfig() (Config, error) {
	cfg := defaultConfig()
	path := filepath.Join(cfg.ConfigDir, "config.toml")
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := expandHome(parseConfigValue(parts[1]))
		switch key {
		case "vault_dir":
			cfg.VaultDir = val
		case "data_dir":
			cfg.DataDir = val
		case "state_dir":
			cfg.StateDir = val
		case "embedder":
			cfg.Embedder = val
		case "context":
			cfg.ContextProfile = val
		}
	}
	return cfg, nil
}

// cmdConfig is the durable-settings surface: `mora config` shows the resolved
// configuration; `mora config context <small|default|large>` sets the context
// profile (the quality/size knob — small for lean agent windows, large for
// denser briefs/digests whose conversation tails survive the snippet clip);
// `mora config embedder <ollama|static>` is the same durable seam the retrieval
// docs point at. "default"/"static" reset by DROPPING the key rather than
// persisting a redundant value, so config.toml stays minimal.
func cmdConfig(args []string, stdout io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		profile := cfg.ContextProfile
		if profile == "" {
			profile = "default"
		}
		embedder := cfg.Embedder
		if embedder == "" {
			embedder = "static"
		}
		fmt.Fprintf(stdout, "vault_dir = %s\ndata_dir  = %s\nstate_dir = %s\nembedder  = %s\ncontext   = %s  (default budget %d tokens, digest snippets %d chars; ceiling %d)\n",
			cfg.VaultDir, cfg.DataDir, cfg.StateDir, embedder, profile,
			cfg.contextDefaultTokens(), cfg.digestSnippetChars(), cfg.contextMaxTokens())
		return nil
	}
	if len(args) != 2 {
		return errors.New("usage: mora config [context <small|default|large> | embedder <ollama|static>]")
	}
	key, val := args[0], strings.ToLower(strings.TrimSpace(args[1]))
	switch key {
	case "context":
		switch val {
		case "small", "large":
			cfg.ContextProfile = val
		case "default":
			cfg.ContextProfile = ""
		default:
			return fmt.Errorf("unknown context profile %q (want small, default, or large)", val)
		}
	case "embedder":
		switch val {
		case "ollama":
			cfg.Embedder = val
		case "static", "default":
			cfg.Embedder = ""
		default:
			return fmt.Errorf("unknown embedder %q (want ollama or static)", val)
		}
	default:
		return fmt.Errorf("unknown config key %q (want context or embedder)", key)
	}
	if err := writeConfig(cfg); err != nil {
		return err
	}
	shown := val
	fmt.Fprintf(stdout, "%s = %s\n", key, shown)
	if key == "context" {
		fmt.Fprintf(stdout, "(default budget %d tokens, digest snippets %d chars; per-call max_tokens still wins, ceiling %d)\n",
			cfg.contextDefaultTokens(), cfg.digestSnippetChars(), cfg.contextMaxTokens())
	}
	return nil
}

// writeConfig persists the five keys this binary owns by READ-MODIFY-WRITE:
// every line it does not own (comments, blank lines, keys written by hand or
// by a newer mora) is preserved byte-for-byte. The old regenerate-from-struct
// behavior silently ate those lines on every rewrite — loadConfig skips
// unknowns, so they survived the load only to vanish on the next save. An
// empty Embedder/ContextProfile DROPS its line (reset-to-default semantics,
// keeping config.toml minimal); an empty DIR value is broken either way but
// is preserved verbatim — dropping it would silently repoint the install to
// the defaults via an unrelated rewrite.
func writeConfig(cfg Config) error {
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(cfg.ConfigDir, "config.toml")
	owned := []struct{ key, val string }{
		{"vault_dir", cfg.VaultDir},
		{"data_dir", cfg.DataDir},
		{"state_dir", cfg.StateDir},
		{"embedder", cfg.Embedder},
		{"context", cfg.ContextProfile},
	}
	ownedVal := func(key string) (string, bool) {
		for _, kv := range owned {
			if kv.key == key {
				return kv.val, true
			}
		}
		return "", false
	}

	var existing []string
	if b, err := os.ReadFile(path); err == nil {
		existing = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		if len(existing) == 1 && existing[0] == "" {
			existing = nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	written := map[string]bool{}
	var out []string
	for _, line := range existing {
		trimmed := strings.TrimSpace(line)
		key := ""
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			if parts := strings.SplitN(trimmed, "=", 2); len(parts) == 2 {
				key = strings.TrimSpace(parts[0])
			}
		}
		val, owns := ownedVal(key)
		if !owns {
			out = append(out, line) // not ours: preserve verbatim
			continue
		}
		if written[key] {
			continue // collapse duplicate owned keys onto the first occurrence
		}
		written[key] = true
		if val == "" {
			if key == "embedder" || key == "context" {
				continue // reset-to-default: drop the line
			}
			out = append(out, line) // empty dir value: preserve, never silently repoint
			continue
		}
		out = append(out, fmt.Sprintf("%s = %q", key, val))
	}
	for _, kv := range owned {
		if kv.val == "" || written[kv.key] {
			continue
		}
		out = append(out, fmt.Sprintf("%s = %q", kv.key, kv.val))
	}
	return atomicWrite(path, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}

func cmdInit(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	vault := fs.String("vault", "", "vault directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Preserve an existing install's config (vault_dir/data_dir/state_dir) so a
	// re-run of `init` never repoints Mora away from a custom vault and orphans
	// it. loadConfig returns defaults when no config.toml exists (first-time
	// init), so brand-new setups still scaffold at the default location.
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if *vault != "" {
		want := expandHome(*vault)
		// Repointing an EXISTING install's vault orphans the current one from
		// Mora's view — it must never happen as a side effect of a scripted
		// init (two live incidents). Same-dir re-init stays idempotent, and
		// the comparison cleans both sides so a trailing slash (shell tab
		// completion, install.sh's MORA_VAULT) is not misread as a repoint.
		if filepath.Clean(cfg.VaultDir) != filepath.Clean(want) && configFileExists(cfg) {
			if err := confirmVaultRepoint(stdin, stdout, cfg.VaultDir, want); err != nil {
				return err
			}
		}
		cfg.VaultDir = want
	}
	for _, dir := range []string{cfg.VaultDir, cfg.ConfigDir, cfg.DataDir, cfg.StateDir, memoriesRoot(cfg), sourcesRoot(cfg), filepath.Join(cfg.ConfigDir, "tokens")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if err := writeConfig(cfg); err != nil {
		return err
	}
	if err := scaffoldControlFiles(cfg); err != nil {
		return err
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "initialized Mora vault at %s\n", cfg.VaultDir)
	// D-08: launch the interactive connector setup menu on a real TTY; on a
	// non-TTY (scripts, CI, tests) runSetupMenu prints a hint and returns.
	return runSetupMenu(ctx, cfg, stdin, stdout)
}

// configFileExists reports whether a config.toml is already on disk —
// loadConfig alone can't distinguish "defaults because no file" from a real
// install, and the repoint guard must only fire for the latter.
func configFileExists(cfg Config) bool {
	_, err := os.Stat(filepath.Join(cfg.ConfigDir, "config.toml"))
	return err == nil
}

// confirmVaultRepoint gates `init --vault <new>` when config.toml already
// points elsewhere. Non-interactive callers are refused with the exact manual
// alternative (a script must never silently repoint a live install); a TTY
// gets an explicit default-NO confirm, mirroring runSetupMenu's gate.
func confirmVaultRepoint(stdin io.Reader, stdout io.Writer, from, to string) error {
	f, ok := stdin.(*os.File)
	if !ok || !isatty.IsTerminal(f.Fd()) {
		return fmt.Errorf("refusing to repoint the vault non-interactively: config.toml already points at %s (requested: %s) — re-run `mora init --vault` in a terminal to confirm, or edit config.toml yourself", from, to)
	}
	var yes bool
	confirm := huh.NewConfirm().
		Title(fmt.Sprintf("Repoint vault from %s to %s?", from, to)).
		Description("The current vault stays on disk, but Mora stops reading it.").
		Affirmative("Repoint").
		Negative("Keep current vault").
		Value(&yes)
	if err := confirm.Run(); err != nil {
		return err
	}
	if !yes {
		fmt.Fprintln(stdout, "init cancelled — vault unchanged.")
		return errors.New("init cancelled — vault unchanged")
	}
	return nil
}

func scaffoldControlFiles(cfg Config) error {
	files := map[string]string{
		"index.md":           "# Mora Index\n\n> Generated by `mora index rebuild`.\n",
		"priority-map.md":    defaultPriorityMap(),
		"live-tasks.md":      defaultLiveTasks(),
		"heartbeat.md":       defaultHeartbeat(),
		"auto-resolver.md":   defaultAutoResolver(),
		"log.md":             "# Mora Log\n\n",
		"meetings/ledger.md": "# Meeting Ledger\n\n> Append-only decisions and action items.\n\n",
	}
	for rel, body := range files {
		path := filepath.Join(cfg.VaultDir, rel)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := atomicWrite(path, []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func cmdWrite(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("write", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	scope := fs.String("scope", "global", "scope")
	mtype := fs.String("type", "insight", "memory type")
	title := fs.String("title", "", "title")
	text := fs.String("text", "", "text")
	tags := fs.String("tags", "", "comma-separated tags")
	source := fs.String("source", "manual", "source")
	jsonOut := fs.Bool("json", false, "json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *text == "" && fs.NArg() > 0 {
		*text = strings.Join(fs.Args(), " ")
	}
	if *title == "" || *text == "" {
		return errors.New("--title and --text are required")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	m := Memory{ID: newID(), Scope: *scope, Type: *mtype, Title: *title, Tags: splitCSV(*tags), Source: *source, CreatedAt: time.Now().Format(time.RFC3339), Text: *text}
	if err := writeMemory(cfg, m); err != nil {
		return err
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		return err
	}
	m.Path = memoryPath(cfg, m)
	return emit(stdout, m, *jsonOut)
}

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

func cmdRead(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "json")
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("read requires memory id")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	m, err := findMemory(cfg, fs.Arg(0))
	if err != nil {
		return err
	}
	return emit(stdout, m, *jsonOut)
}

func cmdList(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	scope := fs.String("scope", "", "scope")
	limit := fs.Int("limit", 20, "limit")
	jsonOut := fs.Bool("json", false, "json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	items, err := listMemories(cfg, *scope, *limit)
	if err != nil {
		return err
	}
	return emit(stdout, items, *jsonOut)
}

func cmdSearch(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) >= 1 && isHelpFlag(args[0]) {
		fmt.Fprintln(stdout, "usage: mora search <query> [--scope S] [--limit N] [--json]")
		return nil
	}
	scope, limit, jsonOut, queryArgs, err := parseSearchArgs(args)
	if err != nil {
		return err
	}
	if len(queryArgs) < 1 {
		return errors.New("search requires query")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	items, err := defaultSearch(ctx, cfg, strings.Join(queryArgs, " "), scope, limit)
	if err != nil {
		return err
	}
	return emit(stdout, items, jsonOut)
}

func cmdDelete(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	yes := fs.Bool("yes", false, "yes")
	if err := fs.Parse(flagsFirst(args)); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("delete requires memory id")
	}
	if !*yes {
		return errors.New("refusing to delete without --yes")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	m, err := findMemory(cfg, fs.Arg(0))
	if err != nil {
		return err
	}
	if err := os.Remove(m.Path); err != nil {
		return err
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "deleted %s\n", m.ID)
	return nil
}

func cmdContext(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("context", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	scope := fs.String("scope", "", "scope")
	query := fs.String("query", "", "query")
	budget := fs.Int("budget", 2000, "character budget")
	jsonOut := fs.Bool("json", false, "json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	var items []Memory
	if *query != "" {
		items, err = hybridSearch(ctx, cfg, *query, *scope, 10)
	} else {
		items, err = listMemories(cfg, *scope, 10)
	}
	if err != nil {
		return err
	}
	text := buildContext(cfg, items, *budget, *query != "")
	if *jsonOut {
		return emit(stdout, map[string]any{"context": text, "items": items}, true)
	}
	fmt.Fprint(stdout, text)
	return nil
}

// cmdThink implements `mora think "<query>" [--scope s] [--limit n] [--json]`:
// the I3 synthesis envelope (hybrid evidence + deterministic gap analysis + a
// synthesis prompt the calling agent's model runs). The deterministic floor is
// fully useful with no model attached.
func cmdThink(ctx context.Context, args []string, stdout io.Writer) error {
	jsonOut := false
	scope := ""
	limit := 8
	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOut = true
		case "--scope":
			if i+1 < len(args) {
				i++
				scope = args[i]
			}
		case "--limit":
			if i+1 < len(args) {
				i++
				if n, perr := strconv.Atoi(args[i]); perr == nil && n > 0 {
					limit = n
				}
			}
		default:
			positional = append(positional, args[i])
		}
	}
	query := strings.TrimSpace(strings.Join(positional, " "))
	if query == "" {
		return errors.New(`usage: mora think "<question>" [--scope s] [--limit n] [--json]`)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	res, err := buildThink(ctx, cfg, query, scope, limit, time.Now())
	if err != nil {
		return err
	}
	logUsage(cfg, usageEvent{Tool: "think", Query: query, Scope: scope, Results: len(res.Evidence)})
	if jsonOut {
		return emit(stdout, res, true)
	}
	printThink(stdout, res)
	return nil
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
	opts := briefOpts{scope: *scope, sinceDays: clampSinceDays(*sinceDays)}
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
		fmt.Fprintln(stdout, digestSynthesisPrompt(d.Sections, buildSourceStates(cfg, d)))
	}
	return nil
}

func printThink(w io.Writer, res ThinkResult) {
	fmt.Fprintf(w, "Q: %s\n\nEvidence (%d):\n", res.Query, len(res.Evidence))
	for _, e := range res.Evidence {
		fmt.Fprintf(w, "  [%s] %s — %s\n", e.StableID, e.Title, e.Snippet)
	}
	if res.Gaps.empty() {
		fmt.Fprintln(w, "\nGaps: none detected.")
	} else {
		fmt.Fprintln(w, "\nWhat the vault does NOT know:")
		for _, s := range res.Gaps.Stale {
			fmt.Fprintf(w, "  · %s\n", s)
		}
		for _, s := range res.Gaps.ThinCoverage {
			fmt.Fprintf(w, "  · %s\n", s)
		}
		for _, s := range res.Gaps.CoverageHoles {
			fmt.Fprintf(w, "  · %s\n", s)
		}
	}
	fmt.Fprintln(w, "\n(Pass this evidence + gaps to your agent, or run `mora think … --json` for the synthesis prompt.)")
}

func cmdIndex(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] != "rebuild" {
		return errors.New("usage: mora index rebuild")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	count, err := rebuildIndex(ctx, cfg)
	if err != nil {
		return err
	}
	if err := writeWikiIndex(cfg, count); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "indexed %d memories\n", count)
	return nil
}

func cmdTasks(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mora tasks <sync [--write] | add <name> [flags] | done <name> | list [--json]>")
	}
	switch args[0] {
	case "sync":
		fs := flag.NewFlagSet("tasks sync", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		write := fs.Bool("write", false, "write")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		added, err := syncTasks(cfg, *write)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "tasks added: %d\n", added)
		return nil
	case "add":
		// Contract: the (quoted) task name is the first positional; flags follow it
		// (`tasks add "<name>" [--pri ...]`). Parsing flags from args[2:] avoids
		// Go's flag pkg stopping at the first non-flag arg, which would otherwise
		// fold a trailing `--pri P0` into the name.
		usage := errors.New("usage: mora tasks add <name> [--pri P1] [--domain ...] [--owner ...] [--horizon ...] [--blocker ...]")
		if len(args) < 2 {
			return usage
		}
		name := strings.TrimSpace(args[1])
		fs := flag.NewFlagSet("tasks add", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		domain := fs.String("domain", "memory", "domain")
		owner := fs.String("owner", "you", "owner")
		pri := fs.String("pri", "P1", "priority (P0|P1|P2)")
		horizon := fs.String("horizon", "this week", "horizon")
		blocker := fs.String("blocker", "None", "blocker")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if name == "" {
			return usage
		}
		// The task name is the row identity and a "|" would break the table, so
		// reject it rather than silently corrupt live-tasks.md.
		if strings.Contains(name, "|") {
			return errors.New("task name must not contain '|'")
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		added, err := addTask(cfg, LiveTask{Task: name, Domain: *domain, Owner: *owner, Pri: *pri, Horizon: *horizon, Blocker: *blocker})
		if err != nil {
			return err
		}
		if !added {
			fmt.Fprintf(stdout, "task exists: %s\n", name)
			return nil
		}
		fmt.Fprintf(stdout, "task added: %s\n", name)
		return nil
	case "list":
		fs := flag.NewFlagSet("tasks list", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		asJSON := fs.Bool("json", false, "json")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		tasks, err := listTasks(cfg)
		if err != nil {
			return err
		}
		if *asJSON {
			b, err := json.Marshal(tasks)
			if err != nil {
				return err
			}
			fmt.Fprintln(stdout, string(b))
			return nil
		}
		for _, lt := range tasks {
			fmt.Fprintf(stdout, "%-8s %-10s %s\n", lt.Pri, lt.Status, lt.Task)
		}
		return nil
	case "done":
		name := strings.TrimSpace(strings.Join(args[1:], " "))
		if name == "" {
			return errors.New("usage: mora tasks done <name>")
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		updated, err := markTaskDone(cfg, name)
		if err != nil {
			return err
		}
		if updated == 0 {
			return fmt.Errorf("no live task matched %q (it must already be a row in live-tasks.md — run `mora tasks sync --write` to seed P0 items first)", name)
		}
		// Task name is the row identity (no task IDs; syncTasks dedups by name).
		// Surface the count so closing multiple same-named rows is never silent.
		if updated > 1 {
			fmt.Fprintf(stdout, "task done: %s (%d rows)\n", name, updated)
		} else {
			fmt.Fprintf(stdout, "task done: %s\n", name)
		}
		return nil
	default:
		return errors.New("usage: mora tasks <sync [--write] | add <name> [flags] | done <name> | list [--json]>")
	}
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
		d, derr := buildDigest(cfg, now, opts)
		if derr != nil {
			return derr
		}
		// renderDigest is the data path (also what the MCP `digest` tool returns,
		// byte-identical). styleDigestTTY is a TTY-only presentation skin on top;
		// pipes, redirects, and the MCP transport get the raw Markdown unchanged.
		out := renderDigest(d, defaultContextTokens*charsPerToken)
		out = styleDigestTTY(out, newStyler(stdout, false))
		fmt.Fprintln(stdout, out)
		// --envelope (15-02): preview-only append AFTER the brief — the human path
		// renders the FULL digest, so the prompt cites the SAME rendered items
		// (d.Sections, no separate item-budgeting). It only Fprintln's an additional
		// block; the brief above + the persisted artifact below are untouched
		// (T-15-07: no watermark, no artifact mutation). Model-free: digestSynthesisPrompt
		// is a pure string builder — Mora makes no model/network call (SC#2).
		if *envelope {
			fmt.Fprintln(stdout, digestSynthesisPrompt(d.Sections, buildSourceStates(cfg, d)))
		}
		// Persist + notify (D13-5) — side effects of the human/scheduled stdout path
		// ONLY (the MCP `digest` tool and any --json path are untouched, T-13-11).
		// Both default OFF so ad-hoc `pulse --digest` is byte-for-byte unchanged.
		if *briefFile {
			// Persist the SAME render to the dated vault artifact using the SAME now
			// (so the artifact date matches the digest). A write error is non-fatal —
			// the brief already printed; a partial honest brief beats no brief (T-13-12).
			path, werr := writeBriefArtifact(cfg, d, now)
			if werr != nil {
				warnf(stdout, "could not persist the brief artifact: %v", werr)
			} else if *notify {
				// Notify is the LAST step and best-effort: only fires when a brief was
				// actually persisted (we have a path to point at), and notifyBriefFn
				// (notify.go) is GOOS/env-gated and swallows its own error.
				_ = notifyBriefFn(path)
			}
		}
	}
	return nil
}

func cmdLint(ctx context.Context, args []string, stdout io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	required := []string{"index.md", "priority-map.md", "live-tasks.md", "heartbeat.md", "auto-resolver.md", "meetings/ledger.md", "log.md"}
	var issues []string
	for _, rel := range required {
		if _, err := os.Stat(filepath.Join(cfg.VaultDir, rel)); err != nil {
			issues = append(issues, "missing "+rel)
		}
	}
	if _, err := os.Stat(filepath.Join(cfg.ConfigDir, "tokens")); err == nil {
		if strings.HasPrefix(filepath.Join(cfg.ConfigDir, "tokens"), cfg.VaultDir) {
			issues = append(issues, "tokens directory is inside vault")
		}
	}
	if len(issues) == 0 {
		fmt.Fprintln(stdout, "lint ok")
		return nil
	}
	for _, issue := range issues {
		fmt.Fprintln(stdout, issue)
	}
	return nil
}

func cmdBackup(ctx context.Context, args []string, stdout io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "backups"), 0o700); err != nil {
		return err
	}
	out := filepath.Join(cfg.StateDir, "backups", "mora-"+time.Now().Format("20060102-150405")+".tar.gz")
	if err := tarGz(out, cfg.VaultDir); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s\n", out)
	return nil
}

func resolveReal(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

func disjointRealPaths(vault, tokenDir string) bool {
	rv := resolveReal(vault)
	rt := resolveReal(tokenDir)
	return !strings.HasPrefix(rt+string(os.PathSeparator), rv+string(os.PathSeparator)) && rt != rv
}

func looksSynced(p string) bool {
	markers := []string{"com~apple~CloudDocs", "Dropbox", "Google Drive", "OneDrive", "Sync"}
	for _, m := range markers {
		if strings.Contains(p, m) {
			return true
		}
	}
	return false
}

func cmdDoctor(ctx context.Context, args []string, stdout io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	checks := map[string]bool{}
	_, err = os.Stat(cfg.VaultDir)
	checks["vault"] = err == nil
	_, err = os.Stat(dbPath(cfg))
	checks["index_db"] = err == nil
	_, err = os.Stat(filepath.Join(cfg.ConfigDir, "tokens"))
	checks["token_dir"] = err == nil && !strings.HasPrefix(filepath.Join(cfg.ConfigDir, "tokens"), cfg.VaultDir)
	sources, _ := loadSources(cfg)
	checks["sources_config"] = len(sources) > 0
	tokenDir := filepath.Join(cfg.ConfigDir, "tokens")
	checks["tokens_disjoint_from_vault"] = disjointRealPaths(cfg.VaultDir, tokenDir)
	sty := newStyler(stdout, false)
	if looksSynced(tokenDir) {
		fmt.Fprintf(stdout, "%s token dir looks like a synced location: %s\n", sty.warn("warn"), tokenDir)
	}
	for k, ok := range checks {
		if ok {
			fmt.Fprintf(stdout, "%s %s\n", sty.ok("ok  "), k)
		} else {
			fmt.Fprintf(stdout, "%s %s\n", sty.warn("warn"), k)
		}
	}
	// Storage footprint vs Neil's target/ceiling — the visibility he asked for.
	// We report only; Mora never deletes or caps automatically.
	used := vaultStorageBytes(cfg)
	st := storageStatus(used)
	prefix := sty.ok("ok  ")
	if st != "ok" {
		prefix = sty.warn("warn")
	}
	fmt.Fprintf(stdout, "%s storage %s used (target ≤ %s, ceiling %s)\n",
		prefix, formatBytes(used), formatBytes(storageTargetBytes), formatBytes(storageCeilingBytes))
	if st == "over" {
		fmt.Fprintf(stdout, "     over the %s ceiling — consider pruning old sources or narrowing backfill windows (--since-days).\n", formatBytes(storageCeilingBytes))
	}
	// Git-sync disclosure (issue #6): if the vault is a git repo, it can leave the
	// device on push — qualify the zero-egress posture loudly and honestly.
	if _, err := os.Stat(filepath.Join(cfg.VaultDir, ".git")); err == nil {
		fmt.Fprintf(stdout, "%s vault git-sync is configured — the vault LEAVES THIS DEVICE on `mora sync git`\n", sty.warn("warn"))
		fmt.Fprintln(stdout, "     it contains decoded iMessages + Gmail in plaintext; ensure the remote is PRIVATE + user-controlled.")
	}
	// Google auth recency: tokens last weeks so a reauth is rare and invisible —
	// surface "last authed / how long ago" per connected account so the user can
	// tell at a glance when they last signed in.
	printGoogleAuthRecency(cfg, stdout, time.Now())
	// iMessage readiness prints in a dedicated ORDERED block (the checks map above is
	// unordered) so the Full Disk Access guidance reads top-to-bottom (Surface 3).
	printIMessageReadiness(stdout, false)
	return nil
}

// printGoogleAuthRecency reports, per connected Google account, when the user
// last completed an auth/reauth (recorded by google.SaveToken). Connected
// accounts are the token files in the tokens dir — one per account, the same
// derivation google.LastAuth uses (filename minus ".json", with "google" being
// the default/legacy account). Additive and non-fatal: any error is swallowed.
func printGoogleAuthRecency(cfg Config, stdout io.Writer, now time.Time) {
	tokenDir := filepath.Join(cfg.ConfigDir, "tokens")
	entries, err := os.ReadDir(tokenDir)
	if err != nil {
		return // no tokens dir yet => nothing connected
	}
	var accounts []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		accounts = append(accounts, strings.TrimSuffix(name, ".json"))
	}
	sort.Strings(accounts) // deterministic order
	sty := newStyler(stdout, false)
	for _, account := range accounts {
		at, ok, err := google.LastAuth(tokenDir, account)
		if err != nil {
			continue
		}
		if !ok {
			fmt.Fprintf(stdout, "%s google auth (%s): no recorded auth yet (run `mora connect google`)\n",
				sty.warn("warn"), account)
			continue
		}
		fmt.Fprintf(stdout, "%s google auth (%s): last authed %s (%s)\n",
			sty.ok("ok  "), account, at.Format(time.RFC3339), humanizeAgo(now.Sub(at)))
	}
}

// humanizeAgo renders a duration as a coarse "N <unit> ago" string for the
// doctor auth line. Sub-day durations collapse to hours/minutes so a fresh
// reauth doesn't read as "0 days ago".
func humanizeAgo(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d / time.Minute)
		return fmt.Sprintf("%d %s ago", m, plural(m, "minute"))
	case d < 24*time.Hour:
		h := int(d / time.Hour)
		return fmt.Sprintf("%d %s ago", h, plural(h, "hour"))
	default:
		days := int(d / (24 * time.Hour))
		return fmt.Sprintf("%d %s ago", days, plural(days, "day"))
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return unit
	}
	return unit + "s"
}

// Storage budget thresholds from Neil's pilot ask: a 2-3 GB target and a 10-15 GB
// hard ceiling. `mora doctor` reports the live footprint against these so the user
// has the visibility he wanted; Mora never deletes or caps automatically.
const (
	storageTargetBytes  = 3 * (1 << 30)  // 3 GiB soft target
	storageCeilingBytes = 15 * (1 << 30) // 15 GiB hard ceiling
)

// dirBytes returns the total size of regular files under root (recursive,
// best-effort: a missing root and unreadable entries contribute 0).
func dirBytes(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, e := d.Info(); e == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// vaultStorageBytes is Mora's on-disk footprint: the human-readable vault plus
// the SQLite index (static embeddings live inside the index DB). The DB is added
// only when it lives OUTSIDE the vault — if data_dir is configured inside
// vault_dir, dirBytes already walked it and adding it again would double-count.
func vaultStorageBytes(cfg Config) int64 {
	total := dirBytes(cfg.VaultDir)
	db := dbPath(cfg)
	if info, err := os.Stat(db); err == nil {
		rv := resolveReal(cfg.VaultDir)
		if !strings.HasPrefix(resolveReal(db), rv+string(os.PathSeparator)) {
			total += info.Size()
		}
	}
	return total
}

// storageStatus classifies a footprint: ok up to the target, warn between target
// and ceiling, over past the ceiling.
func storageStatus(b int64) string {
	switch {
	case b > storageCeilingBytes:
		return "over"
	case b > storageTargetBytes:
		return "warn"
	default:
		return "ok"
	}
}

// formatBytes renders a byte count as a human-readable binary unit (B, KiB, …).
func formatBytes(n int64) string {
	const unit = 1 << 10
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// printIMessageReadiness probes and prints the three iMessage readiness checks plus
// the Full Disk Access guidance per UI-SPEC Surface 3 (IMSG-08). The FDA signal is a
// REAL read probe (imessage.ProbeReadable — open+read one row), never os.Stat: a
// present-but-unreadable chat.db is exactly the FDA-denied case. setupVariant points
// the recovery loop's final step all the way to data (`mora sync imessage`) when shown
// inline during `connectors setup`. Returns true only when all three checks pass.
func printIMessageReadiness(stdout io.Writer, setupVariant bool) bool {
	if runtime.GOOS != "darwin" {
		fmt.Fprintln(stdout, "warn imessage_macos")
		fmt.Fprintf(stdout, "iMessage ingest only runs on macOS — skipping chat.db checks on %s.\n", runtime.GOOS)
		return false
	}
	fmt.Fprintln(stdout, "ok   imessage_macos")

	path := chatDBPath()
	dbExists := false
	if _, err := os.Stat(path); err == nil {
		dbExists = true
	}
	if dbExists {
		fmt.Fprintln(stdout, "ok   imessage_chat_db")
	} else {
		fmt.Fprintln(stdout, "warn imessage_chat_db")
	}

	readable := false
	if dbExists {
		readable, _ = imessage.ProbeReadable(path)
	}
	if readable {
		fmt.Fprintln(stdout, "ok   imessage_full_disk_access")
		fmt.Fprintln(stdout, "iMessage is ready to sync. Run `mora sync imessage`.")
		return true
	}

	fmt.Fprintln(stdout, "warn imessage_full_disk_access")
	fmt.Fprintln(stdout)
	if !dbExists {
		// No chat.db at all (e.g. Messages never set up). Honest about the cause.
		fmt.Fprintln(stdout, "No Messages database found at ~/Library/Messages/chat.db.")
		fmt.Fprintln(stdout, "Open the Messages app and sign in to iMessage, then re-run `mora doctor`.")
		return false
	}
	finalStep := "  4. Re-run `mora doctor` to confirm."
	if setupVariant {
		finalStep = "  4. Re-run `mora doctor` to confirm, then `mora sync imessage`."
	}
	fmt.Fprintln(stdout, "iMessage needs Full Disk Access to read your Messages database.")
	fmt.Fprintln(stdout, "chat.db exists but could not be read (permission denied) — Full Disk Access is not granted.")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "To grant it:")
	fmt.Fprintln(stdout, "  1. Open System Settings → Privacy & Security → Full Disk Access.")
	fmt.Fprintln(stdout, "  2. Click the + button and add your terminal app (Terminal, iTerm, or your editor).")
	fmt.Fprintln(stdout, "     If it is already listed, toggle it OFF and back ON.")
	fmt.Fprintln(stdout, "  3. Fully quit and reopen that terminal app.")
	fmt.Fprintln(stdout, finalStep)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Mora only ever READS the database — it never writes to or modifies your Messages.")
	return false
}

func cmdSchedule(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mora schedule install|list")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		return listSchedules(stdout, cfg)
	case "install":
		if len(args) != 2 {
			return errors.New("usage: mora schedule install <pulse-daily|index-hourly|backup-daily|lint-weekly|ingest-hourly|git-daily>")
		}
		return installSchedule(stdout, cfg, args[1])
	default:
		return errors.New("usage: mora schedule install|list")
	}
}

func cmdSources(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mora sources add|list")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		sources, err := loadSources(cfg)
		if err != nil {
			return err
		}
		return emit(stdout, sources, true)
	case "add":
		return addSource(cfg, args[1:], stdout)
	default:
		return errors.New("usage: mora sources add|list")
	}
}

// cmdConnectors dispatches the connector-registry command group:
// `mora connectors list|enable|disable|setup`. It mirrors cmdSources' shape
// (arg-0 switch, loadConfig up front). stdin is threaded for the Plan-04 setup
// menu; the OAuth consent path reads NO stdin (browser loopback).
func cmdConnectors(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) error {
	if len(args) == 0 {
		return errors.New("usage: mora connectors list|enable|disable|setup")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("connectors list", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		jsonOut := fs.Bool("json", false, "json output")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		sources, err := loadSources(cfg)
		if err != nil {
			return err
		}
		// Per-type enabled state (D-02): a catalog type enabled iff some source
		// row of that Type is enabled; absent => false.
		enabledByType := map[string]bool{}
		for _, s := range sources {
			if s.IsEnabled() {
				enabledByType[s.Type] = true
			}
		}
		rows := make([]catalogRow, 0, len(connectorCatalog))
		for _, c := range connectorCatalog {
			rows = append(rows, catalogRow{
				Type:      c.Type,
				Name:      c.DisplayName,
				Enabled:   enabledByType[c.Type],
				NeedsAuth: c.NeedsAuth,
			})
		}
		return emit(stdout, rows, *jsonOut)
	case "enable":
		if len(args) != 2 {
			return errors.New("usage: mora connectors enable <type>")
		}
		return enableConnector(ctx, cfg, args[1], stdout, stdin)
	case "disable":
		if len(args) != 2 {
			return errors.New("usage: mora connectors disable <type>")
		}
		return disableConnector(cfg, args[1], stdout)
	case "setup":
		// D-08: re-open the same interactive setup menu anytime.
		return runSetupMenu(ctx, cfg, stdin, stdout)
	default:
		return errors.New("usage: mora connectors list|enable|disable|setup")
	}
}

// setSourceEnabled loads sources, flips Enabled for every source whose Type
// matches ctype (D-02 per-type), and persists atomically via saveSources (0600,
// D-10 — no new storage file). If no source row exists yet for ctype, one is
// created so an auth-less type (e.g. filesystem) can be enabled before it has a
// configured source row. Mirrors the addSource load → rebuild → save idiom but
// matches by Type and flips the bit instead of replacing.
// setSourceSinceDays persists the gmail backfill window (in days) onto the
// matching source row so `connect --since-days N` carries over to later syncs.
// No-op if the source row does not exist yet.
// setSourceEnabledByName flips one source row by NAME (the multi-account
// connect path: "gmail-work" must enable exactly that mailbox's row, while the
// type-matching setSourceEnabled would flip BOTH accounts' rows — or mint a
// bogus row with the suffixed name as its Type). Errors on a missing name: the
// connect flow runs ensureGoogleSources first, so absence is a real bug.
func setSourceEnabledByName(cfg Config, name string, enabled bool) error {
	sources, err := loadSources(cfg)
	if err != nil {
		return err
	}
	for i := range sources {
		if sources[i].Name == name {
			sources[i].Enabled = ptr(enabled)
			return saveSources(cfg, sources)
		}
	}
	return fmt.Errorf("no source named %q", name)
}

// setSourceSinceDaysByName mirrors setSourceEnabledByName for the window
// override — account-scoped, never the whole type family.
func setSourceSinceDaysByName(cfg Config, name string, days int) error {
	sources, err := loadSources(cfg)
	if err != nil {
		return err
	}
	for i := range sources {
		if sources[i].Name == name {
			sources[i].SinceDays = days
			return saveSources(cfg, sources)
		}
	}
	return fmt.Errorf("no source named %q", name)
}

func setSourceSinceDays(cfg Config, ctype string, days int) error {
	sources, err := loadSources(cfg)
	if err != nil {
		return err
	}
	for i := range sources {
		if sources[i].Type == ctype {
			sources[i].SinceDays = days
		}
	}
	return saveSources(cfg, sources)
}

func setSourceEnabled(cfg Config, ctype string, enabled bool) error {
	sources, err := loadSources(cfg)
	if err != nil {
		return err
	}
	found := false
	for i := range sources {
		if sources[i].Type == ctype {
			sources[i].Enabled = ptr(enabled)
			found = true
		}
	}
	if !found {
		// No source row yet (e.g. filesystem before any `sources add`). Create a
		// minimal row carrying the consent bit; an explicit Enabled avoids the
		// load-time grandfather flipping a nil to true (D-11).
		sources = append(sources, Source{
			Name:      ctype,
			Type:      ctype,
			Scope:     "personal",
			Enabled:   ptr(enabled),
			CreatedAt: time.Now().Format(time.RFC3339),
		})
	}
	return saveSources(cfg, sources)
}

// enableConnector is the "log me in" half of consent (REG-02): it runs OAuth
// consent if the type needs it, flips the Enabled bit, then STOPS — it pulls
// ZERO data (REG-03 / D-04). Backfill is a separate, explicit step (sync/ingest).
// Unknown types are rejected (D-03 / ASVS V5), never silently no-op'd.
func enableConnector(ctx context.Context, cfg Config, ctype string, stdout io.Writer, stdin io.Reader) error {
	info, ok := lookupCatalog(ctype)
	if !ok {
		return fmt.Errorf("unknown connector %q; run `mora connectors list`", ctype)
	}
	if info.NeedsAuth {
		// Run interactive consent only when we lack a saved token AND stdin is a
		// real terminal. The loopback flow opens a browser and BLOCKS up to 5
		// minutes on the HTTP callback, so it must never run on a non-TTY (tests,
		// pipes, the Plan-04 non-TTY menu path) — there we just flip the bit and
		// hint the user to authorize separately. Token reuse on re-enable.
		if _, err := google.LoadToken(googleTokenPath(cfg)); err != nil {
			if isInteractive(stdin) {
				printGoogleAuthPreamble(stdout)
				oc, err := google.ResolveOAuthConfig(google.Scopes)
				if err != nil {
					return err
				}
				tok, err := google.StartLoopbackAuth(ctx, oc, stdout)
				if err != nil {
					return err
				}
				if err := google.SaveToken(googleTokenPath(cfg), tok); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(stdout, "note: %s needs Google authorization — run `mora connect google` (or `mora connectors enable %s` in a terminal) to grant consent.\n", ctype, ctype)
			}
		} else {
			// CROSS-PHASE TOUCH (UI-SPEC §C): a saved token means the browser step is
			// skipped — say so, so the absent sign-in prompt isn't a "did it work?".
			fmt.Fprintln(stdout, "Reusing your saved Google sign-in — no need to authorize again.")
		}
		// Ensure the gmail/calendar source rows exist before flipping the bit.
		if err := ensureGoogleSources(cfg, ""); err != nil {
			return err
		}
	}
	if err := setSourceEnabled(cfg, ctype, true); err != nil {
		return err
	}
	// STOP — do NOT call ingestSource/NewLiveFetcher/backfill here (REG-03/D-04).
	if ctype == "imessage" {
		// No-auth path: the real gate is Full Disk Access, not a login (Surface 1).
		okf(stdout, "enabled imessage. iMessage reads your local Messages database — no login needed.")
		fmt.Fprintln(stdout, "Next: grant Full Disk Access, then pull data with `mora sync imessage`.")
		fmt.Fprintln(stdout, "Check readiness anytime with `mora doctor`.")
		if runtime.GOOS != "darwin" {
			fmt.Fprintf(stdout, "note: iMessage ingest only runs on macOS; this machine is %s.\n", runtime.GOOS)
		}
		return nil
	}
	if ctype == "applecalendar" {
		// No-auth path, same gate as iMessage: local store + Full Disk Access.
		okf(stdout, "enabled applecalendar. Apple Calendar reads your local Calendar database — no login needed.")
		fmt.Fprintln(stdout, "Next: grant Full Disk Access (the same toggle iMessage uses), then pull data with `mora ingest run --source applecalendar`.")
		if runtime.GOOS != "darwin" {
			fmt.Fprintf(stdout, "note: Apple Calendar ingest only runs on macOS; this machine is %s.\n", runtime.GOOS)
		}
		return nil
	}
	okf(stdout, "enabled %s. Pull data with `mora sync google` (or `mora ingest run --all`).", ctype)
	return nil
}

// printGoogleAuthPreamble prints the warm, read-only-reassuring intro shown right
// before the Google browser sign-in, at BOTH entry points (`connect google` and
// the in-menu enable) so the two read identically and Google feels like a peer of
// iMessage's friendly intro. CROSS-PHASE TOUCH (UI-SPEC §C, copy-only).
func printGoogleAuthPreamble(w io.Writer) {
	fmt.Fprintln(w, "Connecting Google (Gmail + Calendar)…")
	fmt.Fprintln(w, "Mora will open your browser to sign in with Google. It asks for READ-ONLY")
	fmt.Fprintln(w, "access to your mail and calendar — it can never send, delete, or change")
	fmt.Fprintln(w, "anything, and nothing leaves this machine.")
}

// isGoogleAuthError reports whether a sync/ingest error looks like an expired or
// invalid Google sign-in (the 7-day Testing-mode refresh-token trap), so callers
// can surface the specific recovery step instead of a generic resumable warning.
func isGoogleAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, marker := range []string{"oauth", "token", "invalid_grant", "unauthorized", "401", "expired", "refresh"} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// renderSetupState prints the unified "you're ready" closing block shared by
// `connect google`, `connect imessage`, and the interactive setup menu (UI-SPEC
// §D): a per-connector enabled-state line in catalog order, then the two forward
// pointers. It is the ONE place that names the first useful command (`mora
// search`) and then how to wire Mora into an agent (`mora mcp serve`).
func renderSetupState(cfg Config, w io.Writer) {
	sources, _ := loadSources(cfg)
	enabled := map[string]bool{}
	for _, s := range sources {
		if s.IsEnabled() {
			enabled[s.Type] = true
		}
	}
	sty := newStyler(w, false)
	fmt.Fprintf(w, "\n%s\n", sty.accent("Setup complete. Here's where things stand:"))
	for _, c := range connectorCatalog {
		mark := sty.dim("·  not set up yet")
		if enabled[c.Type] {
			mark = sty.ok("✓  enabled")
		}
		fmt.Fprintf(w, "  %-16s %s\n", c.DisplayName, mark)
	}
	fmt.Fprintf(w, "\n%s\n", sty.accent("Next:"))
	fmt.Fprintln(w, "  1. Try a search →  mora search \"<a recent topic>\"")
	fmt.Fprintln(w, "  2. Wire Mora into your agent, once →  mora mcp serve")
	fmt.Fprintf(w, "%s\n", sty.dim("     Claude Code:  claude mcp add mora -s user -- mora mcp serve"))
	fmt.Fprintf(w, "%s\n", sty.dim("     Codex:        codex mcp add mora -- mora mcp serve"))
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

func backfillEnabledGoogle(ctx context.Context, cfg Config, stdout io.Writer) (int, error) {
	sources, _ := loadSources(cfg)
	total := 0
	failures := 0
	for _, s := range sources {
		if s.Type != "gmail" && s.Type != "calendar" {
			continue
		}
		if !s.IsEnabled() {
			continue // gated backfill skips disabled sources (D-07)
		}
		n, e := ingestSource(cfg, s, stdout)
		total += n
		if e != nil {
			failures++
			warnf(stdout, "%s sync incomplete (resumable): %v", s.Name, e)
			if isGoogleAuthError(e) {
				// CROSS-PHASE TOUCH (UI-SPEC §C): name the real cause + fix for the
				// 7-day Testing-mode refresh-token trap instead of a bare resumable warn.
				fmt.Fprintln(stdout, "Google sign-in expired — run `mora connect google` to sign in again.")
				fmt.Fprintln(stdout, "(If this keeps happening every ~7 days, your Google app is in \"Testing\" mode; switch it to \"Production\" for durable access.)")
			}
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		return total, err
	}
	if failures > 0 {
		return total, fmt.Errorf("%d source(s) failed to sync; data may be stale (run `mora sync status`)", failures)
	}
	return total, nil
}

// applySetupSelection is the pure, TTY-free consequential half of the setup menu
// (Plan 04). It enables every selected connector via the Plan-02 enableConnector
// codepath (OAuth-then-STOP, REG-02/REG-03) and then — and ONLY if doBackfill is
// true — runs the gated google backfill. With doBackfill=false it performs ZERO
// ingest: no vault write, no sync call. Taking the confirm result as a parameter
// makes the headline consent guarantee ("no affirmative confirm ⇒ zero ingest",
// D-09) assertable in a unit test without a TTY or huh. It sits ON TOP of
// enableConnector — it never reimplements enable/auth (T-04-03).
func applySetupSelection(ctx context.Context, cfg Config, selected []string, doBackfill bool, stdout io.Writer, stdin io.Reader) error {
	for _, ctype := range selected {
		if err := enableConnector(ctx, cfg, ctype, stdout, stdin); err != nil {
			return err
		}
	}
	if !doBackfill {
		// Default-NO path (D-09): consent without pulling data. ZERO ingest.
		return nil
	}
	total, err := backfillEnabledGoogle(ctx, cfg, stdout)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "backfilled %d item(s). Run `mora sync status`.\n", total)
	return nil
}

// runSetupMenu launches the interactive connector setup menu (REG-01, D-08/D-09):
// a multi-select of catalog connectors, sequential OAuth for each via the enable
// path, then an explicit "Pull data now?" confirm DEFAULTING TO NO. It is
// TTY-guarded (Pitfall 2 / T-04-01): on non-TTY stdin (pipe / CI / `go test` with
// empty stdin) it prints a hint and returns immediately — it NEVER blocks. The
// menu only gathers selection + confirm; the consequential actions are delegated
// to the pure applySetupSelection seam.
func runSetupMenu(ctx context.Context, cfg Config, stdin io.Reader, stdout io.Writer) error {
	f, ok := stdin.(*os.File)
	if !ok || !isatty.IsTerminal(f.Fd()) {
		// Non-interactive (pipe / CI / test). Do NOT block (T-04-01).
		fmt.Fprintln(stdout, "Non-interactive terminal — skipping setup menu.")
		fmt.Fprintln(stdout, "Enable connectors with: mora connectors enable <type>")
		return nil
	}

	// The Apocrypha eye — shown once at the top of interactive setup (TTY only).
	printBanner(stdout)

	options := make([]huh.Option[string], 0, len(connectorCatalog))
	for _, c := range connectorCatalog {
		options = append(options, huh.NewOption(c.DisplayName, c.Type))
	}

	var selected []string
	selForm := huh.NewForm(
		huh.NewGroup(
			huh.NewMultiSelect[string]().
				Title("Select connectors to enable").
				Description("space or x toggles a connector · ↑/↓ moves · ctrl+a selects all · enter continues").
				Filterable(false).
				Options(options...).
				Value(&selected),
		),
	).WithInput(stdin).WithOutput(stdout)
	if err := selForm.Run(); err != nil {
		return err
	}
	if len(selected) == 0 {
		fmt.Fprintln(stdout, "No connectors selected — nothing was enabled.")
		fmt.Fprintln(stdout, "Tip: in the menu, press space or x to toggle each connector (enter only confirms the screen).")
		fmt.Fprintln(stdout, "Re-open the menu anytime with `mora connectors setup`, or enable one directly:")
		fmt.Fprintln(stdout, "  mora connectors enable <gmail|calendar|filesystem|imessage>")
		return nil
	}

	imessageSelected := containsType(selected, "imessage")

	// Canonical guided order (UI-SPEC §B): multi-select → (if iMessage) readiness →
	// Google detect-and-skip → deny-list → backfill confirm → enable → backfill.
	if imessageSelected {
		fmt.Fprintln(stdout, "Checking iMessage readiness…")
		printIMessageReadiness(stdout, true)
	}

	// CROSS-PHASE TOUCH (UI-SPEC §C/E-7, control-flow): detect Google placeholder
	// before opening anything; skip without dead-ending so iMessage/filesystem still
	// complete. The decision is the pure googleSetupStep helper (unit-tested).
	if remaining, skipMsg, skipped := googleSetupStep(selected); skipped {
		fmt.Fprintln(stdout, skipMsg)
		selected = remaining
		if len(selected) == 0 {
			return nil // nothing left to set up
		}
	}

	// Deny-list (optional, skippable — Surface 2). Both inputs default blank; pressing
	// enter twice = include everyone. Only shown when iMessage is selected.
	var denyContacts, denyConvos []string
	if imessageSelected {
		var contactsStr, convosStr string
		denyForm := huh.NewForm(
			huh.NewGroup(huh.NewInput().
				Title("Exclude any contacts? (optional)").
				Description("Enter phone numbers or emails to skip across ALL chats, comma-separated. Leave blank to include everyone.").
				Value(&contactsStr)),
			huh.NewGroup(huh.NewInput().
				Title("Exclude any conversations? (optional)").
				Description("Enter conversation names (as shown in Messages) to skip, comma-separated. Leave blank to include all conversations.").
				Value(&convosStr)),
		).WithInput(stdin).WithOutput(stdout)
		if err := denyForm.Run(); err != nil {
			return err
		}
		denyContacts = parseCSVList(contactsStr)
		denyConvos = parseCSVList(convosStr)
		if len(denyContacts) == 0 && len(denyConvos) == 0 {
			fmt.Fprintln(stdout, "Deny-list: none — all contacts and conversations will be ingested (within the 90-day lookback).")
		} else {
			fmt.Fprintln(stdout, "Deny-list saved:")
			fmt.Fprintf(stdout, "  contacts:      %s\n", strings.Join(denyContacts, ", "))
			fmt.Fprintf(stdout, "  conversations: %s\n", strings.Join(denyConvos, ", "))
			fmt.Fprintln(stdout, "These will be skipped on every iMessage sync. Edit anytime via `mora connectors setup`.")
		}
	}

	doBackfill := false // D-09: backfill defaults to NO.
	confirmForm := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title("Pull data now?").
				Description("You can always backfill later with `mora sync google` or `mora sync imessage`.").
				Affirmative("Yes").
				Negative("No").
				Value(&doBackfill),
		),
	).WithInput(stdin).WithOutput(stdout)
	if err := confirmForm.Run(); err != nil {
		return err
	}

	// Persist the deny-list BEFORE the consent seam (D-07): setIMessageDenyList
	// creates the imessage source row if needed and setSourceEnabled preserves
	// the deny fields, so the order is safe — and it makes the user's privacy
	// exclusions durable even if the google backfill inside
	// applySetupSelection fails (its error used to abort this function AFTER
	// "Deny-list saved:" was printed but BEFORE anything was persisted; the
	// next `mora sync imessage` then ingested exactly what they excluded).
	if imessageSelected {
		if err := setIMessageDenyList(cfg, denyContacts, denyConvos); err != nil {
			return err
		}
	}
	// Enable + (if confirmed) google backfill via the shared consent seam.
	if err := applySetupSelection(ctx, cfg, selected, doBackfill, stdout, stdin); err != nil {
		return err
	}
	if imessageSelected {
		if doBackfill {
			if ready, _ := imessage.ProbeReadable(chatDBPath()); ready && runtime.GOOS == "darwin" {
				total, err := backfillEnabledIMessage(ctx, cfg, stdout)
				if err != nil {
					return err
				}
				fmt.Fprintf(stdout, "backfilled %d iMessage conversation(s).\n", total)
			} else {
				fmt.Fprintln(stdout, "note: iMessage isn't ready yet (Full Disk Access) — skipped. Run `mora doctor`, then `mora sync imessage`.")
			}
		}
	}
	renderSetupState(cfg, stdout)
	return nil
}

// googleSetupStep implements the guided-setup Google placeholder detect-and-skip
// (UI-SPEC §C/E-7, control-flow). When gmail/calendar are selected but Google creds
// are a placeholder/unconfigured, it returns the exact skippable BYO message and the
// selection with the google types removed — so the guided sequence CONTINUES
// (iMessage/filesystem still complete) instead of dead-ending. Detection reuses
// google.IsConfigured (the SAME guard as ResolveOAuthConfig), which opens NO browser
// or loopback. Pure (no I/O) so it is unit-testable without a TTY.
func googleSetupStep(selected []string) (remaining []string, skipMsg string, skipped bool) {
	if !containsType(selected, "gmail") && !containsType(selected, "calendar") {
		return selected, "", false
	}
	if google.IsConfigured() {
		return selected, "", false
	}
	return withoutTypes(selected, "gmail", "calendar"),
		"Skipping Google for now — set up creds later with `mora connectors enable gmail`. iMessage and filesystem need no creds.",
		true
}

// containsType reports whether types contains t.
func containsType(types []string, t string) bool {
	for _, x := range types {
		if x == t {
			return true
		}
	}
	return false
}

// withoutTypes returns types with each of drop removed (preserving order).
func withoutTypes(types []string, drop ...string) []string {
	out := types[:0:0]
	for _, x := range types {
		if !containsType(drop, x) {
			out = append(out, x)
		}
	}
	return out
}

// parseCSVList splits a comma-separated input into trimmed, non-empty entries.
func parseCSVList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if t := strings.TrimSpace(part); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// setIMessageDenyList persists the deny-list onto the imessage source row in
// sources.json (creating the row if needed), so every future `mora sync imessage`
// honors it (D-07; no new config file).
func setIMessageDenyList(cfg Config, contacts, conversations []string) error {
	sources, err := loadSources(cfg)
	if err != nil {
		return err
	}
	found := false
	for i := range sources {
		if sources[i].Type == "imessage" {
			sources[i].DenyContacts = contacts
			sources[i].DenyConversations = conversations
			found = true
		}
	}
	if !found {
		sources = append(sources, Source{
			Name: "imessage", Type: "imessage", Scope: "personal",
			Enabled: ptr(true), CreatedAt: time.Now().Format(time.RFC3339),
			DenyContacts: contacts, DenyConversations: conversations,
		})
	}
	return saveSources(cfg, sources)
}

// disableConnector is a non-destructive bit-flip (REG-04 / D-13): it sets Enabled
// false to stop ingest but KEEPS the OAuth token and all ingested memories
// (D-14/D-15). It is the deliberate ANTI-analog of cmdDisconnect — it MUST NOT
// call google.RevokeToken or remove the token file (that is disconnect's job).
func disableConnector(cfg Config, ctype string, stdout io.Writer) error {
	if _, ok := lookupCatalog(ctype); !ok {
		return fmt.Errorf("unknown connector %q; run `mora connectors list`", ctype)
	}
	if err := setSourceEnabled(cfg, ctype, false); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s disabled. Ingest stopped; existing memories kept and searchable. Re-enable instantly with `mora connectors enable %s`.\n", ctype, ctype)
	return nil
}

func cmdIngest(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "run" {
		return errors.New("usage: mora ingest run --source <name>|--all")
	}
	fs := flag.NewFlagSet("ingest run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sourceName := fs.String("source", "", "source")
	all := fs.Bool("all", false, "all")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	sources, err := loadSources(cfg)
	if err != nil {
		return err
	}
	count := 0
	failures := 0
	var namedErr error
	for _, s := range sources {
		// Enabled gate (D-07): a named disabled source ERRORS before the skip so
		// the user is never silently no-op'd; `--all` silently skips disabled.
		// The gate wraps the ingestSource CALLER, never ingestSource itself.
		if !*all {
			if s.Name != *sourceName {
				continue
			}
			if !s.IsEnabled() {
				return fmt.Errorf("%s is disabled — run `mora connectors enable %s` first", s.Name, s.Type)
			}
		} else if !s.IsEnabled() {
			continue // --all silently skips disabled (D-07)
		}
		n, err := ingestSourceFn(cfg, s, stdout)
		count += n
		if err != nil {
			// Named-source path: the failure IS the result — but a PARTIAL run
			// (Ingest's dropped-item contract) has already written memories to
			// the vault, so the final rebuild below must still run before the
			// error surfaces; returning here left them unsearchable with no
			// auto-heal (the schema check only heals version mismatches).
			if !*all {
				namedErr = err
				break
			}
			// --all (the scheduled ingest-hourly job): one broken connector must
			// not starve the rest or skip the final rebuild — warn, keep going,
			// and surface an aggregate error at the end (never swallow sync
			// errors; mirrors backfillEnabledGoogle).
			failures++
			warnf(stdout, "%s sync incomplete (resumable): %v", s.Name, err)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		if namedErr != nil {
			return fmt.Errorf("%w (and the index rebuild failed: %v — run `mora index rebuild`)", namedErr, err)
		}
		return err
	}
	if namedErr != nil {
		return namedErr
	}
	fmt.Fprintf(stdout, "ingested %d item(s)\n", count)
	if failures > 0 {
		return fmt.Errorf("%d source(s) failed to sync; data may be stale (run `mora sync status`)", failures)
	}
	return nil
}

// ingestSourceFn is the injectable seam cmdIngest routes per-source ingest
// through (defaults to ingestSource); tests swap it (t.Cleanup-restore, never
// t.Parallel) to assert --all's warn-and-continue ordering without real
// connectors.
var ingestSourceFn = ingestSource

func cmdConnect(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) >= 1 && args[0] == "imessage" {
		return connectIMessage(ctx, args[1:], stdout)
	}
	if len(args) >= 1 && args[0] == "filesystem" {
		return connectFilesystem(ctx, args[1:], stdout)
	}
	if len(args) < 1 || args[0] != "google" {
		return errors.New("usage: mora connect google [--since-days N] [--account <label>] | mora connect imessage [--since-days N] | mora connect filesystem <path> [--name <name>]")
	}
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sinceDays := fs.Int("since-days", 0, "gmail backfill window in days (default 90)")
	account := fs.String("account", "", `label for an ADDITIONAL Google account (e.g. "work"); empty = the default account`)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *account != "" && !isValidAccountLabel(*account) {
		return fmt.Errorf("invalid account label %q (use lowercase letters, digits, hyphens)", *account)
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	oc, err := google.ResolveOAuthConfig(google.Scopes)
	if err != nil {
		return err
	}
	// Phase 1: authenticate (loopback consent).
	printGoogleAuthPreamble(stdout)
	tok, err := google.StartLoopbackAuth(ctx, oc, stdout)
	if err != nil {
		return err
	}
	// Phase 2: persist + validate. The token lands in the per-account file so a
	// second mailbox (personal vs business) never clobbers the first's refresh
	// token.
	if err := google.SaveToken(googleTokenPathFor(cfg, *account), tok); err != nil {
		return err
	}
	fetcher, err := google.NewLiveFetcher(ctx, oc, tok)
	if err != nil {
		return err
	}
	email, err := fetcher.AuthedEmail()
	if err != nil {
		return fmt.Errorf("validation call failed: %w", err)
	}
	okf(stdout, "Signed in as %s — verified read-only access to your Gmail and Calendar.", email)

	// Same-account guard: this mailbox may already be connected under another
	// label — proceeding would double-ingest every thread under distinct
	// @account StableIDs. Exit gracefully, and remove the just-written
	// duplicate token so a stray refresh token doesn't linger.
	if existing, found := googleAccountForEmail(loadSourcesOrEmpty(cfg), email); found && existing != *account {
		label := existing
		if label == "" {
			label = "the default account"
		} else {
			label = "account " + strconv.Quote(label)
		}
		_ = os.Remove(googleTokenPathFor(cfg, *account))
		fmt.Fprintf(stdout, "%s is already connected as %s — nothing to do.\n", email, label)
		fmt.Fprintln(stdout, "To refresh that account's data, run `mora sync google`.")
		return nil
	}

	// Phase 3: register default sources if absent, enable them, then backfill.
	// `connect google` is the deliberate enable+backfill convenience (D-06):
	// ensureGoogleSources creates gmail/calendar DISABLED (D-11), so connect must
	// flip the Enabled bit BEFORE the backfill loop, then reload so the loop sees
	// the enabled rows. The backfill loop itself stays UNGATED — it is the named,
	// consented convenience path, NOT a silent backfill (REG-03/Pitfall 3).
	if err := ensureGoogleSources(cfg, *account); err != nil {
		return err
	}
	gmailName, calName := googleSourceNames(*account)
	if err := setSourceEnabledByName(cfg, gmailName, true); err != nil {
		return err
	}
	if err := setSourceEnabledByName(cfg, calName, true); err != nil {
		return err
	}
	if err := setSourceEmailByAccount(cfg, *account, email); err != nil {
		return err
	}
	// Persist an explicit window override so this and future `sync google` runs
	// reuse it (0 keeps the 90-day default in windowForSource).
	if *sinceDays > 0 {
		if err := setSourceSinceDaysByName(cfg, gmailName, *sinceDays); err != nil {
			return err
		}
	}
	sources, _ := loadSources(cfg)
	total := 0
	now := time.Now()
	for _, s := range sources {
		if s.Type != "gmail" && s.Type != "calendar" {
			continue // ungated convenience path (D-06) — no IsEnabled() skip here
		}
		if s.Account != *account {
			continue // backfill only the account just connected — other mailboxes keep their own cadence
		}
		// Skip-if-fresh: a re-auth minutes after a clean backfill should not
		// re-pull the whole window (the full re-pull is the honest-snapshot
		// design; `mora sync google` remains the explicit force path).
		if sourceFreshlySynced(cfg, s, connectFreshWindow, now) {
			fmt.Fprintf(stdout, "  %s: synced recently — skipping re-pull (`mora sync google` to force)\n", s.Name)
			continue
		}
		n, e := ingestSource(cfg, s, stdout)
		total += n
		if e != nil {
			warnf(stdout, "%s sync incomplete (resumable): %v", s.Name, e)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		return err
	}
	okf(stdout, "Enabled gmail + calendar and backfilled %d item(s).", total)
	renderSetupState(cfg, stdout)
	return nil
}

func cmdSync(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) >= 1 && isHelpFlag(args[0]) {
		fmt.Fprintln(stdout, "usage: mora sync [status|google|imessage|git]")
		fmt.Fprintln(stdout, "  status    show per-source freshness (no fetch)")
		fmt.Fprintln(stdout, "  google    re-run the Gmail + Calendar backfill")
		fmt.Fprintln(stdout, "  imessage  re-run the iMessage backfill")
		fmt.Fprintln(stdout, "  git       back up the vault to a private git remote (off-device)")
		fmt.Fprintln(stdout, "            --init [--remote URL | --github [--name repo]] [-m msg]")
		fmt.Fprintln(stdout, "  (no arg)  re-run the Google backfill")
		return nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	// `mora sync git` — one-way, push-only, fail-loud off-device backup to a
	// private git remote (opt-in; the vault otherwise never leaves the device).
	if len(args) >= 1 && args[0] == "git" {
		return syncGit(ctx, cfg, args[1:], stdout, realExec)
	}
	if len(args) >= 1 && args[0] == "status" {
		dir := filepath.Join(cfg.StateDir, "sync")
		entries, _ := os.ReadDir(dir)
		if len(entries) == 0 {
			fmt.Fprintln(stdout, "no sources synced yet")
			return nil
		}
		sty := newStyler(stdout, false)
		for _, e := range entries {
			st, err := memory.LoadStatus(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			stale := ""
			if t, perr := time.Parse(time.RFC3339, st.LastSynced); perr == nil && time.Since(t) > 48*time.Hour {
				stale = " " + sty.bad("(STALE)")
			}
			errs := fmt.Sprintf("%d errors", st.ErrorCount)
			if st.ErrorCount > 0 {
				errs = sty.bad(errs)
			}
			fmt.Fprintf(stdout, "%s: %d items, %s, %s%s\n",
				sty.accent(st.Source), st.ItemCount, errs, sty.dim("last_synced "+st.LastSynced), stale)
		}
		return nil
	}
	// `mora sync imessage` — re-run the gated iMessage backfill (shared seam).
	if len(args) >= 1 && args[0] == "imessage" {
		total, err := backfillEnabledIMessage(ctx, cfg, stdout)
		fmt.Fprintf(stdout, "synced %d item(s)\n", total)
		return err
	}
	// `mora sync google` (or bare `mora sync`) — re-run the gated google backfill.
	total, err := backfillEnabledGoogle(ctx, cfg, stdout)
	fmt.Fprintf(stdout, "synced %d item(s)\n", total)
	return err
}

// reingestFullDays is the Gmail lookback used by `reingest --full` — ~100 years,
// effectively all-time (windowForSource computes Since = now - days; Gmail does not
// treat a negative SinceDays as all-time, so a large positive value is used).
const reingestFullDays = 36500

// cmdReingest re-fetches enabled sources and rewrites memories with the latest
// structured metadata (the Meta-in-content-hash change means a normal sync already
// rewrites within the window; --full extends the lookback to all-time so the
// rewrite reaches memories older than the default window), then rebuilds the graph.
func cmdReingest(ctx context.Context, args []string, stdout io.Writer) error {
	full := false
	for _, a := range args {
		switch a {
		case "--full":
			full = true
		case "-h", "--help":
			fmt.Fprintln(stdout, "usage: mora reingest [--full]")
			fmt.Fprintln(stdout, "  re-fetch enabled sources, rewrite memories with the latest")
			fmt.Fprintln(stdout, "  structured identity metadata, and rebuild the entity graph.")
			fmt.Fprintln(stdout, "  --full  extend the lookback to all-time (backfill older memories)")
			return nil
		default:
			return fmt.Errorf("unknown flag %q (usage: mora reingest [--full])", a)
		}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	sources, _ := loadSources(cfg)
	total, failures := 0, 0
	for _, s := range sources {
		if !s.IsEnabled() {
			continue
		}
		if full {
			switch s.Type {
			case "gmail":
				s.SinceDays = reingestFullDays // all-time lookback (copy; not persisted)
			case "imessage":
				s.SinceDays = -1 // all-time (windowForIMessage)
			}
		}
		n, e := ingestSource(cfg, s, stdout)
		total += n
		if e != nil {
			failures++
			warnf(stdout, "%s reingest incomplete (resumable): %v", s.Name, e)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		return err
	}
	suffix := ""
	if full {
		suffix = " (full lookback)"
	}
	fmt.Fprintf(stdout, "reingested %d item(s)%s\n", total, suffix)
	if failures > 0 {
		return fmt.Errorf("%d source(s) failed to reingest; data may be stale (run `mora sync status`)", failures)
	}
	return nil
}

func cmdUsage(ctx context.Context, args []string, stdout io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	switch {
	case len(args) >= 1 && args[0] == "off":
		return atomicWrite(filepath.Join(cfg.StateDir, "usage", "OFF"), []byte("off\n"), 0o600)
	case len(args) >= 1 && args[0] == "on":
		return os.Remove(filepath.Join(cfg.StateDir, "usage", "OFF"))
	case len(args) >= 1 && args[0] == "report":
		return usageReport(cfg, stdout)
	default:
		return errors.New("usage: mora usage report|off|on")
	}
}

func usageReport(cfg Config, stdout io.Writer) error {
	b, err := os.ReadFile(filepath.Join(cfg.StateDir, "usage", "events.jsonl"))
	if err != nil {
		fmt.Fprintln(stdout, "no usage recorded")
		return nil
	}
	byTool := map[string]int{}
	empty, total := 0, 0
	var latencies []int64
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var e usageEvent
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		byTool[e.Tool]++
		total++
		if e.Results == 0 {
			empty++
		}
		latencies = append(latencies, e.Millis)
	}
	fmt.Fprintf(stdout, "Mora usage (content-free)\n")
	fmt.Fprintf(stdout, "total calls: %d\n", total)
	for tool, n := range byTool {
		fmt.Fprintf(stdout, "  %s: %d\n", tool, n)
	}
	if total > 0 {
		fmt.Fprintf(stdout, "empty-result rate: %d%%\n", empty*100/total)
		fmt.Fprintf(stdout, "latency p50: %dms\n", percentile(latencies, 50))
	}
	return nil
}

func percentile(v []int64, p int) int64 {
	if len(v) == 0 {
		return 0
	}
	sorted := append([]int64(nil), v...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := (p * (len(sorted) - 1)) / 100
	return sorted[idx]
}

func cmdDisconnect(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) < 1 || args[0] != "google" {
		return errors.New("usage: mora disconnect google")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	tokPath := googleTokenPath(cfg)
	if tok, err := google.LoadToken(tokPath); err == nil {
		_ = google.RevokeToken(ctx, tok) // best effort
	}
	if err := os.Remove(tokPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Fprintln(stdout, "disconnected google; token revoked and removed")
	return nil
}

// ensureGoogleSources registers the gmail/calendar source pair for one Google
// account. The unlabeled default keeps the legacy "gmail"/"calendar" names; a
// labeled account (multi-mailbox: personal vs business) registers
// "gmail-<label>"/"calendar-<label>" rows carrying Account=<label>, so each
// mailbox gets its own enable bit, sync status, and digest section. Existence
// is keyed by NAME (not type) so a second account is not mistaken for the
// first. New rows stay disabled (D-11); connect flips them.
func ensureGoogleSources(cfg Config, account string) error {
	sources, _ := loadSources(cfg)
	have := map[string]bool{}
	for _, s := range sources {
		have[s.Name] = true
	}
	gmailName, calName := googleSourceNames(account)
	now := time.Now().Format(time.RFC3339)
	if !have[gmailName] {
		sources = append(sources, Source{Name: gmailName, Type: "gmail", Scope: "personal", Account: account, Enabled: ptr(false), CreatedAt: now})
	}
	if !have[calName] {
		sources = append(sources, Source{Name: calName, Type: "calendar", Scope: "personal", Calendar: "primary", Account: account, Enabled: ptr(false), CreatedAt: now})
	}
	return saveSources(cfg, sources)
}

// loadSourcesOrEmpty is loadSources with the error collapsed to "no sources" —
// for guard paths where a missing/corrupt sources file should mean "no
// conflict", never an abort.
func loadSourcesOrEmpty(cfg Config) []Source {
	sources, err := loadSources(cfg)
	if err != nil {
		return nil
	}
	return sources
}

// googleAccountForEmail reports which existing account label a Google address
// is already connected under. The re-auth guard: connecting the SAME mailbox
// under a SECOND label would double-ingest it (every thread twice, distinct
// @account StableIDs), so connect exits gracefully instead.
func googleAccountForEmail(sources []Source, email string) (label string, found bool) {
	if email == "" {
		return "", false
	}
	for _, s := range sources {
		if (s.Type == "gmail" || s.Type == "calendar") && s.Email != "" && strings.EqualFold(s.Email, email) {
			return s.Account, true
		}
	}
	return "", false
}

// setSourceEmailByAccount stamps the signed-in address onto an account's
// gmail/calendar rows (the guard's lookup data).
func setSourceEmailByAccount(cfg Config, account, email string) error {
	sources, err := loadSources(cfg)
	if err != nil {
		return err
	}
	for i := range sources {
		if (sources[i].Type == "gmail" || sources[i].Type == "calendar") && sources[i].Account == account {
			sources[i].Email = email
		}
	}
	return saveSources(cfg, sources)
}

// sourceFreshlySynced reports whether a source completed a CLEAN sync within
// the window — the connect-path skip guard, so a re-auth minutes after a full
// backfill doesn't re-pull the whole window again. Reads LastSuccessAt (the
// field that survives an aborted attempt without advancing).
func sourceFreshlySynced(cfg Config, s Source, within time.Duration, now time.Time) bool {
	st, err := memory.LoadStatus(syncStatusPathFor(cfg, s))
	if err != nil || st == nil || st.LastSuccessAt == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, st.LastSuccessAt)
	if err != nil {
		return false
	}
	return now.Sub(t) < within
}

// connectFreshWindow is how recently a source must have cleanly synced for the
// connect path to skip its re-pull (run `mora sync google` to force).
const connectFreshWindow = time.Hour

// isValidAccountLabel gates `--account`: lowercase letters, digits, hyphens —
// the label lands in filenames (tokens/google-<label>.json, source names,
// sync-status paths), so it must be path-safe by construction.
func isValidAccountLabel(label string) bool {
	for _, r := range label {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return label != ""
}

// googleSourceNames maps an account label to its gmail/calendar source names.
func googleSourceNames(account string) (gmail, calendar string) {
	if account == "" {
		return "gmail", "calendar"
	}
	return "gmail-" + account, "calendar-" + account
}

func cmdMCP(ctx context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) error {
	if len(args) != 1 || args[0] != "serve" {
		return errors.New("usage: mora mcp serve")
	}
	return serveMCP(ctx, stdout, stdin)
}

func writeMemory(cfg Config, m Memory) error {
	body, err := renderMemory(m)
	if err != nil {
		return err
	}
	return atomicWrite(memoryPath(cfg, m), body, 0o644)
}

func renderMemory(m Memory) ([]byte, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "---\n")
	fmt.Fprintf(&b, "id: %s\nscope: %s\ntype: %s\ntitle: %s\n", m.ID, m.Scope, m.Type, quoteYAML(m.Title))
	fmt.Fprintf(&b, "tags: [%s]\nsource: %s\ncreated_at: %s\n", strings.Join(m.Tags, ", "), quoteYAML(m.Source), m.CreatedAt)
	if m.Provider != "" {
		fmt.Fprintf(&b, "provider: %s\nprovider_id: %s\n", m.Provider, quoteYAML(m.ProviderID))
	}
	if m.Account != "" {
		fmt.Fprintf(&b, "account: %s\n", m.Account)
	}
	if m.ContentHash != "" {
		fmt.Fprintf(&b, "content_hash: %s\n", m.ContentHash)
	}
	if m.LastSynced != "" {
		fmt.Fprintf(&b, "last_synced: %s\n", m.LastSynced)
	}
	if m.Truncated {
		fmt.Fprintf(&b, "truncated: true\n")
	}
	if m.DeletedAt != "" {
		fmt.Fprintf(&b, "deleted_at: %s\n", m.DeletedAt)
	}
	// Meta is one canonical JSON line. json.Marshal sorts keys and never emits a
	// raw newline, so the value survives the line-split parser and the inner colons
	// survive strings.Cut(line, ":") (which splits on the FIRST colon only).
	if metaJSON, err := memory.CanonicalMeta(m.Meta); err != nil {
		return nil, err
	} else if metaJSON != "" {
		fmt.Fprintf(&b, "meta: %s\n", metaJSON)
	}
	fmt.Fprintf(&b, "---\n\n%s\n", m.Text)
	return []byte(b.String()), nil
}

func parseMemory(path string) (Memory, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Memory{}, err
	}
	text := string(b)
	if !strings.HasPrefix(text, "---\n") {
		return Memory{}, errors.New("missing frontmatter")
	}
	parts := strings.SplitN(text[4:], "\n---\n", 2)
	if len(parts) != 2 {
		return Memory{}, errors.New("invalid frontmatter")
	}
	m := Memory{Path: path, Text: strings.TrimSpace(parts[1])}
	for _, line := range strings.Split(parts[0], "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		// Meta is a JSON object value with inner colons/quotes — decode the RAW
		// substring after the first colon, NOT the quote-trimmed val (trimming would
		// not corrupt an object, but decoding the raw form is unambiguous).
		if key == "meta" {
			_, raw, _ := strings.Cut(line, ":")
			// UseNumber so a numeric value (e.g. a 19-digit thread/message id) decodes
			// to json.Number, not a lossy float64 — no silent precision loss in Meta.
			dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(raw)))
			dec.UseNumber()
			meta := map[string]any{}
			if jerr := dec.Decode(&meta); jerr != nil {
				// Never silently drop data: a corrupt meta: line (hand-edit, partial
				// write) loses the memory's entire structured identity from the graph,
				// so surface it instead of swallowing the error.
				fmt.Fprintf(os.Stderr, "warn: %s: meta frontmatter is corrupt and was ignored: %v\n", path, jerr)
			} else if len(meta) > 0 {
				m.Meta = meta
			}
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"`)
		switch key {
		case "id":
			m.ID = val
		case "scope":
			m.Scope = val
		case "type":
			m.Type = val
		case "title":
			m.Title = val
		case "tags":
			m.Tags = parseTags(val)
		case "source":
			m.Source = val
		case "created_at":
			m.CreatedAt = val
		case "provider":
			m.Provider = val
		case "provider_id":
			m.ProviderID = val
		case "account":
			m.Account = val
		case "content_hash":
			m.ContentHash = val
		case "last_synced":
			m.LastSynced = val
		case "truncated":
			m.Truncated = val == "true"
		case "deleted_at":
			m.DeletedAt = val
		}
	}
	if m.ID == "" {
		return Memory{}, errors.New("missing id")
	}
	return m, nil
}

func memoriesRoot(cfg Config) string { return filepath.Join(cfg.VaultDir, "memories") }
func sourcesRoot(cfg Config) string  { return filepath.Join(cfg.VaultDir, "sources") }
func dbPath(cfg Config) string       { return filepath.Join(cfg.DataDir, "index.db") }

// roIndexDSN is the DSN every read-only index open uses. busy_timeout matters
// for READERS too: the hourly rebuild does a whole-index DELETE-then-reinsert
// inside ONE transaction, and its commit flush of a large rollback journal can
// hold the writer lock for longer than a few seconds. A short reader timeout
// surfaces a raw "database is locked" mid-rebuild (and openIndexRO can misread
// that SQLITE_BUSY as a stale schema and launch a spurious rebuild). 15s lets an
// interactive read (brief --entity, prep, think, get_entity) and an MCP tool call
// ride out the rebuild's commit window instead of erroring; humanizeIndexBusy
// gives an actionable message if a read still outlasts it.
// (TestReadOnlyIndexWaitsOnWriteLock pins the wait behavior.)
//
// Note: with modernc.org/sqlite, mode=ro on a non-"file:" DSN is parsed out
// but NOT enforced (connections open read-write); it is kept as
// documentation-of-intent until the read paths adopt a stricter pragma.
func roIndexDSN(cfg Config) string {
	return dbPath(cfg) + "?mode=ro&_pragma=busy_timeout(15000)"
}

// indexSchemaVersion stamps index.db (PRAGMA user_version) with the schema this
// binary writes. Bump it whenever rebuildIndex's shape changes meaning (a new
// table or column, vector layout, salience semantics). Read paths refuse a
// mismatched index with an actionable error instead of degrading silently —
// a binary swapped across a schema change otherwise serves missing columns or
// zeroed salience (the live Phase-14 failure). 1 = the first stamped schema;
// every pre-stamp index reads as 0 and asks for one rebuild.
const indexSchemaVersion = 1

// indexAutoHeal reports whether a version-stale index may be rebuilt inline at
// read time. True on the static-hash floor, where a rebuild is seconds — the
// same self-healing as rebuild-on-missing, and what saves every distributed
// user's FIRST upgrade across the stamp's introduction (their old binary's
// `upgrade` predates the post-upgrade rebuild hook, and Homebrew swaps bypass
// it entirely). False under a semantic embedder: a full re-embed takes minutes
// and must not stall an innocent MCP tool call — those users get the
// actionable error instead. Package var so tests can pin both branches.
var indexAutoHeal = func(cfg Config) bool { return !embedderIsSemantic(chooseEmbedderFor(cfg)) }

// openIndexRO opens the index read-only, refusing to serve a schema this
// binary doesn't understand (a swapped binary otherwise reads missing columns
// or zeroed salience silently). A stale index self-heals inline when
// indexAutoHeal allows; otherwise the error names the exact fix, and
// `mora upgrade` runs the rebuild at the moment the user consented to a slow
// step.
func openIndexRO(ctx context.Context, cfg Config) (*sql.DB, error) {
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		return nil, err
	}
	verr := checkIndexSchema(db)
	if verr == nil {
		return db, nil
	}
	_ = db.Close()
	if !indexAutoHeal(cfg) {
		return nil, verr
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		return nil, fmt.Errorf("rebuilding a stale index (%v) failed: %w", verr, err)
	}
	db, err = sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		return nil, err
	}
	if err := checkIndexSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func checkIndexSchema(db *sql.DB) error {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v != indexSchemaVersion {
		return fmt.Errorf("the search index was built by a different mora version (index schema v%d, this binary expects v%d) — run `mora index rebuild`", v, indexSchemaVersion)
	}
	return nil
}

func memoryPath(cfg Config, m Memory) string {
	scopePath := strings.ReplaceAll(m.Scope, ":", string(os.PathSeparator))
	scopePath = strings.ReplaceAll(scopePath, "/", string(os.PathSeparator))
	return filepath.Join(memoriesRoot(cfg), scopePath, m.ID+".md")
}

func allMemoryFiles(cfg Config) ([]string, error) {
	var paths []string
	for _, root := range []string{memoriesRoot(cfg), sourcesRoot(cfg)} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			// Surface walk errors (an unreadable directory must not silently
			// shrink the index to the readable subset); a missing root is the
			// one benign case — a fresh vault simply has no sources/ tree yet.
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() || filepath.Ext(path) != ".md" {
				return nil
			}
			paths = append(paths, path)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walking %s: %w", root, err)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func rebuildIndex(ctx context.Context, cfg Config) (int, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return 0, err
	}
	db, err := sql.Open("sqlite", dbPath(cfg)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, err
	}
	defer db.Close()

	files, err := allMemoryFiles(cfg)
	if err != nil {
		return 0, err
	}

	// Rebuild the whole index inside ONE transaction so a mid-rebuild failure
	// rolls back to the prior committed index instead of leaving a half-empty
	// one (the DELETE-then-reinsert is destructive). Every write — schema,
	// DELETEs, memories, and FTS — must go through this same tx.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds; the safety net on any early return

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS memories (id TEXT PRIMARY KEY, scope TEXT, type TEXT, title TEXT, tags TEXT, source TEXT, created_at TEXT, path TEXT, text TEXT)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(id, scope, title, tags, source, text)`,
		// Entity graph (I1): rebuildable from the vault Markdown, never the only home of state.
		// salience_micros (Phase 14): the frozen person-ranking sort key. A FRESH db gets it
		// from this CREATE; an EXISTING pre-column db gets it from the additive ALTER below
		// (CREATE TABLE IF NOT EXISTS is a no-op once the table exists, so it can't add it).
		`CREATE TABLE IF NOT EXISTS entities (id TEXT PRIMARY KEY, kind TEXT, display_name TEXT, aliases TEXT, mention_count INTEGER, first_seen TEXT, last_seen TEXT, salience_micros INTEGER)`,
		`CREATE TABLE IF NOT EXISTS edges (src TEXT, rel TEXT, dst TEXT, evidence_id TEXT, valid_from TEXT, valid_to TEXT, observed_at TEXT, invalidated_at TEXT, PRIMARY KEY (src, rel, dst, evidence_id))`,
		`CREATE INDEX IF NOT EXISTS idx_edges_src ON edges(src)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_dst ON edges(dst)`,
		`CREATE INDEX IF NOT EXISTS idx_edges_rel ON edges(rel)`,
		`CREATE INDEX IF NOT EXISTS idx_entities_kind ON entities(kind)`,
		// Hybrid retrieval (I2): one static-embedding vector per memory, rebuildable.
		`CREATE TABLE IF NOT EXISTS mem_vectors (memory_id TEXT PRIMARY KEY, dim INT, model TEXT, vec BLOB)`,
		`DELETE FROM memories`,
		`DELETE FROM memories_fts`,
		`DELETE FROM entities`,
		`DELETE FROM edges`,
		`DELETE FROM mem_vectors`,
		// Stamp the schema this binary writes (read paths refuse a mismatch).
		// Inside the same tx as everything else: a rolled-back rebuild must not
		// leave a fresh stamp on a stale index.
		fmt.Sprintf(`PRAGMA user_version = %d`, indexSchemaVersion),
	}
	for _, s := range stmts {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			return 0, err
		}
	}
	// Additive-by-rebuild migration (Phase 14): an EXISTING entities table predating
	// salience_micros is NOT touched by the CREATE TABLE IF NOT EXISTS above (no-op once
	// the table exists), so add the column here. On a FRESH db the column already exists
	// and SQLite errors "duplicate column name" — tolerated so the atomic rebuild tx is
	// never aborted by this idempotent ALTER. Any OTHER error is fatal.
	if _, err := tx.ExecContext(ctx, `ALTER TABLE entities ADD COLUMN salience_micros INTEGER`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return 0, err
	}
	memStmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO memories VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer memStmt.Close()
	ftsStmt, err := tx.PrepareContext(ctx, `INSERT INTO memories_fts VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer ftsStmt.Close()
	count := 0
	var parsed []Memory
	for _, path := range files {
		m, err := parseMemory(path)
		if err != nil {
			continue
		}
		if _, err := memStmt.ExecContext(ctx,
			m.ID, m.Scope, m.Type, m.Title, strings.Join(m.Tags, ","), m.Source, m.CreatedAt, path, m.Text); err != nil {
			return count, err
		}
		if _, err := ftsStmt.ExecContext(ctx,
			m.ID, m.Scope, m.Title, strings.Join(m.Tags, ","), m.Source, m.Text); err != nil {
			return count, err
		}
		parsed = append(parsed, m)
		count++
	}

	// Materialize the entity graph from the just-indexed memories, in the SAME
	// transaction — a graph failure rolls back the whole index too (atomic).
	if err := writeGraph(ctx, tx, parsed); err != nil {
		return count, err
	}

	// Materialize per-memory embedding vectors (I2 hybrid retrieval), same tx. Use
	// the cfg-aware resolver so a config.toml `embedder = "ollama"` opt-in indexes
	// semantic vectors that the query path (also cfg-aware) will match.
	if err := writeVectors(ctx, tx, chooseEmbedderFor(cfg), parsed); err != nil {
		return count, err
	}

	if err := tx.Commit(); err != nil {
		return count, err
	}
	return count, nil
}

// writeGraph derives the structural entity graph from the indexed memories and
// inserts the entity + edge rows on the given transaction. Empty bi-temporal
// timestamps persist as SQL NULL. Statements are prepared once and reused — a
// real vault is ~10^5 edges, so per-row SQL re-parsing dominated the rebuild.
func writeGraph(ctx context.Context, tx *sql.Tx, mems []Memory) error {
	ents, edges, warnings := buildGraph(mems)
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, w)
	}
	entStmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO entities (id, kind, display_name, aliases, mention_count, first_seen, last_seen, salience_micros) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer entStmt.Close()
	for _, e := range ents {
		aliases := e.Aliases
		if aliases == nil {
			aliases = []string{}
		}
		aj, err := json.Marshal(aliases)
		if err != nil {
			return err
		}
		if _, err := entStmt.ExecContext(ctx, e.ID, e.Kind, e.DisplayName, string(aj), e.MentionCount, nullStr(e.FirstSeen), nullStr(e.LastSeen), e.Salience); err != nil {
			return err
		}
	}
	edgeStmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO edges (src, rel, dst, evidence_id, valid_from, valid_to, observed_at, invalidated_at) VALUES (?, ?, ?, ?, ?, NULL, ?, ?)`)
	if err != nil {
		return err
	}
	defer edgeStmt.Close()
	for _, ed := range edges {
		if _, err := edgeStmt.ExecContext(ctx, ed.Src, ed.Rel, ed.Dst, ed.EvidenceID, nullStr(ed.ValidFrom), nullStr(ed.ObservedAt), nullStr(ed.InvalidatedAt)); err != nil {
			return err
		}
	}
	return nil
}

// writeVectors embeds each memory (title + body) and inserts its vector on the
// given transaction. The embedder is deterministic, so the same vault produces
// byte-identical vectors across rebuilds. Statement prepared once (a real vault is
// ~10^4 memories).
func writeVectors(ctx context.Context, tx *sql.Tx, emb Embedder, mems []Memory) error {
	stmt, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO mem_vectors (memory_id, dim, model, vec) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, m := range mems {
		vec := emb.Embed(m.Title + "\n" + m.Text)
		if _, err := stmt.ExecContext(ctx, m.ID, emb.Dim(), emb.ModelID(), encodeVec(vec)); err != nil {
			return err
		}
	}
	return nil
}

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
)

// snippetMemories returns copies of the results with each body flattened to a
// single line and clipped to searchSnippetLen (Truncated flags the clip), and
// drops the Meta map so a row's total size is bounded (Meta is entity-graph
// frontmatter — agents get it via get_entity/read_memory, not a search preview).
// The clip window is centered on the earliest query-term match (matchSnippet),
// so a preview shows the evidence for the hit, not the memory's opening lines.
// Only the token-budgeted MCP surface calls this; the CLI keeps full bodies+meta.
func snippetMemories(mems []Memory, query string) []Memory {
	if mems == nil {
		return nil
	}
	out := make([]Memory, len(mems))
	for i, m := range mems {
		full := strings.Join(strings.Fields(m.Text), " ")
		if utf8.RuneCountInString(full) > searchSnippetLen {
			m.Text = matchSnippet(m.Text, query, searchSnippetLen)
			m.Truncated = true
		} else {
			m.Text = full
		}
		m.Meta = nil // unbounded graph frontmatter — not part of a search preview
		out[i] = m
	}
	return out
}

func searchMemories(ctx context.Context, cfg Config, query, scope string, limit int) ([]Memory, error) {
	if _, err := os.Stat(dbPath(cfg)); err != nil {
		if _, err := rebuildIndex(ctx, cfg); err != nil {
			return nil, err
		}
	}
	db, err := openIndexRO(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	match := ftsQuery(query)
	if strings.TrimSpace(match) == "" {
		// An empty / all-punctuation query has no terms to MATCH. FTS5 errors on
		// an empty MATCH string ("fts5: syntax error near \"\""), so short-circuit
		// to zero results rather than crashing the search command.
		return nil, nil
	}
	sqlq := `SELECT m.id, m.scope, m.type, m.title, m.tags, m.source, m.created_at, m.path, m.text, bm25(memories_fts) AS score
		FROM memories_fts JOIN memories m ON m.id = memories_fts.id WHERE memories_fts MATCH ?`
	args := []any{match}
	if scope != "" {
		sqlq += ` AND m.scope = ?`
		args = append(args, scope)
	}
	sqlq += ` ORDER BY score, m.id LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, sqlq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var m Memory
		var tags string
		if err := rows.Scan(&m.ID, &m.Scope, &m.Type, &m.Title, &tags, &m.Source, &m.CreatedAt, &m.Path, &m.Text, &m.Score); err != nil {
			return nil, err
		}
		m.Tags = splitCSV(tags)
		out = append(out, m)
	}
	return out, rows.Err()
}

func findMemory(cfg Config, id string) (Memory, error) {
	files, err := allMemoryFiles(cfg)
	if err != nil {
		return Memory{}, err
	}
	// Google memories store an ID like "gmail_thread/abc" but are filed under the
	// SafeFilename form "gmail_thread_abc.md", so match both shapes.
	base := id + ".md"
	safeBase := memory.SafeFilename(id) + ".md"
	for _, path := range files {
		b := filepath.Base(path)
		if b != base && b != safeBase && !strings.Contains(b, id) {
			continue
		}
		m, err := parseMemory(path)
		if err == nil && m.ID == id {
			return m, nil
		}
	}
	return Memory{}, fmt.Errorf("memory not found: %s", id)
}

func listMemories(cfg Config, scope string, limit int) ([]Memory, error) {
	files, err := allMemoryFiles(cfg)
	if err != nil {
		return nil, err
	}
	var out []Memory
	for _, path := range files {
		m, err := parseMemory(path)
		if err != nil {
			continue
		}
		if scope == "" || m.Scope == scope {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// truncateRunes returns s clipped to at most max bytes, never splitting a
// multi-byte UTF-8 rune (a raw s[:max] could leave an invalid trailing byte).
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func buildContext(cfg Config, items []Memory, budget int, hasQuery bool) string {
	if budget <= 0 {
		return ""
	}
	var wiki strings.Builder
	for _, rel := range []string{"index.md", "priority-map.md", "live-tasks.md", "heartbeat.md", "auto-resolver.md"} {
		if body, err := os.ReadFile(filepath.Join(cfg.VaultDir, rel)); err == nil {
			fmt.Fprintf(&wiki, "\n# %s\n%s\n", rel, string(body))
		}
	}
	var its strings.Builder
	for _, m := range items {
		fmt.Fprintf(&its, "\n# %s\n%s\n", m.Title, m.Text)
	}
	// Ordering by intent: when there IS a query, the caller already filtered
	// items to the most relevant memories — surface them first so the static
	// wiki preamble can never starve them out of the budget. With no query
	// (session-start briefing), the wiki preamble leads and items fill the rest.
	first, second := wiki.String(), its.String()
	if hasQuery {
		first, second = its.String(), wiki.String()
	}
	var out strings.Builder
	out.WriteString(truncateRunes(first, budget))
	if rem := budget - out.Len(); rem > 0 {
		out.WriteString(truncateRunes(second, rem))
	}
	return out.String()
}

func writeWikiIndex(cfg Config, count int) error {
	var sections []string
	for _, dir := range []string{"memories", "sources", "meetings"} {
		root := filepath.Join(cfg.VaultDir, dir)
		var n int
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && filepath.Ext(path) == ".md" {
				n++
			}
			return nil
		})
		sections = append(sections, fmt.Sprintf("- %s: %d pages", dir, n))
	}
	body := fmt.Sprintf("# Mora Index\n\n> Generated by `mora index rebuild`.\n> Updated: %s\n> Indexed memories: %d\n\n%s\n", time.Now().Format(time.RFC3339), count, strings.Join(sections, "\n"))
	return atomicWrite(filepath.Join(cfg.VaultDir, "index.md"), []byte(body), 0o644)
}

func syncTasks(cfg Config, write bool) (int, error) {
	p0, err := parseP0(filepath.Join(cfg.VaultDir, "priority-map.md"))
	if err != nil {
		return 0, err
	}
	livePath := filepath.Join(cfg.VaultDir, "live-tasks.md")
	bodyBytes, err := os.ReadFile(livePath)
	if err != nil {
		return 0, err
	}
	body := string(bodyBytes)
	added := 0
	var rows []string
	for _, task := range p0 {
		if strings.Contains(body, "| "+task+" |") {
			continue
		}
		rows = append(rows, fmt.Sprintf("| %s | memory | you | P0 | queued | None | this week | %s |", task, time.Now().Format("2006-01-02")))
		added++
	}
	if write && added > 0 {
		body = strings.TrimRight(body, "\n") + "\n" + strings.Join(rows, "\n") + "\n"
		if err := atomicWrite(livePath, []byte(body), 0o644); err != nil {
			return 0, err
		}
	}
	return added, nil
}

var p0Re = regexp.MustCompile(`^(\d+\.|-)\s+\*\*([^*]+)\*\*`)

func parseP0(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var inP0 bool
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "## ") {
			inP0 = strings.Contains(strings.ToLower(line), "p0")
			continue
		}
		if !inP0 {
			continue
		}
		m := p0Re.FindStringSubmatch(strings.TrimSpace(line))
		if len(m) == 3 {
			out = append(out, strings.TrimSpace(m[2]))
		}
	}
	return out, nil
}

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

func isTerminalStatus(status string) bool {
	return terminalTaskStatuses[strings.ToLower(strings.TrimSpace(status))]
}

func staleTasks(cfg Config, days int) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(cfg.VaultDir, "live-tasks.md"))
	if err != nil {
		return nil, err
	}
	var stale []string
	cutoff := time.Now().AddDate(0, 0, -days)
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "| ") || strings.Contains(line, "Last touched") || strings.Contains(line, "---") {
			continue
		}
		cols := tableCols(line)
		if len(cols) < 8 {
			continue
		}
		// Status-aware (issue #19): a terminal-state row is closed work, not stale.
		if isTerminalStatus(cols[4]) {
			continue
		}
		t, err := time.Parse("2006-01-02", cols[7])
		if err == nil && t.Before(cutoff) {
			stale = append(stale, cols[0])
		}
	}
	return stale, nil
}

// markTaskDone flips the live-tasks.md row whose Task (col 0) equals name to a
// terminal Status and stamps today as the completion date (Last touched, col 7).
// The row is KEPT — it is the closed-record that makes completion resurrection-
// safe: syncTasks sees the still-present row and refuses to re-add the P0 item
// (issue #19), without needing a separate closed-task ledger. Returns the number
// of rows updated (0 => not found).
func markTaskDone(cfg Config, name string) (int, error) {
	livePath := filepath.Join(cfg.VaultDir, "live-tasks.md")
	b, err := os.ReadFile(livePath)
	if err != nil {
		return 0, err
	}
	name = strings.TrimSpace(name)
	today := time.Now().Format("2006-01-02")
	lines := strings.Split(string(b), "\n")
	updated := 0
	for i, line := range lines {
		if !strings.HasPrefix(line, "| ") || strings.Contains(line, "Last touched") || strings.Contains(line, "---") {
			continue
		}
		cols := tableCols(line)
		if len(cols) < 8 || cols[0] != name {
			continue
		}
		cols[4] = "done"
		cols[7] = today
		lines[i] = "| " + strings.Join(cols, " | ") + " |"
		updated++
	}
	if updated == 0 {
		return 0, nil
	}
	if err := atomicWrite(livePath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return 0, err
	}
	return updated, nil
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

// listTasks parses live-tasks.md into rows (header/separator lines skipped).
func listTasks(cfg Config) ([]LiveTask, error) {
	b, err := os.ReadFile(filepath.Join(cfg.VaultDir, "live-tasks.md"))
	if err != nil {
		return nil, err
	}
	out := []LiveTask{}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "| ") || strings.Contains(line, "Last touched") || strings.Contains(line, "---") {
			continue
		}
		cols := tableCols(line)
		if len(cols) < 8 {
			continue
		}
		out = append(out, LiveTask{
			Task: cols[0], Domain: cols[1], Owner: cols[2], Pri: cols[3],
			Status: cols[4], Blocker: cols[5], Horizon: cols[6], LastTouched: cols[7],
		})
	}
	return out, nil
}

// addTask appends a queued live-task row. It is idempotent by Task name (the row
// identity, matching syncTasks's dedup): if a row with that name already exists
// it is a no-op and reports added=false, so a daily automation re-running the
// brief write-back never mints duplicates. Last touched is stamped today.
func addTask(cfg Config, lt LiveTask) (bool, error) {
	livePath := filepath.Join(cfg.VaultDir, "live-tasks.md")
	bodyBytes, err := os.ReadFile(livePath)
	if err != nil {
		return false, err
	}
	body := string(bodyBytes)
	// Idempotency by EXACT Task-name (col 0), not a substring scan of the whole
	// table — so a name that happens to appear in another row's Blocker/Horizon
	// cell, or that is a prefix of another task, does not falsely suppress the add.
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "| ") || strings.Contains(line, "Last touched") || strings.Contains(line, "---") {
			continue
		}
		if cols := tableCols(line); len(cols) >= 1 && cols[0] == lt.Task {
			return false, nil
		}
	}
	row := fmt.Sprintf("| %s | %s | %s | %s | queued | %s | %s | %s |",
		lt.Task, lt.Domain, lt.Owner, lt.Pri, lt.Blocker, lt.Horizon, time.Now().Format("2006-01-02"))
	body = strings.TrimRight(body, "\n") + "\n" + row + "\n"
	if err := atomicWrite(livePath, []byte(body), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

func addSource(cfg Config, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mora sources add <filesystem|gmail|calendar|gdrive> [flags]")
	}
	stype := args[0]
	fs := flag.NewFlagSet("sources add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", stype, "source name")
	scope := fs.String("scope", "personal", "scope")
	path := fs.String("path", "", "path")
	label := fs.String("label", "", "gmail label")
	cal := fs.String("calendar", "", "calendar")
	folder := fs.String("folder", "", "drive folder id")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	// New sources are consent-gated: default-disabled (D-11). Enabled is set
	// explicitly to false so the grandfather migration in loadSources (which
	// normalizes nil => true for pre-Enabled legacy sources) cannot silently
	// auto-enable a freshly added source on the next load.
	s := Source{Name: *name, Type: stype, Scope: *scope, Path: expandHome(*path), Label: *label, Calendar: *cal, FolderID: *folder, Enabled: ptr(false), CreatedAt: time.Now().Format(time.RFC3339)}
	if s.Type == "filesystem" && s.Path == "" {
		return errors.New("filesystem source requires --path")
	}
	sources, _ := loadSources(cfg)
	var next []Source
	for _, existing := range sources {
		if existing.Name != s.Name {
			next = append(next, existing)
		}
	}
	next = append(next, s)
	if err := saveSources(cfg, next); err != nil {
		return err
	}
	return emit(stdout, s, true)
}

func loadSources(cfg Config) ([]Source, error) {
	path := filepath.Join(cfg.ConfigDir, "sources.json")
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var sources []Source
	if err := json.Unmarshal(b, &sources); err != nil {
		return nil, err
	}
	// Grandfather migration (D-12): a missing `enabled` key means a pre-Enabled
	// binary wrote this source, i.e. the user had already explicitly added it —
	// treat absence as prior consent and normalize nil => true. An explicit
	// `false` is preserved as disabled (it is non-nil, so the loop skips it).
	for i := range sources {
		if sources[i].Enabled == nil {
			sources[i].Enabled = ptr(true)
		}
	}
	return sources, nil
}

// saveSources persists the source registry. atomicWrite stages through a unique
// temp per writer, so this is safe against the temp-collision race (two writers
// clobbering a shared `.tmp`). It does NOT serialize the higher-level
// read-modify-write on sources.json: two callers each doing load → mutate → save
// can still lose an update. That needs caller-level serialization, out of scope here.
func saveSources(cfg Config, sources []Source) error {
	b, err := json.MarshalIndent(sources, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(cfg.ConfigDir, "sources.json"), append(b, '\n'), 0o600)
}

func ingestSource(cfg Config, s Source, out io.Writer) (int, error) {
	switch s.Type {
	case "filesystem":
		return ingestFilesystem(cfg, s, out)
	case "gmail":
		return ingestGoogle(cfg, s, google.KindGmailThread, out)
	case "calendar":
		return ingestGoogle(cfg, s, google.KindCalEvent, out)
	case "imessage":
		return ingestIMessage(cfg, s, out)
	case "applecalendar":
		return ingestAppleCal(cfg, s, out)
	case "gdrive":
		return 0, nil // deferred to week 2
	default:
		return 0, fmt.Errorf("unknown source type %q", s.Type)
	}
}

func writeMappedMemory(cfg Config, mm memory.MappedMemory) error {
	m := Memory{
		ID: mm.StableID, Scope: mm.Scope, Type: mm.Type, Title: mm.Title,
		Tags: mm.Tags, Source: mm.Source, CreatedAt: mm.CreatedAt, Text: mm.Body,
		Provider: mm.Provider, Account: mm.Account, ProviderID: mm.ProviderID, ContentHash: mm.ContentHash,
		LastSynced: mm.LastSynced, Truncated: mm.Truncated, DeletedAt: mm.DeletedAt,
		Meta: mm.Meta,
	}
	out := filepath.Join(sourcesRoot(cfg), mm.Provider, memory.SafeFilename(mm.StableID)+".md")
	// Skip rewrite if content unchanged (preserve created_at).
	if existing, err := parseMemory(out); err == nil {
		if existing.ContentHash == mm.ContentHash && mm.DeletedAt == "" {
			return nil
		}
		m.CreatedAt = existing.CreatedAt // preserve original
	}
	body, err := renderMemory(m)
	if err != nil {
		return err
	}
	return atomicWrite(out, body, 0o644)
}

func ingestGoogle(cfg Config, s Source, kind google.ItemKind, out io.Writer) (int, error) {
	ctx := context.Background()
	oc, err := google.ResolveOAuthConfig(google.Scopes)
	if err != nil {
		return 0, err
	}
	tok, err := google.LoadToken(googleTokenPathFor(cfg, s.Account))
	if err != nil {
		connectCmd := "mora connect google"
		if s.Account != "" {
			connectCmd += " --account " + s.Account
		}
		return 0, fmt.Errorf("not connected to google (run `%s`): %w", connectCmd, err)
	}
	fetcher, err := google.NewLiveFetcher(ctx, oc, tok)
	if err != nil {
		return 0, err
	}
	statusPath := googleStatusPath(cfg, s.Name)
	st, _ := memory.LoadStatus(statusPath)
	st.Source = s.Name
	win := windowForSource(s, kind)

	// Progress output: a thread-level backfill makes one API call per item and
	// prints nothing, so a large pull looks frozen for minutes (#ux). Emit a
	// start line and a periodic running count by wrapping the Write callback.
	noun := "items"
	switch kind {
	case google.KindGmailThread:
		noun = "threads"
	case google.KindCalEvent:
		noun = "events"
	}
	prog := newProgress(out, s.Name, noun)
	write := func(mm memory.MappedMemory) error {
		// Account-scoped provenance (the sourceInstanceKey multi-account seam):
		// a labeled account stamps Account so the instance key composes
		// "gmail:work" — separate watermark, digest section, and three-state per
		// mailbox instead of collapsing into the default account's; and tags
		// StableID ("…@work") because a meeting shared across both mailboxes
		// carries the SAME iCal UID — untagged, the second account's copy would
		// collide with (and clobber) the first's.
		if s.Account != "" {
			mm.Account = s.Account
			mm.StableID = mm.StableID + "@" + s.Account
		}
		if err := writeMappedMemory(cfg, mm); err != nil {
			return err
		}
		prog.tick()
		return nil
	}

	res, ingErr := memory.Ingest(memory.IngestParams{
		Fetcher: fetcher, Kind: kind, Window: win, Scope: s.Scope, BodyBudget: 16 * 1024,
		Status: st, Write: write,
	})
	prog.done()
	return res.Status.ItemCount, persistSyncStatus(out, statusPath, res.Status, ingErr)
}

// persistSyncStatus writes a sync path's final SyncStatus to disk — the single
// boundary all four sync paths (google, imessage, applecal, filesystem) route
// through. A failed save is warned AND folded into the returned error instead
// of swallowed: the status file drives the digest's three-state health, so
// losing it silently turns a real outcome into permanent "unavailable"/stale
// readings. ingErr (the sync's own error) stays primary; the save error is
// returned only when the sync itself succeeded.
func persistSyncStatus(out io.Writer, statusPath string, st *memory.SyncStatus, ingErr error) error {
	if serr := memory.SaveStatus(statusPath, st); serr != nil {
		if out != nil {
			warnf(out, "could not persist sync status (%s): %v", statusPath, serr)
		}
		if ingErr == nil {
			return fmt.Errorf("persisting sync status: %w", serr)
		}
	}
	return ingErr
}

func windowForSource(s Source, kind google.ItemKind) google.FetchWindow {
	now := time.Now()
	w := google.FetchWindow{Labels: s.LabelIDs, CalendarID: s.Calendar}
	switch kind {
	case google.KindGmailThread:
		// Default to a lean 90-day window: a year of mail is mostly low-signal
		// noise for a memory index (~6.7k threads vs ~1.6k here). Override with
		// `mora connect google --since-days N` (persisted on the source, so
		// future `sync google` reuses it).
		days := s.SinceDays
		if days == 0 {
			days = 90
		}
		w.Since = now.AddDate(0, 0, -days)
	case google.KindCalEvent:
		w.Since = now.AddDate(0, -6, 0)
		w.Until = now.AddDate(0, 3, 0)
	}
	return w
}

// googleTokenPathFor maps an account label to its token file. The unlabeled
// default keeps the legacy tokens/google.json (existing installs untouched);
// a labeled account (a second mailbox, e.g. personal vs business) gets its own
// tokens/google-<label>.json so two Google identities never clobber each
// other's refresh tokens.
func googleTokenPathFor(cfg Config, account string) string {
	name := "google.json"
	if account != "" {
		name = "google-" + account + ".json"
	}
	return filepath.Join(cfg.ConfigDir, "tokens", name)
}

func googleTokenPath(cfg Config) string { return googleTokenPathFor(cfg, "") }
func googleStatusPath(cfg Config, name string) string {
	return filepath.Join(cfg.StateDir, "sync", "google-"+name+".json")
}

// imessageStatusPath mirrors googleStatusPath so `mora sync status` reads the
// honest-snapshot freshness for iMessage generically (no special-casing).
func imessageStatusPath(cfg Config, name string) string {
	return filepath.Join(cfg.StateDir, "sync", "imessage-"+name+".json")
}

// chatDBPath is the default local iMessage database location. macOS-only; the
// caller gates on runtime.GOOS before reading it.
func chatDBPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "Messages", "chat.db")
}

// addressBookRoot is the AddressBook Sources/ root used for handle→name resolution.
func addressBookRoot() string {
	home, _ := os.UserHomeDir()
	return imessage.DefaultAddressBookRoot(home)
}

// windowForIMessage builds the lookback window: a lean 90-day default (D-06),
// overridable per source via SinceDays (0 ⇒ 90; a negative SinceDays means all-time,
// matching the Gmail since-days ergonomic). Zero Since = all-time at the SQL bound.
func windowForIMessage(s Source) memory.FetchWindow {
	days := s.SinceDays
	switch {
	case days < 0:
		return memory.FetchWindow{} // all-time (Since zero ⇒ no lower bound)
	case days == 0:
		days = 90
	}
	return memory.FetchWindow{Since: time.Now().AddDate(0, 0, -days)}
}

// ingestIMessage reads the local chat.db read-only and writes one memory per
// conversation (IMSG-01/03). It is macOS-gated (a non-darwin host prints an honest
// note and returns 0, never a false error), resolves contact names via the
// AddressBook, honors the source's deny-list (IMSG-06), and surfaces resumable
// errors. Rendering/truncation is the connector's inverted-truncation mapper, routed
// through the shared resumable Ingest loop via the Map hook — the writeMappedMemory
// boundary is reused, never reimplemented.
func ingestIMessage(cfg Config, s Source, out io.Writer) (int, error) {
	if runtime.GOOS != "darwin" {
		if out != nil {
			fmt.Fprintf(out, "note: iMessage ingest only runs on macOS; this machine is %s.\n", runtime.GOOS)
		}
		return 0, nil
	}

	path := chatDBPath()
	deny := imessage.DenyList{Contacts: s.DenyContacts, Conversations: s.DenyConversations}
	fetcher, err := imessage.NewLiveFetcher(path, deny)
	if err != nil {
		// A present-but-unreadable chat.db is the FDA-denied case — point the user at
		// the doctor guidance rather than dumping a raw sqlite error.
		return 0, fmt.Errorf("cannot read your Messages database (Full Disk Access not granted?) — run `mora doctor`: %w", err)
	}
	defer fetcher.Close()

	// Handle→name resolution; an unreadable AddressBook degrades to raw handles (D-09).
	resolver, _ := imessage.NewResolver(addressBookRoot())

	statusPath := imessageStatusPath(cfg, s.Name)
	st, _ := memory.LoadStatus(statusPath)
	st.Source = s.Name
	win := windowForIMessage(s)

	if out != nil && s.SinceDays < 0 {
		fmt.Fprintf(out, "  %s: lookback set to all-time (since-days 0)\n", s.Name)
	}
	prog := newProgress(out, s.Name, "conversations")
	write := func(mm memory.MappedMemory) error {
		if err := writeMappedMemory(cfg, mm); err != nil {
			return err
		}
		if _, err := writeAttachmentMemories(cfg, mm); err != nil {
			return err
		}
		prog.tick()
		return nil
	}

	res, ingErr := memory.Ingest(memory.IngestParams{
		Fetcher: fetcher, Kind: imessage.KindIMessageChat, Window: win, Scope: s.Scope,
		BodyBudget: 16 * 1024, Status: st, Write: write,
		Map: imessage.MapConversationFn(resolver),
	})
	prog.done()
	ingErr = persistSyncStatus(out, statusPath, res.Status, ingErr)
	if ingErr != nil {
		if out != nil {
			warnf(out, "imessage sync incomplete: %v", ingErr)
		}
		return res.Status.ItemCount, ingErr
	}
	return res.Status.ItemCount, nil
}

// appleCalStatusPath mirrors imessageStatusPath for the Apple Calendar store.
func appleCalStatusPath(cfg Config, name string) string {
	return filepath.Join(cfg.StateDir, "sync", "applecal-"+name+".json")
}

// appleCalDBPath probes the modern group-container store first, then the
// legacy ~/Library/Calendars location; returns the modern default when neither
// exists yet (the open error then names the real path the user must grant).
func appleCalDBPath() string {
	home, _ := os.UserHomeDir()
	modern := applecal.DefaultDBPath(home)
	if _, err := os.Stat(modern); err == nil {
		return modern
	}
	if legacy := applecal.LegacyDBPath(home); fileExists(legacy) {
		return legacy
	}
	return modern
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// windowForAppleCal builds the event window: SinceDays back (default 90;
// negative = all-time past) and a FIXED 180-day forward bound — Apple Calendar
// stores subscribed-holiday events YEARS out, and an unbounded Until would
// flood the vault (and the digest's upcoming-events section) with them.
func windowForAppleCal(s Source, now time.Time) memory.FetchWindow {
	days := s.SinceDays
	switch {
	case days < 0:
		return memory.FetchWindow{Until: now.AddDate(0, 0, 180)}
	case days == 0:
		days = 90
	}
	return memory.FetchWindow{Since: now.AddDate(0, 0, -days), Until: now.AddDate(0, 0, 180)}
}

// ingestAppleCal reads the local Apple Calendar store read-only and writes one
// memory per event. macOS-gated like iMessage (same Full Disk Access story —
// chat.db's TCC lesson applies verbatim: launchd grants are per-binary), and
// routed through the shared resumable Ingest loop + writeMappedMemory boundary.
func ingestAppleCal(cfg Config, s Source, out io.Writer) (int, error) {
	if runtime.GOOS != "darwin" {
		if out != nil {
			fmt.Fprintf(out, "note: Apple Calendar ingest only runs on macOS; this machine is %s.\n", runtime.GOOS)
		}
		return 0, nil
	}
	fetcher, err := applecal.NewLiveFetcher(appleCalDBPath())
	if err != nil {
		return 0, fmt.Errorf("cannot read your Calendar database (Full Disk Access not granted?) — run `mora doctor`: %w", err)
	}
	defer fetcher.Close()

	statusPath := appleCalStatusPath(cfg, s.Name)
	st, _ := memory.LoadStatus(statusPath)
	st.Source = s.Name
	prog := newProgress(out, s.Name, "events")
	write := func(mm memory.MappedMemory) error {
		if err := writeMappedMemory(cfg, mm); err != nil {
			return err
		}
		prog.tick()
		return nil
	}
	res, ingErr := memory.Ingest(memory.IngestParams{
		Fetcher: fetcher, Kind: applecal.KindAppleCalEvent, Window: windowForAppleCal(s, time.Now()),
		Scope: s.Scope, BodyBudget: 16 * 1024, Status: st, Write: write,
	})
	prog.done()
	ingErr = persistSyncStatus(out, statusPath, res.Status, ingErr)
	if ingErr != nil {
		if out != nil {
			warnf(out, "applecalendar sync incomplete: %v", ingErr)
		}
		return res.Status.ItemCount, ingErr
	}
	return res.Status.ItemCount, nil
}

// connectIMessage is the one-command convenience (parallel to `connect google`):
// enable iMessage, show readiness with the Full Disk Access guidance, and — only if
// the database is actually readable — backfill conversations now. When FDA is not yet
// granted it stops at the honest guidance (no false backfill), so the user can grant
// access and re-run. macOS-only.
// connectFilesystem is the one-shot convenience for a filesystem source: it
// registers (or refreshes) the directory as an ENABLED filesystem source, indexes
// it, and rebuilds the search index — the same add+enable+ingest flow `connect
// google`/`connect imessage` run, so a folder is as turnkey as the other
// connectors (D-06: connect is the deliberate enable+backfill path). Unlike
// `sources add` (which lands DISABLED behind the consent gate, D-11), naming a
// folder here IS the consent. The default source name is the folder's base name
// so two folders coexist without an explicit --name; re-connecting the same name
// refreshes in place rather than stacking duplicates (mirrors addSource).
func connectFilesystem(ctx context.Context, args []string, stdout io.Writer) error {
	// Pull a leading positional <path> out before flag parsing: Go's flag package
	// stops at the first non-flag arg, so `connect filesystem ~/dir --name x` would
	// otherwise silently drop --name. A leading flag form (`--name x ~/dir`) still
	// works via fs.Arg(0) below.
	var positional string
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional, rest = args[0], args[1:]
	}
	fs := flag.NewFlagSet("connect filesystem", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "source name (default: the folder's base name)")
	scope := fs.String("scope", "personal", "scope")
	pathFlag := fs.String("path", "", "directory to index (alternative to the positional <path>)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	path := *pathFlag
	if path == "" {
		path = positional
	}
	if path == "" {
		path = fs.Arg(0)
	}
	if path == "" {
		return errors.New("usage: mora connect filesystem <path> [--name <name>] [--scope <scope>]")
	}
	path = expandHome(path)
	// Canonicalize to absolute: the scheduled `ingest --all` job runs from
	// launchd's cwd, not the user's shell, so a persisted relative path would
	// target the wrong (or no) folder.
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("cannot resolve %q: %w", path, err)
	}
	path = abs
	// Fail loudly on a typo'd path, and require a directory: the filesystem walk
	// swallows a missing-root error to stay resumable, so without these checks a bad
	// or non-directory path would register an enabled source that indexes nothing.
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("cannot read %q: %w", path, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%q is not a directory; connect filesystem takes a folder", path)
	}
	// Resolve symlinks so the persisted path is the real folder ingest will walk:
	// filepath.WalkDir does NOT descend a symlinked root, so a symlinked folder
	// (common with iCloud "Desktop & Documents", Google Drive for Desktop, or
	// Dropbox) passes the os.Stat check above yet would index zero files. Resolving
	// here keeps validation and ingest pointed at the same directory.
	if resolved, rerr := filepath.EvalSymlinks(path); rerr == nil {
		path = resolved
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	srcName := *name
	if srcName == "" {
		srcName = defaultFilesystemSourceName(path)
	}
	s := Source{Name: srcName, Type: "filesystem", Scope: *scope, Path: path, Enabled: ptr(true), CreatedAt: time.Now().Format(time.RFC3339)}
	// Refuse to overwrite an unreadable sources.json: with the error swallowed, a
	// corrupt file would be replaced by ONLY the new source, destroying every other
	// registered connector. Bail and leave the file for the user to repair.
	sources, err := loadSources(cfg)
	if err != nil {
		return fmt.Errorf("cannot read existing sources (fix or remove %s): %w", filepath.Join(cfg.ConfigDir, "sources.json"), err)
	}
	var next []Source
	for _, existing := range sources {
		if existing.Name == s.Name {
			// Same name + same folder => a deliberate re-connect: refresh in place
			// and preserve the original add time. Same name + a DIFFERENT folder
			// (e.g. two dirs whose base name collides) would silently clobber the
			// first — refuse and point at --name instead.
			if existing.Type == "filesystem" && existing.Path == s.Path {
				s.CreatedAt = existing.CreatedAt
				continue
			}
			return fmt.Errorf("a source named %q already exists (path %q); pick another name with --name", existing.Name, existing.Path)
		}
		next = append(next, existing)
	}
	next = append(next, s)
	if err := saveSources(cfg, next); err != nil {
		return err
	}
	// Ingest now (the named, consented convenience path — same as connect google).
	// Surface a partial-ingest warning BEFORE the rebuild so a resumable ingest
	// error is never masked by a later index-rebuild failure.
	n, ingestErr := ingestSource(cfg, s, stdout)
	if ingestErr != nil {
		warnf(stdout, "%s indexed %d file(s) before stopping (resumable): %v", s.Name, n, ingestErr)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		return err
	}
	if ingestErr != nil {
		return ingestErr
	}
	okf(stdout, "Enabled filesystem and indexed %d file(s) from %s.", n, path)
	renderSetupState(cfg, stdout)
	return nil
}

// defaultFilesystemSourceName derives a stable, filesystem-safe source name from a
// directory path (its base name), so `connect filesystem ~/notes` and `connect
// filesystem ~/docs` coexist without an explicit --name. Falls back to
// "filesystem" for degenerate paths (root, ".", empty base).
func defaultFilesystemSourceName(path string) string {
	base := filepath.Base(filepath.Clean(path))
	if base == "." || base == string(filepath.Separator) || base == "" {
		return "filesystem"
	}
	return base
}

func connectIMessage(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("connect imessage", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sinceDays := fs.Int("since-days", 0, "iMessage backlog window in days (default 90; negative = all-time)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := setSourceEnabled(cfg, "imessage", true); err != nil {
		return err
	}
	// Persist an explicit window override so this and future `sync imessage` runs
	// reuse it (0 keeps the 90-day default in windowForIMessage; negative = all-time).
	// Mirrors `connect google --since-days`.
	if *sinceDays != 0 {
		if err := setSourceSinceDays(cfg, "imessage", *sinceDays); err != nil {
			return err
		}
	}
	okf(stdout, "enabled imessage. iMessage reads your local Messages database — no login needed.")
	ready := printIMessageReadiness(stdout, true)
	if !ready {
		// Honest stop: readiness guidance already printed the next steps.
		return nil
	}
	total, err := backfillEnabledIMessage(ctx, cfg, stdout)
	fmt.Fprintf(stdout, "synced %d item(s).\n", total)
	renderSetupState(cfg, stdout)
	return err
}

// backfillEnabledIMessage runs the gated iMessage backfill for every ENABLED
// imessage source, then rebuilds the index. Mirrors backfillEnabledGoogle: disabled
// sources are skipped (D-07), sync errors are surfaced (never swallowed).
func backfillEnabledIMessage(ctx context.Context, cfg Config, stdout io.Writer) (int, error) {
	sources, _ := loadSources(cfg)
	total, failures := 0, 0
	for _, s := range sources {
		if s.Type != "imessage" || !s.IsEnabled() {
			continue
		}
		n, e := ingestSource(cfg, s, stdout)
		total += n
		if e != nil {
			failures++
			warnf(stdout, "%s sync incomplete (resumable): %v", s.Name, e)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		return total, err
	}
	if failures > 0 {
		return total, fmt.Errorf("%d source(s) failed to sync; data may be stale (run `mora sync status`)", failures)
	}
	return total, nil
}

func ingestFilesystem(cfg Config, s Source, out io.Writer) (int, error) {
	count := 0
	ignore := map[string]bool{".git": true, "node_modules": true, "dist": true, "build": true, ".next": true, ".venv": true, "__pycache__": true, "site-packages": true, ".tox": true, "vendor": true, ".gradle": true, ".idea": true}
	err := filepath.WalkDir(s.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if ignore[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		base := d.Name()
		ext := filepath.Ext(path)
		if !curatedMetadataFile(base) && !curatedAllowedExt(ext) && !curatedExtractExt(ext) {
			return nil
		}
		var text string
		if curatedExtractExt(ext) {
			// Non-plain-text (.docx/.pdf): extract the words rather than index raw bytes.
			var t string
			var derr error
			switch strings.ToLower(ext) {
			case ".pdf":
				t, derr = extractPDFText(path)
			default:
				t, derr = extractDocxText(path)
			}
			if derr != nil || t == "" {
				return nil // unreadable/empty/oversized — skip, never index garbage.
			}
			text = t
		} else {
			b, rerr := os.ReadFile(path)
			if rerr != nil || len(b) == 0 {
				return nil
			}
			text = string(b)
		}
		if len(text) > 512*1024 {
			return nil // keep the index lean — same bound as the raw-read path.
		}
		rel, _ := filepath.Rel(s.Path, path)
		id := "src_" + ContentHash(s.Name+":"+rel)
		m := Memory{ID: id, Scope: s.Scope, Type: "source", Title: rel, Tags: []string{s.Type, s.Name}, Source: path, CreatedAt: time.Now().Format(time.RFC3339), Text: text}
		dest := filepath.Join(sourcesRoot(cfg), s.Type, s.Name, id+".md")
		body, _ := renderMemory(m)
		if err := atomicWrite(dest, body, 0o644); err != nil {
			return err
		}
		count++
		return nil
	})
	// Record freshness so the brief/digest classifies this source by its real sync
	// health (new/no-changes/stale) instead of "unavailable (sync error)". Filesystem
	// has no fetcher Status of its own, so the walk persists one here — mirroring what
	// the gmail/calendar/imessage sync paths write via memory.SaveStatus.
	if p := syncStatusPathFor(cfg, s); p != "" {
		now := time.Now().UTC().Format(time.RFC3339)
		st := &memory.SyncStatus{Source: s.Name, ItemCount: count, LastSynced: now, LastAttemptAt: now}
		if err == nil {
			st.LastSuccessAt = now
		} else {
			st.ErrorCount = 1
			st.LastError = err.Error()
		}
		err = persistSyncStatus(out, p, st, err)
	}
	return count, err
}

// mcpMaxRequestBytes caps one JSON-RPC request line. bufio.Scanner's 64KB
// default is too small for real tool calls (a write_memory body or a think
// query with pasted context), and overflowing it doesn't drop the request —
// it kills the whole server mid-session. 4MB is far above any legitimate
// call yet still bounds a runaway client.
const mcpMaxRequestBytes = 4 << 20

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
		m := Memory{ID: newID(), Scope: strArg(args, "scope", "global"), Type: strArg(args, "type", "insight"), Title: strArg(args, "title", ""), Text: strArg(args, "text", ""), Source: strArg(args, "source", "mcp"), CreatedAt: time.Now().Format(time.RFC3339)}
		if m.Title == "" || m.Text == "" {
			return nil, errors.New("title and text required")
		}
		if err := writeMemory(cfg, m); err != nil {
			return nil, err
		}
		// The vault write succeeded (vault is truth; the index is a derived
		// cache), but a failed rebuild must SURFACE — as a degraded SUCCESS,
		// never an isError result: signaling failure for a write that stuck
		// invites the client to retry, and each retry mints a fresh server-side
		// ID (N retries = N duplicate memories). The structured result keeps
		// the saved memory + its ID so the client has nothing to re-send.
		// (delete_memory below is the deliberate asymmetry: its retry is
		// harmless, and serving deleted content warrants the loud error.)
		if _, rerr := rebuildIndex(ctx, cfg); rerr != nil {
			return map[string]any{
				"memory":      m,
				"index_stale": true,
				"warning":     fmt.Sprintf("memory %s saved, but the search index could not be updated: %v — run `mora index rebuild` (do NOT retry the write; it is saved)", m.ID, rerr),
			}, nil
		}
		return m, nil
	case "read_memory":
		return findMemory(cfg, strArg(args, "id", ""))
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
		return map[string]any{"results": snippetMemories(res, query), "freshness": sourceFreshness(cfg)}, nil
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
		// keeps SERVING the deleted content as if it still existed.
		if _, rerr := rebuildIndex(ctx, cfg); rerr != nil {
			return nil, fmt.Errorf("memory %s deleted, but the search index could not be updated and may still serve it: %w — run `mora index rebuild`", id, rerr)
		}
		return map[string]any{"deleted": id}, nil
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

// sourceFreshness maps each synced source to its last-synced timestamp, scanning
// only the sync/ dir (the watermark store lives under brief/ and is never read
// here). It keys off the loaded SyncStatus.Source — NOT a "google-" filename-
// prefix strip, which mis-keyed iMessage ("imessage-<name>.json" → "imessage-
// <name>" instead of "imessage"). A never-synced source (status present but
// LastSynced=="") is INCLUDED with an empty value so it can read "unavailable"
// downstream (SC#3 gap) rather than being silently dropped, which hid a broken
// source.
func sourceFreshness(cfg Config) map[string]string {
	out := map[string]string{}
	dir := filepath.Join(cfg.StateDir, "sync")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		st, err := memory.LoadStatus(filepath.Join(dir, e.Name()))
		if err != nil || st == nil {
			continue
		}
		key := st.Source
		if key == "" {
			// Fall back to the filename stem (sans the known prefixes) only when the
			// status carries no Source — never invent a mangled key.
			key = strings.TrimSuffix(e.Name(), ".json")
			key = strings.TrimPrefix(key, "google-")
			key = strings.TrimPrefix(key, "imessage-")
		}
		out[key] = st.LastSynced // "" for a never-synced source — surfaced, not dropped.
	}
	return out
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

// scheduleRunAtLoad reports whether a job's plist should set RunAtLoad. It is
// FALSE for pulse-daily: that job is a one-shot daily COMMIT (it advances the
// watermark), so re-firing on every reboot/login would consume the morning delta
// before the user reads it (the Plan 03 brief lock covers any residual races).
// Periodic refresh jobs (index/ingest/backup/lint) are idempotent re-runs, so
// they keep RunAtLoad to catch up after a login.
func scheduleRunAtLoad(job string) bool { return job != "pulse-daily" }

// schedulePlistFor renders a job's launchd plist deterministically (no disk I/O)
// so installSchedule and the tests share one builder. The bool is false for an
// unknown job (mirrors the command-map guard).
func schedulePlistFor(cfg Config, job string) (string, bool) {
	cmdArgs, ok := scheduleCommands[job]
	if !ok {
		return "", false
	}
	exe, _ := os.Executable()
	label := "com.mora." + job
	runAtLoad := ""
	if scheduleRunAtLoad(job) {
		runAtLoad = "<key>RunAtLoad</key><true/>\n"
	}
	// launchd jobs do NOT inherit the user's shell environment, so any exported
	// var the job depends on must be snapshotted into the plist at install time
	// (these are PATHS, not secrets):
	//   - MORA_GOOGLE_CREDENTIALS: without it a BYO-creds setup silently hits the
	//     embedded DEV_PLACEHOLDER client on every scheduled Google sync while
	//     terminal syncs keep working — the vault goes stale with no visible error.
	//   - MORA_CONFIG_DIR: without it a re-rooted (scratch/isolated) install's job
	//     runs against the DEFAULT vault — syncing/advancing the wrong installation.
	var envVars []string
	if creds := os.Getenv("MORA_GOOGLE_CREDENTIALS"); creds != "" {
		envVars = append(envVars, "<key>MORA_GOOGLE_CREDENTIALS</key><string>"+creds+"</string>")
	}
	if cfgDir := os.Getenv("MORA_CONFIG_DIR"); cfgDir != "" {
		envVars = append(envVars, "<key>MORA_CONFIG_DIR</key><string>"+cfgDir+"</string>")
	}
	envBlock := ""
	if len(envVars) > 0 {
		envBlock = "<key>EnvironmentVariables</key><dict>" + strings.Join(envVars, "") + "</dict>\n"
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string>%s</array>
%s%s%s
<key>StandardOutPath</key><string>%s</string>
<key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, label, exe, plistArgs(cmdArgs), runAtLoad, envBlock, launchdSchedule(job), filepath.Join(cfg.StateDir, job+".out.log"), filepath.Join(cfg.StateDir, job+".err.log"))
	return plist, true
}

func installSchedule(stdout io.Writer, cfg Config, job string) error {
	cmdArgs, ok := scheduleCommands[job]
	if !ok {
		return fmt.Errorf("unknown job %q", job)
	}
	exe, _ := os.Executable()
	if runtime.GOOS == "darwin" {
		dir := filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents")
		label := "com.mora." + job
		plist, _ := schedulePlistFor(cfg, job)
		if err := atomicWrite(filepath.Join(dir, label+".plist"), []byte(plist), 0o644); err != nil {
			return err
		}
		okf(stdout, "installed launchd job %s", label)
		return nil
	}
	// Linux / WSL2: launchd unavailable. cron also won't inherit the shell env,
	// so a re-rooted install must carry MORA_CONFIG_DIR on the cron line or the
	// job runs against the default vault (mirrors the launchd EnvironmentVariables
	// snapshot above).
	cronEnv := ""
	if cfgDir := os.Getenv("MORA_CONFIG_DIR"); cfgDir != "" {
		cronEnv = "MORA_CONFIG_DIR=" + cfgDir + " "
	}
	if google.IsWSL() {
		fmt.Fprintf(stdout, "WSL detected: no launchd. Add to crontab or run manually:\n  */60 * * * * %s%s %s\nOr just run `mora sync google` when you want fresh data.\n", cronEnv, exe, cmdArgs)
		return nil
	}
	fmt.Fprintf(stdout, "Linux: launchd unavailable. cron line:\n  */60 * * * * %s%s %s\nOr a systemd user timer. Or run `mora sync google` manually.\n", cronEnv, exe, cmdArgs)
	return nil
}

func listSchedules(stdout io.Writer, cfg Config) error {
	if runtime.GOOS == "darwin" {
		matches, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", "com.mora.*.plist"))
		for _, m := range matches {
			fmt.Fprintln(stdout, filepath.Base(m))
		}
		return nil
	}
	fmt.Fprintln(stdout, "cron listing not implemented")
	return nil
}

func tarGz(out, root string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(filepath.Dir(root), path)
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(tw, in)
		return err
	})
}

func emit(w io.Writer, v any, jsonOut bool) error {
	if jsonOut {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(w, string(b))
		return nil
	}
	sty := newStyler(w, jsonOut)
	switch x := v.(type) {
	case Memory:
		fmt.Fprintf(w, "%s\t%s\t%s\n", sty.dim(x.ID), sty.dim(x.Scope), x.Title)
	case []Memory:
		for _, m := range x {
			fmt.Fprintf(w, "%s\t%s\t%s\n", sty.dim(m.ID), sty.dim(m.Scope), m.Title)
		}
	case []catalogRow:
		for _, r := range x {
			// Off-path stays byte-identical ("enabled"/"disabled"); glyph + color
			// only appear on a real TTY.
			state := "disabled"
			if r.Enabled {
				state = "enabled"
			}
			if sty.on {
				if r.Enabled {
					state = sty.ok("● enabled")
				} else {
					state = sty.dim("○ disabled")
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", r.Type, r.Name, state)
		}
	default:
		fmt.Fprintf(w, "%v\n", v)
	}
	return nil
}

func atomicWrite(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Stage through a unique temp file (not a fixed `<path>.tmp`) so two
	// processes writing the same target never share, truncate, or rename each
	// other's in-flight temp. The temp stays in
	// the target dir so the final os.Rename remains atomic on the same
	// filesystem. NOTE: this does not fix the higher-level read-modify-write
	// lost-update on sources.json (two writers each load → mutate → save);
	// that needs caller-level serialization and is out of scope here.
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// Remove the temp on any failure path; a no-op once the rename succeeds.
	defer os.Remove(tmp)
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// CreateTemp opens at 0600; raise to the caller's requested mode.
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// usageEvent records a single MCP tool invocation for local analytics.
type usageEvent struct {
	TS      string `json:"ts"`
	Tool    string `json:"tool"`
	Query   string `json:"query,omitempty"` // raw tier only; never sent
	Scope   string `json:"scope,omitempty"`
	Results int    `json:"results"`
	Millis  int64  `json:"millis"`
}

func usageEnabled(cfg Config) bool {
	if os.Getenv("DO_NOT_TRACK") == "1" {
		return false
	}
	if _, err := os.Stat(filepath.Join(cfg.StateDir, "usage", "OFF")); err == nil {
		return false
	}
	return true
}

func logUsage(cfg Config, e usageEvent) {
	if !usageEnabled(cfg) {
		return
	}
	e.TS = time.Now().UTC().Format(time.RFC3339)
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	_ = appendFile(filepath.Join(cfg.StateDir, "usage", "events.jsonl"), string(b)+"\n")
}

func appendFile(path, line string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(line)
	return err
}

func defaultPriorityMap() string {
	return `# Priority Map

> P0 = this week, P1 = this month, P2 = backlog.

## P0 — Active This Week

1. **Set up Mora** — connect a source and run your first recall.
   - Outcome: daily use across CLI/MCP memory recall.

## P1 — This Month

- **Connector hardening** — filesystem + Google read-only ingestion.

## P2 — Backlog

- **Embeddings** — defer until FTS5 is proven insufficient.
`
}

func defaultLiveTasks() string {
	return `# Live Tasks

| Task | Domain | Owner | Pri | Status | Blocker | Horizon | Last touched |
|------|--------|-------|-----|--------|---------|---------|--------------|
`
}

func defaultHeartbeat() string {
	return `# HEARTBEAT

Read in order: index.md, priority-map.md, live-tasks.md, auto-resolver.md, meetings/ledger.md, log.md.
Run ` + "`mora pulse --write --digest`" + ` to reconcile tasks and stale work.
`
}

func defaultAutoResolver() string {
	return `# Auto Resolver

- P0 without live task: create a live task.
- Owner-flagged blocker: surface in digest, do not auto-action.
- Routine successful cron: log only.
- Failed cron twice: report exact blocker.
`
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parseTags(s string) []string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	return splitCSV(s)
}

func tableCols(line string) []string {
	raw := strings.Split(strings.Trim(line, "|"), "|")
	out := make([]string, 0, len(raw))
	for _, c := range raw {
		out = append(out, strings.TrimSpace(c))
	}
	return out
}

func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "mem_" + time.Now().Format("20060102_150405") + "_" + hex.EncodeToString(b[:])
}

func ContentHash(s string) string {
	// FNV-like small stable hash without another dependency.
	var h uint64 = 1469598103934665603
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return strconv.FormatUint(h, 16)
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

func quoteYAML(s string) string {
	if strings.ContainsAny(s, ":#[]") {
		return strconv.Quote(s)
	}
	return s
}

// isHelpFlag reports whether a subcommand arg is a help request. Used by subcommands
// (sync, search) that otherwise treat a leading flag as data and act on it.
func isHelpFlag(s string) bool {
	return s == "--help" || s == "-h" || s == "help"
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

// ftsToken normalizes a raw field into its bare term and a lowercase key used
// for stopword lookup. The key takes the part before any apostrophe (straight
// or curly) so contractions collapse to their head ("what's"→"what",
// "it's"→"it", curly "what’s"→"what").
func ftsToken(f string) (term, key string) {
	term = strings.Trim(f, `"':;,.!?()[]{}<>-`)
	if term == "" {
		return "", ""
	}
	key = strings.ToLower(term)
	if i := strings.IndexAny(key, "'’"); i > 0 {
		key = key[:i]
	}
	return term, key
}

// ftsIsStopword decides whether a token is a droppable function word. It is
// deliberately case-aware: a function word is dropped only when written in
// lowercase. An explicit capital or all-caps form (Will, WHO, IT, CAN, AM)
// signals a proper noun or acronym that is discriminative in a real query, so
// it survives — this generalizes past a hand-picked collision list to protect
// every name/acronym (Mora, Neil, GEO, MFA, IP, SF, …). Single-character
// function words ("a", "i") are pure noise and always dropped regardless of case.
func ftsIsStopword(term, key string) bool {
	if !ftsStopwords[key] {
		return false
	}
	if utf8.RuneCountInString(term) == 1 {
		return true
	}
	return term == strings.ToLower(term)
}

func ftsQuery(q string) string {
	// Build an OR of quoted content tokens. Space-joining (the original behavior)
	// made FTS5 treat the query as an implicit AND of every token, so a
	// natural-language query like "what did neil say about the offsite" matched
	// nothing (it required every word, stopwords included). OR-joining lets any
	// term match while bm25 ranks the best matches first.
	//
	// But a pure OR of *every* token dilutes ranking: stopwords ("the/with/what")
	// match nearly everything, ballooning the candidate pool and letting docs that
	// hit several common words (while missing the rare, meaningful ones) outrank
	// the true match. Measured on Adit's real-query golden set, dropping function
	// words lifts FTS recall@5 0.591→0.667 (and the hybrid surface 0.394→0.439),
	// with no query regressing inside the top-5 cutoff. So we OR only the
	// content terms; if a query is ALL stopwords we fall back to every token so we
	// never emit an empty MATCH (FTS5 errors on ""). Each term is double-quoted so
	// operators/specials (AND, OR, NOT, *, :, -) inside a term can't raise
	// "fts5: syntax error".
	type tok struct{ term, key string }
	var toks []tok
	for _, f := range strings.Fields(q) {
		term, key := ftsToken(f)
		if term == "" {
			continue
		}
		toks = append(toks, tok{term, key})
	}
	content := make([]tok, 0, len(toks))
	for _, t := range toks {
		if ftsIsStopword(t.term, t.key) {
			continue
		}
		content = append(content, t)
	}
	if len(content) == 0 {
		content = toks // all-stopword query: keep everything rather than match nothing
	}
	terms := make([]string, 0, len(content))
	for _, t := range content {
		terms = append(terms, `"`+strings.ReplaceAll(t.term, `"`, `""`)+`"`)
	}
	return strings.Join(terms, " OR ")
}

func parseSearchArgs(args []string) (string, int, bool, []string, error) {
	scope := ""
	limit := 10
	jsonOut := false
	var query []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			jsonOut = true
		case a == "--scope":
			if i+1 >= len(args) {
				return "", 0, false, nil, errors.New("--scope requires value")
			}
			i++
			scope = args[i]
		case strings.HasPrefix(a, "--scope="):
			scope = strings.TrimPrefix(a, "--scope=")
		case a == "--limit":
			if i+1 >= len(args) {
				return "", 0, false, nil, errors.New("--limit requires value")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return "", 0, false, nil, err
			}
			limit = n
		case strings.HasPrefix(a, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit="))
			if err != nil {
				return "", 0, false, nil, err
			}
			limit = n
		default:
			query = append(query, a)
		}
	}
	return scope, limit, jsonOut, query, nil
}

func curatedAllowedExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".md", ".markdown", ".txt", ".text", ".rst", ".json", ".yaml", ".yml", ".toml", ".csv":
		return true
	default:
		return false
	}
}

// curatedExtractExt reports whether ext is a non-plain-text format Mora ingests by
// EXTRACTING its text (vs reading raw bytes). Today: .docx (stdlib zip+xml) and
// .pdf (pinned ledongthuc/pdf — pure Go, recover-wrapped, capped; see pdf.go).
// PDF extraction is lossy on exotic font encodings and yields nothing on scanned
// documents (no OCR — that would break the no-CGO/single-binary constraint); such
// files are skipped, never indexed as garbage.
func curatedExtractExt(ext string) bool {
	switch strings.ToLower(ext) {
	case ".docx", ".pdf":
		return true
	default:
		return false
	}
}

// docxMaxDecompressed caps the decompressed bytes read from word/document.xml. A
// .docx is a ZIP, so a few-KB file can decompress to gigabytes (a zip bomb); the
// LimitReader stops well before that. Past the cap the XML is truncated and the
// decoder errors — extractDocxText returns that error and the file is skipped.
const docxMaxDecompressed = 8 << 20 // 8 MiB

// extractDocxText returns the visible text of a .docx by reading word/document.xml
// (the main body) and concatenating its <w:t> runs, breaking paragraphs on </w:p>.
// Pure stdlib (archive/zip + encoding/xml) — no new dependency, no CGO. Untrusted
// input is bounded by docxMaxDecompressed (zip-bomb guard); Go's xml.Decoder does
// not expand DTD entities, so billion-laughs does not apply.
func extractDocxText(path string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	var doc *zip.File
	for _, f := range zr.File {
		if f.Name == "word/document.xml" {
			doc = f
			break
		}
	}
	if doc == nil {
		return "", fmt.Errorf("not a docx (no word/document.xml): %s", filepath.Base(path))
	}
	rc, err := doc.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()

	dec := xml.NewDecoder(io.LimitReader(rc, docxMaxDecompressed))
	var b strings.Builder
	inText := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t": // <w:t> — a text run
				inText = true
			case "tab":
				b.WriteByte('\t')
			case "br", "cr":
				b.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p": // paragraph boundary
				b.WriteByte('\n')
			}
		case xml.CharData:
			if inText {
				b.Write(t)
			}
		}
	}
	return strings.TrimSpace(b.String()), nil
}

func curatedMetadataFile(name string) bool {
	switch name {
	case "go.mod", "go.sum", "Makefile", "Dockerfile", "CLAUDE.md", "AGENTS.md",
		"README", "package.json", "pyproject.toml", "requirements.txt", "Cargo.toml", "CHANGELOG.md":
		return true
	}
	return false
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

func plistArgs(args string) string {
	var b strings.Builder
	for _, a := range strings.Fields(args) {
		fmt.Fprintf(&b, "<string>%s</string>", a)
	}
	return b.String()
}

func launchdSchedule(job string) string {
	switch job {
	case "index-hourly", "ingest-hourly":
		return "<key>StartInterval</key><integer>3600</integer>"
	case "pulse-daily":
		return "<key>StartCalendarInterval</key><dict><key>Hour</key><integer>8</integer><key>Minute</key><integer>0</integer></dict>"
	case "backup-daily":
		return "<key>StartCalendarInterval</key><dict><key>Hour</key><integer>2</integer><key>Minute</key><integer>0</integer></dict>"
	case "git-daily":
		return "<key>StartCalendarInterval</key><dict><key>Hour</key><integer>3</integer><key>Minute</key><integer>0</integer></dict>"
	case "lint-weekly":
		return "<key>StartCalendarInterval</key><dict><key>Weekday</key><integer>0</integer><key>Hour</key><integer>9</integer><key>Minute</key><integer>0</integer></dict>"
	default:
		return "<key>StartInterval</key><integer>3600</integer>"
	}
}
