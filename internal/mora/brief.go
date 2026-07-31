package mora

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// briefHashSchemaVersion stamps the snapshot's hashing scheme. A bump is handled
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
const briefHashSchemaVersion = 2

// briefSnapshot is the per-instance watermark record persisted at
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
type briefSnapshot struct {
	Key               string            `json:"key"`
	LastBriefAt       string            `json:"last_brief_at"`
	HashSchemaVersion int               `json:"hash_schema_version"`
	Items             map[string]string `json:"items"`
}

// briefDeltaItem is one surfaced delta entry. Change is "new" or "updated".
// Unchanged items are never surfaced (so this type only ever carries deltas).
type briefDeltaItem struct {
	ID     string
	Change string // "new" | "updated"
}

// briefDelta is the typed result of classify — the seam Phases 13–16 consume
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
type briefDelta struct {
	Items       []briefDeltaItem
	ColdStart   bool
	SchemaReset bool
	Baseline    map[string]string
}

// briefPath returns <StateDir>/brief/<key>.json. Kept OUT of sync/ on purpose so
// sourceFreshness (which scans sync/) never reads the watermark.
func briefPath(cfg Config, key string) string {
	return filepath.Join(cfg.StateDir, "brief", key+".json")
}

// loadBriefSnapshot reads the watermark for one instance. It mirrors the
// memory.LoadStatus convention (zero value on os.ErrNotExist) and EXTENDS it for
// the watermark's safety requirements: any read error, unmarshal/corruption
// error, OR a hash_schema_version mismatch returns a zero (cold-start-equivalent)
// snapshot for THAT instance — a per-instance recover that NEVER propagates a
// fatal error that would blank the whole brief (T-12-05). The caller then sees
// ColdStart through classify and re-baselines.
func loadBriefSnapshot(cfg Config, key string) briefSnapshot {
	b, err := os.ReadFile(briefPath(cfg, key))
	if err != nil {
		// Missing (os.ErrNotExist) OR any other read error => cold-start-equivalent.
		_ = errors.Is(err, os.ErrNotExist) // documented: not distinguished; both recover.
		return briefSnapshot{}
	}
	var snap briefSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		// Corrupt/truncated/garbage => cold-start-equivalent for this instance.
		return briefSnapshot{}
	}
	if snap.HashSchemaVersion != briefHashSchemaVersion {
		// Schema bump => re-baseline. DROP the stale items (their hashes were
		// computed under the old scheme and can't be diffed) so classify treats this
		// instance as cold-start-equivalent. We PRESERVE the key and the on-disk
		// (mismatched) version so classify can distinguish a post-upgrade reset
		// (SchemaReset, "baseline reset after upgrade") from a fresh install — only
		// the items are unusable, not the fact that a prior snapshot existed.
		return briefSnapshot{Key: snap.Key, HashSchemaVersion: snap.HashSchemaVersion}
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
func saveBriefSnapshot(cfg Config, snap briefSnapshot, now time.Time) error {
	out := briefSnapshot{
		Key:               snap.Key,
		LastBriefAt:       now.UTC().Format(time.RFC3339),
		HashSchemaVersion: briefHashSchemaVersion,
		Items:             snap.Items,
	}
	if out.Items == nil {
		out.Items = map[string]string{}
	}
	body, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteDurable(briefPath(cfg, snap.Key), append(body, '\n'), 0o600)
}

// classify is the PURE delta engine: it takes a loaded snapshot, the parsed
// memories for ONE instance, and the injected now, and returns the typed delta.
// It performs NO file I/O (the store is the I/O boundary) so idempotence,
// determinism, and cold-start are provable in unit tests before any wiring.
//
// Rules (content-hash diff ONLY — never created_at):
//   - sourceInstanceKey(m) ok=false (empty Provider — filesystem connector) => the
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
func classify(snap briefSnapshot, mems []Memory, now time.Time) briefDelta {
	_ = now // reserved for future relative-window logic; the delta itself is hash-only.

	d := briefDelta{Baseline: map[string]string{}}

	// A snapshot whose hash_schema_version differs from current is cold-start-
	// EQUIVALENT regardless of whether its (now un-diffable, old-scheme) items were
	// already stripped by loadBriefSnapshot — classify owns this decision so the
	// rule holds for both the load path and a directly-constructed snapshot.
	schemaMismatch := snap.HashSchemaVersion != 0 && snap.HashSchemaVersion != briefHashSchemaVersion

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
		if _, ok := sourceInstanceKey(m); !ok {
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
			d.Items = append(d.Items, briefDeltaItem{ID: m.ID, Change: "new"})
		case prev != m.ContentHash:
			d.Items = append(d.Items, briefDeltaItem{ID: m.ID, Change: "updated"})
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
func acquireBriefLock(cfg Config) (release func(), err error) {
	dir := filepath.Join(cfg.StateDir, "brief")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(dir, ".lock")
	guardPath := leaseGuardPath(lockPath)
	if err := os.MkdirAll(filepath.Dir(guardPath), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(guardPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tryLockLeaseGuard(f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		_ = unlockLeaseGuard(f)
		_ = f.Close()
	}, nil
}

// ---------------------------------------------------------------------------
// READ SIDE — the dated brief artifact resolver (Phase 16, D16-1/D16-2)
//
// This is the sibling of artifact.go's WRITE side (briefArtifactPath /
// writeBriefArtifact). artifact.go writes <VaultDir>/briefs/<UTC-date>-brief.md;
// these helpers read the freshest such file (or generate one on demand). The
// resolver is PURE, LOCAL-ONLY, and WATERMARK-SAFE by construction:
//
//   - every date/freshness decision flows from the INJECTED now (never a fresh
//     time.Now() inside a helper), so the tests are deterministic and the UTC
//     scheme matches briefArtifactPath exactly;
//   - it makes ZERO network calls (no net/* import, no connector fetch/sync
//     function) — it only reads the vault from disk + computes (D16-2 / T-16-01);
//   - it NEVER advances or mutates the Phase-12 watermark — every generate-path
//     buildDigest forces advance:false and it never calls saveBriefSnapshot /
//     acquireBriefLock (D16-2 / T-16-02). The resolver is the read half; the
//     watermark store above is the write half, and the two never cross.
// ---------------------------------------------------------------------------

// briefFallbackWindowHours is the fixed, watermark-INDEPENDENT look-back used
// ONLY when the DELTA preview surfaces zero items (e.g. the scheduled --advance
// job already consumed today's delta). Re-building in WINDOW mode over the last
// 24h guarantees a session-start brief is never useless, while passing
// advance:false keeps it strictly read-only (no watermark mutation). It is a
// fixed constant so the fallback choice is fully deterministic and honest
// (T-16-04). 24h mirrors the digest's own digestDefaultHours framing.
const briefFallbackWindowHours = 24

// latestBriefPath resolves the NEWEST persisted brief under <VaultDir>/briefs by
// PARSING the YYYY-MM-DD date in each "<date>-brief.md" filename and returning
// the highest one. Selection is by parsed FILENAME date (UTC), NOT os file mtime
// — mtime is non-deterministic and timezone-fragile, and the filename date is the
// canonical UTC-day key briefArtifactPath writes (artifact.go:21).
//
// Returns (path, parsedDateUTC, true) for the highest-dated parseable file, or
// ("", zero, false) when briefs/ is absent (os.ReadDir error) or holds no
// parseable "-brief.md" file. now is accepted for call-site symmetry with
// briefIsFresh (and a future TTL) but never drives the SELECTION — the freshest
// FILE is purely a function of the filenames on disk, so two calls over the same
// dir are byte-identical.
func latestBriefPath(cfg Config, now time.Time) (string, time.Time, bool) {
	_ = now // selection is by parsed filename date, never now — kept for symmetry.
	dir := filepath.Join(cfg.VaultDir, "briefs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", time.Time{}, false
	}
	var (
		bestName string
		bestDate time.Time
		found    bool
	)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		prefix, ok := strings.CutSuffix(name, "-brief.md")
		if !ok {
			continue
		}
		d, perr := time.Parse("2006-01-02", prefix)
		if perr != nil {
			continue // unparseable date prefix (e.g. "2026-99-99") — ignore.
		}
		if !found || d.After(bestDate) {
			bestName, bestDate, found = name, d, true
		}
	}
	if !found {
		return "", time.Time{}, false
	}
	return filepath.Join(dir, bestName), bestDate.UTC(), true
}

// briefIsFresh reports whether a resolved brief's parsed date is fresh relative
// to the injected now: true iff dated's UTC day equals now's UTC day OR the day
// before. The yesterday allowance is the UTC-boundary fallback — the scheduled
// pulse-daily job persists today's file on a UTC day, and a user opening a
// session at local morning may still be on yesterday's UTC day, so treating
// today-or-yesterday as fresh avoids needlessly regenerating a brief the
// scheduled job just wrote (and avoids two different briefs in one local day).
//
// PURE: a function of the two times only — no clock, no filesystem. The
// comparison is on the "2006-01-02" string to avoid any sub-day drift.
func briefIsFresh(dated, now time.Time) bool {
	today := now.UTC().Format("2006-01-02")
	yday := now.UTC().AddDate(0, 0, -1).Format("2006-01-02")
	d := dated.UTC().Format("2006-01-02")
	return d == today || d == yday
}

// resolveBrief returns the LOCAL brief: the freshest persisted brief read
// VERBATIM when one exists for today's or yesterday's UTC day, otherwise a brief
// GENERATED on demand from the already-ingested local vault. It NEVER syncs,
// NEVER persists, and NEVER advances the watermark — zero egress + read-only
// (D16-1/D16-2). Returns (body, generated, err) where generated reports whether
// the body was freshly built (true) vs read from disk (false).
//
// Read path: if latestBriefPath finds a file AND briefIsFresh, os.ReadFile it and
// return its bytes VERBATIM (no re-render that could drift from what the
// scheduled job persisted — the printed-verbatim trust boundary).
//
// Generate path: build the DELTA digest first (briefOpts{advance:false} — the
// canonical "what changed since the last brief"). If that surfaces ZERO items
// across all sections (the scheduled --advance job already consumed today's
// delta), RE-build in WINDOW mode (a fixed briefFallbackWindowHours look-back,
// watermark-independent) so the session-start brief is never useless yet stays
// honest (T-16-04). BOTH builds force advance:false so neither mutates the
// watermark. The result is renderDigest at the same budget the WRITE side
// persists, so a generated brief is byte-shaped like a read one.
func resolveBrief(cfg Config, now time.Time, opts briefOpts) (string, bool, error) {
	// Only the GLOBAL (unfiltered) brief uses the persisted cache — the disk file is
	// the unfiltered brief, so a filtered request must bypass it and generate fresh
	// (§3), or it would masquerade as "nothing's up".
	if !opts.filtered() && !opts.forceRegen {
		if path, dated, ok := latestBriefPath(cfg, now); ok && briefIsFresh(dated, now) {
			body, err := os.ReadFile(path)
			if err != nil {
				return "", false, err
			}
			return reconcileCachedBriefHealth(cfg, now, string(body)), false, nil
		}
	}

	d, err := filteredBriefDigest(cfg, now, opts)
	if err != nil {
		return "", false, err
	}
	return renderDigest(d, cfg.contextDefaultTokens()*charsPerToken), true, nil
}

// healthBannerLinePrefix is the fixed prefix healthBannerLine/healthBannerFrom
// always emit — the marker reconcileCachedBriefHealth uses to find (and
// remove) an EMBEDDED banner line without re-parsing the whole render.
const healthBannerLinePrefix = "🔴 MORA HEALTH:"

// reconcileCachedBriefHealth closes the cached-brief hole (Packet C2, the live
// HEALTH-02 failure): resolveBrief's cache-read path returns a persisted file
// VERBATIM, but the file may be hours or days old — a source that died AFTER
// it was written must still redden THIS session's brief, and a source that
// RECOVERED since must not keep showing yesterday's red line forever. Fixed at
// the READ path, never the write path (the persisted file itself stays
// byte-stable — the "printed-verbatim trust boundary" is deliberate): re-derive
// the CURRENT banner from cfg/now and prepend or strip it on top of the cached
// body's existing (possibly stale, possibly absent) banner line.
//
// A no-op (returns body unchanged) whenever the current banner and the
// embedded one already agree — including the common "both empty" case — so a
// healthy fixture's cached brief stays byte-identical (the T0 budget fixture
// and every existing byte-stability test depend on this).
func reconcileCachedBriefHealth(cfg Config, now time.Time, body string) string {
	banner := healthBannerFrom(healthOf(cfg, now))

	header := body
	remainder := ""
	if idx := strings.IndexByte(body, '\n'); idx >= 0 {
		header, remainder = body[:idx], body[idx+1:]
	}

	embedded := ""
	rest := remainder
	if strings.HasPrefix(remainder, healthBannerLinePrefix) {
		if idx := strings.IndexByte(remainder, '\n'); idx >= 0 {
			embedded, rest = remainder[:idx], remainder[idx+1:]
		} else {
			embedded, rest = remainder, ""
		}
	}

	if banner == embedded {
		return body // already current — including the common healthy/no-banner case.
	}
	if banner == "" {
		return header + "\n" + rest // health recovered since the file was written: strip it.
	}
	return header + "\n" + banner + "\n" + rest
}

// filteredBriefDigest factors resolveBrief's generate path: a DELTA preview with a
// fixed 24h WINDOW fallback when the delta is empty, forwarding the full filter set
// and forcing advance:false on both builds. Shared by resolveBrief (human + --json),
// the `mora brief --envelope` cited-items prompt, and the MCP `brief` tool, so all
// three cite the SAME items. Read-only; never mutates the Phase-12 watermark.
func filteredBriefDigest(cfg Config, now time.Time, opts briefOpts) (Digest, error) {
	d, err := buildDigest(cfg, now, briefOpts{
		advance: false, perSourceCap: opts.perSourceCap,
		source: opts.source, entityIDSet: opts.entityIDSet, scope: opts.scope, sinceDays: opts.sinceDays,
	})
	if err != nil {
		return Digest{}, err
	}
	if briefSurfacedItemCount(d) == 0 {
		fallback, fallbackErr := buildDigest(cfg, now, briefOpts{
			advance: false, sinceHours: briefFallbackWindowHours, perSourceCap: opts.perSourceCap,
			source: opts.source, entityIDSet: opts.entityIDSet, scope: opts.scope, sinceDays: opts.sinceDays,
		})
		if fallbackErr != nil {
			return Digest{}, fallbackErr
		}
		d = preserveBriefFallbackEmptyExplanation(d, fallback)
	}
	return d, nil
}

// preserveBriefFallbackEmptyExplanation keeps the reason from the first DELTA
// pass when the brief's internal 24-hour WINDOW fallback is also empty. The
// fallback is not a caller-requested since_hours mode, so its window-specific
// reason must not replace a true steady-state "no changes since last brief."
func preserveBriefFallbackEmptyExplanation(delta, fallback Digest) Digest {
	if briefSurfacedItemCount(fallback) == 0 && delta.EmptyExplanation != "" {
		fallback.EmptyExplanation = delta.EmptyExplanation
	}
	return fallback
}

// briefSurfacedItemCount sums len(section.Items) across a digest — the
// "is the delta empty" predicate resolveBrief uses to decide whether to fall back
// to the 24h window. A digest with zero surfaced items everywhere is the
// post-advance common case the fallback exists to rescue (T-16-04).
func briefSurfacedItemCount(d Digest) int {
	// The Urgent shelf counts too (issue #62): its items are lifted OUT of the sections,
	// so ignoring them would treat an urgent-only delta as empty and fall back to the
	// 24h window — dropping the very shelf the delta produced.
	n := len(d.Urgent)
	for _, s := range d.Sections {
		n += len(s.Items)
	}
	return n
}
