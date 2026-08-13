package mora

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
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
	operationSchemaVersion = 1
	operationHeartbeatTTL  = 15 * time.Minute
	operationTerminalKeep  = 16
)

type operationKind string

const (
	operationKindIngest       operationKind = "ingest"
	operationKindIndexRebuild operationKind = "index_rebuild"
)

type operationState string

const (
	operationRunning   operationState = "running"
	operationStalled   operationState = "stalled" // derived; never persisted
	operationFailed    operationState = "failed"
	operationCompleted operationState = "completed"
)

// operationCounts is intentionally a closed vocabulary. Provider names,
// account labels, paths, memory ids, and source text have no place in a health
// receipt and cannot be smuggled in through arbitrary map keys.
type operationCounts struct {
	Items  int `json:"items,omitempty"`
	Files  int `json:"files,omitempty"`
	Errors int `json:"errors,omitempty"`
}

// operationRecord is the durable writer-owned shape. OwnerPID is used only as
// liveness corroboration and is never exposed in health JSON.
type operationRecord struct {
	SchemaVersion int             `json:"schema_version"`
	Kind          operationKind   `json:"kind"`
	State         operationState  `json:"state"`
	RunID         string          `json:"run_id"`
	OwnerPID      int             `json:"owner_pid"`
	StartedAt     string          `json:"started_at"`
	HeartbeatAt   string          `json:"heartbeat_at"`
	FinishedAt    string          `json:"finished_at,omitempty"`
	Phase         string          `json:"phase"`
	Counts        operationCounts `json:"counts"`
	FailureCode   string          `json:"failure_code,omitempty"`
}

// operationActivity is the sanitized, read-only health projection.
type operationActivity struct {
	Kind          operationKind   `json:"kind"`
	State         operationState  `json:"state"`
	RunID         string          `json:"run_id"`
	StartedAt     string          `json:"started_at,omitempty"`
	LastHeartbeat string          `json:"last_heartbeat,omitempty"`
	FinishedAt    string          `json:"finished_at,omitempty"`
	Phase         string          `json:"phase,omitempty"`
	Counts        operationCounts `json:"counts"`
	FailureCode   string          `json:"failure_code,omitempty"`
}

type operationHandle struct {
	kind  operationKind
	runID string
	pid   int
}

type operationLiveness func(pid int) bool

var operationProcessAlive operationLiveness = processAlive
var operationClock = time.Now

type operationHeartbeatTicker struct {
	C    <-chan time.Time
	stop func()
}

var newOperationHeartbeatTicker = func(d time.Duration) operationHeartbeatTicker {
	ticker := time.NewTicker(d)
	return operationHeartbeatTicker{C: ticker.C, stop: ticker.Stop}
}

var activeOperationProgress sync.Map // run id -> *operationProgress

func operationRoot(cfg Config) string { return filepath.Join(cfg.StateDir, "operations") }

func operationKindValid(kind operationKind) bool {
	return kind == operationKindIngest || kind == operationKindIndexRebuild
}

func operationPath(cfg Config, kind operationKind, runID string) string {
	return filepath.Join(operationRoot(cfg), string(kind), runID+".json")
}

// One stable guard per kind keeps the guard domain bounded while still
// serializing every receipt transition for concurrent runs of that kind.
func operationGuardPath(cfg Config, kind operationKind) string {
	return filepath.Join(operationRoot(cfg), string(kind), ".activity.lock")
}

