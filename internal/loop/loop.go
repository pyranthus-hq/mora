// Package loop owns durable recurring-run state and at-most-once effect fences.
package loop

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/leasefile"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// loop.go is the `mora loop` durable-run spine — the Trigger primitive Mora
// lacked. It gives any recurring task a run-identity + same-period idempotency
// gate + a crash-safe lease + an append-only journal, so a scheduled agent loop
// (the daily-brief loop is the first) can answer "did I already run this period,
// did it succeed, is a prior run abandoned?" without a scheduler or a daemon.
//
// DESIGN INVARIANTS (mirroring brief.go, deliberately):
//   - Files are truth. Run-state lives in <StateDir>/loops/<id>/ FILES via
//     atomicWrite (temp+rename), NEVER in index.db — the DB is a gitignored,
//     rebuilt-from-scratch cache (dbPath, mora.go), so a run table there would be
//     silently wiped on `index rebuild`.
//   - Tolerant reads, fail-closed mutation. Read/status helpers map an invalid
//     run record to absent, but `loop begin` distinguishes a missing path from an
//     existing corrupt/unreadable/future-schema record and refuses to overwrite
//     it. Losing idempotency evidence must never silently reopen an effect.
//   - Injected now. Every date/TTL decision flows from an injected now (never a
//     fresh time.Now() inside a helper) so tests are deterministic and the UTC
//     scheme matches saveBriefSnapshot/briefArtifactPath.
//   - GENERIC. loop.go knows NOTHING about briefs (no digest/brief import). A
//     second loop is one `register` + one SKILL.md, zero Go changes. The
//     brief-specific status refinements (ran-uncommitted, source-stale) are
//     deliberately DEFERRED to keep the spine reusable (incl. for Azad).
//
// CONCURRENCY & DURABILITY BOUNDS (the deliberate scope of this tier):
// This is a SINGLE-HOST, SINGLE-USER local lease, not a distributed lock.
// Mutual exclusion is exact for a fresh lease: a concurrent begin within the TTL
// gets errLoopLockHeld and no-ops. Crash recovery is the TTL: an abandoned lease
// is reclaimed once acquired_at exceeds loopLockTTL. Every publish/reap/release/
// heartbeat transition is serialized by a persistent OS-backed guard, and both
// heartbeat and done validate the run record + lock owner under that same guard.
// Long advancing work refreshes the lease every TTL/3 and owner-fences again
// immediately before the non-idempotent commit. A process already reaped or
// superseded therefore cannot revive its lease, advance, or close a newer run.
// The record, lease, journal, and brief watermark remain separate local files,
// not one ACID/distributed transaction; the OS guard is the exact single-host
// exclusion boundary around the commit, and the active heartbeat covers work
// before that boundary that legitimately runs longer than one TTL.
// A money-touching or multi-host loop must GRADUATE to a real durable-execution
// runtime (Temporal) for exactly-once across hosts — this file lease is the local
// tier and is intentionally not a substitute for it.

const (
	sourceStaleHours          = 48
	loopsSubdir               = "loops"
	loopRunSchemaVersion      = 1
	loopRegistrySchemaVersion = 1

	// loopLockTTL is the hard ceiling on a leaked lock's lifetime: a SIGKILL'd
	// run leaves its .lock behind, and the next begin force-reaps it once
	// acquired_at is older than this — regardless of pid liveness — which bounds
	// the pid-reuse hazard to <=TTL (see reapStaleLock). Active advancing work
	// refreshes both the lease and latest.json every loopLockTTL/3, and fences the
	// non-idempotent commit with one final owner heartbeat immediately beforehand.
	loopLockTTL        = 15 * time.Minute
	loopAcquireBackoff = 100 * time.Millisecond

	// SkipExitCode is the process exit code `loop begin` returns when the
	// current period already succeeded — the idempotent "already ran today" skip
	// the SKILL branches on (distinct from a real failure's exit 1).
	SkipExitCode = 10

	loopDefaultCadence = "daily"
)

const (
	RunSchemaVersion      = loopRunSchemaVersion
	RegistrySchemaVersion = loopRegistrySchemaVersion
	LockTTL               = loopLockTTL
	DefaultCadence        = loopDefaultCadence
)

// ErrLockHeld is returned by acquireLoopLock when a LIVE, non-expired holder
// owns the lease — the caller no-ops and re-runs next period (the idempotency
// gate makes the skip safe). It is exported so the CLI layer can name the same
// sentinel value in its error taxonomy rather than a look-alike copy.
var ErrLockHeld = errors.New("loop lock held by a live run")

// errLoopLockHeld is the in-package spelling this file already uses.
var errLoopLockHeld = ErrLockHeld

// loopClock is the real liveness clock for begin/done/heartbeat and the
// scheduled wrapper. It is a seam only so end-to-end scheduler tests can pin a
// logical day without sleeping or depending on the host date.

// SkipError carries a specific process exit code up to main(), which honors
// any error implementing ExitCode() (see cmd/mora/main.go). The skip path emits
// its JSON to stdout and returns SkipError{code:10, msg:""} so main exits 10
// WITHOUT printing noise to stderr.
type SkipError struct {
	code int
	msg  string
}

func (e SkipError) Error() string { return e.msg }
func (e SkipError) ExitCode() int { return e.code }

