package mora

// share_gc.go — Packet H3b: bounded generation GC with a preflight liveness
// sweep, plus ONE whole-product byte admission limit (default 15 GiB, the
// shipped doctor ceiling; unforgeable local opt-in via `mora share
// storage-limit`). The sweep is idempotent/reentrant so it is safe at pull
// preflight, at end-of-pull, and from the manual `mora share gc` command.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// shareGCSweep reclaims, in order: committed losers/superseded gens (keeping K),
// uncommitted crash orphans, stale bucket staging, orphaned git import refs, and
// stale commit records — never touching the published head or lowering the
// bucket floor. A deletion blocked by a Windows sharing violation / open handle
// is deferred to the next sweep, never forced or looped.
func shareGCSweep(cfg Config, name string, now time.Time) error {
	root := shareSubRoot(cfg, name)
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	published, ok, err := resolvePublishedCommit(cfg, name)
	if err != nil {
		return err
	}
	records, err := readAllCommits(cfg, name)
	if err != nil {
		return err
	}
	owner := liveImportOwner(cfg, name, now)

	// Retain set: the published seq plus the K highest superseded seqs.
	retainSeq := map[int]bool{}
	genBySeq := map[int]string{}
	var loserSeqs []int
	for _, r := range records {
		genBySeq[r.Seq] = r.Gen
		if ok && r.Seq < published.Seq {
			loserSeqs = append(loserSeqs, r.Seq)
		}
	}
	if ok {
		retainSeq[published.Seq] = true
	}
	sort.Sort(sort.Reverse(sort.IntSlice(loserSeqs)))
	for i, s := range loserSeqs {
		if i < shareGenRetain {
			retainSeq[s] = true
		}
	}
	retainGen := map[string]bool{}
	for s := range retainSeq {
		if g, has := genBySeq[s]; has {
			retainGen[g] = true
		}
	}

	// Floor-safety assertion: before deleting any record, prove the published
	// head carries a floor >= every floor we retain or delete (GC may never lower
	// the replay floor).
	willDeleteRecord := false
	maxFloor := 0
	for _, r := range records {
		if r.BucketFloor > maxFloor {
			maxFloor = r.BucketFloor
		}
		if ok && r.Seq < published.Seq && !retainSeq[r.Seq] {
			willDeleteRecord = true
		}
	}
	if willDeleteRecord && ok && published.BucketFloor < maxFloor {
		return fmt.Errorf("share %q: GC aborted — published head floor %d < max committed floor %d; refusing to lower the replay floor", name, published.BucketFloor, maxFloor)
	}

	genSeqOf := func(gen string) (int, bool) {
		for s, g := range genBySeq {
			if g == gen {
				return s, true
			}
		}
		return 0, false
	}

	// Sweep the generation directories.
	if gens, gerr := os.ReadDir(shareGensDir(cfg, name)); gerr == nil {
		for _, e := range gens {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), "gen-") {
				continue
			}
			gen := e.Name()
			if retainGen[gen] {
				continue
			}
			if ok && gen == published.Gen {
				continue // never touch the published gen
			}
			if seq, committed := genSeqOf(gen); committed {
				if ok && seq < published.Seq {
					if rerr := deferrableRemoveAll(shareGenDir(cfg, name, gen)); rerr != nil {
						return rerr
					}
				}
				continue
			}
			// Uncommitted crash orphan: reclaim only past TTL and only if its run-id
			// does not own the live lease.
			if genRunID(gen) == owner {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				return ierr
			}
			if now.Sub(info.ModTime()) < shareImportTTL {
				continue
			}
			if rerr := deferrableRemoveAll(shareGenDir(cfg, name, gen)); rerr != nil {
				return rerr
			}
		}
	} else if !errors.Is(gerr, os.ErrNotExist) {
		return gerr
	}

	// Stale bucket staging: fetch-* dirs older than TTL not owned by the live lease.
	if subs, serr := os.ReadDir(root); serr == nil {
		for _, e := range subs {
			if !e.IsDir() || !strings.HasPrefix(e.Name(), "fetch-") {
				continue
			}
			if strings.TrimPrefix(e.Name(), "fetch-") == owner {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				return ierr
			}
			if now.Sub(info.ModTime()) < shareImportTTL {
				continue
			}
			if rerr := deferrableRemoveAll(filepath.Join(root, e.Name())); rerr != nil {
				return rerr
			}
		}
	} else {
		return serr
	}

	// Orphaned git import refs (best-effort; touches only the named private ref).
	reapOrphanedGitPins(cfg, name, owner, retainGen)

	// Stale commit records: seq < published beyond the K retained.
	if ok {
		for _, r := range records {
			if r.Seq < published.Seq && !retainSeq[r.Seq] {
				if rerr := deferrableRemove(shareCommitPath(cfg, name, r.Seq)); rerr != nil {
					return rerr
				}
			}
		}
	}
	return nil
}

