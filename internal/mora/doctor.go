package mora

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/google"
	"github.com/pyranthus-hq/mora/internal/imessage"
)

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

// doctorCheck is one named health probe. Critical checks gate `--strict` (and
// the JSON report's `healthy`); non-critical ones are advisory only — notably
// the iMessage/macOS surfaces, which "warn" off-darwin without meaning Mora is
// broken, so a Linux regression run still reports healthy.
type doctorCheck struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Critical bool   `json:"critical"`
}

// doctorReport is the machine-readable shape emitted by `mora doctor --json`,
// designed so a release regression harness can gate on `.healthy` (and inspect
// individual checks) instead of scraping human text.
type doctorReport struct {
	Healthy           bool          `json:"healthy"`
	Checks            []doctorCheck `json:"checks"`
	StorageBytes      int64         `json:"storage_bytes"`
	StorageStatus     string        `json:"storage_status"`
	GitSyncConfigured bool          `json:"git_sync_configured"`
	// Share egress surface (`mora share`): how many scopes this machine
	// publishes and how many foreign corpora it subscribes to.
	SharePublishes     int           `json:"share_publishes"`
	ShareSubscriptions int           `json:"share_subscriptions"`
	Version            string        `json:"version"`
	Platform           string        `json:"platform"`
	RebuildBlock       *rebuildBlock `json:"rebuild_block,omitempty"`
	// Sources is the per-connector freshness snapshot (HEALTH-01/-03). Always a
	// deterministic non-null array — `[]` on a vault with no enabled sources,
	// never `null` — so a JSON consumer never needs a nil-check.
	Sources []sourceHealth `json:"sources"`
	// Index and Producers are the Gate 2 typed arms (HEALTH-09/-10/-11/-12). Index
	// is a value (always present); Producers is a deterministic non-null array
	// (empty until PR 4 builds the producer ledger).
	Index     indexHealth      `json:"index"`
	Producers []producerHealth `json:"producers"`
}

// doctorClock is the wall clock doctor's freshness checks (and --pulse) resolve
// against — a var (mirrors briefClock/prepClock) so tests can pin "now" instead
// of racing the real clock (D-03 determinism invariant: no time.Now() in a
// check path). Production never reassigns it.
var doctorClock = time.Now

// doctorNotifyRunner is the injectable exec seam `doctor --pulse` posts its
// toast through (notify.go's notifyRunner). Defaults to the real osascript
// runner; tests swap it (t.Cleanup-restore, never t.Parallel) to capture argv
// without spawning a process, mirroring notify_test.go's recordingRunner.
var doctorNotifyRunner notifyRunner = osascriptRunner

// sourceHealthDetailLine renders one unhealthy source's human-readable line,
// e.g. "gmail        FAILED — last success 52h ago — database or disk is full (13)".
func sourceHealthDetailLine(h sourceHealth, now time.Time) string {
	var status string
	switch h.State {
	case healthNever:
		return fmt.Sprintf("%-12s NEVER SYNCED", h.Key)
	case healthFailed:
		status = "FAILED"
	case healthStale:
		status = "STALE"
	default:
		status = strings.ToUpper(h.State)
	}
	ago := "unknown"
	if t, err := time.Parse(time.RFC3339, h.LastSuccessAt); err == nil {
		ago = humanizeAgo(now.Sub(t))
	}
	line := fmt.Sprintf("%-12s %s — last success %s ago", h.Key, status, ago)
	if h.LastError != "" {
		line += " — " + h.LastError
	}
	return line
}