// ExitCodeFor reports the structured process exit code a command wants main() to
// use, if the error chain carries one. It matches ONLY mora's own SkipError
// (the loop skip sentinel) — deliberately not any error that merely implements
// ExitCode() int, so a %w-wrapped *exec.ExitError from a failed subprocess (git,
// schtasks, ...) can never hijack mora's exit status.
func ExitCodeFor(err error) (int, bool) {
	var e SkipError
	if errors.As(err, &e) {
		return e.code, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// records
// ---------------------------------------------------------------------------

type RunStatus string

const (
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "succeeded"
	RunFailed    RunStatus = "failed"
)

func validRunStatus(s RunStatus) bool {
	return s == RunRunning || s == RunSucceeded || s == RunFailed
}

// RunRecord is the per-loop "current/last run" pointer at
// <StateDir>/loops/<id>/latest.json. It is self-contained (LoopID is
// cross-checked against the path it loads from) and 0600 (carries cursor + paths).
type RunRecord struct {
	SchemaVersion int       `json:"schema_version"`
	LoopID        string    `json:"loop_id"`
	RunID         string    `json:"run_id"`  // run_YYYYMMDD_HHMMSS_hex8, unique per attempt
	Period        string    `json:"period"`  // logical due bucket, e.g. "2026-06-24"
	Status        RunStatus `json:"status"`  // running | succeeded | failed
	Attempt       int       `json:"attempt"` // 1-based; matches journal attempt column

	StartedAt   string `json:"started_at"`
	UpdatedAt   string `json:"updated_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
	HeartbeatAt string `json:"heartbeat_at,omitempty"`
	// EffectStartedAt is the durable fail-closed intent written before entering a
	// non-idempotent effect. If it exists without EffectCommittedAt, the outcome is
	// uncertain (the process may have died or partially committed) and automatic
	// same-period retry is forbidden.
	EffectStartedAt string `json:"effect_started_at,omitempty"`
	// EffectCommittedAt is the durable at-most-once fence for a completed
	// non-idempotent effect. It may be present while Status is still running or
	// failed (for example, the process committed a brief and then crashed before
	// presentation/done). Same-period begin must skip whenever it is present.
	EffectCommittedAt string `json:"effect_committed_at,omitempty"`

	IdempotencyKey string `json:"idempotency_key"`        // stable per (loop_id+period)
	CursorToken    string `json:"cursor_token,omitempty"` // committed resume point; carried across attempts
	LastError      string `json:"last_error,omitempty"`   // set when Status==failed
}

// Registration records a loop's cadence + command at
// <StateDir>/loops/<id>/registry.json so status/list know the expected cadence.
type Registration struct {
	SchemaVersion int    `json:"schema_version"`
	LoopID        string `json:"loop_id"`
	Enabled       bool   `json:"enabled"`

	Cadence     string   `json:"cadence"`                // "daily" (v1), "hourly", "weekly"
	Command     []string `json:"command,omitempty"`      // argv the trigger runs, e.g. ["pulse","--advance",...]
	ScheduleJob string   `json:"schedule_job,omitempty"` // backing launchd job (e.g. "pulse-daily"); defaults to LoopID

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// scheduleJob is the launchd job name whose com.mora.<job>.plist presence means
// this loop is scheduled. v1 daily-brief is driven by the existing pulse-daily
// timer, so the registration links it explicitly; absent => the loop id itself.
func (r Registration) scheduleJob() string {
	if strings.TrimSpace(r.ScheduleJob) != "" {
		return r.ScheduleJob
	}
	return r.LoopID
}

// Health is the status output: a PRIMARY lifecycle state plus a SECONDARY
// scheduler annotation (so "never-run" + missing timer renders without hiding
// either fact). The primary ladder is derived purely from the run record +
// registry — generic, no brief coupling.
type Health struct {
	LoopID    string `json:"loop_id"`
	State     string `json:"state"` // never-run | running | uncertain | stale | failed | ok
	Scheduled bool   `json:"scheduled"`
	Period    string `json:"period,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	LastRunAt string `json:"last_run_at,omitempty"`
	Message   string `json:"message"`
}

// ---------------------------------------------------------------------------
// path helpers (pure)
// ---------------------------------------------------------------------------

func loopsRoot(cfg config.Config) string          { return filepath.Join(cfg.StateDir, loopsSubdir) }
func loopDir(cfg config.Config, id string) string { return filepath.Join(loopsRoot(cfg), id) }
func LatestPath(cfg config.Config, id string) string {
	return filepath.Join(loopDir(cfg, id), "latest.json")
}
func loopRegistryPath(cfg config.Config, id string) string {
	return filepath.Join(loopDir(cfg, id), "registry.json")
}
func loopJournalPath(cfg config.Config, id string) string {
	return filepath.Join(loopDir(cfg, id), "journal.log")
}
func LockPath(cfg config.Config, id string) string {
	return filepath.Join(loopDir(cfg, id), ".lock")
}
func loopRunArchivePath(cfg config.Config, id, period, runID string) string {
	return filepath.Join(loopDir(cfg, id), "runs", period+"_"+runID+".json")
}

var loopIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidID gates every loop id BEFORE any filepath.Join, so an id can never
// escape <StateDir>/loops/. Rejects "", ".", "..", and any separator/space.
func ValidID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	return loopIDPattern.MatchString(id)
}

// newRunID mints a unique-per-attempt run id from the INJECTED now + a random
// suffix (mirrors newID, mora.go). The timestamp is deterministic under an
// injected now; the random tail makes two same-second attempts distinct.
func NewRunID(now time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "run_" + now.UTC().Format("20060102_150405") + "_" + hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// run-record store (self-heal, mirrors loadBriefSnapshot)
// ---------------------------------------------------------------------------

// LoadRunRecord reads <id>/latest.json. ANY problem — missing, read error,
// corrupt JSON, schema-version mismatch, a body whose loop_id disagrees with the
// directory, or an unknown status — reads as (zero, false): cold-start-
// equivalent for tolerant read/status callers. The invalid file is left intact
// for diagnosis; loadRunRecordForBegin adds the fail-closed mutation boundary.
func LoadRunRecord(cfg config.Config, loopID string) (RunRecord, bool) {
	b, err := os.ReadFile(LatestPath(cfg, loopID))
	if err != nil {
		return RunRecord{}, false // missing OR any read error => cold start
	}
	var rec RunRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return RunRecord{}, false // corrupt/truncated/garbage
	}
	if rec.SchemaVersion != loopRunSchemaVersion {
		return RunRecord{}, false // written by another version; can't trust the shape
	}
	if rec.LoopID != loopID {
		return RunRecord{}, false // misfiled record (path/identity mismatch)
	}
	if !validRunStatus(rec.Status) {
		return RunRecord{}, false // unknown status; treat as absent
	}
	return rec, true
}

// loadRunRecordForBegin distinguishes a genuinely absent record from one that
// exists but cannot be trusted. Read/status callers retain the historical
// self-heal-to-absent behavior, but the non-idempotent begin gate must fail
// closed: overwriting corrupt, unreadable, or future-schema evidence could turn
// a previously committed same-period effect into a second advance.
func loadRunRecordForBegin(cfg config.Config, loopID string) (RunRecord, bool, error) {
	if rec, ok := LoadRunRecord(cfg, loopID); ok {
		return rec, true, nil
	}
	path := LatestPath(cfg, loopID)
	if _, err := os.Stat(path); err == nil {
		return RunRecord{}, false, fmt.Errorf("loop %q has an existing but invalid run record at %s; refusing to overwrite idempotency evidence", loopID, path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return RunRecord{}, false, fmt.Errorf("inspect loop %q run record at %s: %w", loopID, path, err)
	}
	return RunRecord{}, false, nil
}

// saveRunRecord persists latest.json, stamping schema_version + updated_at=now
// (UTC RFC3339). Written 0600 via atomicWrite (temp+rename) because it carries a
// cursor token and command/error text.
func saveRunRecord(cfg config.Config, r RunRecord, now time.Time) error {
	r.SchemaVersion = loopRunSchemaVersion
	r.UpdatedAt = now.UTC().Format(time.RFC3339)
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.Write(LatestPath(cfg, r.LoopID), append(body, '\n'), 0o600)
}

// saveRunRecordDurable is reserved for the two non-idempotent effect
// transitions. Its file + directory barriers make "started" durable before the
// effect can run and "committed" durable before the guard can be released.
func saveRunRecordDurable(cfg config.Config, r RunRecord, now time.Time) error {
	r.SchemaVersion = loopRunSchemaVersion
	r.UpdatedAt = now.UTC().Format(time.RFC3339)
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.WriteDurable(LatestPath(cfg, r.LoopID), append(body, '\n'), 0o600)
}

// saveRunRecordPreservingEffect keeps durability monotonic after a durable
// effect intent exists. A later heartbeat or terminal transition must not replace
// fsynced idempotency evidence with a merely atomic (page-cache-only) rename.
func saveRunRecordPreservingEffect(cfg config.Config, r RunRecord, now time.Time) error {
	if r.EffectStartedAt != "" || r.EffectCommittedAt != "" {
		return saveRunRecordDurable(cfg, r, now)
	}
	return saveRunRecord(cfg, r, now)
}

// sanitizeJournalNote keeps a journal note single-line and bounded — the
// journal.log is 0644 (like log.md), so the note must never carry raw output,
// secrets, or the '|' column delimiter / newlines that would corrupt the format.
func sanitizeJournalNote(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "|", "/")
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// appendLoopJournal appends ONE line per terminal transition to journal.log
// (0644, append-only), format: "<RFC3339> | <loop_id> | <run_id> | <status> | <attempt> | <note>".
// Mirrors the log.md pulse-line style (mora.go cmdPulse).
func appendLoopJournal(cfg config.Config, r RunRecord, now time.Time, note string) error {
	line := fmt.Sprintf("%s | %s | %s | %s | %d | %s\n",
		now.UTC().Format(time.RFC3339), r.LoopID, r.RunID, r.Status, r.Attempt, sanitizeJournalNote(note))
	return atomicio.AppendFile(loopJournalPath(cfg, r.LoopID), line)
}

// ---------------------------------------------------------------------------
// crash-safe lease (extends acquireBriefLock with a body + reaping)
// ---------------------------------------------------------------------------

// maxLoopAcquireAttempts bounds the contention spin. With a fixed backoff this
// is a deterministic short retry (no wall-clock dependency), so the begin gate
// either wins, reaps-then-wins, or fails fast as errLoopLockHeld.
const maxLoopAcquireAttempts = 4

// loopLockBody is the .lock contents. run_id IDENTIFIES the owning run (so done
// releases only its own lease); acquired_at drives the TTL. pid is diagnostic
// ONLY — it is NOT used for reaping: begin and done are separate short-lived
// processes, so the begin-process pid is dead while the run is legitimately in
// progress between begin and done. Liveness is therefore a false stale signal;
// the lock's lifetime is bounded purely by the TTL (an over-TTL lease is
// abandoned). Mirrors saveBriefSnapshot's UTC RFC3339 stamp.
type loopLockBody = leasefile.Body

// acquireLoopLock takes the per-loop lease at <id>/.lock for run runID. It
// mirrors acquireBriefLock's exclusive shape and ADDS a {run_id,pid,acquired_at}
// body + crash-safe TTL reaping. Mutual exclusion is by file EXISTENCE (not a
// held fd), so the lock survives across the begin->done process gap; Done
// releases it by path, but only if the lock still belongs to its run. A
// non-expired holder => errLoopLockHeld (the caller no-ops; the cadence retries).
func leaseRemovalOptions() leasefile.RemovalOptions          { return leasefile.DefaultRemovalOptions() }
func publishLockFile(path string, body []byte) (bool, error) { return leasefile.Publish(path, body) }

func loopLockReleaser(path string, observed []byte) func() {
	return leasefile.Releaser(path, observed, leaseRemovalOptions())
}
func removeLeaseFileGuarded(path string) error { return leasefile.Remove(path, leaseRemovalOptions()) }
func reapStaleLock(path string, now time.Time) (bool, error) {
	return leasefile.Reap(path, now, loopLockTTL, leaseRemovalOptions())
}

func releaseLockFileFor(path, owner string) { leasefile.Release(path, owner, leaseRemovalOptions()) }

func acquireLoopLock(cfg config.Config, id, runID string, now time.Time) (release func(), err error) {
	dir := loopDir(cfg, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lockPath := LockPath(cfg, id)
	body, _ := json.Marshal(loopLockBody{RunID: runID, PID: os.Getpid(), AcquiredAt: now.UTC().Format(time.RFC3339)})
	for attempt := 0; attempt < maxLoopAcquireAttempts; attempt++ {
		// Publish ATOMICALLY: write the full body to a unique temp, then os.Link it
		// into place. Link is atomic and fails EEXIST if the lock is held — so the
		// lockfile, once present, ALWAYS holds the complete {pid,acquired_at} body
		// and is NEVER observable empty. A plain O_EXCL-create-then-Write leaves a
		// brief empty window in which a concurrent reaper would (correctly, by its
		// own rules) judge the half-written lock corrupt-and-stale and steal it —
		// the multi-winner race. Link closes that window.
		published, perr := publishLockFile(lockPath, body)
		if perr != nil {
			return nil, perr // real fs error: do NOT interleave (acquireBriefLock rule)
		}
		if published {
			return loopLockReleaser(lockPath, body), nil
		}
		reaped, rerr := reapStaleLock(lockPath, now)
		if rerr != nil {
			return nil, rerr // transient read error: caller no-ops, re-run next period
		}
		if reaped {
			continue // we cleared a stale lock; retry publish (may lose to a racer — fine)
		}
		if attempt < maxLoopAcquireAttempts-1 {
			time.Sleep(loopAcquireBackoff)
		}
	}
	return nil, errLoopLockHeld // live holder: no-op, the cadence is the retry
}

// publishLockFile atomically creates lockPath holding body, via a fully-written
// temp + os.Link. Returns (true,nil) on success, (false,nil) if the lock is
// already held (EEXIST), or (false,err) on a real fs error. The temp is always
// cleaned up — on success its extra name is dropped, leaving only lockPath.

// publishLockFileGuarded is publishLockFile's inner operation. The caller must
// hold lockPath's lease-file guard.

// leaseRemovalTimeout is the WALL-CLOCK budget for removeLeaseFileGuarded's
// retry, deliberately much SHORTER than any acquire budget (sourcesAcquireTimeout
// and its siblings): removal and acquisition have different latency envelopes.
// Removal runs at shutdown (`loop done`, releaseLockFileFor) and INSIDE the lease
// guard on the reap path (breakLock), where every waiting acquirer is blocked
// behind it, so spending an acquire-sized wait here would tax the very acquirers
// the retry exists to unblock. The contended window it covers is one rival's
// in-flight os.Link/os.ReadFile — microseconds — so half a second is already
// ~20 jittered retries of headroom.

// removeLeaseFileGuarded frees a held lease while its cross-process guard is
// held, and the release MUST actually succeed. On Windows os.Remove can
// transiently fail with
// ERROR_SHARING_VIOLATION / ERROR_ACCESS_DENIED when a concurrent acquirer is
// touching the same path in the same instant — reading the body in
// reapStaleLockTTL, or os.Link-ing its own temp into place in publishLockFile.
// A dropped remove ORPHANS the lock: it lingers with a fresh acquired_at, so
// every later acquirer sees a non-reapable lease and starves for the whole
// sourcesLockTTL. That is the #113 Windows lease-liveness bug — even two
// concurrent governance writers exhaust the acquire budget, and a forget racing
// an hourly re-sync then can't append its suppression or remove the file and
// silently resurrects the forgotten memory. The acquire loop already tolerates
// this same sharing violation on publish/reap (it just backs off and retries);
// release was the one path that ignored it. So retry the remove on that transient
// error with the same jittered backoff the acquire loops use, bounded by its own
// leaseRemovalTimeout wall-clock budget (release therefore can never hang, and
// giving up returns the last transient error): a lock already gone (IsNotExist)
// is success, and a genuine non-contention error is terminal. Off Windows
// leaseRemovalRetryableFn is always false, so this collapses to exactly one
// os.Remove — behavior there is unchanged. It surfaces a permanent error to the
// lifecycle caller instead of silently leaking a terminal run's lease.

// reapStaleLock removes an ABANDONED loop lease and reports whether it did,
// using loopLockTTL as the abandonment bound. See reapStaleLockTTL for the
// mechanics; this thin wrapper keeps the loop call sites (and their tests)
// unchanged while the crash-safe lease primitives (publishLockFile / breakLock /
// loopLockReleaser / loopLockBody + this reaper) are shared with the sources.json
// lease in sources_lock.go.

// reapStaleLockTTL removes an ABANDONED lease and reports whether it did. Stale :=
// corrupt/empty/partial OR acquired_at older than ttl. Liveness is NOT a
// signal: the lock outlives its short-lived begin process by design, so a dead
// pid is the NORMAL state of a legitimately-held lease and must never trigger a
// reap (doing so would let a second begin start a concurrent run mid-flight).
// The TTL is therefore the sole abandonment bound. Mirrors loadBriefSnapshot:
// a corrupt lock is reapable, never a fatal error. ttl is a parameter (not the
// loopLockTTL constant) so the shorter-lived sources.json lease can reuse this
// exact reaper with its own abandonment bound.

// breakLock removes a lock judged stale only if it still contains the exact
// observed bytes. Every Mora publisher and reaper takes the persistent OS-backed
// guard first, so the compare+remove is one serialized transition: lockPath is
// never renamed away and therefore never has a restore window in which a third
// process can acquire and then be dropped.

// ---------------------------------------------------------------------------
// registry store (self-heal, same shape as the run record)
// ---------------------------------------------------------------------------

func LoadRegistration(cfg config.Config, loopID string) (Registration, bool) {
	b, err := os.ReadFile(loopRegistryPath(cfg, loopID))
	if err != nil {
		return Registration{}, false
	}
	var reg Registration
	if err := json.Unmarshal(b, &reg); err != nil {
		return Registration{}, false
	}
	if reg.SchemaVersion != loopRegistrySchemaVersion || reg.LoopID != loopID {
		return Registration{}, false
	}
	return reg, true
}

func saveLoopRegistration(cfg config.Config, reg Registration, now time.Time) error {
	reg.SchemaVersion = loopRegistrySchemaVersion
	reg.UpdatedAt = now.UTC().Format(time.RFC3339)
	if reg.CreatedAt == "" {
		reg.CreatedAt = reg.UpdatedAt
	}
	body, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.Write(loopRegistryPath(cfg, reg.LoopID), append(body, '\n'), 0o600)
}

// listLoopRegistrations enumerates every registered loop, skipping any whose
// registry self-heals to absent (corrupt/version-mismatch) — listing is
// best-effort, never fatal.
func listLoopRegistrations(cfg config.Config) []Registration {
	matches, _ := filepath.Glob(filepath.Join(loopsRoot(cfg), "*", "registry.json"))
	out := make([]Registration, 0, len(matches))
	for _, m := range matches {
		id := filepath.Base(filepath.Dir(m))
		if reg, ok := LoadRegistration(cfg, id); ok {
			out = append(out, reg)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// period + cadence
// ---------------------------------------------------------------------------

// PeriodFor maps an injected now to the logical due bucket for a cadence — the
// idempotency key's time component. Daily is the UTC day; hourly the UTC hour;
// weekly the ISO year-week. Same now + same cadence => same period (deterministic).
func PeriodFor(cadence string, now time.Time) string {
	switch cadence {
	case "hourly":
		return now.UTC().Format("2006-01-02T15")
	case "weekly":
		y, w := now.UTC().ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	default: // daily
		return now.UTC().Format("2006-01-02")
	}
}

// CadenceFor resolves a loop's cadence from its registry, defaulting to daily so
// `loop begin <id>` works even before `loop register` (the SKILL calls begin first).
func CadenceFor(cfg config.Config, id string) string {
	if reg, ok := LoadRegistration(cfg, id); ok && strings.TrimSpace(reg.Cadence) != "" {
		return reg.Cadence
	}
	return loopDefaultCadence
}

// loopAllowedLag is the staleness window for a cadence: how long after a
// successful run before status reports "stale". Daily reuses sourceStaleHours
// (48h, the brief's own threshold); a dead hourly loop must not read ok for days.
func loopAllowedLag(cadence string) time.Duration {
	switch cadence {
	case "hourly":
		return 2 * time.Hour
	case "weekly":
		return 8 * 24 * time.Hour
	default: // daily
		return sourceStaleHours * time.Hour
	}
}

// ---------------------------------------------------------------------------
// output helper
// ---------------------------------------------------------------------------

// loopEmit prints JSON (any payload) under --json, else a tailored human line.
func loopEmit(stdout io.Writer, jsonOut bool, payload any, humanLine string) error {
	if jsonOut {
		b, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, string(b))
		return nil
	}
	fmt.Fprintln(stdout, humanLine)
	return nil
}

// ---------------------------------------------------------------------------
// begin — the idempotency gate + crash reclaim
// ---------------------------------------------------------------------------

// Begin is the once-per-period gate. If the current period already
// SUCCEEDED, or its non-idempotent effect is durably committed even though
// terminal bookkeeping did not finish, it emits an exit-10 skip (the SKILL
// prints the saved artifact and stops). Otherwise it takes the lease (reaping a
// stale one), reclaims a crashed/failed same-period attempt (attempt++ carrying
// the committed cursor forward, recording a synthetic failed terminal for an
// abandoned run), writes a fresh running record, and returns nil. On success it
// intentionally does NOT release the lease — Done removes it.
func Begin(cfg config.Config, id string, jsonOut bool, now time.Time, stdout io.Writer) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid loop id %q", id)
	}
	cadence := CadenceFor(cfg, id)
	period := PeriodFor(cadence, now)

	emitSkip := func(reason string) error {
		payload := map[string]string{"skip": reason, "loop_id": id, "period": period}
		humanReason := "already succeeded"
		if reason == "effect-already-committed" {
			humanReason = "already committed its effect"
		}
		_ = loopEmit(stdout, jsonOut, payload,
			fmt.Sprintf("loop %q %s for %s — nothing to do", id, humanReason, period))
		return SkipError{code: SkipExitCode} // empty msg => no stderr noise, exit 10
	}
	uncertainEffect := func(rec RunRecord) error {
		return fmt.Errorf("loop %q run %s started a non-idempotent effect at %s but has no commit checkpoint; outcome is uncertain and automatic retry for %s is blocked; do not run --advance again", id, rec.RunID, rec.EffectStartedAt, period)
	}

	// FAST-PATH gate (pre-acquire): already succeeded this period => skip without
	// taking the lease. Also covers a success whose lock leaked (done crashed
	// mid-release), where acquire would otherwise wrongly report "already running".
	// A committed effect is equally final for at-most-once purposes even when the
	// process crashed before terminal presentation bookkeeping.
	prev, hasPrev, err := loadRunRecordForBegin(cfg, id)
	if err != nil {
		return err
	}
	if hasPrev && prev.Period == period {
		if prev.EffectStartedAt != "" && prev.EffectCommittedAt == "" && prev.Status != RunRunning {
			return uncertainEffect(prev)
		}
		if prev.Status == RunSucceeded {
			return emitSkip("already-succeeded")
		}
		if prev.EffectCommittedAt != "" {
			return emitSkip("effect-already-committed")
		}
	}

	runID := NewRunID(now) // identifies this attempt; stamped into both lock + record
	release, err := acquireLoopLock(cfg, id, runID, now)
	if err != nil {
		if errors.Is(err, errLoopLockHeld) {
			return fmt.Errorf("loop %q is already running (lease held)", id)
		}
		return err
	}

	// AUTHORITATIVE gate (post-acquire, under the lease): RE-LOAD so a run that
	// succeeded DURING our acquire is observed — closes the read-before-acquire
	// TOCTOU that would otherwise let a duplicate run bypass the gate.
	prev, hasPrev, err = loadRunRecordForBegin(cfg, id)
	if err != nil {
		release()
		return err
	}
	if hasPrev && prev.Period == period && prev.EffectStartedAt != "" && prev.EffectCommittedAt == "" {
		release()
		return uncertainEffect(prev)
	}
	if hasPrev && prev.Period == period && prev.Status == RunSucceeded {
		release()
		return emitSkip("already-succeeded")
	}
	if hasPrev && prev.Period == period && prev.EffectCommittedAt != "" {
		release()
		return emitSkip("effect-already-committed")
	}

	attempt := 1
	idem := id + "@" + period
	cursor := ""
	if hasPrev && prev.Period == period && (prev.Status == RunFailed || prev.Status == RunRunning) {
		attempt = prev.Attempt + 1
		if prev.IdempotencyKey != "" {
			idem = prev.IdempotencyKey
		}
		cursor = prev.CursorToken
		if prev.Status == RunRunning {
			// The prior attempt's lease was reaped above (TTL-abandoned) => record
			// ONE synthetic failed terminal for it so the journal stays honest.
			abandoned := prev
			abandoned.Status = RunFailed
			abandoned.FinishedAt = now.UTC().Format(time.RFC3339)
			abandoned.HeartbeatAt = ""
			_ = appendLoopJournal(cfg, abandoned, now, "recovered: abandoned run reclaimed")
		}
	}

	stamp := now.UTC().Format(time.RFC3339)
	rec := RunRecord{
		LoopID: id, RunID: runID, Period: period, Status: RunRunning,
		Attempt: attempt, StartedAt: stamp, HeartbeatAt: stamp,
		IdempotencyKey: idem, CursorToken: cursor,
	}
	if err := saveRunRecord(cfg, rec, now); err != nil {
		release() // don't leak the lease on a failed start
		return err
	}
	// SUCCESS: keep the lease (Done releases by path across the process gap).
	payload := map[string]any{"loop_id": id, "run_id": rec.RunID, "period": period, "attempt": attempt, "status": string(RunRunning)}
	return loopEmit(stdout, jsonOut, payload,
		fmt.Sprintf("loop %q begun: run %s, period %s, attempt %d", id, rec.RunID, period, attempt))
}

// ---------------------------------------------------------------------------
// done — terminal commit + lease release
// ---------------------------------------------------------------------------

// Done closes a RUNNING run: flips status to succeeded/failed, stamps finished_at,
// clears the heartbeat, appends ONE terminal journal line, archives an immutable
// runs/ copy on success, and releases the lease BY OWNERSHIP (the lock was
// acquired in a separate begin process). If runID is non-empty it must match the
// current run — otherwise a newer attempt has superseded this one and done
// refuses, so a late done from an abandoned run can never clobber a live run's
// record or steal its lock. An empty runID closes whatever run is current
// (manual/legacy use).
func Done(cfg config.Config, id, runID string, ok bool, failReason string, now time.Time, stdout io.Writer) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid loop id %q", id)
	}
	var completed RunRecord
	err := leasefile.WithGuard(LockPath(cfg, id), func() error {
		rec, found := LoadRunRecord(cfg, id)
		if !found {
			return fmt.Errorf("loop %q has no run to close (call `loop begin` first)", id)
		}
		if runID != "" && rec.RunID != runID {
			return fmt.Errorf("loop %q run %s is no longer current (superseded by %s); not closing", id, runID, rec.RunID)
		}
		if rec.Status != RunRunning {
			return fmt.Errorf("loop %q run %s is already terminal (%s); not closing again", id, rec.RunID, rec.Status)
		}
		if ok && rec.EffectStartedAt != "" && rec.EffectCommittedAt == "" {
			return fmt.Errorf("loop %q run %s has an uncertain non-idempotent effect; refusing successful close without a commit checkpoint", id, rec.RunID)
		}

		// The lock can reflect a newer run before latest.json does. Validate the
		// owner while the same guard excludes publish/reap/heartbeat, then keep the
		// guard through the terminal record and lease removal so heartbeat cannot
		// race a succeeded record back to running metadata.
		lockData, lerr := os.ReadFile(LockPath(cfg, id))
		if lerr != nil {
			if runID != "" || !errors.Is(lerr, os.ErrNotExist) {
				return fmt.Errorf("loop %q run %s no longer holds a lease; not closing", id, runID)
			}
		} else {
			var body loopLockBody
			if json.Unmarshal(lockData, &body) != nil || (body.RunID != "" && body.RunID != rec.RunID) {
				return fmt.Errorf("loop %q run %s no longer holds the lease; not closing", id, rec.RunID)
			}
		}

		stamp := now.UTC().Format(time.RFC3339)
		rec.FinishedAt = stamp
		rec.HeartbeatAt = ""
		note := "ok"
		if ok {
			rec.Status = RunSucceeded
			rec.LastError = ""
		} else {
			rec.Status = RunFailed
			rec.LastError = strings.TrimSpace(failReason)
			note = "fail: " + rec.LastError
		}
		if err := saveRunRecordPreservingEffect(cfg, rec, now); err != nil {
			return err
		}
		var terminalErr error
		if err := appendLoopJournal(cfg, rec, now, note); err != nil {
			terminalErr = fmt.Errorf("append loop journal: %w", err)
		}
		if ok {
			// Immutable audit copy; ignore only a benign audit-copy write failure;
			// latest.json + journal remain the authoritative terminal pair.
			if body, merr := json.MarshalIndent(rec, "", "  "); merr == nil {
				_ = atomicio.Write(loopRunArchivePath(cfg, id, rec.Period, rec.RunID), append(body, '\n'), 0o600)
			}
		}
		if lerr == nil {
			if err := removeLeaseFileGuarded(LockPath(cfg, id)); err != nil {
				terminalErr = errors.Join(terminalErr, fmt.Errorf("release loop lease: %w", err))
			}
		}
		completed = rec
		return terminalErr
	})
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "loop %q done: %s (run %s)\n", id, completed.Status, completed.RunID)
	return nil
}

