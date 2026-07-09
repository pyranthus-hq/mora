package mora

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// demo.go seeds a self-contained, synthetic vault so anyone can SEE Mora's
// cross-source recall without connecting their real accounts — and so the launch
// demo can be recorded with ZERO real PII on screen.
//
// Why a dedicated command (not `mora write` in a loop): `mora write` sets no
// provider/meta, so the seeded memories never group into the brief's per-source
// sections (sourceInstanceKey rejects an empty Provider) and never produce entity
// graph person nodes (the graph is compiled from Meta). This command mints proper
// frontmatter — provider + structured Meta — exactly as the live connectors do,
// then enables matching source rows so the brief enumerates them. The result is a
// real, live-binary render of the flagship: one daily brief across Gmail/iMessage/
// Calendar/Files, and one person resolved across all of them.

// demoMemory is one synthetic vault entry. Dates are RELATIVE (off, in days from
// now) so a seeded demo always lands inside the brief's cold-start courtesy window
// (last 7d for mail/texts, next 7d for calendar) no matter when `mora demo` runs —
// a fixed date would silently age out and render an empty brief.
type demoMemory struct {
	id       string
	provider string // gmail | imessage | calendar | filesystem (catalog Type)
	typ      string // email | imessage | event | note
	title    string
	off      int // days relative to now; negative = past, positive = future
	body     string
	meta     map[string]any // from/to/cc/attendees/organizer/participants/names — occurred_at is stamped by toMemory
}

// toMemory realizes a demoMemory against a concrete clock, mirroring the Memory a
// real connector mapper would mint (provider + Meta + occurred_at).
func (dm demoMemory) toMemory(now time.Time) Memory {
	ts := now.AddDate(0, 0, dm.off).UTC().Format(time.RFC3339)
	var meta map[string]any
	if len(dm.meta) > 0 {
		meta = make(map[string]any, len(dm.meta)+1)
		for k, v := range dm.meta {
			meta[k] = v
		}
		if _, ok := meta["occurred_at"]; !ok {
			meta["occurred_at"] = ts
		}
	}
	return Memory{
		ID:          dm.id,
		Scope:       "personal",
		Type:        dm.typ,
		Title:       dm.title,
		Tags:        []string{dm.provider},
		Source:      dm.id,
		Provider:    dm.provider,
		ProviderID:  dm.id,
		ContentHash: memory.ContentHash(dm.title, dm.body),
		CreatedAt:   ts,
		Text:        dm.body,
		Meta:        meta,
	}
}

