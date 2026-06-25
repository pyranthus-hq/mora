package mora

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
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
//   - Self-heal, never fatal. Any read/unmarshal/version/identity problem on a
//     run record reads as ABSENT (cold-start-equivalent), exactly like
//     loadBriefSnapshot — a corrupt journal never blanks the loop, it just
//     re-runs the period (which the idempotency gate makes safe).
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
// Mutual exclusion is EXACT for a fresh lease: a concurrent begin within the TTL
// gets errLoopLockHeld and no-ops (the os.Link publish is atomic; the run record
// is re-checked under the lease). Crash recovery is the TTL: an abandoned lease
// is reclaimed once acquired_at exceeds loopLockTTL. Run-id ownership on BOTH the
// record (the supersede guard) and the lock (lockOwner / breakLock's atomic
// claim) prevents one run from stealing another's lease.
// The residual: begin's gate, done's commit, and the lease are separate files
// mutated across separate short-lived processes without a held critical section,
// so a micro-window TOCTOU remains — but it can only open when a run's lease has
// ALREADY EXPIRED (>TTL), i.e. a process stalled >15m then resumes to commit
// while a fresh run starts. That does not occur for an interactive agent loop
// (a turn is seconds), and the harm is bounded: never lock theft (run-id guards),
// at worst a redundant IDEMPOTENT run or a stale record the next begin reconciles.
// A money-touching or multi-host loop must GRADUATE to a real durable-execution
// runtime (Temporal) for exactly-once across hosts — this file lease is the local
// tier and is intentionally not a substitute for it.

const (
	loopsSubdir               = "loops"
	loopRunSchemaVersion      = 1
	loopRegistrySchemaVersion = 1

	// loopLockTTL is the hard ceiling on a leaked lock's lifetime: a SIGKILL'd
	// run leaves its .lock behind, and the next begin force-reaps it once
	// acquired_at is older than this — regardless of pid liveness — which bounds
	// the pid-reuse hazard to <=TTL (see reapStaleLock). loopLockMaxRunHint is the
	// budget a single period MUST finish under; with no heartbeat refresh, a run
	// exceeding TTL would be (wrongly) reaped, so the daily-brief period (seconds)
	// stays far under it. If a future loop's period can exceed TTL, add a lock
	// heartbeat before relaxing the TTL-OR-dead rule.
	loopLockTTL        = 15 * time.Minute
	loopLockMaxRunHint = 10 * time.Minute
	loopAcquireBudget  = 3 * time.Second
	loopAcquireBackoff = 100 * time.Millisecond

	// loopSkipExitCode is the process exit code `loop begin` returns when the
	// current period already succeeded — the idempotent "already ran today" skip
	// the SKILL branches on (distinct from a real failure's exit 1).
	loopSkipExitCode = 10

	loopDefaultCadence = "daily"
)

// errLoopLockHeld is returned by acquireLoopLock when a LIVE, non-expired holder
// owns the lease — the caller no-ops and re-runs next period (the idempotency
// gate makes the skip safe).
var errLoopLockHeld = errors.New("loop lock held by a live run")

// exitCodeError carries a specific process exit code up to main(), which honors
// any error implementing ExitCode() (see cmd/mora/main.go). The skip path emits
// its JSON to stdout and returns exitCodeError{code:10, msg:""} so main exits 10
// WITHOUT printing noise to stderr.
type exitCodeError struct {
	code int
	msg  string
}

func (e exitCodeError) Error() string { return e.msg }
func (e exitCodeError) ExitCode() int { return e.code }

// ---------------------------------------------------------------------------
// records
// ---------------------------------------------------------------------------

type loopRunStatus string

const (
	loopRunRunning   loopRunStatus = "running"
	loopRunSucceeded loopRunStatus = "succeeded"
	loopRunFailed    loopRunStatus = "failed"
)

func validRunStatus(s loopRunStatus) bool {
	return s == loopRunRunning || s == loopRunSucceeded || s == loopRunFailed
}