// HeartbeatRun refreshes one active run's durable liveness evidence. The
// record check, owner-CAS lease re-stamp, and latest.json heartbeat happen under
// the same cross-process guard as done, so a terminal transition can never be
// overwritten by an in-flight heartbeat and a superseded holder can never revive.
func HeartbeatRun(cfg config.Config, id, runID string, now time.Time) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid loop id %q", id)
	}
	if strings.TrimSpace(runID) == "" {
		return errors.New("loop heartbeat requires --run <run_id>")
	}
	return leasefile.WithGuard(LockPath(cfg, id), func() error {
		return heartbeatLoopRunGuarded(cfg, id, runID, now)
	})
}

// heartbeatLoopRunGuarded is HeartbeatRun's owner-CAS body. The caller must
// hold this loop's lease-file guard.
func heartbeatLoopRunGuarded(cfg config.Config, id, runID string, now time.Time) error {
	rec, found := LoadRunRecord(cfg, id)
	if !found {
		return fmt.Errorf("loop %q has no active run", id)
	}
	if rec.RunID != runID {
		return fmt.Errorf("loop %q run %s is no longer current (superseded by %s)", id, runID, rec.RunID)
	}
	if rec.Status != RunRunning {
		return fmt.Errorf("loop %q run %s is already terminal (%s)", id, runID, rec.Status)
	}

	data, err := os.ReadFile(LockPath(cfg, id))
	if err != nil {
		return fmt.Errorf("loop %q run %s no longer holds a lease", id, runID)
	}
	var body loopLockBody
	if json.Unmarshal(data, &body) != nil || body.RunID != runID {
		return fmt.Errorf("loop %q run %s no longer holds the lease", id, runID)
	}
	stamp := now.UTC().Format(time.RFC3339)
	body.AcquiredAt = stamp
	next, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if err := atomicio.Write(LockPath(cfg, id), next, 0o600); err != nil {
		return err
	}
	rec.HeartbeatAt = stamp
	return saveRunRecordPreservingEffect(cfg, rec, now)
}