// cmdDoctorPulse is `mora doctor --pulse` (HEALTH-02 delivery): the freshness-
// only health check meant to run on a schedule. It skips every other doctor
// check, posts a best-effort native toast when unhealthy (notifyHealthAlarm —
// GOOS/env-gated, best-effort, never fails the check itself), and returns a
// TYPED exit-code error so main() exits 2 — a plain error maps to exit 1 and
// could never distinguish "sick" from "broken" for a caller/automation.
// --pulse --json emits ONLY the sources array (no banner text, no other
// checks); --pulse --strict is a no-op (--pulse already exits 2 on its own).
func cmdDoctorPulse(cfg Config, now time.Time, jsonOut bool, stdout io.Writer) error {
	// Phase 1 (evaluate): classify the PRIOR ledger — INCLUDING the watchman's own
	// prior doctor-pulse stamp — before writing anything, so a genuinely-missed
	// cadence is honestly reported (E4). --pulse now covers sources, the index arm
	// AND producer liveness; still freshness-only in spirit (no O(vault) walk).
	srcHealth := sourceHealthAll(cfg, now)
	banner := healthBannerFrom(Health{
		Sources:   srcHealth,
		Index:     indexHealthOf(cfg, now),
		Producers: producerHealthAll(cfg, now),
	})

	// Phase 2 (stamp): AFTER evaluation, record that THIS pulse ran to COMPLETION —
	// unconditionally on the completion path, including the exit-2 path (E4). A dead
	// watchman must be an alarm, not silence: "doctor-pulse succeeded" means the pulse
	// executed, NOT that everything is healthy, so a legitimately-failing source can
	// never rot the watchman arm to stale. Best-effort (never turns exit-2 into exit-1
	// or masks the verdict). Only the scheduled --pulse path stamps — a plain
	// `mora doctor` never advances it, so a dev running doctor once cannot silence the
	// watchman for a cadence. nonInteractive from the writer gates ADOPTION only.
	defer func() { _ = withProducerStamp(cfg, "doctor-pulse", now, !writerIsTTY(stdout), nil) }()

	if jsonOut {
		b, err := json.MarshalIndent(struct {
			Sources []sourceHealth `json:"sources"`
		}{Sources: srcHealth}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, string(b))
	}

	if banner == "" {
		if !jsonOut {
			sty := newStyler(stdout, false)
			fmt.Fprintf(stdout, "%s all sources fresh\n", sty.ok("ok  "))
		}
		return nil
	}

	if !jsonOut {
		fmt.Fprintln(stdout, banner)
	}
	_ = notifyHealthAlarm(banner, doctorNotifyRunner, runtimeGOOS())
	// Blank message: the banner/JSON already printed above (mirrors loop.go's
	// exitCodeError convention — a non-empty message would double-print to stderr).
	return exitCodeError{code: 2}
}

// doctorFailSummary lists the failing critical checks for the --strict error.
func doctorFailSummary(checks []doctorCheck) string {
	var failed []string
	for _, c := range checks {
		if c.Critical && !c.OK {
			failed = append(failed, c.Name)
		}
	}
	return fmt.Sprintf("%d critical check(s) failed: %s", len(failed), strings.Join(failed, ", "))
}