func operationStateRootErr(cfg Config) error {
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

func beginOperation(cfg Config, kind operationKind, phase string, now time.Time) (operationHandle, error) {
	if err := operationStateRootErr(cfg); err != nil {
		return operationHandle{}, err
	}
	if !operationKindValid(kind) {
		return operationHandle{}, fmt.Errorf("invalid operation kind %q", kind)
	}
	phase = strings.TrimSpace(phase)
	if !validOperationToken(phase) {
		return operationHandle{}, fmt.Errorf("invalid operation phase %q", phase)
	}
	runID := newOperationRunID(now)
	h := operationHandle{kind: kind, runID: runID, pid: os.Getpid()}
	stamp := now.UTC().Format(time.RFC3339Nano)
	rec := operationRecord{
		SchemaVersion: operationSchemaVersion,
		Kind:          kind, State: operationRunning, RunID: runID, OwnerPID: h.pid,
		StartedAt: stamp, HeartbeatAt: stamp, Phase: phase,
	}
	path := operationPath(cfg, kind, runID)
	if err := withLeaseFileGuard(operationGuardPath(cfg, kind), func() error {
		if _, err := os.Stat(path); err == nil {
			return errors.New("operation run id collision")
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return saveOperationRecord(path, rec)
	}); err != nil {
		return operationHandle{}, err
	}
	return h, nil
}

func heartbeatOperation(cfg Config, h operationHandle, phase string, counts operationCounts, now time.Time) error {
	phase = strings.TrimSpace(phase)
	if !validOperationToken(phase) {
		return fmt.Errorf("invalid operation phase %q", phase)
	}
	if counts.Items < 0 || counts.Files < 0 || counts.Errors < 0 {
		return errors.New("operation counts cannot be negative")
	}
	return mutateOperation(cfg, h, func(rec *operationRecord) error {
		if rec.State != operationRunning {
			return fmt.Errorf("operation %s is %s, not running", h.runID, rec.State)
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

func finishOperation(cfg Config, h operationHandle, state operationState, phase string, counts operationCounts, failureCode string, now time.Time) error {
	if state != operationFailed && state != operationCompleted {
		return fmt.Errorf("invalid terminal operation state %q", state)
	}
	phase = strings.TrimSpace(phase)
	if !validOperationToken(phase) {
		return fmt.Errorf("invalid operation phase %q", phase)
	}
	if counts.Items < 0 || counts.Files < 0 || counts.Errors < 0 {
		return errors.New("operation counts cannot be negative")
	}
	if state == operationCompleted && failureCode != "" {
		return errors.New("completed operation cannot carry a failure code")
	}
	if state == operationFailed && failureCode == "" {
		failureCode = "operation_failed"
	}
	if failureCode != "" && !validOperationToken(failureCode) {
		failureCode = "operation_failed"
	}
	err := mutateOperation(cfg, h, func(rec *operationRecord) error {
		if rec.State != operationRunning {
			return fmt.Errorf("operation %s is %s, not running", h.runID, rec.State)
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
		pruneTerminalOperations(cfg, h.kind)
	}
	return err
}

func mutateOperation(cfg Config, h operationHandle, fn func(*operationRecord) error) error {
	if err := operationStateRootErr(cfg); err != nil {
		return err
	}
	if !operationKindValid(h.kind) || !validOperationToken(h.runID) || h.pid <= 0 {
		return errors.New("invalid operation handle")
	}
	path := operationPath(cfg, h.kind, h.runID)
	return withLeaseFileGuard(operationGuardPath(cfg, h.kind), func() error {
		rec, err := loadOperationRecord(path)
		if err != nil {
			return err
		}
		// The run id, kind, and pid form the owner fence. PID liveness alone never
		// grants mutation authority, which bounds the PID-reuse hazard.
		if rec.RunID != h.runID || rec.Kind != h.kind || rec.OwnerPID != h.pid {
			return errors.New("operation ownership changed")
		}
		if err := fn(&rec); err != nil {
			return err
		}
		return saveOperationRecord(path, rec)
	})
}

func saveOperationRecord(path string, rec operationRecord) error {
	body, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return atomicio.WriteDurable(path, body, 0o600)
}

func loadOperationRecord(path string) (operationRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return operationRecord{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var rec operationRecord
	if err := dec.Decode(&rec); err != nil {
		return operationRecord{}, err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return operationRecord{}, errors.New("operation receipt has trailing JSON value")
		}
		return operationRecord{}, fmt.Errorf("operation receipt trailing data: %w", err)
	}
	return rec, nil
}

// operationActivities is read-only: it never repairs, reaps, rewrites, or
// deletes a marker. now and live are injected for deterministic non-macOS tests.
func operationActivities(cfg Config, now time.Time, live operationLiveness) []operationActivity {
	if err := operationStateRootErr(cfg); err != nil {
		return []operationActivity{invalidOperationActivity(operationKindIndexRebuild, "ledger_unreadable")}
	}
	if live == nil {
		live = operationProcessAlive
	}
	var out []operationActivity
	for _, kind := range []operationKind{operationKindIngest, operationKindIndexRebuild} {
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
			rec, rerr := loadOperationRecord(filepath.Join(dir, e.Name()))
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
	latestTerminal := map[operationKind]operationActivity{}
	var current []operationActivity
	for _, a := range out {
		if (a.State == operationFailed || a.State == operationCompleted) && a.FinishedAt != "" {
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
		out = []operationActivity{}
	}
	return out
}

func classifyOperationRecord(rec operationRecord, pathKind operationKind, pathRunID string, now time.Time, live operationLiveness) operationActivity {
	bad := func(code string) operationActivity { return invalidOperationActivityWithRun(pathKind, pathRunID, code) }
	if rec.SchemaVersion != operationSchemaVersion {
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
	if rec.State != operationRunning && rec.State != operationFailed && rec.State != operationCompleted {
		return bad("invalid_state")
	}
	if !validOperationToken(rec.Phase) {
		return bad("invalid_phase")
	}
	switch rec.State {
	case operationRunning:
		if rec.FinishedAt != "" || rec.FailureCode != "" {
			return bad("incoherent_state")
		}

	case operationFailed:
		if rec.FinishedAt == "" || rec.FailureCode == "" {
			return bad("incoherent_state")
		}
	case operationCompleted:
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
	if rec.State != operationRunning {
		finished, ferr := time.Parse(time.RFC3339Nano, rec.FinishedAt)
		if ferr != nil || finished.Before(started) || finished.Before(heartbeat) || finished.After(now.Add(time.Minute)) {
			return bad("invalid_timestamp")
		}
	}
	a := operationActivity{
		Kind: rec.Kind, State: rec.State, RunID: rec.RunID,
		StartedAt: rec.StartedAt, LastHeartbeat: rec.HeartbeatAt, FinishedAt: rec.FinishedAt,
		Phase: sanitizeOperationPhase(rec.Phase), Counts: rec.Counts, FailureCode: rec.FailureCode,
	}
	if rec.State == operationRunning {
		switch {
		case now.Sub(heartbeat) > operationHeartbeatTTL:
			a.State, a.FailureCode = operationStalled, "heartbeat_expired"
		case rec.OwnerPID <= 0 || !live(rec.OwnerPID):
			a.State, a.FailureCode = operationStalled, "owner_dead"
		}
	}
	return a
}

func invalidOperationActivity(kind operationKind, code string) operationActivity {
	return invalidOperationActivityWithRun(kind, "unknown", code)
}

func invalidOperationActivityWithRun(kind operationKind, runID, code string) operationActivity {
	if !validOperationToken(runID) {
		runID = "unknown"
	}
	return operationActivity{Kind: kind, State: operationFailed, RunID: runID, Phase: "unknown", FailureCode: code}
}

// operationProgress keeps a running receipt live during provider/network and
// embedding work that can exceed the TTL. Updates and the ticker serialize on
// mu, so a periodic heartbeat can never overwrite a newer phase/count snapshot.
type operationProgress struct {
	cfg    Config
	handle operationHandle
	clock  func() time.Time

	mu     sync.Mutex
	phase  string
	counts operationCounts
	err    error
	stopCh chan struct{}
	doneCh chan struct{}
	once   sync.Once
}

func startOperationProgress(cfg Config, h operationHandle, phase string) *operationProgress {
	p := &operationProgress{cfg: cfg, handle: h, clock: operationClock, phase: phase, stopCh: make(chan struct{}), doneCh: make(chan struct{})}
	activeOperationProgress.Store(h.runID, p)
	go func() {
		defer close(p.doneCh)
		defer activeOperationProgress.CompareAndDelete(h.runID, p)
		ticker := newOperationHeartbeatTicker(operationHeartbeatTTL / 3)
		defer ticker.stop()
		for {
			select {
			case <-ticker.C:
				p.mu.Lock()
				if p.err == nil {
					p.err = heartbeatOperation(p.cfg, p.handle, p.phase, p.counts, p.clock())
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

func (p *operationProgress) update(phase string, counts operationCounts) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	if err := heartbeatOperation(p.cfg, p.handle, phase, counts, p.clock()); err != nil {
		p.err = err
		return err
	}
	p.phase, p.counts = phase, counts
	return nil
}

func (p *operationProgress) stop() error {
	p.once.Do(func() { close(p.stopCh) })
	<-p.doneCh
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// completeOperationAfterCoverage is the narrow cross-process completion seam:
// a committed rebuild may close an ingest run only after its journal was
// actually retired. The retired journal's run id is the authority; no PID-only
// takeover is permitted. Missing records are legacy journal headers and benign.
func completeOperationAfterCoverage(cfg Config, runID string, now time.Time) error {
	if !validOperationToken(runID) {
		return errors.New("invalid covered operation run id")
	}
	path := operationPath(cfg, operationKindIngest, runID)
	err := withLeaseFileGuard(operationGuardPath(cfg, operationKindIngest), func() error {
		rec, err := loadOperationRecord(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if rec.Kind != operationKindIngest || rec.RunID != runID {
			return errors.New("covered operation identity mismatch")
		}
		if rec.State == operationCompleted || rec.State == operationFailed {
			return nil
		}
		if rec.State != operationRunning {
			return fmt.Errorf("covered operation has invalid state %q", rec.State)
		}
		previous, err := time.Parse(time.RFC3339Nano, rec.HeartbeatAt)
		if err != nil || now.Before(previous) {
			return errors.New("covered operation has invalid heartbeat")
		}
		stamp := now.UTC().Format(time.RFC3339Nano)
		rec.State = operationCompleted
		rec.HeartbeatAt = stamp
		rec.FinishedAt = stamp
		rec.Phase = "journal_retired"
		rec.FailureCode = ""
		return saveOperationRecord(path, rec)
	})
	if err == nil {
		if tracked, ok := activeOperationProgress.Load(runID); ok {
			_ = tracked.(*operationProgress).stop()
		}
		pruneTerminalOperations(cfg, operationKindIngest)
	}
	return err
}

// pruneTerminalOperations bounds retained completion evidence. It runs only
// after a writer publishes a terminal transition; health/status reads stay pure.
func pruneTerminalOperations(cfg Config, kind operationKind) {
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
		rec, err := loadOperationRecord(path)
		if err == nil && (rec.State == operationFailed || rec.State == operationCompleted) {
			terms = append(terms, terminal{path: path, finished: rec.FinishedAt})
		}
	}
	sort.Slice(terms, func(i, j int) bool {
		if terms[i].finished != terms[j].finished {
			return terms[i].finished > terms[j].finished
		}
		return terms[i].path > terms[j].path
	})
	if len(terms) <= operationTerminalKeep {
		return
	}
	for _, old := range terms[operationTerminalKeep:] {
		_ = os.Remove(old.path)
	}
}