// testHookLoopEffectAfterRun injects a failure after fn returns but before its
// commit checkpoint. It deterministically models the otherwise process-kill-only
// crash boundary. Nil in production.
var testHookLoopEffectAfterRun func() error

// WithRunEffect executes a non-idempotent effect while holding the same
// persistent OS guard that publish/reap use. It validates ownership and durably
// records effect_started_at before entering fn, then records effect_committed_at
// on proven success. A started-without-committed record is deliberately left
// uncertain on any error or crash, so automatic same-period retry fails closed.
// A process suspended inside fn keeps reapers blocked; a process already reaped
// while waiting fails validation and never enters fn.
func WithRunEffect(cfg config.Config, id, runID string, clock func() time.Time, fn func() error) error {
	if clock == nil {
		clock = time.Now
	}
	return WithRunEffectAt(cfg, id, runID, clock(), clock, fn)
}

// WithRunEffectAt binds the authorized run period to the effect's own
// logical timestamp. Scheduled/manual callers pass the same `now` used for the
// brief artifact and watermark, so a run opened before a cadence boundary can
// never commit the next period's effect and then be followed by a second run.
func WithRunEffectAt(cfg config.Config, id, runID string, effectNow time.Time, clock func() time.Time, fn func() error) error {
	if clock == nil {
		clock = time.Now
	}
	if !ValidID(id) {
		return fmt.Errorf("invalid loop id %q", id)
	}
	if strings.TrimSpace(runID) == "" {
		return errors.New("loop effect requires a run id")
	}
	return leasefile.WithGuard(LockPath(cfg, id), func() error {
		if err := heartbeatLoopRunGuarded(cfg, id, runID, clock()); err != nil {
			return err
		}
		rec, found := LoadRunRecord(cfg, id)
		if !found {
			return fmt.Errorf("loop %q has no active run", id)
		}
		effectPeriod := PeriodFor(CadenceFor(cfg, id), effectNow)
		if rec.Period != effectPeriod {
			return fmt.Errorf("loop %q run %s authorizes period %s, but the effect belongs to %s; refusing cross-period advance", id, runID, rec.Period, effectPeriod)
		}
		if rec.EffectCommittedAt != "" {
			return fmt.Errorf("loop %q run %s already committed its effect", id, runID)
		}
		if rec.EffectStartedAt != "" {
			return fmt.Errorf("loop %q run %s already started its effect but has no commit checkpoint; outcome is uncertain", id, runID)
		}
		if err := markLoopRunEffectStartedGuarded(cfg, id, runID, clock()); err != nil {
			return fmt.Errorf("persist pre-effect loop intent: %w", err)
		}
		effectErr := fn()
		if testHookLoopEffectAfterRun != nil {
			effectErr = errors.Join(effectErr, testHookLoopEffectAfterRun())
		}
		if effectErr != nil {
			return fmt.Errorf("loop effect outcome may be partial; automatic retry is blocked: %w", effectErr)
		}
		if err := markLoopRunEffectCommittedGuarded(cfg, id, runID, clock()); err != nil {
			return fmt.Errorf("persist post-effect loop checkpoint; automatic retry is blocked: %w", err)
		}
		return nil
	})
}

