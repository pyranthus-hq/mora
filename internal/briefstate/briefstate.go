package briefstate

import (
	"encoding/json"
	"errors"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/leasefile"
	"github.com/pyranthus-hq/mora/internal/memory"
	"github.com/pyranthus-hq/mora/internal/registry"
	"os"
	"path/filepath"
	"time"
)

// brief.go is the ISOLATED, unit-testable CORE of the Phase-12 delta (M-5 store
// half): a per-instance watermark store (load/save/lock) plus a PURE hash-diff
// classifier. It is deliberately split from the buildDigest render/integration
// wiring (Plan 04) so the heavy correctness logic — cold start, hash-version
// reset, corruption recovery, byte-stability, the commit lock — gets first-class
// TDD attention. The store is the only I/O boundary; classify touches no files.
//
// THE LOAD-BEARING FINDING (reframes the phase): the delta is the CONTENT-HASH
// set, NOT timestamps. writeMappedMemory preserves existing.CreatedAt on a
// content change (mora.go), so a grown iMessage conversation or edited Gmail
// thread keeps its ORIGINAL created_at and only its content_hash moves; calendar
// items are future-dated with no upper bound. A created_at-based delta provably
// misses the exact case the phase exists to catch. So classify diffs
// content_hash against the stored snapshot — created_at never drives the delta.

// HashSchemaVersion stamps the snapshot's hashing scheme. A bump is handled
// as COLD-START-EQUIVALENT for that instance (re-baseline to all current hashes,
// suppress the flood) so an empty post-upgrade brief is not misread as broken.
// Increment this whenever the upstream ContentHash algorithm changes — or when
// the INSTANCE-KEYING scheme changes, which re-buckets memories under keys
// whose existing snapshots no longer describe them.
//
// v2: the applecal→applecalendar keying fix. Broken-era installs committed
// stamped-EMPTY snapshots under "applecalendar" (the memories were keyed
// "applecal" and never reconciled); post-fix those snapshots would read as
// steady state and flood the whole backlog as [new]
// (TestBrokenKeyingEraSnapshotResetsNotFloods).
const HashSchemaVersion = 2

// Snapshot is the per-instance watermark record persisted at
// <StateDir>/brief/<sourceInstanceKey>.json. It is NEW state — deliberately NOT
// an extension of memory.SyncStatus (whose LastSynced advances on every sync
// regardless of change) — and is kept OUT of sync/ so sourceFreshness never
// reads it. The JSON tags are the settled record shape:
//
//	{ "key": "gmail", "last_brief_at": "<UTC RFC3339>",
//	  "hash_schema_version": 1, "items": { "<stableID>": "<contentHash>", ... } }
//
// Items maps stableID -> contentHash. It stores sensitive stableIDs
// (gmail_thread/<id>, imessage_chat/<guid>), so the file is written 0600.
type Snapshot struct {
	Key               string            `json:"key"`
	LastBriefAt       string            `json:"last_brief_at"`
	HashSchemaVersion int               `json:"hash_schema_version"`
	Items             map[string]string `json:"items"`
}

// DeltaItem is one surfaced delta entry. Change is "new" or "updated".
// Unchanged items are never surfaced (so this type only ever carries deltas).
type DeltaItem struct {
	ID     string
	Change string // "new" | "updated"
}

// Delta is the typed result of classify — the seam Phases 13–16 consume
// (order-before-truncate) without re-entering buildDigest.
//
//   - Items:       the surfaced new/updated deltas (unchanged are omitted).
//   - ColdStart:   no usable snapshot for this instance (true delta begins next
//     run); the caller applies the 7-day courtesy display window and
//     records the full Baseline.
//   - SchemaReset: ColdStart was caused by a hash_schema_version mismatch — the
//     caller emits a "baseline reset after upgrade" sentinel so an
//     empty post-upgrade brief is not misread as broken.
//   - Baseline:    ALL current present hashes for the instance (every kept
//     stableID -> contentHash). The caller persists THIS on commit —
//     not just the surfaced items — so unchanged ids stay in the
//     watermark and archived backfill becomes the starting line.
type Delta struct {
	Items       []DeltaItem
	ColdStart   bool
	SchemaReset bool
	Baseline    map[string]string
}