// cmdDemo seeds an isolated synthetic install and prints how to explore it.
//
// Safety: the demo install is FULLY isolated. The Config is constructed directly
// (never loadConfig) and rooted entirely under --dir, and guardDemoDir refuses any
// path that resolves onto the real ~/.config/mora or ~/vault/mora — so a demo can
// never repoint, reseed, or reindex the user's live vault (the failure class
// MORA_CONFIG_DIR exists to prevent).
func cmdDemo(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dir := fs.String("dir", "", "isolated directory for the demo install (default: <tmpdir>/mora-demo)")
	force := fs.Bool("force", false, "reseed even if a demo vault already exists at --dir (overwrites it)")
	quiet := fs.Bool("quiet", false, "suppress the next-steps hint")
	if err := fs.Parse(args); err != nil {
		return err
	}

	demoDir := *dir
	if demoDir == "" {
		demoDir = filepath.Join(os.TempDir(), "mora-demo")
	}
	abs, err := filepath.Abs(expandHome(demoDir))
	if err != nil {
		return err
	}
	demoDir = abs

	cfg := Config{
		VaultDir:  filepath.Join(demoDir, "vault"),
		ConfigDir: demoDir,
		DataDir:   filepath.Join(demoDir, "data"),
		StateDir:  filepath.Join(demoDir, "state"),
	}
	if err := guardDemoDir(demoDir, cfg); err != nil {
		return err
	}

	// Clobber-protection: --force may only ever overwrite a directory THIS command
	// created (it carries a .mora-demo marker). A populated dir without the marker
	// is someone's real data — never touch it, even with --force.
	markerPath := filepath.Join(cfg.ConfigDir, demoMarker)
	_, markerErr := os.Stat(markerPath)
	demoOwned := markerErr == nil
	if entries, _ := os.ReadDir(memoriesRoot(cfg)); len(entries) > 0 {
		if !demoOwned {
			return fmt.Errorf("%s already contains memories but is not a demo install (no %s marker) — refusing to touch it; choose an empty --dir", cfg.VaultDir, demoMarker)
		}
		if !*force {
			return fmt.Errorf("a demo vault already exists at %s — pass --force to reseed (it will be overwritten)", cfg.VaultDir)
		}
		if err := os.RemoveAll(memoriesRoot(cfg)); err != nil {
			return err
		}
	}

	// Scaffold the isolated install (the non-interactive core of `mora init`).
	for _, d := range []string{cfg.VaultDir, cfg.ConfigDir, cfg.DataDir, cfg.StateDir, memoriesRoot(cfg), sourcesRoot(cfg), filepath.Join(cfg.StateDir, "sync"), filepath.Join(cfg.ConfigDir, "tokens")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	// Stamp the demo marker before any data lands, so a re-run can recognize this
	// as a demo-owned dir (the only thing --force is allowed to overwrite).
	if err := os.WriteFile(markerPath, []byte("mora demo install — synthetic data, safe to delete\n"), 0o644); err != nil {
		return err
	}
	if err := writeConfig(cfg); err != nil {
		return err
	}
	if err := scaffoldControlFiles(cfg); err != nil {
		return err
	}
	// Enable synthetic source rows so the brief ENUMERATES and buckets them —
	// without an enabled+ingesting source a provider's memories build no section.
	if err := saveSources(cfg, demoSources()); err != nil {
		return err
	}
	// Stamp a healthy, just-now sync status per source so the brief reads each
	// section as a fresh "baseline (N)" rather than "unavailable (sync error)" —
	// the synthetic data IS the successful sync.
	if err := writeDemoSyncStatuses(cfg); err != nil {
		return err
	}
	// A couple of open loops left untouched, so the brief's "Open tasks" renders.
	if err := writeDemoTasks(cfg); err != nil {
		return err
	}

	now := time.Now().UTC()
	for _, dm := range demoMemories() {
		if err := writeMemory(cfg, dm.toMemory(now)); err != nil {
			return fmt.Errorf("seeding %s: %w", dm.id, err)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "seeded a synthetic demo vault at %s (synthetic data only — no real accounts touched)\n", cfg.VaultDir)
	if !*quiet {
		fmt.Fprintf(stdout, `
Explore it by pointing MORA_CONFIG_DIR at the demo install:

  export MORA_CONFIG_DIR=%s
  mora brief --clean              # one digest across Calendar, Texts, Emails, Files
  mora graph "Priya Nair"         # one person resolved across email, texts, and calendar
  mora entities                   # the people/projects across the demo vault
  mora think "what is still open on Project Halcyon"

Delete %s when you're done — it never touches your real vault.
`, demoDir, demoDir)
	}
	return nil
}

// demoMarker is the sentinel a demo install carries so --force can only ever
// overwrite a directory this command itself created (never an arbitrary
// Mora-shaped dir a user pointed at).
const demoMarker = ".mora-demo"

// guardDemoDir refuses any demo path that would collide with the user's real
// install. The demo is a throwaway and must never become — or delete — the live
// one. It defends against three failure modes that have actually bitten the live
// vault: an exact path match, a nested path (--dir ~/vault/mora/x), and a symlink
// whose target is the live vault. All paths are symlink-resolved on their deepest
// existing ancestor before comparison.
func guardDemoDir(demoDir string, cfg Config) error {
	if demoDir == "" || filepath.Clean(demoDir) == string(filepath.Separator) {
		return errors.New("refusing to use an unsafe demo dir")
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		return nil
	}
	if pathsOverlap(demoDir, home) {
		return fmt.Errorf("refusing to seed the demo into your home directory (%s) — pass --dir <a scratch path>", home)
	}
	// Compare the symlink-resolved demo dirs against the symlink-resolved live
	// install. Overlap in EITHER direction (equal / ancestor / descendant) is
	// fatal — a nested demo or a symlinked vault both end up touching live data.
	live := []string{
		filepath.Join(home, "vault", "mora"),
		filepath.Join(home, ".config", "mora"),
	}
	for _, target := range []string{cfg.VaultDir, cfg.ConfigDir, demoDir} {
		for _, p := range live {
			if pathsOverlap(target, p) {
				return fmt.Errorf("refusing: %s resolves onto your real Mora install (%s) — choose a scratch path", demoDir, p)
			}
		}
	}
	return nil
}

// pathsOverlap reports whether a and b are the same path or one contains the
// other, after resolving symlinks on each path's deepest existing ancestor (so a
// symlinked demo dir cannot smuggle a live path past the equality check).
func pathsOverlap(a, b string) bool {
	a, b = resolveExisting(a), resolveExisting(b)
	return a == b || isUnder(a, b) || isUnder(b, a)
}

// isUnder reports whether child is at or beneath parent (lexically, on cleaned
// absolute paths).
func isUnder(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// resolveExisting returns p with symlinks resolved as far as the path exists on
// disk (EvalSymlinks fails on a not-yet-created leaf), re-joining the missing
// tail. This catches a /tmp/demo/vault -> ~/vault/mora symlink before any write.
func resolveExisting(p string) string {
	p = filepath.Clean(p)
	tail := ""
	for {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(resolved, tail)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, tail) // reached the root with nothing resolvable
		}
		tail = filepath.Join(filepath.Base(p), tail)
		p = parent
	}
}

// demoSources enables the four connectors the demo seeds, so the brief enumerates
// them (Calendar / Texts / Emails / Files). Enabled is set explicitly (the demo IS
// the consent moment for synthetic data).
func demoSources() []Source {
	now := time.Now().Format(time.RFC3339)
	mk := func(t string) Source {
		return Source{Name: t, Type: t, Scope: "personal", Enabled: ptr(true), CreatedAt: now}
	}
	fsSrc := mk("filesystem")
	fsSrc.Path = filepath.Join("demo", "notes")
	return []Source{mk("gmail"), mk("calendar"), mk("imessage"), fsSrc}
}

// writeDemoSyncStatuses stamps a healthy, just-now SyncStatus for each demo source
// so classifyState reads them as fresh baselines (no error, recent success), giving
// the brief clean "baseline (N)" section headings instead of "unavailable".
func writeDemoSyncStatuses(cfg Config) error {
	now := time.Now().UTC().Format(time.RFC3339)
	counts := map[string]int{}
	for _, dm := range demoMemories() {
		counts[dm.provider]++
	}
	for _, s := range demoSources() {
		path := syncStatusPathFor(cfg, s)
		if path == "" {
			continue
		}
		st := &memory.SyncStatus{
			Source:        s.Name,
			LastSynced:    now,
			LastAttemptAt: now,
			LastSuccessAt: now,
			ItemCount:     counts[s.Type],
		}
		if err := memory.SaveStatus(path, st); err != nil {
			return err
		}
	}
	return nil
}

// writeDemoTasks plants two open loops in live-tasks.md, dated past the staleness
// cutoff so the brief's "Open tasks" section has something honest to show (these
// are manual task rows — the brief does not extract tasks from messages).
func writeDemoTasks(cfg Config) error {
	old := time.Now().AddDate(0, 0, -6).Format("2006-01-02")
	var b strings.Builder
	b.WriteString(defaultLiveTasks())
	fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s |\n",
		"Send Priya the Halcyon security addendum", "Halcyon", "me", "P1", "open", "", "this week", old)
	fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s |\n",
		"Reply to Marcus about the Northwind MSA", "Northwind", "me", "P2", "open", "", "this week", old)
	return atomicWrite(filepath.Join(cfg.VaultDir, "live-tasks.md"), []byte(b.String()), 0o644)
}