// markLoopRunEffectStartedGuarded writes the fail-closed intent before a
// non-idempotent effect can run. The caller holds this loop's lease-file guard.
// Saving the record before refreshing the lease is intentional: if the latter
// fails, no effect runs and the false-positive uncertainty is safer than a retry
// after an unrecorded partial effect.
func markLoopRunEffectStartedGuarded(cfg config.Config, id, runID string, now time.Time) error {
	rec, found := LoadRunRecord(cfg, id)
	if !found {
		return fmt.Errorf("loop %q has no active run", id)
	}
	if rec.RunID != runID {
		return fmt.Errorf("loop %q run %s is no longer current (superseded by %s)", id, runID, rec.RunID)
	}
	if rec.Status != RunRunning {
		return fmt.Errorf("loop %q run %s is already terminal (%s)", id, runID, rec.Status)
	}
	if rec.EffectStartedAt != "" || rec.EffectCommittedAt != "" {
		return fmt.Errorf("loop %q run %s already has an effect checkpoint", id, runID)
	}
	data, err := os.ReadFile(LockPath(cfg, id))
	if err != nil {
		return fmt.Errorf("loop %q run %s no longer holds a lease", id, runID)
	}
	var body loopLockBody
	if json.Unmarshal(data, &body) != nil || body.RunID != runID {
		return fmt.Errorf("loop %q run %s no longer holds the lease", id, runID)
	}
	stamp := now.UTC().Format(time.RFC3339)
	rec.HeartbeatAt = stamp
	rec.EffectStartedAt = stamp
	if err := saveRunRecordDurable(cfg, rec, now); err != nil {
		return err
	}
	body.AcquiredAt = stamp
	next, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return atomicio.Write(LockPath(cfg, id), next, 0o600)
}

