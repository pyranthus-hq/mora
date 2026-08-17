package sharing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/gitsync"
)

// Sweep reclaims, in order: committed losers/superseded gens (keeping K),
// uncommitted crash orphans, stale bucket staging, orphaned git import refs, and
// stale commit records — never touching the published head or lowering the
// bucket floor. A deletion blocked by a Windows sharing violation / open handle
// is deferred to the next sweep, never forced or looped.
func Sweep(store GenerationStore, name, owner string, now time.Time, ops GCOptions) error {
	ops = ops.defaults()
	root := store.Root(name)
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	published, ok, err := store.Resolve(name)
	if err != nil {
		return err
	}
	records, err := store.ReadAll(name)
	if err != nil {
		return err
	}

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
		if i < GenRetain {
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
	if gens, gerr := os.ReadDir(store.GensDir(name)); gerr == nil {
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
					if rerr := deferrableRemoveAll(store.GenDir(name, gen), ops); rerr != nil {
						return rerr
					}
				}
				continue
			}
			// Uncommitted crash orphan: reclaim only past TTL and only if its run-id
			// does not own the live lease.
			if RunID(gen) == owner {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				return ierr
			}
			if now.Sub(info.ModTime()) < ops.TTL {
				continue
			}
			if rerr := deferrableRemoveAll(store.GenDir(name, gen), ops); rerr != nil {
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
			if now.Sub(info.ModTime()) < ops.TTL {
				continue
			}
			if rerr := deferrableRemoveAll(filepath.Join(root, e.Name()), ops); rerr != nil {
				return rerr
			}
		}
	} else {
		return serr
	}

	// Orphaned git import refs (best-effort; touches only the named private ref).
	reapOrphanedGitPins(store, name, owner, retainGen, ops)

	// Stale commit records: seq < published beyond the K retained.
	if ok {
		for _, r := range records {
			if r.Seq < published.Seq && !retainSeq[r.Seq] {
				if rerr := deferrableRemove(store.CommitPath(name, r.Seq), ops); rerr != nil {
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
func reapOrphanedGitPins(store GenerationStore, name, owner string, retainGen map[string]bool, ops GCOptions) {
	repo := RepoDir(store.DataDir, name)
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		if _, err2 := os.Stat(filepath.Join(repo, "HEAD")); err2 != nil {
			return
		}
	}
	out, err := ops.GitExec(context.Background(), repo, "git", "for-each-ref", "--format=%(refname) %(objectname)", "refs/mora/import/")
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
		_, _ = ops.GitExec(context.Background(), repo, "git", "update-ref", "-d", ref, sha)
	}
}

// GCOptions supplies caller-owned lease timing and injectable operating-system operations.
type GCOptions struct {
	GitExec          gitsync.Runner
	Remove           func(string) error
	RemoveAll        func(string) error
	RemovalRetryable func(error) bool
	TTL              time.Duration
}

func (o GCOptions) defaults() GCOptions {
	if o.GitExec == nil {
		o.GitExec = gitsync.RealExec
	}
	if o.Remove == nil {
		o.Remove = os.Remove
	}
	if o.RemoveAll == nil {
		o.RemoveAll = os.RemoveAll
	}
	if o.RemovalRetryable == nil {
		o.RemovalRetryable = atomicio.SharingViolationRetryable
	}
	if o.TTL <= 0 {
		o.TTL = 10 * time.Minute
	}
	return o
}

// deferrableRemove/deferrableRemoveAll delete a path but defer only a Windows
// sharing violation / open-handle failure to the next sweep (mirroring
// removeLeaseFile's non-forcing policy). Other filesystem failures are loud:
// silently swallowing EACCES/EIO would claim reclamation that never happened.
func deferrableRemove(path string, ops GCOptions) error {
	err := ops.Remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) || ops.RemovalRetryable(err) {
		return nil
	}
	return err
}

func deferrableRemoveAll(path string, ops GCOptions) error {
	err := ops.RemoveAll(path)
	if err == nil || errors.Is(err, os.ErrNotExist) || ops.RemovalRetryable(err) {
		return nil
	}
	return err
}

// DeferrableRemove applies the one-attempt sharing-violation deferral policy.

// DeferrableRemoveAll applies the one-attempt sharing-violation deferral policy recursively.
func DeferrableRemoveAll(path string, ops GCOptions) error {
	return deferrableRemoveAll(path, ops.defaults())
}