func cmdDoctor(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "emit a machine-readable JSON health report (with --pulse: only the sources array)")
	strict := fs.Bool("strict", false, "exit non-zero if a critical health check fails (a no-op alongside --pulse, which already exits 2 on any unhealthy source)")
	pulse := fs.Bool("pulse", false, "run ONLY the per-source freshness checks; post a native toast and exit 2 when any source is unhealthy, exit 0 when all are fresh")
	forgetProducer := fs.String("forget-producer", "", "retire a producer by name: remove its expectation and evidence so a stopped or wrongly-adopted job stops reddening health")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	now := doctorClock()

	if *forgetProducer != "" {
		if err := forgetProducerLedger(cfg, *forgetProducer); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "retired producer %q (expectation + evidence removed)\n", *forgetProducer)
		return nil
	}

	if *pulse {
		return cmdDoctorPulse(cfg, now, *jsonOut, stdout)
	}

	tokenDir := filepath.Join(cfg.ConfigDir, "tokens")
	_, vErr := os.Stat(cfg.VaultDir)
	_, iErr := os.Stat(dbPath(cfg))
	_, tErr := os.Stat(tokenDir)
	sources, _ := loadSources(cfg)

	// Ordered so both the JSON report and the text output are deterministic.
	// Critical = "Mora is actually broken if false": the vault is gone, the
	// index is missing, or tokens are colocated with the vault (a data-egress
	// hazard). token_dir/sources_config are advisory (a freshly seeded vault has
	// neither tokens nor configured connectors yet, and that is fine).
	shares, sharesErr := loadShares(cfg)

	checks := []doctorCheck{
		{Name: "vault", OK: vErr == nil, Critical: true},
		{Name: "index_db", OK: iErr == nil, Critical: true},
		{Name: "token_dir", OK: tErr == nil && !strings.HasPrefix(tokenDir, cfg.VaultDir), Critical: false},
		// ▸R sources_config needs a PREDICATE, not just a count: loadSources returns
		// DISABLED rows too, so `len(sources) > 0` stayed green after `mora sources
		// disable gmail` while deleting every source_fresh:* check. Critical now, and
		// green only if some source is enabled OR there is no connector corpus to
		// have an alarm for (a freshly-seeded vault).
		{Name: "sources_config", OK: enabledSourceCount(sources) > 0 || !vaultHasConnectorMemories(cfg), Critical: true},
		{Name: "tokens_disjoint_from_vault", OK: disjointRealPaths(cfg.VaultDir, tokenDir), Critical: true},
		// Only ciphertext belongs in a share staging repo; plaintext markdown
		// there means something is wrong with the export path or the user
		// hand-placed files where `git add -A` will publish them.
		{Name: "share_staging_clean", OK: shareStagingClean(cfg, shares.Publishes), Critical: false},
		// Critical because a corrupt registry fails EVERY search/think once it
		// exists — doctor must not report healthy while recall is down.
		{Name: "shares_registry_readable", OK: sharesErr == nil, Critical: true},
		// Mirrors tokens_disjoint_from_vault: the age identity and decrypted
		// share corpora inside the vault would ride `mora backup`/git-sync.
		{Name: "share_disjoint_from_vault", OK: shareGuardPaths(cfg) == nil, Critical: true},
	}
	// Per-source freshness (HEALTH-01/-03): one critical check per enabled
	// connector instance, so an enabled-but-never-synced/stale/failed source
	// makes `.healthy` false — not just a digest heading nobody reads.
	srcHealth := sourceHealthAll(cfg, now)
	for _, h := range srcHealth {
		checks = append(checks, doctorCheck{Name: "source_fresh:" + h.Key, OK: h.State == healthFresh, Critical: true})
	}

	// Gate 2 index arm (HEALTH-03 completion / HEALTH-09/-12 exposure): the index is
	// a derived corpus that can die silently while every source is fresh, so its
	// state and embedder provenance are critical checks — a stale source timestamp
	// can never mask an older/degraded index.
	idxH := indexHealthOf(cfg, now)
	checks = append(checks, doctorCheck{Name: "index_fresh", OK: idxH.State == idxFresh, Critical: true})
	checks = append(checks, doctorCheck{Name: "index_embedder", OK: idxH.Embedder.Match, Critical: true})
	// ▸R per source TYPE with a corpus but no ENABLED instance: the zero-sources
	// case is the extreme; the realistic one is disabling ONE connector and silently
	// losing its alarm while the others keep doctor green.
	for _, typ := range disabledCorpusTypes(cfg, sources) {
		checks = append(checks, doctorCheck{Name: "source_disabled_with_corpus:" + typ, OK: false, Critical: true})
	}
	// B1a content manifest — the O(vault) recompute runs only OUTSIDE --pulse (we
	// already returned above for --pulse) and never on the MCP hot path. Absent =>
	// unverified (non-critical, so a legacy index does not redden every first
	// doctor); present+mismatch => the index provably does not reflect the vault
	// (critical).
	mOK, mCritical := indexMatchesVault(cfg)
	checks = append(checks, doctorCheck{Name: "index_matches_vault", OK: mOK, Critical: mCritical})

	// PR 4 (HEALTH-11): producer liveness flips PR 1's fail-open contract. With no
	// expectation present the slice is empty, so a user who scheduled nothing is not
	// nagged; once a producer is declared/scheduled/adopted, a silence past 2× its
	// interval fails this critical check — a healthy vault and clean index can no
	// longer report green while nothing has consumed them.
	for _, p := range producerHealthAll(cfg, now) {
		checks = append(checks, doctorCheck{Name: "producer_live:" + p.Name, OK: p.State == prodFresh, Critical: true})
	}
	// E3 consumer-side detector: if the user demonstrably uses the brief surface (a
	// dated artifact exists) but the newest is older than 2× the daily cadence, the
	// surface is stale even if no producer was ever registered.
	if aOK, present := briefArtifactFresh(cfg, now); present {
		checks = append(checks, doctorCheck{Name: "brief_artifact_fresh", OK: aOK, Critical: true})
	}

	// Packet H4: one critical share_fresh:<name> check per subscription (never/
	// unreadable fail closed; a `failed` share still serves its last-good head but
	// is surfaced for investigation). The Index arm carries the per-share sub-arm.
	shareHealth := shareHealthAll(cfg, now)
	for _, sh := range shareHealth {
		checks = append(checks, doctorCheck{Name: "share_fresh:" + sh.Name, OK: sh.State == healthFresh, Critical: true})
	}
	idxH.Shares = shareIndexHealthAll(cfg, now)

	// Whole-product storage accountant (Packet H3b): doctor's storage_status and
	// share admission measure the same footprint. A walk/stat failure is a critical
	// `unknown`, never a silent undercount. This MUST precede the `healthy` collapse
	// below so its critical check is actually counted.
	used, storageErr := productStorageBytes(cfg)
	var st string
	if storageErr != nil {
		st = "unknown"
		used = 0
		checks = append(checks, doctorCheck{Name: "storage_status", OK: false, Critical: true})
	} else {
		st = storageStatus(used)
	}

	healthy := true
	for _, c := range checks {
		if c.Critical && !c.OK {
			healthy = false
		}
	}

	gitSync := false
	if _, err := os.Stat(filepath.Join(cfg.VaultDir, ".git")); err == nil {
		gitSync = true
	}

	if *jsonOut {
		rep := doctorReport{
			Healthy:            healthy,
			Checks:             checks,
			StorageBytes:       used,
			StorageStatus:      st,
			GitSyncConfigured:  gitSync,
			SharePublishes:     len(shares.Publishes),
			ShareSubscriptions: len(shares.Subscriptions),
			Version:            BuildVersion,
			Platform:           runtimeGOOS(),
			Sources:            srcHealth,
			Index:              idxH,
			Producers:          []producerHealth{},
		}
		if rec, present, _ := readBlockRecord(cfg); present {
			rep.RebuildBlock = &rec
		}
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, string(b))
		if *strict && !healthy {
			return fmt.Errorf("doctor: %s", doctorFailSummary(checks))
		}
		return nil
	}

	sty := newStyler(stdout, false)
	if looksSynced(tokenDir) {
		fmt.Fprintf(stdout, "%s token dir looks like a synced location: %s\n", sty.warn("warn"), tokenDir)
	}
	for _, c := range checks {
		if c.OK {
			fmt.Fprintf(stdout, "%s %s\n", sty.ok("ok  "), c.Name)
		} else {
			fmt.Fprintf(stdout, "%s %s\n", sty.warn("warn"), c.Name)
		}
	}
	for _, h := range srcHealth {
		if h.State == healthFresh {
			continue
		}
		fmt.Fprintf(stdout, "     %s\n", sourceHealthDetailLine(h, now))
	}
	if rec, present, _ := readBlockRecord(cfg); present {
		fmt.Fprintf(stdout, "%s index_rebuild BLOCKED (%s; vault %s, index held %d) — fix vault_dir in config.toml then `mora index rebuild`; `--force` only if the current vault is correct (it discards the %d indexed memories)\n",
			sty.warn("warn"), rec.Reason, rec.VaultDir, rec.OldCount, rec.OldCount)
	}
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
	if gitSync {
		fmt.Fprintf(stdout, "%s vault git-sync is configured — the vault LEAVES THIS DEVICE on `mora sync git`\n", sty.warn("warn"))
		fmt.Fprintln(stdout, "     it contains decoded iMessages + Gmail in plaintext; ensure the remote is PRIVATE + user-controlled.")
	}
	// Share egress disclosure: qualify the zero-egress posture for every
	// configured publish, mirroring the git-sync disclosure above.
	for _, p := range shares.Publishes {
		fmt.Fprintf(stdout, "%s share %q publishes scope %s (age-encrypted to %d recipient key(s)) on `mora share push`\n",
			sty.warn("warn"), p.Name, p.Scope, len(p.Recipients))
		fmt.Fprintln(stdout, "     ciphertext only, but keep the remote PRIVATE + user-controlled.")
	}
	// Google auth recency: tokens last weeks so a reauth is rare and invisible —
	// surface "last authed / how long ago" per connected account so the user can
	// tell at a glance when they last signed in.
	printGoogleAuthRecency(cfg, stdout, now)
	// iMessage readiness prints in a dedicated ORDERED block so the Full Disk
	// Access guidance reads top-to-bottom (Surface 3).
	printIMessageReadiness(stdout, false)
	if *strict && !healthy {
		return fmt.Errorf("doctor: %s", doctorFailSummary(checks))
	}
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
	if runtimeGOOS() != "darwin" {
		fmt.Fprintln(stdout, "warn imessage_macos")
		fmt.Fprintf(stdout, "iMessage ingest only runs on macOS — skipping chat.db checks on %s.\n", runtimeGOOS())
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