// markLoopRunEffectCommittedGuarded durably records a successful non-idempotent
// effect before releasing its OS guard. Save the run record first: once the
// effect returned success, a durable committed marker is the fail-closed source
// of truth even if the subsequent lease heartbeat itself cannot be written.
func markLoopRunEffectCommittedGuarded(cfg config.Config, id, runID string, now time.Time) error {
	rec, found := LoadRunRecord(cfg, id)
	if !found {
		return fmt.Errorf("loop %q has no active run", id)
	}
	if rec.RunID != runID {
		return fmt.Errorf("loop %q run %s is no longer current (superseded by %s)", id, runID, rec.RunID)
	}
	if rec.Status != RunRunning {
		return fmt.Errorf("loop %q run %s is already terminal (%s)", id, runID, rec.Status)
	}
	if rec.EffectStartedAt == "" {
		return fmt.Errorf("loop %q run %s has no durable effect intent", id, runID)
	}
	data, err := os.ReadFile(LockPath(cfg, id))
	if err != nil {
		return fmt.Errorf("loop %q run %s no longer holds a lease", id, runID)
	}
	var body loopLockBody
	if json.Unmarshal(data, &body) != nil || body.RunID != runID {
		return fmt.Errorf("loop %q run %s no longer holds the lease", id, runID)
	}
	stamp := now.UTC().Format(time.RFC3339)
	rec.HeartbeatAt = stamp
	rec.EffectCommittedAt = stamp
	if err := saveRunRecordDurable(cfg, rec, now); err != nil {
		return err
	}
	body.AcquiredAt = stamp
	next, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return atomicio.Write(LockPath(cfg, id), next, 0o600)
}

