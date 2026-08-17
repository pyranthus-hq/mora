package mora

// share_gc.go — Packet H3b: bounded generation GC with a preflight liveness
// sweep, plus ONE whole-product byte admission limit (default 15 GiB, the
// shipped doctor ceiling; unforgeable local opt-in via `mora share
// storage-limit`). The sweep is idempotent/reentrant so it is safe at pull
// preflight, at end-of-pull, and from the manual `mora share gc` command.

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	sharingpkg "github.com/pyranthus-hq/mora/internal/sharing"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// shareStorageLimit is the durable admission opt-in at
// ConfigDir/share/storage-limit.json. Absent ⇒ the default 15 GiB ceiling.
type shareStorageLimit struct {
	Bytes     int64  `json:"bytes"`
	UpdatedAt string `json:"updated_at"`
}

func shareStorageLimitPath(cfg Config) string {
	return filepath.Join(cfg.ConfigDir, "share", "storage-limit.json")
}

// shareStorageLimitBytes returns the configured whole-product limit, defaulting
// to the shipped doctor hard ceiling (15 GiB) when no opt-in file exists.
func shareStorageLimitBytes(cfg Config) (int64, error) {
	b, err := os.ReadFile(shareStorageLimitPath(cfg))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return storageCeilingBytes, nil
		}
		return 0, err
	}
	var lim shareStorageLimit
	if err := json.Unmarshal(b, &lim); err != nil {
		return 0, fmt.Errorf("%s is corrupt: %w", shareStorageLimitPath(cfg), err)
	}
	if lim.Bytes <= 0 {
		return 0, fmt.Errorf("%s is corrupt: bytes must be positive", shareStorageLimitPath(cfg))
	}
	return lim.Bytes, nil
}

// liveImportOwner returns the run-id that currently, validly holds the import
// lease for name (within TTL), or "" if none.
func liveImportOwner(cfg Config, name string, now time.Time) string {
	data, err := os.ReadFile(shareImportLockPath(cfg, name))
	if err != nil {
		return ""
	}
	var body loopLockBody
	if json.Unmarshal(data, &body) != nil || body.RunID == "" {
		return ""
	}
	if t, perr := time.Parse(time.RFC3339, body.AcquiredAt); perr != nil || now.UTC().Sub(t.UTC()) >= shareImportTTL {
		return ""
	}
	return body.RunID
}

var (
	shareGCGitExecFn          = execFunc(realExec)
	shareGCRemoveFn           = os.Remove
	shareGCRemoveAllFn        = os.RemoveAll
	shareGCRemovalRetryableFn = atomicio.SharingViolationRetryable
)

func shareGCGCOptions() sharingpkg.GCOptions {
	return sharingpkg.GCOptions{GitExec: shareGCGitExecFn, Remove: shareGCRemoveFn, RemoveAll: shareGCRemoveAllFn, RemovalRetryable: shareGCRemovalRetryableFn, TTL: shareImportTTL}
}
func shareGCSweep(cfg Config, name string, now time.Time) error {
	return sharingpkg.Sweep(sharingpkg.GenerationStore{DataDir: cfg.DataDir}, name, liveImportOwner(cfg, name, now), now, shareGCGCOptions())
}
func deferrableRemoveAll(path string) error {
	return sharingpkg.DeferrableRemoveAll(path, shareGCGCOptions())
}

// ---- CLI: mora share gc / mora share storage-limit ----

// cmdShareGC runs the same idempotent sweep out of band (no pull, no successful
// publication prerequisite). It takes storage.lock, reclaims after readers close,
// and reports before/after whole-product bytes.
var testHookShareGCAfterStorageLease func()