// briefPath returns <StateDir>/brief/<key>.json. Kept OUT of sync/ on purpose so
// sourceFreshness (which scans sync/) never reads the watermark.
func Path(cfg config.Config, key string) string {
	return filepath.Join(cfg.StateDir, "brief", key+".json")
}

// loadBriefSnapshot reads the watermark for one instance. It mirrors the
// memory.LoadStatus convention (zero value on os.ErrNotExist) and EXTENDS it for
// the watermark's safety requirements: any read error, unmarshal/corruption
// error, OR a hash_schema_version mismatch returns a zero (cold-start-equivalent)
// snapshot for THAT instance — a per-instance recover that NEVER propagates a
// fatal error that would blank the whole brief (T-12-05). The caller then sees
// ColdStart through classify and re-baselines.
func Load(cfg config.Config, key string) Snapshot {
	b, err := os.ReadFile(Path(cfg, key))
	if err != nil {
		// Missing (os.ErrNotExist) OR any other read error => cold-start-equivalent.
		_ = errors.Is(err, os.ErrNotExist) // documented: not distinguished; both recover.
		return Snapshot{}
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		// Corrupt/truncated/garbage => cold-start-equivalent for this instance.
		return Snapshot{}
	}
	if snap.HashSchemaVersion != HashSchemaVersion {
		// Schema bump => re-baseline. DROP the stale items (their hashes were
		// computed under the old scheme and can't be diffed) so classify treats this
		// instance as cold-start-equivalent. We PRESERVE the key and the on-disk
		// (mismatched) version so classify can distinguish a post-upgrade reset
		// (SchemaReset, "baseline reset after upgrade") from a fresh install — only
		// the items are unusable, not the fact that a prior snapshot existed.
		return Snapshot{Key: snap.Key, HashSchemaVersion: snap.HashSchemaVersion}
	}
	if snap.Items == nil {
		snap.Items = map[string]string{}
	}
	return snap
}