// loopRunRecord is the per-loop "current/last run" pointer at
// <StateDir>/loops/<id>/latest.json. It is self-contained (LoopID is
// cross-checked against the path it loads from) and 0600 (carries cursor + paths).
type loopRunRecord struct {
	SchemaVersion int           `json:"schema_version"`
	LoopID        string        `json:"loop_id"`
	RunID         string        `json:"run_id"`  // run_YYYYMMDD_HHMMSS_hex8, unique per attempt
	Period        string        `json:"period"`  // logical due bucket, e.g. "2026-06-24"
	Status        loopRunStatus `json:"status"`  // running | succeeded | failed
	Attempt       int           `json:"attempt"` // 1-based; matches journal attempt column

	StartedAt   string `json:"started_at"`
	UpdatedAt   string `json:"updated_at"`
	FinishedAt  string `json:"finished_at,omitempty"`
	HeartbeatAt string `json:"heartbeat_at,omitempty"`

	IdempotencyKey string `json:"idempotency_key"`        // stable per (loop_id+period)
	CursorToken    string `json:"cursor_token,omitempty"` // committed resume point; carried across attempts
	LastError      string `json:"last_error,omitempty"`   // set when Status==failed
}

// loopRegistration records a loop's cadence + command at
// <StateDir>/loops/<id>/registry.json so status/list know the expected cadence.
type loopRegistration struct {
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
func (r loopRegistration) scheduleJob() string {
	if strings.TrimSpace(r.ScheduleJob) != "" {
		return r.ScheduleJob
	}
	return r.LoopID
}

// loopHealth is the status output: a PRIMARY lifecycle state plus a SECONDARY
// scheduler annotation (so "never-run" + missing timer renders without hiding
// either fact). The primary ladder is derived purely from the run record +
// registry — generic, no brief coupling.
type loopHealth struct {
	LoopID    string `json:"loop_id"`
	State     string `json:"state"` // never-run | running | stale | failed | ok
	Scheduled bool   `json:"scheduled"`
	Period    string `json:"period,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	LastRunAt string `json:"last_run_at,omitempty"`
	Message   string `json:"message"`
}

// ---------------------------------------------------------------------------
// path helpers (pure)
// ---------------------------------------------------------------------------

func loopsRoot(cfg Config) string          { return filepath.Join(cfg.StateDir, loopsSubdir) }
func loopDir(cfg Config, id string) string { return filepath.Join(loopsRoot(cfg), id) }
func loopLatestPath(cfg Config, id string) string {
	return filepath.Join(loopDir(cfg, id), "latest.json")
}
func loopRegistryPath(cfg Config, id string) string {
	return filepath.Join(loopDir(cfg, id), "registry.json")
}
func loopJournalPath(cfg Config, id string) string {
	return filepath.Join(loopDir(cfg, id), "journal.log")
}
func loopLockPath(cfg Config, id string) string {
	return filepath.Join(loopDir(cfg, id), ".lock")
}
func loopRunArchivePath(cfg Config, id, period, runID string) string {
	return filepath.Join(loopDir(cfg, id), "runs", period+"_"+runID+".json")
}

var loopIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validLoopID gates every loop id BEFORE any filepath.Join, so an id can never
// escape <StateDir>/loops/. Rejects "", ".", "..", and any separator/space.
func validLoopID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	return loopIDPattern.MatchString(id)
}

// newRunID mints a unique-per-attempt run id from the INJECTED now + a random
// suffix (mirrors newID, mora.go). The timestamp is deterministic under an
// injected now; the random tail makes two same-second attempts distinct.
func newRunID(now time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "run_" + now.UTC().Format("20060102_150405") + "_" + hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// run-record store (self-heal, mirrors loadBriefSnapshot)
// ---------------------------------------------------------------------------

// loadRunRecord reads <id>/latest.json. ANY problem — missing, read error,
// corrupt JSON, schema-version mismatch, a body whose loop_id disagrees with the
// directory, or an unknown status — reads as (zero, false): cold-start-
// equivalent, never a fatal that would blank the loop. The corrupt file is left
// on disk untouched (a future save overwrites it atomically).
func loadRunRecord(cfg Config, loopID string) (loopRunRecord, bool) {
	b, err := os.ReadFile(loopLatestPath(cfg, loopID))
	if err != nil {
		return loopRunRecord{}, false // missing OR any read error => cold start
	}
	var rec loopRunRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return loopRunRecord{}, false // corrupt/truncated/garbage
	}
	if rec.SchemaVersion != loopRunSchemaVersion {
		return loopRunRecord{}, false // written by another version; can't trust the shape
	}
	if rec.LoopID != loopID {
		return loopRunRecord{}, false // misfiled record (path/identity mismatch)
	}
	if !validRunStatus(rec.Status) {
		return loopRunRecord{}, false // unknown status; treat as absent
	}
	return rec, true
}

// saveRunRecord persists latest.json, stamping schema_version + updated_at=now
// (UTC RFC3339). Written 0600 via atomicWrite (temp+rename) because it carries a
// cursor token and command/error text.
func saveRunRecord(cfg Config, r loopRunRecord, now time.Time) error {
	r.SchemaVersion = loopRunSchemaVersion
	r.UpdatedAt = now.UTC().Format(time.RFC3339)
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(loopLatestPath(cfg, r.LoopID), append(body, '\n'), 0o600)
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
func appendLoopJournal(cfg Config, r loopRunRecord, now time.Time, note string) error {
	line := fmt.Sprintf("%s | %s | %s | %s | %d | %s\n",
		now.UTC().Format(time.RFC3339), r.LoopID, r.RunID, r.Status, r.Attempt, sanitizeJournalNote(note))
	return appendFile(loopJournalPath(cfg, r.LoopID), line)
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
type loopLockBody struct {
	RunID      string `json:"run_id"`
	PID        int    `json:"pid"`
	AcquiredAt string `json:"acquired_at"`
}

// acquireLoopLock takes the per-loop lease at <id>/.lock for run runID. It
// mirrors acquireBriefLock's exclusive shape and ADDS a {run_id,pid,acquired_at}
// body + crash-safe TTL reaping. Mutual exclusion is by file EXISTENCE (not a
// held fd), so the lock survives across the begin->done process gap; loopDone
// releases it by path, but only if the lock still belongs to its run. A
// non-expired holder => errLoopLockHeld (the caller no-ops; the cadence retries).
func acquireLoopLock(cfg Config, id, runID string, now time.Time) (release func(), err error) {
	dir := loopDir(cfg, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lockPath := loopLockPath(cfg, id)
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
			return loopLockReleaser(lockPath), nil
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
func publishLockFile(lockPath string, body []byte) (bool, error) {
	dir := filepath.Dir(lockPath)
	tmp, err := os.CreateTemp(dir, ".lock-*.tmp")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // drop the temp name whether link succeeds or not
	if _, werr := tmp.Write(body); werr != nil {
		tmp.Close()
		return false, werr
	}
	if cerr := tmp.Close(); cerr != nil {
		return false, cerr
	}
	if err := os.Link(tmpName, lockPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil // already held
		}
		return false, err
	}
	return true, nil
}

func loopLockReleaser(lockPath string) func() {
	released := false
	return func() {
		if released {
			return
		}
		released = true
		_ = os.Remove(lockPath)
	}
}

// reapStaleLock removes an ABANDONED lease and reports whether it did. Stale :=
// corrupt/empty/partial OR acquired_at older than the TTL. Liveness is NOT a
// signal: the lock outlives its short-lived begin process by design, so a dead
// pid is the NORMAL state of a legitimately-held lease and must never trigger a
// reap (doing so would let a second begin start a concurrent run mid-flight).
// The TTL is therefore the sole abandonment bound. Mirrors loadBriefSnapshot:
// a corrupt lock is reapable, never a fatal error.
func reapStaleLock(lockPath string, now time.Time) (bool, error) {
	data, err := os.ReadFile(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil // freed between our publish attempt and read: path is free
	}
	if err != nil {
		return false, err // transient read error: do NOT reap blindly
	}

	stale := false
	var body loopLockBody
	if jerr := json.Unmarshal(data, &body); jerr != nil || body.AcquiredAt == "" {
		stale = true // corrupt / empty / partial
	} else if t, perr := time.Parse(time.RFC3339, body.AcquiredAt); perr != nil {
		stale = true // unparseable timestamp => corrupt => stale
	} else if now.UTC().Sub(t.UTC()) >= loopLockTTL {
		stale = true // EXPIRED — the run exceeded the abandonment bound
	}
	if !stale {
		return false, nil // a fresh lease: the real holder (regardless of pid liveness)
	}
	return breakLock(lockPath, data)
}

// breakLock atomically removes a lock judged stale, only if it is still that
// exact lock. It rename-CLAIMS the path first (os.Rename is the atomic move of
// the current inode — exactly one concurrent reaper's rename of a given inode
// succeeds), THEN verifies the claimed bytes match what was judged stale. If a
// fresh holder republished in the window between the stale judgement and the
// claim, the claimed bytes differ and we RESTORE the lock via os.Link (which
// fails benignly if a newer lock already exists) instead of deleting it — closing
// the A-deletes-B's-fresh-lock race for good. The genuine acquirer is still
// whoever's subsequent publish (os.Link in acquireLoopLock) succeeds.
func breakLock(lockPath string, observed []byte) (bool, error) {
	tmp := lockPath + ".reap-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := os.Rename(lockPath, tmp); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil // already reaped by someone else => path is free
		}
		return false, err
	}
	// We now exclusively hold whatever inode was at lockPath. Verify it's the
	// stale lock we judged — not a fresh one published in the race window.
	if cur, rerr := os.ReadFile(tmp); rerr == nil && !bytes.Equal(cur, observed) {
		// Claimed a freshly-republished lock. Put it back WITHOUT clobbering: Link
		// recreates lockPath from our inode only if the path is still free; if a
		// newer lock already sits there, Link fails and we just drop our copy.
		_ = os.Link(tmp, lockPath)
		_ = os.Remove(tmp)
		return false, nil
	}
	_ = os.Remove(tmp)
	return true, nil
}

// ---------------------------------------------------------------------------
// registry store (self-heal, same shape as the run record)
// ---------------------------------------------------------------------------

func loadLoopRegistration(cfg Config, loopID string) (loopRegistration, bool) {
	b, err := os.ReadFile(loopRegistryPath(cfg, loopID))
	if err != nil {
		return loopRegistration{}, false
	}
	var reg loopRegistration
	if err := json.Unmarshal(b, &reg); err != nil {
		return loopRegistration{}, false
	}
	if reg.SchemaVersion != loopRegistrySchemaVersion || reg.LoopID != loopID {
		return loopRegistration{}, false
	}
	return reg, true
}

func saveLoopRegistration(cfg Config, reg loopRegistration, now time.Time) error {
	reg.SchemaVersion = loopRegistrySchemaVersion
	reg.UpdatedAt = now.UTC().Format(time.RFC3339)
	if reg.CreatedAt == "" {
		reg.CreatedAt = reg.UpdatedAt
	}
	body, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(loopRegistryPath(cfg, reg.LoopID), append(body, '\n'), 0o600)
}

// listLoopRegistrations enumerates every registered loop, skipping any whose
// registry self-heals to absent (corrupt/version-mismatch) — listing is
// best-effort, never fatal.
func listLoopRegistrations(cfg Config) []loopRegistration {
	matches, _ := filepath.Glob(filepath.Join(loopsRoot(cfg), "*", "registry.json"))
	out := make([]loopRegistration, 0, len(matches))
	for _, m := range matches {
		id := filepath.Base(filepath.Dir(m))
		if reg, ok := loadLoopRegistration(cfg, id); ok {
			out = append(out, reg)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// period + cadence
// ---------------------------------------------------------------------------

// periodFor maps an injected now to the logical due bucket for a cadence — the
// idempotency key's time component. Daily is the UTC day; hourly the UTC hour;
// weekly the ISO year-week. Same now + same cadence => same period (deterministic).
func periodFor(cadence string, now time.Time) string {
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

// cadenceFor resolves a loop's cadence from its registry, defaulting to daily so
// `loop begin <id>` works even before `loop register` (the SKILL calls begin first).
func cadenceFor(cfg Config, id string) string {
	if reg, ok := loadLoopRegistration(cfg, id); ok && strings.TrimSpace(reg.Cadence) != "" {
		return reg.Cadence
	}
	return loopDefaultCadence
}

// loopAllowedLag is the staleness window for a cadence: how long after a
// successful run before status reports "stale". Daily reuses digestStaleHours
// (48h, the brief's own threshold); a dead hourly loop must not read ok for days.
func loopAllowedLag(cadence string) time.Duration {
	switch cadence {
	case "hourly":
		return 2 * time.Hour
	case "weekly":
		return 8 * 24 * time.Hour
	default: // daily
		return digestStaleHours * time.Hour
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

// loopBegin is the once-per-period gate. If the current period already
// SUCCEEDED, it emits {"skip":"already-succeeded"} and returns the exit-10
// sentinel (the SKILL prints the saved artifact and stops). Otherwise it takes
// the lease (reaping a stale one), reclaims a crashed/failed same-period attempt
// (attempt++ carrying the committed cursor forward, recording a synthetic failed
// terminal for an abandoned run), writes a fresh running record, and returns nil.
// On success it intentionally does NOT release the lease — loopDone removes it.
func loopBegin(cfg Config, id string, jsonOut bool, now time.Time, stdout io.Writer) error {
	if !validLoopID(id) {
		return fmt.Errorf("invalid loop id %q", id)
	}
	cadence := cadenceFor(cfg, id)
	period := periodFor(cadence, now)

	emitSkip := func() error {
		payload := map[string]string{"skip": "already-succeeded", "loop_id": id, "period": period}
		_ = loopEmit(stdout, jsonOut, payload,
			fmt.Sprintf("loop %q already succeeded for %s — nothing to do", id, period))
		return exitCodeError{code: loopSkipExitCode} // empty msg => no stderr noise, exit 10
	}

	// FAST-PATH gate (pre-acquire): already succeeded this period => skip without
	// taking the lease. Also covers a success whose lock leaked (done crashed
	// mid-release), where acquire would otherwise wrongly report "already running".
	if prev, ok := loadRunRecord(cfg, id); ok && prev.Period == period && prev.Status == loopRunSucceeded {
		return emitSkip()
	}

	runID := newRunID(now) // identifies this attempt; stamped into both lock + record
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
	prev, hasPrev := loadRunRecord(cfg, id)
	if hasPrev && prev.Period == period && prev.Status == loopRunSucceeded {
		release()
		return emitSkip()
	}

	attempt := 1
	idem := id + "@" + period
	cursor := ""
	if hasPrev && prev.Period == period && (prev.Status == loopRunFailed || prev.Status == loopRunRunning) {
		attempt = prev.Attempt + 1
		if prev.IdempotencyKey != "" {
			idem = prev.IdempotencyKey
		}
		cursor = prev.CursorToken
		if prev.Status == loopRunRunning {
			// The prior attempt's lease was reaped above (TTL-abandoned) => record
			// ONE synthetic failed terminal for it so the journal stays honest.
			abandoned := prev
			abandoned.Status = loopRunFailed
			abandoned.FinishedAt = now.UTC().Format(time.RFC3339)
			abandoned.HeartbeatAt = ""
			_ = appendLoopJournal(cfg, abandoned, now, "recovered: abandoned run reclaimed")
		}
	}

	stamp := now.UTC().Format(time.RFC3339)
	rec := loopRunRecord{
		LoopID: id, RunID: runID, Period: period, Status: loopRunRunning,
		Attempt: attempt, StartedAt: stamp, HeartbeatAt: stamp,
		IdempotencyKey: idem, CursorToken: cursor,
	}
	if err := saveRunRecord(cfg, rec, now); err != nil {
		release() // don't leak the lease on a failed start
		return err
	}
	// SUCCESS: keep the lease (loopDone releases by path across the process gap).
	payload := map[string]any{"loop_id": id, "run_id": rec.RunID, "period": period, "attempt": attempt, "status": string(loopRunRunning)}
	return loopEmit(stdout, jsonOut, payload,
		fmt.Sprintf("loop %q begun: run %s, period %s, attempt %d", id, rec.RunID, period, attempt))
}

// ---------------------------------------------------------------------------
// done — terminal commit + lease release
// ---------------------------------------------------------------------------

// loopDone closes a run: flips status to succeeded/failed, stamps finished_at,
// clears the heartbeat, appends ONE terminal journal line, archives an immutable
// runs/ copy on success, and releases the lease BY OWNERSHIP (the lock was
// acquired in a separate begin process). If runID is non-empty it must match the
// current run — otherwise a newer attempt has superseded this one and done
// refuses, so a late done from an abandoned run can never clobber a live run's
// record or steal its lock. An empty runID closes whatever run is current
// (manual/legacy use).
func loopDone(cfg Config, id, runID string, ok bool, failReason string, now time.Time, stdout io.Writer) error {
	if !validLoopID(id) {
		return fmt.Errorf("invalid loop id %q", id)
	}
	rec, found := loadRunRecord(cfg, id)
	if !found {
		return fmt.Errorf("loop %q has no run to close (call `loop begin` first)", id)
	}
	if runID != "" && rec.RunID != runID {
		return fmt.Errorf("loop %q run %s is no longer current (superseded by %s); not closing", id, runID, rec.RunID)
	}
	// Lease-ownership guard: the lock reflects a newer run EARLIER than latest.json
	// does (begin publishes the lock before saving the record), so checking it
	// closes the window where a late done passes the record check while a fresher
	// run already holds the lease.
	if runID != "" {
		if owner, present := lockOwner(cfg, id); present && owner != "" && owner != runID {
			return fmt.Errorf("loop %q run %s no longer holds the lease (held by %s); not closing", id, runID, owner)
		}
	}

	stamp := now.UTC().Format(time.RFC3339)
	rec.FinishedAt = stamp
	rec.HeartbeatAt = ""
	note := "ok"
	if ok {
		rec.Status = loopRunSucceeded
		rec.LastError = ""
	} else {
		rec.Status = loopRunFailed
		rec.LastError = strings.TrimSpace(failReason)
		note = "fail: " + rec.LastError
	}
	if err := saveRunRecord(cfg, rec, now); err != nil {
		return err
	}
	if err := appendLoopJournal(cfg, rec, now, note); err != nil {
		return err
	}
	if ok {
		// Immutable audit copy; ignore a benign re-write of the same terminal.
		if body, merr := json.MarshalIndent(rec, "", "  "); merr == nil {
			_ = atomicWrite(loopRunArchivePath(cfg, id, rec.Period, rec.RunID), append(body, '\n'), 0o600)
		}
	}
	// Release the lease BY OWNERSHIP — only if the lock still belongs to this run.
	releaseLoopLockFor(cfg, id, rec.RunID)

	fmt.Fprintf(stdout, "loop %q done: %s (run %s)\n", id, rec.Status, rec.RunID)
	return nil
}

// lockOwner returns the run_id currently holding the lease. present=false when
// no lock exists; present=true with an empty owner when the lock is corrupt.
func lockOwner(cfg Config, id string) (owner string, present bool) {
	data, err := os.ReadFile(loopLockPath(cfg, id))
	if err != nil {
		return "", false
	}
	var body loopLockBody
	if json.Unmarshal(data, &body) != nil {
		return "", true
	}
	return body.RunID, true
}

// releaseLoopLockFor removes the lease only if it still belongs to owner (or is
// absent / unreadable / legacy with no run_id). The delete routes through
// breakLock's atomic content compare-and-claim, so if a newer run republishes
// the lock in the read->remove window, the new lock is RESTORED, never deleted —
// the fix for a late done stealing a live lease.
func releaseLoopLockFor(cfg Config, id, owner string) {
	lockPath := loopLockPath(cfg, id)
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return // absent/unreadable: nothing to release
	}
	var body loopLockBody
	if json.Unmarshal(data, &body) == nil && body.RunID != "" && body.RunID != owner {
		return // a different run owns the lease now; leave it
	}
	_, _ = breakLock(lockPath, data) // atomic: deletes only if still these exact bytes
}

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

// classifyLoopHealth derives a loop's health PURELY from its run record + registry
// over an injected now — no brief/digest coupling. Precedence (first match wins):
//
//	never-run  no run record at all
//	running    status==running AND heartbeat within runningStaleAfter
//	stale      status==running but abandoned (heartbeat too old), OR a success
//	           older than the cadence allows  (stale beats a dead 'running')
//	failed     last run failed
//	ok         a success within the cadence window
//
// Scheduled is always computed as a secondary annotation; when false it is
// appended to the message without overriding the primary state (D3).
func classifyLoopHealth(reg loopRegistration, rec loopRunRecord, recOK bool, now time.Time) loopHealth {
	cadence := reg.Cadence
	if strings.TrimSpace(cadence) == "" {
		cadence = loopDefaultCadence
	}
	allowed := loopAllowedLag(cadence)

	h := loopHealth{LoopID: firstNonEmpty(reg.LoopID, rec.LoopID), Scheduled: loopScheduled(reg.scheduleJob())}
	if recOK {
		h.Period = rec.Period
		h.Attempt = rec.Attempt
		h.LastRunAt = firstNonEmpty(rec.FinishedAt, rec.UpdatedAt, rec.StartedAt)
	}

	switch {
	case !recOK:
		h.State = "never-run"
		h.Message = "no run on record — this loop has never run"
	case rec.Status == loopRunRunning:
		hb := parseLoopTime(rec.HeartbeatAt, rec.StartedAt)
		if !hb.IsZero() && now.UTC().Sub(hb) <= runningStaleAfter {
			h.State = "running"
			h.Message = fmt.Sprintf("run %s in progress (period %s, attempt %d)", rec.RunID, rec.Period, rec.Attempt)
		} else {
			h.State = "stale"
			h.Message = fmt.Sprintf("run %s appears abandoned — started %s, never completed", rec.RunID, firstNonEmpty(rec.StartedAt, rec.HeartbeatAt))
		}
	case rec.Status == loopRunFailed:
		h.State = "failed"
		h.Message = "last run failed"
		if rec.LastError != "" {
			h.Message += ": " + rec.LastError
		}
	case rec.Status == loopRunSucceeded:
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

// loopStatus prints a loop's classified health (JSON under --json, else human).
func loopStatus(cfg Config, id string, jsonOut bool, now time.Time, stdout io.Writer) error {
	if !validLoopID(id) {
		return fmt.Errorf("invalid loop id %q", id)
	}
	registration, _ := loadLoopRegistration(cfg, id)
	if registration.LoopID == "" {
		registration.LoopID = id // status works pre-register
	}
	rec, ok := loadRunRecord(cfg, id)
	h := classifyLoopHealth(registration, rec, ok, now)
	return loopEmit(stdout, jsonOut, h, fmt.Sprintf("%s: %s — %s", h.LoopID, h.State, h.Message))
}

// ---------------------------------------------------------------------------
// register + list
// ---------------------------------------------------------------------------

var loopValidCadence = map[string]bool{"daily": true, "hourly": true, "weekly": true}

// loopRegister records (or updates) a loop's cadence + command + backing OS-timer
// job. Re-registering preserves created_at. Idempotent.
func loopRegister(cfg Config, id, cadence string, command []string, scheduleJob string, now time.Time, stdout io.Writer) error {
	if !validLoopID(id) {
		return fmt.Errorf("invalid loop id %q", id)
	}
	cadence = strings.TrimSpace(cadence)
	if cadence == "" {
		cadence = loopDefaultCadence
	}
	if !loopValidCadence[cadence] {
		return fmt.Errorf("invalid cadence %q (want daily|hourly|weekly)", cadence)
	}
	existing, _ := loadLoopRegistration(cfg, id) // preserve created_at on re-register
	reg := loopRegistration{
		LoopID: id, Enabled: true, Cadence: cadence,
		Command: command, ScheduleJob: strings.TrimSpace(scheduleJob),
		CreatedAt: existing.CreatedAt,
	}
	if err := saveLoopRegistration(cfg, reg, now); err != nil {
		return err
	}
	okf(stdout, "registered loop %q (cadence %s)", id, cadence)
	return nil
}

// loopList prints every registered loop with its current health.
func loopList(cfg Config, jsonOut bool, now time.Time, stdout io.Writer) error {
	regs := listLoopRegistrations(cfg)
	items := make([]loopHealth, 0, len(regs))
	for _, r := range regs {
		rec, ok := loadRunRecord(cfg, r.LoopID)
		items = append(items, classifyLoopHealth(r, rec, ok, now))
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

// cmdLoop dispatches `mora loop begin|done|status|register|list`. The id is the
// first positional arg (so the documented `loop begin <id> --json` form works
// regardless of flag ordering); list takes no id. Mirrors cmdSchedule/cmdPulse
// (flag.ContinueOnError + io.Discard). now is the real wall clock (the only
// place a fresh time.Now() is taken — every helper receives it injected).
func cmdLoop(ctx context.Context, args []string, stdout io.Writer) error {
	_ = ctx
	if len(args) == 0 {
		return errors.New("usage: mora loop begin|done|status|register|list <id> [flags]")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	now := time.Now()
	sub, rest := args[0], args[1:]

	newFS := func(name string) *flag.FlagSet {
		fs := flag.NewFlagSet("loop "+name, flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		return fs
	}

	switch sub {
	case "begin":
		if len(rest) == 0 {
			return errors.New("usage: mora loop begin <id> [--json]")
		}
		id, flagArgs := rest[0], rest[1:]
		fs := newFS("begin")
		jsonOut := fs.Bool("json", false, "json output")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		return loopBegin(cfg, id, *jsonOut, now, stdout)

	case "done":
		if len(rest) == 0 {
			return errors.New(`usage: mora loop done <id> (--ok | --fail "reason")`)
		}
		id, flagArgs := rest[0], rest[1:]
		fs := newFS("done")
		okFlag := fs.Bool("ok", false, "mark the run succeeded")
		failReason := fs.String("fail", "", "mark the run failed with a short reason")
		runID := fs.String("run", "", "the run id from `loop begin` (guards against closing a superseded run)")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		if !*okFlag && *failReason == "" {
			return errors.New(`mora loop done: pass --ok or --fail "reason"`)
		}
		if *okFlag && *failReason != "" {
			return errors.New("mora loop done: pass --ok OR --fail, not both")
		}
		return loopDone(cfg, id, *runID, *okFlag, *failReason, now, stdout)

	case "status":
		if len(rest) == 0 {
			return errors.New("usage: mora loop status <id> [--json]")
		}
		id, flagArgs := rest[0], rest[1:]
		fs := newFS("status")
		jsonOut := fs.Bool("json", false, "json output")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		return loopStatus(cfg, id, *jsonOut, now, stdout)

	case "register":
		if len(rest) == 0 {
			return errors.New("usage: mora loop register <id> [--cadence daily] [--command \"...\"] [--schedule-job <job>]")
		}
		id, flagArgs := rest[0], rest[1:]
		fs := newFS("register")
		cadence := fs.String("cadence", loopDefaultCadence, "daily|hourly|weekly")
		command := fs.String("command", "", "argv the trigger runs (whitespace-separated)")
		scheduleJob := fs.String("schedule-job", "", "backing launchd job (e.g. pulse-daily)")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		return loopRegister(cfg, id, *cadence, strings.Fields(*command), *scheduleJob, now, stdout)

	case "list":
		fs := newFS("list")
		jsonOut := fs.Bool("json", false, "json output")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return loopList(cfg, *jsonOut, now, stdout)

	default:
		return fmt.Errorf("unknown loop subcommand %q (want begin|done|status|register|list)", sub)
	}
}