func cmdShareGC(cfg Config, args []string, stdout io.Writer, now time.Time) error {
	name := ""
	if len(args) > 0 {
		if strings.HasPrefix(args[0], "-") || len(args) > 1 {
			return errors.New("usage: mora share gc [<name>]")
		}
		name = args[0]
	}
	sf, err := loadShares(cfg)
	if err != nil {
		return err
	}
	registered := make(map[string]bool, len(sf.Subscriptions))
	for _, s := range sf.Subscriptions {
		registered[s.Name] = true
	}
	targets := map[string]bool{} // name -> unregistered local root
	subsRoot := filepath.Join(cfg.DataDir, "share", "subs")
	addLocalRoots := func() error {
		entries, rerr := os.ReadDir(subsRoot)
		if errors.Is(rerr, os.ErrNotExist) {
			return nil
		}
		if rerr != nil {
			return rerr
		}
		for _, entry := range entries {
			if entry.IsDir() && validShareName(entry.Name()) {
				targets[entry.Name()] = !registered[entry.Name()]
			}
		}
		return nil
	}
	if name != "" {
		if !validShareName(name) {
			return fmt.Errorf("invalid subscription name %q", name)
		}
		if registered[name] {
			targets[name] = false
		} else {
			info, serr := os.Lstat(shareSubRoot(cfg, name))
			if serr != nil || !info.IsDir() {
				return fmt.Errorf("no subscription or unregistered local share state named %q", name)
			}
			targets[name] = true
		}
	} else {
		for registeredName := range registered {
			targets[registeredName] = false
		}
		if err := addLocalRoots(); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(targets))
	for target := range targets {
		names = append(names, target)
	}
	sort.Strings(names)

	runID := newRunID(now)
	rel, err := acquireStorageLease(cfg, runID, now)
	if err != nil {
		return err
	}
	defer rel()
	stopHeartbeat := startStorageHeartbeat(cfg, runID)
	defer stopHeartbeat()
	if testHookShareGCAfterStorageLease != nil {
		testHookShareGCAfterStorageLease()
	}
	before, berr := productStorageBytes(cfg)
	if berr != nil {
		return fmt.Errorf("share gc: storage accounting failed: %w", berr)
	}
	for _, target := range names {
		if !targets[target] {
			if serr := shareGCSweep(cfg, target, now); serr != nil {
				return serr
			}
			continue
		}
		// An unregistered first-subscribe crash has no shares.json row, but its
		// repo/fetch/generation can be the largest product artifact. Take the
		// per-name lease under storage.lock, re-check the registry, then remove the
		// entire unreachable root. The import.lock itself is removed by release;
		// storage.lock prevents a new subscriber entering before final RemoveAll.
		importRel, ierr := acquireImportLease(cfg, target, runID, time.Now())
		if ierr != nil {
			return ierr
		}
		stopImport := startImportHeartbeat(cfg, target, runID)
		latest, lerr := loadShares(cfg)
		if lerr == nil {
			lerr = validateSubscriptionNameAvailable(latest, target)
		}
		if lerr == nil {
			entries, derr := os.ReadDir(shareSubRoot(cfg, target))
			if derr != nil && !errors.Is(derr, os.ErrNotExist) {
				lerr = derr
			}
			for _, entry := range entries {
				if entry.Name() == filepath.Base(shareImportLockPath(cfg, target)) {
					continue
				}
				if derr := deferrableRemoveAll(filepath.Join(shareSubRoot(cfg, target), entry.Name())); derr != nil {
					lerr = derr
					break
				}
			}
		}
		stopImport()
		importRel()
		if lerr != nil {
			return fmt.Errorf("share gc: refusing unregistered root %q: %w", target, lerr)
		}
		if serr := deferrableRemoveAll(shareSubRoot(cfg, target)); serr != nil {
			return serr
		}
	}
	after, aerr := productStorageBytes(cfg)
	if aerr != nil {
		return fmt.Errorf("share gc: storage accounting failed: %w", aerr)
	}
	fmt.Fprintf(stdout, "share gc: whole-product footprint %s → %s (reclaimed %s).\n",
		formatBytes(before), formatBytes(after), formatBytes(before-after))
	return nil
}

// cmdShareStorageLimit writes the durable whole-product admission opt-in. It
// takes storage.lock before replacing the config so a concurrent build reads one
// stable value.
func cmdShareStorageLimit(cfg Config, args []string, stdout io.Writer, now time.Time) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: mora share storage-limit <bytes|15GiB>")
	}
	bytes, err := parseByteSize(args[0])
	if err != nil {
		return err
	}
	if bytes <= 0 {
		return errors.New("storage limit must be positive")
	}
	rel, err := acquireStorageLease(cfg, newRunID(now), now)
	if err != nil {
		return err
	}
	defer rel()
	lim := shareStorageLimit{Bytes: bytes, UpdatedAt: now.UTC().Format(time.RFC3339)}
	body, merr := json.MarshalIndent(lim, "", "  ")
	if merr != nil {
		return merr
	}
	if werr := atomicio.WriteDurable(shareStorageLimitPath(cfg), append(body, '\n'), 0o600); werr != nil {
		return werr
	}
	fmt.Fprintf(stdout, "share storage limit set to %s (%d bytes). Doctor's recommended ceiling stays 15 GiB.\n", formatBytes(bytes), bytes)
	return nil
}

// shareStorageAdmission holds the one stable limit read while storage.lock is
// held. Every check re-accounts the actual product footprint, so prior corpus
// writes, transport staging, SQLite sidecars, and every other subscription are
// included rather than tracked by a lossy local counter.
type shareStorageAdmission struct {
	cfg   Config
	name  string
	limit int64
}

func newShareStorageAdmission(cfg Config, name string) (*shareStorageAdmission, error) {
	limit, err := shareStorageLimitBytes(cfg)
	if err != nil {
		return nil, err
	}
	return &shareStorageAdmission{cfg: cfg, name: name, limit: limit}, nil
}