// saveBriefSnapshot persists the watermark for one instance, stamping the current
// hash_schema_version and last_brief_at = now.UTC().Format(RFC3339). It is the
// commit half of the store and is byte-stable by construction:
//
//   - encoding/json marshals map[string]string keys in sorted (lexical) order, so
//     Items serialize identically regardless of insertion/iteration order — no
//     map-iteration-order dependence (project determinism invariant, T-12-08).
//   - last_brief_at is canonicalized to UTC RFC3339 (fixed-width, no fractional
//     seconds), so the same logical snapshot + the same injected now produce a
//     byte-identical file.
//
// Written 0600 via atomicWriteDurable (MkdirAll 0700 + synced temp + rename +
// parent-directory sync) because the file stores sensitive stableIDs at rest
// (T-12-06) and a loop commit checkpoint may only follow durable watermarks. The
// trailing newline mirrors memory.SaveStatus.
func Save(cfg config.Config, snap Snapshot, now time.Time) error {
	out := Snapshot{
		Key:               snap.Key,
		LastBriefAt:       now.UTC().Format(time.RFC3339),
		HashSchemaVersion: HashSchemaVersion,
		Items:             snap.Items,
	}
	if out.Items == nil {
		out.Items = map[string]string{}
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.WriteDurable(Path(cfg, snap.Key), append(body, '\n'), 0o600)
}

// classify is the PURE delta engine: it takes a loaded snapshot, the parsed
// memories for ONE instance, and the injected now, and returns the typed delta.
// It performs NO file I/O (the store is the I/O boundary) so idempotence,
// determinism, and cold-start are provable in unit tests before any wiring.
//
// Rules (content-hash diff ONLY — never created_at):
//   - registry.SourceInstanceKey(m) ok=false (empty Provider — filesystem connector) => the
//     memory is SKIPPED entirely: never bucketed, never in Baseline (M-1, prevents
//     a shared empty-key collapse / silent data loss).
//   - id NOT in snapshot.Items                 => Change="new".
//   - hash != snapshot.Items[id]               => Change="updated" (D-01).
//   - hash == snapshot.Items[id]               => skipped (unchanged, not surfaced).
//
// Cold start (no snapshot OR a re-baselined schema mismatch): ColdStart=true and
// NO items are surfaced (suppress the backfill flood); Baseline is still ALL
// current hashes so the store records them — archived backfill becomes the
// starting line, not a lost delta (D-04). SchemaReset is true iff the cold start
// was caused by a hash_schema_version bump.
//
// Baseline ALWAYS contains every kept (non-empty-key) memory's current hash — the
// caller persists this on commit so unchanged ids remain in the watermark.
//
// Boundary: classify deliberately does NOT filter deleted_at — tombstone handling
// (M-4: skip in render, drop id from the committed snapshot) is buildDigest's
// concern in Plan 04. classify stays a pure hash-diff.
func Classify(snap Snapshot, mems []memory.Memory, now time.Time) Delta {
	_ = now // reserved for future relative-window logic; the delta itself is hash-only.

	d := Delta{Baseline: map[string]string{}}

	// A snapshot whose hash_schema_version differs from current is cold-start-
	// EQUIVALENT regardless of whether its (now un-diffable, old-scheme) items were
	// already stripped by loadBriefSnapshot — classify owns this decision so the
	// rule holds for both the load path and a directly-constructed snapshot.
	schemaMismatch := snap.HashSchemaVersion != 0 && snap.HashSchemaVersion != HashSchemaVersion

	// Cold start: NO prior commit for this instance, OR the schema version moved
	// (its items can't be diffed). A NEVER-committed snapshot is the zero value
	// (LastBriefAt==""); a committed-but-empty snapshot (LastBriefAt stamped, no
	// items — e.g. an instance whose first commit ran over an empty vault, or one
	// whose every memory was later deleted) is STEADY STATE with an empty baseline,
	// NOT cold start — otherwise it would re-enter the 7-day courtesy window on
	// every run and never become a true delta. Either way we re-baseline all hashes
	// and surface nothing (suppress the backfill / post-upgrade flood).
	coldStart := (len(snap.Items) == 0 && snap.LastBriefAt == "") || schemaMismatch

	for _, m := range mems {
		if _, ok := registry.SourceInstanceKey(m); !ok {
			// Empty-key memory (filesystem connector): skip entirely (M-1).
			continue
		}
		d.Baseline[m.ID] = m.ContentHash
		if coldStart {
			// Suppress surfacing on cold start; Baseline still records the hash.
			continue
		}
		prev, seen := snap.Items[m.ID]
		switch {
		case !seen:
			d.Items = append(d.Items, DeltaItem{ID: m.ID, Change: "new"})
		case prev != m.ContentHash:
			d.Items = append(d.Items, DeltaItem{ID: m.ID, Change: "updated"})
			// prev == m.ContentHash => unchanged => skipped (not surfaced).
		}
	}

	if coldStart {
		d.ColdStart = true
		// SchemaReset is true iff this instance HAD a prior snapshot whose
		// hash_schema_version differed from current. loadBriefSnapshot preserves the
		// Key + the on-disk (mismatched) version while dropping the unusable items,
		// so a non-zero, non-current version is the exact "baseline reset after
		// upgrade" signal (vs a fresh install, where the snapshot is fully zero).
		// This also holds when classify is handed a mismatched snapshot directly
		// (unit tests), making the predicate uniform.
		d.SchemaReset = schemaMismatch
	}
	return d
}

// acquireBriefLock takes a fail-fast OS lock on the persistent guard selected for
// <StateDir>/brief/.lock, so the --advance read-modify-write (load -> classify ->
// write) cannot interleave with another commit (T-12-07). A hand-run --advance
// racing the scheduled job fails instead of corrupting the snapshot. The guard
// file is persistent but ownership is only the kernel lock on this open handle,
// so SIGKILL/power-loss releases it automatically and cannot strand the brief
// subsystem behind an O_EXCL file.
func AcquireLock(cfg config.Config) (release func(), err error) {
	dir := filepath.Join(cfg.StateDir, "brief")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, ".lock")
	guardPath := leasefile.GuardPath(lockPath)
	if err := os.MkdirAll(filepath.Dir(guardPath), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(guardPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := leasefile.TryLock(f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		_ = leasefile.Unlock(f)
		_ = f.Close()
	}, nil
}