// reapOrphanedGitPins enumerates refs/mora/import/ and removes a private pin whose
// run-id owns neither the live lease nor a retained generation, via the exact
// read-only for-each-ref + CAS update-ref -d invocation. Best-effort.
var shareGCGitExecFn execFunc = realExec

func reapOrphanedGitPins(cfg Config, name, owner string, retainGen map[string]bool) {
	repo := shareRepoDir(cfg, name)
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		if _, err2 := os.Stat(filepath.Join(repo, "HEAD")); err2 != nil {
			return
		}
	}
	out, err := shareGCGitExecFn(context.Background(), repo, "git", "for-each-ref", "--format=%(refname) %(objectname)", "refs/mora/import/")
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		ref, sha := fields[0], fields[1]
		runID := strings.TrimPrefix(ref, "refs/mora/import/")
		if runID == owner || retainGen["gen-"+runID] {
			continue
		}
		_, _ = shareGCGitExecFn(context.Background(), repo, "git", "update-ref", "-d", ref, sha)
	}
}

// The seams make the Windows sharing-violation branch deterministic on every
// test host. Production always uses the operating-system implementations.
var (
	shareGCRemoveFn           = os.Remove
	shareGCRemoveAllFn        = os.RemoveAll
	shareGCRemovalRetryableFn = sharingViolationRetryable
)

// deferrableRemove/deferrableRemoveAll delete a path but defer only a Windows
// sharing violation / open-handle failure to the next sweep (mirroring
// removeLeaseFile's non-forcing policy). Other filesystem failures are loud:
// silently swallowing EACCES/EIO would claim reclamation that never happened.
func deferrableRemove(path string) error {
	err := shareGCRemoveFn(path)
	if err == nil || errors.Is(err, os.ErrNotExist) || shareGCRemovalRetryableFn(err) {
		return nil
	}
	return err
}

func deferrableRemoveAll(path string) error {
	err := shareGCRemoveAllFn(path)
	if err == nil || errors.Is(err, os.ErrNotExist) || shareGCRemovalRetryableFn(err) {
		return nil
	}
	return err
}

