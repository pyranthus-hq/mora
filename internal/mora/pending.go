package mora

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// pending.go — the index-state ledger (Gate 2, Packet A / HEALTH-09). The
// invariant is "mark before visible, clear only on commit": every vault mutation
// writes a CRASH-DURABLE pending-op file BEFORE the vault byte becomes visible, and
// only a successfully-committed rebuild/upsert transaction may retire it. A pending
// op (or a non-empty ingest journal) makes the index read DIRTY on every surface —
// so a memory that landed in the vault but not the index can never masquerade as
// indexed. Ops are FILES under StateDir/pending/, not index rows, because an
// in-database marker bricked existing v2 installs and deadlocked against the
// rebuild's own writer lock (see the head of Packet A in the execution packet).

// indexClock is the injectable clock the ledger and the rebuild's index_meta
// stamps resolve against — a var (mirrors doctorClock/briefClock) so tests pin a
// deterministic "now" instead of racing the real clock. Production never reassigns
// it. Used for a pending op's marked_at, the rebuild's listing_started_at snapshot,
// and the index_meta indexed_at/*_indexed_at/last_attempt stamps, so all three read
// from one clock and their orderings are consistent.
var indexClock = time.Now

// errIndexUnmarkable is returned by markIndexDirty when the pending marker cannot
// be made crash-durable — the state dir is unwritable, or an f.Sync/syncDir barrier
// faulted (a real I/O fault, never mere lock contention). The mutation MUST abort
// before a single vault byte changes: a client retry then cannot mint a duplicate
// memory, preserving mcp.go's isError-asymmetry rule. A swallowed marker fault is
// exactly the silent false-clean this gate exists to kill.
var errIndexUnmarkable = errors.New("index cannot be marked dirty")

// Pending op kinds. Ingest is journaled, not a pendingOp (A3 rule d); the
// subscribed-share import uses no served-state marker (Packet H), so the enum stays
// exactly these three for the personal vault.
const (
	opKindWrite   = "write"
	opKindDelete  = "delete"
	opKindRebuild = "rebuild"
)

// pendingOp is one in-flight vault mutation, serialized to
// StateDir/pending/<op_id>.json.
type pendingOp struct {
	OpID     string `json:"op_id"`
	Kind     string `json:"kind"`                // write | delete | rebuild
	Path     string `json:"path,omitempty"`      // filepath.Clean'd absolute vault path ("" for rebuild)
	MemoryID string `json:"memory_id,omitempty"` // delete -> the read-path suppression list (B4)
	MarkedAt string `json:"marked_at"`           // RFC3339
}

var removePendingOpFile = os.Remove

func pendingDir(cfg Config) string { return filepath.Join(cfg.StateDir, "pending") }

func pendingOpPath(cfg Config, opID string) string {
	return filepath.Join(pendingDir(cfg), opID+".json")
}

// cleanVaultPath reduces a path to filepath.Clean + absolute so an op's minted
// path (memoryPath) and the rebuild's listing (allMemoryFiles) compare equal on
// every platform — without it, Windows never clears an op and the banner becomes
// permanent wallpaper (A3 "Path normalization is load-bearing").
func cleanVaultPath(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}

// markIndexDirty writes a crash-durable pending-op file for one in-flight
// mutation, returning the op with its OpID/MarkedAt filled so the caller can
// retire it after the covering commit. It is a NO-OP whenever
// indexReadyForUpsert(cfg) == false (missing db, stale schema, missing tables,
// unbound vault id): every such state already reads never/failed — strictly worse
// than dirty — so skipping the mark is fail-closed and reuses the readiness rule
// the write path already consults (A2). A durability fault returns
// errIndexUnmarkable so the mutation aborts before publishing anything.
func markIndexDirty(ctx context.Context, cfg Config, op pendingOp) (pendingOp, error) {
	if op.OpID == "" {
		op.OpID = newID()
	}
	if op.MarkedAt == "" {
		op.MarkedAt = indexClock().UTC().Format(time.RFC3339)
	}
	op.Path = cleanVaultPath(op.Path)

	ready, _, rerr := indexReadyForUpsert(ctx, cfg)
	if rerr != nil {
		// The readiness probe itself faulted (unreadable/locked index). That is a
		// state indexHealthOf already reports as failed (worse than dirty); do not
		// fail the mutation over it — treat as not-ready and skip the mark.
		return op, nil
	}
	if !ready {
		return op, nil
	}
	body, err := json.Marshal(op)
	if err != nil {
		return op, err
	}
	if err := atomicWriteDurable(pendingOpPath(cfg, op.OpID), body, 0o644); err != nil {
		return op, fmt.Errorf("%w: %v", errIndexUnmarkable, err)
	}
	if testHookPostMarkerWrite != nil {
		testHookPostMarkerWrite()
	}
	return op, nil
}