// demoMemories is the synthetic narrative: Sam Rivera runs "Project Halcyon" for
// the client "Northwind"; Priya Nair and Marcus Vance are on the Northwind side.
// Priya appears across Gmail, iMessage, AND Calendar under ONE address
// (priya@northwind.com — her iMessage handle is her email, so RULE-1 mailbox
// merge collapses all three into a single person node). Everything is fictional.
func demoMemories() []demoMemory {
	const (
		sam    = "sam@halcyon.dev"
		priya  = "priya@northwind.com"
		marcus = "marcus@northwind.com"
		dana   = "dana@halcyon.dev"
	)
	names := map[string]any{
		sam:    "Sam Rivera",
		priya:  "Priya Nair",
		marcus: "Marcus Vance",
		dana:   "Dana Brooks",
	}
	pick := func(addrs ...string) map[string]any {
		m := make(map[string]any, len(addrs))
		for _, a := range addrs {
			m[a] = names[a]
		}
		return m
	}
	return []demoMemory{
		// Calendar (future-dated; the brief shows the upcoming 7 days).
		{
			id: "calendar_event/halcyon-kickoff", provider: "calendar", typ: "event", off: 1,
			title: "Project Halcyon — Northwind kickoff",
			body:  "Kickoff for the Halcyon rollout with the Northwind team. Agenda: scope, the security addendum, and the phase-1 timeline.",
			meta: map[string]any{
				"organizer": sam,
				"attendees": []string{sam, priya, marcus},
				"names":     pick(sam, priya, marcus),
			},
		},
		{
			id: "calendar_event/halcyon-design-review", provider: "calendar", typ: "event", off: 3,
			title: "Halcyon design review",
			body:  "Walk through spec v2 internally before sharing it with Northwind.",
			meta: map[string]any{
				"organizer": sam,
				"attendees": []string{sam, priya, dana},
				"names":     pick(sam, priya, dana),
			},
		},
		// Texts (past-dated). iMessage handles are the people's emails, so the same
		// person merges across sources with no special-casing.
		{
			id: "imessage_chat/priya-1on1", provider: "imessage", typ: "imessage", off: -1,
			title: "Priya Nair",
			body:  "Priya Nair: Confirming Thursday's kickoff — did legal clear the security addendum?\nSam Rivera: Pushing spec v2 tonight; the addendum goes to Marcus tomorrow.",
			meta: map[string]any{
				"participants": []map[string]string{
					{"handle": priya, "name": "Priya Nair"},
					{"handle": sam, "name": "Sam Rivera"},
				},
			},
		},
		{
			id: "imessage_chat/halcyon-crew", provider: "imessage", typ: "imessage", off: -2,
			title: "Halcyon launch crew",
			body:  "Marcus Vance: Northwind security wants the addendum by Friday.\nPriya Nair: On it.\nDana Brooks: Spec v2 draft is ready for review.",
			meta: map[string]any{
				"participants": []map[string]string{
					{"handle": priya, "name": "Priya Nair"},
					{"handle": marcus, "name": "Marcus Vance"},
					{"handle": dana, "name": "Dana Brooks"},
					{"handle": sam, "name": "Sam Rivera"},
				},
			},
		},
		// Emails (past-dated).
		{
			id: "gmail_thread/halcyon-kickoff-agenda", provider: "gmail", typ: "email", off: -1,
			title: "Re: Halcyon kickoff agenda",
			body:  "From: Priya Nair\n\nThanks Sam — the agenda looks good. Can you bring the security addendum draft? Marcus will join for the review.",
			meta: map[string]any{
				"from":  []string{priya},
				"to":    []string{sam},
				"names": pick(priya, sam),
			},
		},
		{
			id: "gmail_thread/northwind-security-addendum", provider: "gmail", typ: "email", off: -3,
			title: "Northwind security review — addendum needed",
			body:  "From: Marcus Vance\n\nSam, our security team needs the data-handling addendum before kickoff. Priya can route it internally.",
			meta: map[string]any{
				"from":  []string{marcus},
				"to":    []string{sam},
				"cc":    []string{priya},
				"names": pick(marcus, sam, priya),
			},
		},
		{
			id: "gmail_thread/halcyon-spec-v2", provider: "gmail", typ: "email", off: -2,
			title: "Halcyon spec v2",
			body:  "From: Sam Rivera\n\nPriya — spec v2 attached. Key change: phase 1 is scoped to two regions.",
			meta: map[string]any{
				"from":  []string{sam},
				"to":    []string{priya},
				"names": pick(sam, priya),
			},
		},
		// Files (past-dated). No people meta — a plain note on disk.
		{
			id: "fs/halcyon-spec", provider: "filesystem", typ: "note", off: -2,
			title: "halcyon-spec.md",
			body:  "# Project Halcyon — Spec v2\n\nScope: phased rollout to Northwind, two regions in phase 1.\nOpen: security addendum sign-off.",
		},
		{
			id: "fs/northwind-msa", provider: "filesystem", typ: "note", off: -4,
			title: "northwind-msa.md",
			body:  "# Northwind MSA notes\n\nMaster service agreement draft.\nPending: the data-handling addendum (security review).",
		},
	}
}