// ---- whole-product byte accountant ----
// productStorageBytes is the strict whole-product accountant: the union of all
// regular-file bytes under the four configured product roots (VaultDir,
// ConfigDir, DataDir, StateDir), canonical-root-overlap-deduped and hard-link
// deduped by file identity, with checked addition. It returns an error on any
// walk/stat/identity failure rather than treating unreadable bytes as zero.
func productStorageBytes(cfg Config) (int64, error) {
	rawRoots := []string{cfg.VaultDir, cfg.ConfigDir, cfg.DataDir, cfg.StateDir}
	// Canonicalize first, then sort shortest-first before dropping nested roots.
	// Doing this in config-field order is wrong when (for example) VaultDir is
	// nested under a later DataDir: both roots would be walked on Windows, where
	// path-level dedup must work even before file identity is consulted.
	var roots []string
	for _, r := range rawRoots {
		if r == "" {
			continue
		}
		cr := resolveRealDeep(r)
		roots = append(roots, cr)
	}
	sort.Slice(roots, func(i, j int) bool {
		if len(roots[i]) == len(roots[j]) {
			return roots[i] < roots[j]
		}
		return len(roots[i]) < len(roots[j])
	})
	canonical := roots[:0]
	for _, cr := range roots {
		nested := false
		for _, seen := range canonical {
			if storagePathWithin(cr, seen) {
				nested = true
				break
			}
		}
		if !nested {
			canonical = append(canonical, cr)
		}
	}
	roots = canonical
	var total int64
	seen := map[fileIDKey]bool{}
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // a not-yet-created root contributes 0, not an error
			}
			return 0, err
		}
		werr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				// SQLite read-only handles may retire a transient -shm/-wal entry
				// between its parent directory listing and this callback. A path
				// that no longer exists contributes zero bytes; permission, I/O,
				// and every other walk failure remain fail-closed below.
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			info, ierr := d.Info()
			if ierr != nil {
				if errors.Is(ierr, os.ErrNotExist) {
					return nil
				}
				return ierr
			}
			key, ierr := fileIdentity(path, info)
			if ierr != nil {
				if errors.Is(ierr, os.ErrNotExist) {
					return nil
				}
				return ierr
			}
			if seen[key] {
				return nil // hard link already counted
			}
			seen[key] = true
			sz := info.Size()
			if sz < 0 || total > math.MaxInt64-sz {
				return fmt.Errorf("storage accounting overflow at %s", path)
			}
			total += sz
			return nil
		})
		if werr != nil {
			return 0, werr
		}
	}
	return total, nil
}

func storagePathWithin(path, root string) bool {
	if runtimeGOOS() == "darwin" || runtimeGOOS() == "windows" {
		path, root = strings.ToLower(path), strings.ToLower(root)
	}
	sep := string(os.PathSeparator)
	return path == root || strings.HasPrefix(path+sep, root+sep)
}

// ---- CLI: mora share gc / mora share storage-limit ----

// cmdShareGC runs the same idempotent sweep out of band (no pull, no successful
// publication prerequisite). It takes storage.lock, reclaims after readers close,
// and reports before/after whole-product bytes.
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
	var subs []shareSubscription
	if name != "" {
		for _, s := range sf.Subscriptions {
			if s.Name == name {
				subs = append(subs, s)
			}
		}
		if len(subs) == 0 {
			return fmt.Errorf("no subscription named %q — see `mora share list`", name)
		}
	} else {
		subs = sf.Subscriptions
	}
	rel, err := acquireStorageLease(cfg, newRunID(now), now)
	if err != nil {
		return err
	}
	defer rel()
	before, berr := productStorageBytes(cfg)
	if berr != nil {
		return fmt.Errorf("share gc: storage accounting failed: %w", berr)
	}
	for _, s := range subs {
		if serr := shareGCSweep(cfg, s.Name, now); serr != nil {
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
	if werr := atomicWriteDurable(shareStorageLimitPath(cfg), append(body, '\n'), 0o600); werr != nil {
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
	return fmt.Errorf("share %q needs at least %d whole-product bytes; configured limit is %d (doctor ceiling 15 GiB). Run 'mora share storage-limit %d' to opt in, free space/run 'mora share gc', or unsubscribe.", a.name, needed, a.limit, needed)
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
	return admitShareGenerationBytes(cfg, name, genBytes)
}

// admitShareGenerationBytes is the metadata-only form used for the protocol's
// legal 50,000 x 4 MiB upper-bound decision without allocating a 195 GiB slice.
// Production's entry-based path delegates here after checked summation.
func admitShareGenerationBytes(cfg Config, name string, corpusBytes int64) error {
	if corpusBytes < 0 || corpusBytes > math.MaxInt64/2 {
		return fmt.Errorf("share %q: generation reservation overflows int64", name)
	}
	a, err := newShareStorageAdmission(cfg, name)
	if err != nil {
		return err
	}
	return a.checkAdditional(corpusBytes * 2) // frozen corpus + worst-case index reservation
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