// unmarkIndexDirty retires one pending op. Best-effort: a lost removal only
// creates a false-DIRTY, which the next committed rebuild clears (A3) — the safe
// direction, and the reason files (not index rows) are acceptable here.
func unmarkIndexDirty(cfg Config, opID string) error {
	if opID == "" {
		return nil
	}
	path := pendingOpPath(cfg, opID)
	var lastErr error
	deadline := time.Now().Add(leaseRemovalTimeout)
	for attempt := 0; ; attempt++ {
		err := removePendingOpFile(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if !leaseRemovalRetryableFn(err) {
			return err
		}
		lastErr = err
		if !sleepWithinDeadline(sourcesAcquireBackoff(attempt), deadline) {
			return lastErr
		}
	}
}

// listPendingOps reads every pending-op file. A missing dir is the common,
// benign case (nothing in flight). A corrupt op file fails CLOSED: it is reported
// with Kind "" so it still makes the index dirty (and a committed rebuild reaps it
// so the banner cannot become permanent).
func listPendingOps(cfg Config) ([]pendingOp, error) {
	entries, err := os.ReadDir(pendingDir(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ops []pendingOp
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		b, rerr := os.ReadFile(filepath.Join(pendingDir(cfg), e.Name()))
		if errors.Is(rerr, os.ErrNotExist) {
			continue // a concurrent committed writer retired it after ReadDir
		}
		if rerr != nil {
			return nil, rerr
		}
		var op pendingOp
		if json.Unmarshal(b, &op) != nil || op.OpID == "" {
			op = pendingOp{OpID: id} // corrupt: fail closed, no valid kind/path
		}
		ops = append(ops, op)
	}
	return ops, nil
}

// pendingDeleteIDs is the read-path suppression list (B4): the memory ids of
// every in-flight delete op. While a delete is pending — its rebuild failed or was
// killed — the deleted content must not be served, so search and the graph read
// path filter these ids out. Returns a set for O(1) membership.
func pendingDeleteIDs(cfg Config) map[string]bool {
	ops, err := listPendingOps(cfg)
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, op := range ops {
		if op.Kind == opKindDelete && op.MemoryID != "" {
			out[op.MemoryID] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// clearCoveredPendingOps retires the pending ops a just-committed rebuild
// demonstrably covered (A3). Call it ONLY after tx.Commit returns nil.
// listingStartedAt is captured immediately before the rebuild listed the vault;
// files is the raw listing; parsed is the subset parseMemory succeeded on. Every
// path is compared after cleanVaultPath. Removal errors are returned so a
// post-commit rebuild reports partial failure rather than false completion.
func clearCoveredPendingOps(cfg Config, listingStartedAt time.Time, files, parsed []string) error {
	filesSet := cleanPathSet(files)
	parsedSet := cleanPathSet(parsed)
	ops, err := listPendingOps(cfg)
	if err != nil {
		return err
	}
	var clearErr error
	for _, op := range ops {
		if shouldClearOp(op, listingStartedAt, filesSet, parsedSet) {
			if err := unmarkIndexDirty(cfg, op.OpID); err != nil {
				clearErr = errors.Join(clearErr, fmt.Errorf("retiring pending operation: %w", err))
			}
		}
	}
	return clearErr
}

// shouldClearOp encodes A3's clearing table. Every clause exists because a
// specific state is otherwise unreachable — see the packet.
func shouldClearOp(op pendingOp, listingStartedAt time.Time, files, parsed map[string]bool) bool {
	switch op.Kind {
	case opKindRebuild:
		// (a) any rebuild that LISTED after this op was marked covers it — NOT
		// only its own op_id, or a SIGKILLed rebuild's op is unclearable forever.
		t, perr := time.Parse(time.RFC3339, op.MarkedAt)
		if perr != nil {
			return true // an unparseable rebuild stamp is garbage; reap it
		}
		return !t.After(listingStartedAt)
	case opKindWrite:
		// (b) parsed, NOT listed: a truncated/hand-mangled file is listed yet not
		// indexed, and must stay dirty.
		return op.Path != "" && parsed[op.Path]
	case opKindDelete:
		// (c) gone OR re-ingested (a connector rewrote the memory onto its own
		// stable path, so it legitimately reappears in parsed).
		return op.Path != "" && (!files[op.Path] || parsed[op.Path])
	default:
		// Corrupt/unknown op: fail-closed while present, but a committed rebuild
		// is allowed to reap it so a permanent red banner never becomes wallpaper.
		return true
	}
}

// suppressPendingDeletes drops any memory whose id has an in-flight delete op —
// the B4 read-path guarantee, applied at the search JOIN and the graph
// loadMemoriesByID chokepoint. Fast-path returns the input untouched when nothing
// is pending (the common case).
func suppressPendingDeletes(cfg Config, mems []Memory) []Memory {
	sup := pendingDeleteIDs(cfg)
	if sup == nil {
		return mems
	}
	out := make([]Memory, 0, len(mems))
	for _, m := range mems {
		if sup[m.ID] {
			continue
		}
		out = append(out, m)
	}
	return out
}

func cleanPathSet(paths []string) map[string]bool {
	s := make(map[string]bool, len(paths))
	for _, p := range paths {
		s[cleanVaultPath(p)] = true
	}
	return s
}