// HeartbeatCommand is the CLI-facing heartbeat, with the same JSON/human output
// convention as begin/status. Long-running in-process callers use
// HeartbeatRun directly and discard no failures at their commit fence.
func HeartbeatCommand(cfg config.Config, id, runID string, jsonOut bool, now time.Time, stdout io.Writer) error {
	if err := HeartbeatRun(cfg, id, runID, now); err != nil {
		return err
	}
	payload := map[string]string{
		"loop_id": id, "run_id": runID, "status": string(RunRunning),
		"heartbeat_at": now.UTC().Format(time.RFC3339),
	}
	return loopEmit(stdout, jsonOut, payload,
		fmt.Sprintf("loop %q heartbeat: run %s", id, runID))
}

// StartHeartbeat keeps a long in-process advancing pulse live. Ownership
// loss stops the ticker; cmdPulse performs its own synchronous heartbeat at the
// non-idempotent commit fence and therefore surfaces that loss before advancing.
func StartHeartbeat(cfg config.Config, id, runID string, clock func() time.Time) func() {
	if clock == nil {
		clock = time.Now
	}
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(loopLockTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if err := HeartbeatRun(cfg, id, runID, clock()); err != nil {
					return
				}
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

// releaseLoopLockFor removes the lease only if it still belongs to owner (or is
// absent / unreadable / legacy with no run_id). Owner check and removal share
// the same guard as publish/reap/heartbeat, so neither a newer owner nor a
// same-owner heartbeat can race the release into deleting or leaking a lease.
func releaseLoopLockFor(cfg config.Config, id, owner string) {
	releaseLockFileFor(LockPath(cfg, id), owner)
}

// releaseLockFileFor is the PATH-based extract of releaseLoopLockFor (Packet H):
// it releases the lease at lockPath only if it still belongs to owner (or is
// absent / unreadable / legacy with no run_id). The share import lease reuses
// this against subs/<name>/import.lock, so a reaped holder's late release can
// NEVER remove its successor's lease (the blind-release-drops-B's-lease hole).

// heartbeatLockFileFor is the owner-CAS re-stamp: it updates acquired_at only if
// the guarded on-disk run_id is still owner. The replacement happens while the
// same cross-process guard excludes publish/reap, so no absent-path window can
// resurrect a reaped holder over its successor.

// ---------------------------------------------------------------------------
// status — the GENERIC health ladder (run record + registry only)
// ---------------------------------------------------------------------------

// runningStaleAfter is how long a 'running' record can go without completing
// before status treats it as abandoned (a crash that leaked the lease). Tied to
// the lock TTL: past it, the next begin would reap the lease anyway.
const runningStaleAfter = loopLockTTL

// firstNonEmpty is shared with upgrade.go (returns the first non-"" arg).

func parseLoopTime(ss ...string) time.Time {
	for _, s := range ss {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// loopScheduled reports whether an OS timer backs this loop — a best-effort
// SECONDARY annotation. v1 detects the launchd plist (com.mora.<job>.plist);
// off darwin it is the TTL-only floor and reports false (the loop may still be
// agent/session-triggered — the primary health states never depend on this).
func loopScheduled(job string) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	matches, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), "Library", "LaunchAgents", "com.mora."+job+".plist"))
	return len(matches) > 0
}

