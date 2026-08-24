// Package operation owns durable state-directory work receipts and heartbeats.
package operation

import (
	"bytes"
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
	"sort"
	"strings"
	"sync"
	"time"
)

// operation_activity.go records bounded, content-free evidence about work that
// can legitimately keep the index dirty. It is deliberately separate from the
// loop subsystem: an ingest or rebuild has no cadence or period-idempotency
// semantics. Receipts live in StateDir, never in the vault or disposable index.

const (
	SchemaVersion = 1
	HeartbeatTTL  = 15 * time.Minute
	TerminalKeep  = 16
)

type Kind string

const (
	KindIngest       Kind = "ingest"
	KindIndexRebuild Kind = "index_rebuild"
)

type State string

const (
	Running   State = "running"
	Stalled   State = "stalled" // derived; never persisted
	Failed    State = "failed"
	Completed State = "completed"
)

// Counts is intentionally a closed vocabulary. Provider names,
// account labels, paths, memory ids, and source text have no place in a health
// receipt and cannot be smuggled in through arbitrary map keys.
type Counts struct {
	Items        int `json:"items,omitempty"`
	Files        int `json:"files,omitempty"`
	Errors       int `json:"errors,omitempty"`
	Examined     int `json:"examined,omitempty"`
	Materialized int `json:"materialized,omitempty"`
	Missing      int `json:"missing,omitempty"`
}

func validCounts(counts Counts) bool {
	return counts.Items >= 0 && counts.Files >= 0 && counts.Errors >= 0 &&
		counts.Examined >= 0 && counts.Materialized >= 0 && counts.Missing >= 0
}

// Record is the durable writer-owned shape. OwnerPID is used only as
// liveness corroboration and is never exposed in health JSON.
type Record struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          Kind   `json:"kind"`
	State         State  `json:"state"`
	RunID         string `json:"run_id"`
	OwnerPID      int    `json:"owner_pid"`
	StartedAt     string `json:"started_at"`
	HeartbeatAt   string `json:"heartbeat_at"`
	FinishedAt    string `json:"finished_at,omitempty"`
	Phase         string `json:"phase"`
	Counts        Counts `json:"counts"`
	FailureCode   string `json:"failure_code,omitempty"`
}

// Activity is the sanitized, read-only health projection.
type Activity struct {
	Kind          Kind   `json:"kind"`
	State         State  `json:"state"`
	RunID         string `json:"run_id"`
	StartedAt     string `json:"started_at,omitempty"`
	LastHeartbeat string `json:"last_heartbeat,omitempty"`
	FinishedAt    string `json:"finished_at,omitempty"`
	Phase         string `json:"phase,omitempty"`
	Counts        Counts `json:"counts"`
	FailureCode   string `json:"failure_code,omitempty"`
}

type Handle struct {
	Kind  Kind
	RunID string
	PID   int
}

type Liveness func(pid int) bool

var ProcessAlive Liveness = processAlive

type operationHeartbeatTicker struct {
	C    <-chan time.Time
	stop func()
}

var newOperationHeartbeatTicker = func(d time.Duration) operationHeartbeatTicker {
	ticker := time.NewTicker(d)
	return operationHeartbeatTicker{C: ticker.C, stop: ticker.Stop}
}

var activeOperationProgress sync.Map // run id -> *Progress

func operationRoot(cfg config.Config) string { return filepath.Join(cfg.StateDir, "operations") }

func operationKindValid(kind Kind) bool {
	return kind == KindIngest || kind == KindIndexRebuild
}

func Path(cfg config.Config, kind Kind, runID string) string {
	return filepath.Join(operationRoot(cfg), string(kind), runID+".json")
}

// One stable guard per kind keeps the guard domain bounded while still
// serializing every receipt transition for concurrent runs of that kind.
func operationGuardPath(cfg config.Config, kind Kind) string {
	return filepath.Join(operationRoot(cfg), string(kind), ".activity.lock")
}

func operationStateRootErr(cfg config.Config) error {
	if cfg.StateDir == "" || !filepath.IsAbs(cfg.StateDir) {
		return fmt.Errorf("operation receipt requires an absolute state_dir, got %q", cfg.StateDir)
	}
	return nil
}

