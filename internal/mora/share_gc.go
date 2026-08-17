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
	"strings"
	"time"
)

type shareStorageLimit = sharingpkg.StorageLimit

func shareStorageLimitPath(cfg Config) string { return sharingpkg.StorageLimitPath(cfg.ConfigDir) }

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
	bytes, err := sharingpkg.ParseByteSize(args[0])
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
	if err := sharingpkg.WriteStorageLimit(cfg.ConfigDir, bytes, now); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "share storage limit set to %s (%d bytes). Doctor's recommended ceiling stays 15 GiB.\n", formatBytes(bytes), bytes)
	return nil
}

type shareStorageAdmission struct{ inner *sharingpkg.StorageAdmission }

func newShareStorageAdmission(cfg Config, name string) (*shareStorageAdmission, error) {
	a, err := sharingpkg.NewStorageAdmission(productStorageRoots(cfg), cfg.ConfigDir, name, storageCeilingBytes)
	if err != nil {
		return nil, err
	}
	return &shareStorageAdmission{inner: a}, nil
}
func (a *shareStorageAdmission) checkAdditional(next int64) error {
	return a.inner.CheckAdditional(next)
}
func (a *shareStorageAdmission) checkCurrent() error       { return a.inner.CheckCurrent() }
func (a *shareStorageAdmission) remaining() (int64, error) { return a.inner.Remaining() }

// admitShareGeneration refuses a build whose whole-product footprint would cross
// the configured limit. Mora owns entry conversion; sharing owns admission.
func admitShareGeneration(cfg Config, name string, entries []shareBlobEntry) error {
	var genBytes int64
	for _, e := range entries {
		n := int64(len(e.body))
		if genBytes > math.MaxInt64-n {
			return fmt.Errorf("share %q: generation size overflows int64", name)
		}
		genBytes += n
	}
	return sharingpkg.AdmitGenerationBytes(productStorageRoots(cfg), cfg.ConfigDir, name, storageCeilingBytes, genBytes, len(entries))
}