// ClassifyHealth derives a loop's health PURELY from its run record + registry
// over an injected now — no brief/digest coupling. Precedence (first match wins):
//
//	never-run  no run record at all
//	running    status==running AND heartbeat within runningStaleAfter
//	uncertain  a non-idempotent effect started without a commit checkpoint
//	stale      status==running but abandoned (heartbeat too old), OR a success
//	           older than the cadence allows  (stale beats a dead 'running')
//	failed     last run failed
//	ok         a success within the cadence window
//
// Scheduled is always computed as a secondary annotation; when false it is
// appended to the message without overriding the primary state (D3).
func ClassifyHealth(reg Registration, rec RunRecord, recOK bool, now time.Time) Health {
	cadence := reg.Cadence
	if strings.TrimSpace(cadence) == "" {
		cadence = loopDefaultCadence
	}
	allowed := loopAllowedLag(cadence)

	h := Health{LoopID: firstNonEmpty(reg.LoopID, rec.LoopID), Scheduled: loopScheduled(reg.scheduleJob())}
	if recOK {
		h.Period = rec.Period
		h.Attempt = rec.Attempt
		h.LastRunAt = firstNonEmpty(rec.FinishedAt, rec.UpdatedAt, rec.StartedAt)
	}

	switch {
	case !recOK:
		h.State = "never-run"
		h.Message = "no run on record — this loop has never run"
	case rec.Status == RunRunning:
		hb := parseLoopTime(rec.HeartbeatAt, rec.StartedAt)
		if !hb.IsZero() && now.UTC().Sub(hb) <= runningStaleAfter {
			h.State = "running"
			h.Message = fmt.Sprintf("run %s in progress (period %s, attempt %d)", rec.RunID, rec.Period, rec.Attempt)
		} else if rec.EffectStartedAt != "" && rec.EffectCommittedAt == "" {
			h.State = "uncertain"
			h.Message = fmt.Sprintf("run %s started a non-idempotent effect at %s without a commit checkpoint — automatic same-period retry is blocked", rec.RunID, rec.EffectStartedAt)
		} else {
			h.State = "stale"
			h.Message = fmt.Sprintf("run %s appears abandoned — started %s, never completed", rec.RunID, firstNonEmpty(rec.StartedAt, rec.HeartbeatAt))
		}
	case rec.EffectStartedAt != "" && rec.EffectCommittedAt == "":
		h.State = "uncertain"
		h.Message = fmt.Sprintf("run %s started a non-idempotent effect at %s without a commit checkpoint — automatic same-period retry is blocked", rec.RunID, rec.EffectStartedAt)
	case rec.Status == RunFailed:
		h.State = "failed"
		h.Message = "last run failed"
		if rec.LastError != "" {
			h.Message += ": " + rec.LastError
		}
	case rec.Status == RunSucceeded:
		last := parseLoopTime(rec.FinishedAt, rec.UpdatedAt)
		if last.IsZero() || now.UTC().Sub(last) > allowed {
			h.State = "stale"
			h.Message = fmt.Sprintf("last success is older than the %s cadence allows (run again)", cadence)
		} else {
			h.State = "ok"
			h.Message = fmt.Sprintf("last run succeeded %s (period %s)", last.Format(time.RFC3339), rec.Period)
		}
	default:
		h.State = "stale"
		h.Message = "unknown run state"
	}

	if !h.Scheduled {
		h.Message += " (scheduler missing)"
	}
	return h
}

// Status prints a loop's classified health (JSON under --json, else human).
func Status(cfg config.Config, id string, jsonOut bool, now time.Time, stdout io.Writer) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid loop id %q", id)
	}
	registration, _ := LoadRegistration(cfg, id)
	if registration.LoopID == "" {
		registration.LoopID = id // status works pre-register
	}
	rec, ok := LoadRunRecord(cfg, id)
	h := ClassifyHealth(registration, rec, ok, now)
	return loopEmit(stdout, jsonOut, h, fmt.Sprintf("%s: %s — %s", h.LoopID, h.State, h.Message))
}

// ---------------------------------------------------------------------------
// register + list
// ---------------------------------------------------------------------------

var loopValidCadence = map[string]bool{"daily": true, "hourly": true, "weekly": true}

// Register records (or updates) a loop's cadence + command + backing OS-timer
// job. Re-registering preserves created_at. Idempotent. It returns the
// registration AS CONSTRUCTED so the CLI layer can publish it as the command's
// receipt without rebuilding the same struct from its own copy of these rules.
func Register(cfg config.Config, id, cadence string, command []string, scheduleJob string, now time.Time) (Registration, error) {
	if !ValidID(id) {
		return Registration{}, fmt.Errorf("invalid loop id %q", id)
	}
	cadence = strings.TrimSpace(cadence)
	if cadence == "" {
		cadence = loopDefaultCadence
	}
	if !loopValidCadence[cadence] {
		return Registration{}, fmt.Errorf("invalid cadence %q (want daily|hourly|weekly)", cadence)
	}
	existing, _ := LoadRegistration(cfg, id) // preserve created_at on re-register
	reg := Registration{
		LoopID: id, Enabled: true, Cadence: cadence,
		Command: command, ScheduleJob: strings.TrimSpace(scheduleJob),
		CreatedAt: existing.CreatedAt,
	}
	if err := saveLoopRegistration(cfg, reg, now); err != nil {
		return Registration{}, err
	}
	return reg, nil
}

// List prints every registered loop with its current health.
func List(cfg config.Config, jsonOut bool, now time.Time, stdout io.Writer) error {
	regs := listLoopRegistrations(cfg)
	items := make([]Health, 0, len(regs))
	for _, r := range regs {
		rec, ok := LoadRunRecord(cfg, r.LoopID)
		items = append(items, ClassifyHealth(r, rec, ok, now))
	}
	if jsonOut {
		return loopEmit(stdout, true, items, "")
	}
	if len(items) == 0 {
		fmt.Fprintln(stdout, "no loops registered")
		return nil
	}
	for _, h := range items {
		fmt.Fprintf(stdout, "%s\t%s\t%s\n", h.LoopID, h.State, h.Message)
	}
	return nil
}

// ---------------------------------------------------------------------------
// command dispatch
// ---------------------------------------------------------------------------

// cmdLoop dispatches `mora loop begin|heartbeat|done|status|register|list`. The id is the
// first positional arg (so the documented `loop begin <id> --json` form works
// regardless of flag ordering); list takes no id. Mirrors cmdSchedule/cmdPulse
// (flag.ContinueOnError + io.Discard). now is the real wall clock (the only
// place a fresh time.Now() is taken — every helper receives it injected).

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// SaveRecord persists a loop record with canonical timestamps and durability.
func SaveRecord(cfg config.Config, rec RunRecord, now time.Time) error {
	return saveRunRecord(cfg, rec, now)
}

// JournalPath returns the append-only journal path for a loop.
func JournalPath(cfg config.Config, id string) string { return loopJournalPath(cfg, id) }