func (a *shareStorageAdmission) used() (int64, error) {
	used, uerr := productStorageBytes(a.cfg)
	if uerr != nil {
		return 0, fmt.Errorf("share %q: storage accounting failed (fail-closed): %w", a.name, uerr)
	}
	return used, nil
}

func (a *shareStorageAdmission) checkAdditional(next int64) error {
	if next < 0 {
		return fmt.Errorf("share %q: invalid negative storage reservation %d", a.name, next)
	}
	used, err := a.used()
	if err != nil {
		return err
	}
	if used > math.MaxInt64-next {
		return fmt.Errorf("share %q: storage reservation overflows int64", a.name)
	}
	return a.checkNeeded(used + next)
}

func (a *shareStorageAdmission) checkCurrent() error {
	return a.checkAdditional(0)
}

func (a *shareStorageAdmission) remaining() (int64, error) {
	used, err := a.used()
	if err != nil {
		return 0, err
	}
	if err := a.checkNeeded(used); err != nil {
		return 0, err
	}
	return a.limit - used, nil
}

func (a *shareStorageAdmission) checkNeeded(needed int64) error {
	if needed <= a.limit {
		return nil
	}
	// The opt-in file itself is part of whole-product accounting. Reserve a page
	// of headroom so replacing a short limit (for example 16) with the printed
	// multi-digit value cannot make the immediate retry miss by a few bytes.
	const decisionFileHeadroom = int64(4 << 10)
	required := needed
	if required <= math.MaxInt64-decisionFileHeadroom {
		required += decisionFileHeadroom
	}
	return fmt.Errorf("share %q needs at least %d whole-product bytes; configured limit is %d (doctor ceiling 15 GiB). Run 'mora share storage-limit %d' to opt in, free space/run 'mora share gc', or unsubscribe.", a.name, required, a.limit, required)
}

// admitShareGeneration refuses a build whose whole-product footprint would cross
// the configured limit. The refusal is an explicit oversubscription DECISION
// (never a dead end): it names the required bytes and the storage-limit command.
func admitShareGeneration(cfg Config, name string, entries []shareBlobEntry) error {
	var genBytes int64
	for _, e := range entries {
		n := int64(len(e.body))
		if genBytes > math.MaxInt64-n {
			return fmt.Errorf("share %q: generation size overflows int64", name)
		}
		genBytes += n
	}
	return admitShareGenerationBytes(cfg, name, genBytes, len(entries))
}

// admitShareGenerationBytes is the metadata-only form used for the protocol's
// legal 50,000 x 4 MiB upper-bound decision without allocating a 195 GiB slice.
// Production's entry-based path delegates here after checked summation.
func admitShareGenerationBytes(cfg Config, name string, corpusBytes int64, entries int) error {
	// The index stores each body in the ordinary table and again in FTS5's
	// content/index structures. A corpus-sized guess is not a sufficient retry
	// decision. Reserve a deliberately conservative bounded expansion plus fixed
	// SQLite/page and per-row overhead so the printed storage-limit value admits
	// the same input instead of leading to a second SQLITE_FULL refusal.
	const (
		generationExpansion = int64(8)        // corpus + content table + FTS/index headroom
		generationBaseBytes = int64(64 << 10) // schema and minimum SQLite pages
		generationRowBytes  = int64(4 << 10)  // row/page/term metadata headroom
	)
	if corpusBytes < 0 || entries < 0 || corpusBytes > (math.MaxInt64-generationBaseBytes)/generationExpansion {
		return fmt.Errorf("share %q: generation reservation overflows int64", name)
	}
	entryCount := int64(entries)
	reserve := corpusBytes*generationExpansion + generationBaseBytes
	if entryCount > (math.MaxInt64-reserve)/generationRowBytes {
		return fmt.Errorf("share %q: generation reservation overflows int64", name)
	}
	reserve += entryCount * generationRowBytes
	a, err := newShareStorageAdmission(cfg, name)
	if err != nil {
		return err
	}
	return a.checkAdditional(reserve)
}

// parseByteSize accepts a plain byte count or a binary-unit suffix (KiB/MiB/GiB/
// TiB, case-insensitive; a bare number is bytes).
func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	up := strings.ToUpper(s)
	mult := int64(1)
	// Longest suffix first, in a DETERMINISTIC order, so "15GIB" is not matched by
	// the bare "B" unit.
	for _, u := range []struct {
		suf string
		m   int64
	}{{"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(up, u.suf) {
			mult = u.m
			up = strings.TrimSpace(strings.TrimSuffix(up, u.suf))
			break
		}
	}
	n, err := strconv.ParseInt(up, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q — use a byte count or a binary unit like 15GiB", s)
	}
	if n < 0 || (mult != 0 && n > math.MaxInt64/mult) {
		return 0, fmt.Errorf("invalid size %q — value is out of range", s)
	}
	return n * mult, nil
}