func validOperationToken(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func sanitizeOperationPhase(phase string) string {
	phase = strings.TrimSpace(phase)
	if !validOperationToken(phase) {
		return "unknown"
	}
	return phase
}

func newOperationRunID(now time.Time) string {
	var suffix [4]byte
	_, _ = rand.Read(suffix[:])
	return "op_" + now.UTC().Format("20060102_150405") + "_" + hex.EncodeToString(suffix[:])
}

func Begin(cfg config.Config, kind Kind, phase string, now time.Time) (Handle, error) {
	if err := operationStateRootErr(cfg); err != nil {
		return Handle{}, err
	}
	if !operationKindValid(kind) {
		return Handle{}, fmt.Errorf("invalid operation kind %q", kind)
	}
	phase = strings.TrimSpace(phase)
	if !validOperationToken(phase) {
		return Handle{}, fmt.Errorf("invalid operation phase %q", phase)
	}
	runID := newOperationRunID(now)
	h := Handle{Kind: kind, RunID: runID, PID: os.Getpid()}
	stamp := now.UTC().Format(time.RFC3339Nano)
	rec := Record{
		SchemaVersion: SchemaVersion,
		Kind:          kind, State: Running, RunID: runID, OwnerPID: h.PID,
		StartedAt: stamp, HeartbeatAt: stamp, Phase: phase,
	}
	path := Path(cfg, kind, runID)
	if err := leasefile.WithGuard(operationGuardPath(cfg, kind), func() error {
		// A terminal writer normally prunes its own receipt, but a crashed writer
		// never reaches that path. The next writer of the same kind owns cleanup:
		// it already holds the per-kind guard, so it cannot race another receipt
		// transition. Require BOTH an expired heartbeat and a dead owner; a slow
		// live writer, PID reuse, and corrupt evidence all fail closed.
		if err := pruneDeadOwnerRecordsLocked(cfg, kind, now, ProcessAlive); err != nil {
			return err
		}
		if _, err := os.Stat(path); err == nil {
			return errors.New("operation run id collision")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return SaveRecord(path, rec)
	}); err != nil {
		return Handle{}, err
	}
	return h, nil
}

// pruneDeadOwnerRecordsLocked removes abandoned running receipts before a new
// writer starts. The caller must hold operationGuardPath(cfg, kind). It does not
// repair or reinterpret malformed records; those remain visible to Activities.
func pruneDeadOwnerRecordsLocked(cfg config.Config, kind Kind, now time.Time, live Liveness) error {
	if live == nil {
		live = ProcessAlive
	}
	dir := filepath.Join(operationRoot(cfg), string(kind))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		runID := strings.TrimSuffix(entry.Name(), ".json")
		rec, err := LoadRecord(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || rec.State != Running || rec.OwnerPID <= 0 {
			continue
		}
		activity := classifyOperationRecord(rec, kind, runID, now, live)
		heartbeat, err := time.Parse(time.RFC3339Nano, rec.HeartbeatAt)
		if err != nil || activity.State != Stalled || now.Sub(heartbeat) <= HeartbeatTTL || live(rec.OwnerPID) {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func Heartbeat(cfg config.Config, h Handle, phase string, counts Counts, now time.Time) error {
	phase = strings.TrimSpace(phase)
	if !validOperationToken(phase) {
		return fmt.Errorf("invalid operation phase %q", phase)
	}
	if !validCounts(counts) {
		return errors.New("operation counts cannot be negative")
	}
	return mutateOperation(cfg, h, func(rec *Record) error {
		if rec.State != Running {
			return fmt.Errorf("operation %s is %s, not running", h.RunID, rec.State)
		}
		previous, err := time.Parse(time.RFC3339Nano, rec.HeartbeatAt)
		if err != nil {
			return errors.New("operation has invalid heartbeat")
		}
		if now.Before(previous) {
			return errors.New("operation heartbeat cannot move backward")
		}
		rec.HeartbeatAt = now.UTC().Format(time.RFC3339Nano)
		rec.Phase = phase
		rec.Counts = counts
		return nil
	})
}

func Finish(cfg config.Config, h Handle, state State, phase string, counts Counts, failureCode string, now time.Time) error {
	if state != Failed && state != Completed {
		return fmt.Errorf("invalid terminal operation state %q", state)
	}
	phase = strings.TrimSpace(phase)
	if !validOperationToken(phase) {
		return fmt.Errorf("invalid operation phase %q", phase)
	}
	if !validCounts(counts) {
		return errors.New("operation counts cannot be negative")
	}
	if state == Completed && failureCode != "" {
		return errors.New("completed operation cannot carry a failure code")
	}
	if state == Failed && failureCode == "" {
		failureCode = "operation_failed"
	}
	if failureCode != "" && !validOperationToken(failureCode) {
		failureCode = "operation_failed"
	}
	err := mutateOperation(cfg, h, func(rec *Record) error {
		if rec.State != Running {
			return fmt.Errorf("operation %s is %s, not running", h.RunID, rec.State)
		}
		previous, err := time.Parse(time.RFC3339Nano, rec.HeartbeatAt)
		if err != nil {
			return errors.New("operation has invalid heartbeat")
		}
		if now.Before(previous) {
			return errors.New("operation heartbeat cannot move backward")
		}
		stamp := now.UTC().Format(time.RFC3339Nano)
		rec.State, rec.HeartbeatAt, rec.FinishedAt = state, stamp, stamp
		rec.Phase, rec.Counts, rec.FailureCode = phase, counts, failureCode
		return nil
	})
	if err == nil {
		PruneTerminal(cfg, h.Kind)
	}
	return err
}

func mutateOperation(cfg config.Config, h Handle, fn func(*Record) error) error {
	if err := operationStateRootErr(cfg); err != nil {
		return err
	}
	if !operationKindValid(h.Kind) || !validOperationToken(h.RunID) || h.PID <= 0 {
		return errors.New("invalid operation handle")
	}
	path := Path(cfg, h.Kind, h.RunID)
	return leasefile.WithGuard(operationGuardPath(cfg, h.Kind), func() error {
		rec, err := LoadRecord(path)
		if err != nil {
			return err
		}
		// The run id, kind, and pid form the owner fence. PID liveness alone never
		// grants mutation authority, which bounds the PID-reuse hazard.
		if rec.RunID != h.RunID || rec.Kind != h.Kind || rec.OwnerPID != h.PID {
			return errors.New("operation ownership changed")
		}
		if err := fn(&rec); err != nil {
			return err
		}
		return SaveRecord(path, rec)
	})
}

func SaveRecord(path string, rec Record) error {
	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return atomicio.WriteDurable(path, body, 0o600)
}

func LoadRecord(path string) (Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var rec Record
	if err := dec.Decode(&rec); err != nil {
		return Record{}, err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Record{}, errors.New("operation receipt has trailing JSON value")
		}
		return Record{}, fmt.Errorf("operation receipt trailing data: %w", err)
	}
	return rec, nil
}

// Activities is read-only: it never repairs, reaps, rewrites, or
// deletes a marker. now and live are injected for deterministic non-macOS tests.
func Activities(cfg config.Config, now time.Time, live Liveness) []Activity {
	if err := operationStateRootErr(cfg); err != nil {
		return []Activity{invalidOperationActivity(KindIndexRebuild, "ledger_unreadable")}
	}
	if live == nil {
		live = ProcessAlive
	}
	var out []Activity
	for _, kind := range []Kind{KindIngest, KindIndexRebuild} {
		dir := filepath.Join(operationRoot(cfg), string(kind))
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			out = append(out, invalidOperationActivity(kind, "ledger_unreadable"))
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			pathRunID := strings.TrimSuffix(e.Name(), ".json")
			rec, rerr := LoadRecord(filepath.Join(dir, e.Name()))
			if errors.Is(rerr, os.ErrNotExist) {
				continue // a terminal writer pruned it after ReadDir; reads never reap
			}
			if rerr != nil {
				out = append(out, invalidOperationActivityWithRun(kind, pathRunID, "receipt_invalid"))
				continue
			}
			out = append(out, classifyOperationRecord(rec, kind, pathRunID, now, live))
		}
	}
	// Retain every active/stalled/corrupt record plus only the newest valid
	// terminal record per kind. Older terminals remain on disk as bounded audit
	// evidence, but an old failure must not keep health red after a newer success.
	latestTerminal := map[Kind]Activity{}
	var current []Activity
	for _, a := range out {
		if (a.State == Failed || a.State == Completed) && a.FinishedAt != "" {
			prev, ok := latestTerminal[a.Kind]
			if !ok || a.FinishedAt > prev.FinishedAt || (a.FinishedAt == prev.FinishedAt && a.RunID > prev.RunID) {
				latestTerminal[a.Kind] = a
			}
			continue
		}
		current = append(current, a)
	}
	for _, a := range latestTerminal {
		current = append(current, a)
	}
	out = current
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt != out[j].StartedAt {
			return out[i].StartedAt < out[j].StartedAt
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].RunID < out[j].RunID
	})
	if out == nil {
		out = []Activity{}
	}
	return out
}

func classifyOperationRecord(rec Record, pathKind Kind, pathRunID string, now time.Time, live Liveness) Activity {
	bad := func(code string) Activity { return invalidOperationActivityWithRun(pathKind, pathRunID, code) }
	if rec.SchemaVersion != SchemaVersion {
		return bad("unsupported_schema")
	}
	if rec.Kind != pathKind || rec.RunID != pathRunID || !operationKindValid(rec.Kind) || !validOperationToken(rec.RunID) {
		return bad("identity_mismatch")
	}
	started, serr := time.Parse(time.RFC3339Nano, rec.StartedAt)
	heartbeat, herr := time.Parse(time.RFC3339Nano, rec.HeartbeatAt)
	if serr != nil || herr != nil || heartbeat.Before(started) || heartbeat.After(now.Add(time.Minute)) {
		return bad("invalid_timestamp")
	}
	if rec.State != Running && rec.State != Failed && rec.State != Completed {
		return bad("invalid_state")
	}
	if !validOperationToken(rec.Phase) {
		return bad("invalid_phase")
	}
	switch rec.State {
	case Running:
		if rec.FinishedAt != "" || rec.FailureCode != "" {
			return bad("incoherent_state")
		}

	case Failed:
		if rec.FinishedAt == "" || rec.FailureCode == "" {
			return bad("incoherent_state")
		}
	case Completed:
		if rec.FinishedAt == "" || rec.FailureCode != "" {
			return bad("incoherent_state")
		}
	}
	if rec.Counts.Items < 0 || rec.Counts.Files < 0 || rec.Counts.Errors < 0 {
		return bad("invalid_counts")
	}
	if rec.FailureCode != "" && !validOperationToken(rec.FailureCode) {
		return bad("invalid_failure_code")
	}
	if rec.State != Running {
		finished, ferr := time.Parse(time.RFC3339Nano, rec.FinishedAt)
		if ferr != nil || finished.Before(started) || finished.Before(heartbeat) || finished.After(now.Add(time.Minute)) {
			return bad("invalid_timestamp")
		}
	}
	a := Activity{
		Kind: rec.Kind, State: rec.State, RunID: rec.RunID,
		StartedAt: rec.StartedAt, LastHeartbeat: rec.HeartbeatAt, FinishedAt: rec.FinishedAt,
		Phase: sanitizeOperationPhase(rec.Phase), Counts: rec.Counts, FailureCode: rec.FailureCode,
	}
	if rec.State == Running {
		switch {
		case now.Sub(heartbeat) > HeartbeatTTL:
			a.State, a.FailureCode = Stalled, "heartbeat_expired"
		case rec.OwnerPID <= 0 || !live(rec.OwnerPID):
			a.State, a.FailureCode = Stalled, "owner_dead"
		}
	}
	return a
}

func invalidOperationActivity(kind Kind, code string) Activity {
	return invalidOperationActivityWithRun(kind, "unknown", code)
}

func invalidOperationActivityWithRun(kind Kind, runID, code string) Activity {
	if !validOperationToken(runID) {
		runID = "unknown"
	}
	return Activity{Kind: kind, State: Failed, RunID: runID, Phase: "unknown", FailureCode: code}
}

// Progress keeps a running receipt live during provider/network and
// embedding work that can exceed the TTL. Updates and the ticker serialize on
// mu, so a periodic heartbeat can never overwrite a newer phase/count snapshot.
type Progress struct {
	cfg    config.Config
	handle Handle
	clock  func() time.Time

	mu     sync.Mutex
	phase  string
	counts Counts
	err    error
	stopCh chan struct{}
	doneCh chan struct{}
	once   sync.Once
}

func StartProgress(cfg config.Config, h Handle, phase string, clock func() time.Time) *Progress {
	if clock == nil {
		clock = time.Now
	}
	p := &Progress{cfg: cfg, handle: h, clock: clock, phase: phase, stopCh: make(chan struct{}), doneCh: make(chan struct{})}
	activeOperationProgress.Store(h.RunID, p)
	go func() {
		defer close(p.doneCh)
		defer activeOperationProgress.CompareAndDelete(h.RunID, p)
		ticker := newOperationHeartbeatTicker(HeartbeatTTL / 3)
		defer ticker.stop()
		for {
			select {
			case <-ticker.C:
				p.mu.Lock()
				if p.err == nil {
					p.err = Heartbeat(p.cfg, p.handle, p.phase, p.counts, p.clock())
				}
				failed := p.err != nil
				p.mu.Unlock()
				if failed {
					return
				}
			case <-p.stopCh:
				return
			}
		}
	}()
	return p
}

func (p *Progress) Update(phase string, counts Counts) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	if err := Heartbeat(p.cfg, p.handle, phase, counts, p.clock()); err != nil {
		p.err = err
		return err
	}
	p.phase, p.counts = phase, counts
	return nil
}

