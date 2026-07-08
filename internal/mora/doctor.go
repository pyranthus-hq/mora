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
	jsonOut := fs.Bool("json", false, "emit a machine-readable JSON health report")
	strict := fs.Bool("strict", false, "exit non-zero if a critical health check fails")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
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
		{Name: "sources_config", OK: len(sources) > 0, Critical: false},
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
	healthy := true
	for _, c := range checks {
		if c.Critical && !c.OK {
			healthy = false
		}
	}

	// Storage footprint vs target/ceiling — visibility only; Mora never caps.
	used := vaultStorageBytes(cfg)
	st := storageStatus(used)
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
	printGoogleAuthRecency(cfg, stdout, time.Now())
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