func (p *Progress) Stop() error {
	p.once.Do(func() { close(p.stopCh) })
	<-p.doneCh
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// CompleteAfterCoverage is the narrow cross-process completion seam:
// a committed rebuild may close an ingest run only after its journal was
// actually retired. The retired journal's run id is the authority; no PID-only
// takeover is permitted. Missing records are legacy journal headers and benign.
func CompleteAfterCoverage(cfg config.Config, runID string, now time.Time) error {
	if !validOperationToken(runID) {
		return errors.New("invalid covered operation run id")
	}
	path := Path(cfg, KindIngest, runID)
	err := leasefile.WithGuard(operationGuardPath(cfg, KindIngest), func() error {
		rec, err := LoadRecord(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if rec.Kind != KindIngest || rec.RunID != runID {
			return errors.New("covered operation identity mismatch")
		}
		if rec.State == Completed || rec.State == Failed {
			return nil
		}
		if rec.State != Running {
			return fmt.Errorf("covered operation has invalid state %q", rec.State)
		}
		previous, err := time.Parse(time.RFC3339Nano, rec.HeartbeatAt)
		if err != nil || now.Before(previous) {
			return errors.New("covered operation has invalid heartbeat")
		}
		stamp := now.UTC().Format(time.RFC3339Nano)
		rec.State = Completed
		rec.HeartbeatAt = stamp
		rec.FinishedAt = stamp
		rec.Phase = "journal_retired"
		rec.FailureCode = ""
		return SaveRecord(path, rec)
	})
	if err == nil {
		if tracked, ok := activeOperationProgress.Load(runID); ok {
			_ = tracked.(*Progress).Stop()
		}
		PruneTerminal(cfg, KindIngest)
	}
	return err
}

// PruneTerminal bounds retained completion evidence. It runs only
// after a writer publishes a terminal transition; health/status reads stay pure.
func PruneTerminal(cfg config.Config, kind Kind) {
	dir := filepath.Join(operationRoot(cfg), string(kind))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type terminal struct{ path, finished string }
	var terms []terminal
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		rec, err := LoadRecord(path)
		if err == nil && (rec.State == Failed || rec.State == Completed) {
			terms = append(terms, terminal{path: path, finished: rec.FinishedAt})
		}
	}
	sort.Slice(terms, func(i, j int) bool {
		if terms[i].finished != terms[j].finished {
			return terms[i].finished > terms[j].finished
		}
		return terms[i].path > terms[j].path
	})
	if len(terms) <= TerminalKeep {
		return
	}
	for _, old := range terms[TerminalKeep:] {
		_ = os.Remove(old.path)
	}
}

func Active(runID string) bool { _, ok := activeOperationProgress.Load(runID); return ok }

func Root(cfg config.Config) string { return operationRoot(cfg) }
func ValidToken(s string) bool      { return validOperationToken(s) }
func SanitizePhase(s string) string { return sanitizeOperationPhase(s) }
