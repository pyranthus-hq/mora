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
	// AcknowledgedAt records an operator's explicit review of a terminal
	// owner_abandoned receipt (#498). It is never written by a writer, never
	// inferred from age, and never turns a failure into a success: State stays
	// Failed and FailureCode is preserved. Only the acknowledgement stops the
	// receipt from reddening health and lets bounded terminal retention age it
	// out. Optional, so records written before this field still load.
	AcknowledgedAt string `json:"acknowledged_at,omitempty"`
}

// Activity is the sanitized, read-only health projection.
type Activity struct {
	Kind           Kind   `json:"kind"`
	State          State  `json:"state"`
	RunID          string `json:"run_id"`
	StartedAt      string `json:"started_at,omitempty"`
	LastHeartbeat  string `json:"last_heartbeat,omitempty"`
	FinishedAt     string `json:"finished_at,omitempty"`
	Phase          string `json:"phase,omitempty"`
	Counts         Counts `json:"counts"`
	FailureCode    string `json:"failure_code,omitempty"`
	AcknowledgedAt string `json:"acknowledged_at,omitempty"`
}

// Acknowledged reports an operator's explicit review of a terminal
// owner_abandoned receipt. Only that failure code can carry an acknowledgement
// (classifyOperationRecord rejects any other), so an ordinary failure or a
// stalled run can never be silenced through this field.
func (a Activity) Acknowledged() bool {
	return a.State == Failed && a.FailureCode == FailureOwnerAbandoned && a.AcknowledgedAt != ""
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

// Begin starts an operation. Without an ingest coverage probe, abandoned ingest
// evidence is retained; index rebuild receipts need no journal coverage.
func Begin(cfg config.Config, kind Kind, phase string, now time.Time) (Handle, error) {
	return begin(cfg, kind, phase, now, nil)
}

// BeginIngest probes journal coverage under the receipt guard before cleanup.
// A nil probe, nil verdict, probe error, or any uncovered evidence retains all
// eligible receipts: journals identify only their first writer, not later runs.
func BeginIngest(cfg config.Config, phase string, now time.Time, probe func() (UncoveredRuns, error)) (Handle, error) {
	return begin(cfg, KindIngest, phase, now, probe)
}

func begin(cfg config.Config, kind Kind, phase string, now time.Time, probe func() (UncoveredRuns, error)) (Handle, error) {
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
		retain := kind == KindIngest
		if retain && probe != nil {
			uncovered, err := probe()
			retain = err != nil || uncovered == nil || len(uncovered) > 0
		}
		if err := pruneDeadOwnerRecordsLocked(cfg, kind, now, ProcessAlive, retain); err != nil {
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

// pruneDeadOwnerRecordsLocked recovers abandoned running receipts before a new
// writer starts. The caller must hold operationGuardPath(cfg, kind). It does not
// repair or reinterpret malformed records; those remain visible to Activities.
func pruneDeadOwnerRecordsLocked(cfg config.Config, kind Kind, now time.Time, live Liveness, retain bool) error {
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
		if err != nil || !abandonedDeadOwnerEligible(rec, kind, runID, now, live) {
			continue
		}
		if err := retireAbandonedRecordLocked(path, rec, now, retain); err != nil {
			return err
		}
	}
	return nil
}

// RetirementAction names how an abandoned running receipt was recovered.
const (
	RetirementRemoved         = "removed"
	RetirementFailedUncovered = "failed_uncovered"
	FailureOwnerAbandoned     = "owner_abandoned"
)

// Retirement is one abandoned-receipt recovery outcome. Removed receipts leave
// no completion claim; uncovered receipts become terminal failed so publication
// evidence survives instead of a fabricated success watermark.
type Retirement struct {
	RunID  string `json:"run_id"`
	Action string `json:"action"`
	Phase  string `json:"phase,omitempty"`
	Counts Counts `json:"counts"`
}

// UncoveredRuns names ingest run ids whose journals still have uncovered
// publication evidence. RetireAbandonedDeadOwners refuses to erase those
// receipts; it marks them failed so health stays honest.
type UncoveredRuns map[string]bool

func abandonedDeadOwnerEligible(rec Record, kind Kind, runID string, now time.Time, live Liveness) bool {
	if live == nil {
		live = ProcessAlive
	}
	if rec.State != Running || rec.OwnerPID <= 0 {
		return false
	}
	activity := classifyOperationRecord(rec, kind, runID, now, live)
	heartbeat, err := time.Parse(time.RFC3339Nano, rec.HeartbeatAt)
	if err != nil || activity.State != Stalled || now.Sub(heartbeat) <= HeartbeatTTL || live(rec.OwnerPID) {
		return false
	}
	return true
}

// AbandonedReceipt is a recovery candidate, not an executed retirement action.
type AbandonedReceipt struct {
	RunID  string `json:"run_id"`
	Phase  string `json:"phase,omitempty"`
	Counts Counts `json:"counts"`
}

// ListAbandonedDeadOwners is the read-only planning seam for Doctor dry-run.
// It never mutates receipts; live/slow owners, recent deaths, PID reuse, and
// malformed records are omitted (fail closed).
func ListAbandonedDeadOwners(cfg config.Config, kind Kind, now time.Time, live Liveness) ([]AbandonedReceipt, error) {
	if err := operationStateRootErr(cfg); err != nil {
		return nil, err
	}
	if !operationKindValid(kind) {
		return nil, fmt.Errorf("invalid operation kind %q", kind)
	}
	if live == nil {
		live = ProcessAlive
	}
	dir := filepath.Join(operationRoot(cfg), string(kind))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []AbandonedReceipt
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		runID := strings.TrimSuffix(entry.Name(), ".json")
		rec, err := LoadRecord(filepath.Join(dir, entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !abandonedDeadOwnerEligible(rec, kind, runID, now, live) {
			continue
		}
		out = append(out, AbandonedReceipt{RunID: runID, Phase: rec.Phase, Counts: rec.Counts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out, nil
}

// RetireAbandonedDeadOwners recovers abandoned running receipts while holding
// the per-kind guard. Requires BOTH an expired heartbeat and a confirmed-dead
// owner — the same fail-closed gate as Begin's sibling prune.
//
// UncoveredRuns retain failure/publication evidence: those receipts become
// terminal failed with FailureOwnerAbandoned and are never stamped completed.
// Receipts without uncovered journal evidence are removed (dead liveness only;
// no completion claim). Live/slow owners, recent deaths, PID reuse, and
// malformed records are left untouched. A non-nil planned map limits mutations
// to the named runs and expected dispositions; changed plans are left untouched.
func RetireAbandonedDeadOwners(cfg config.Config, kind Kind, now time.Time, live Liveness, uncovered UncoveredRuns, planned map[string]string) ([]Retirement, error) {
	if err := operationStateRootErr(cfg); err != nil {
		return nil, err
	}
	if !operationKindValid(kind) {
		return nil, fmt.Errorf("invalid operation kind %q", kind)
	}
	if live == nil {
		live = ProcessAlive
	}
	if uncovered == nil {
		uncovered = UncoveredRuns{}
	}
	var out []Retirement
	err := leasefile.WithGuard(operationGuardPath(cfg, kind), func() error {
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
			if err != nil || !abandonedDeadOwnerEligible(rec, kind, runID, now, live) {
				continue
			}
			action := RetirementRemoved
			if uncovered[runID] {
				action = RetirementFailedUncovered
			}
			if planned != nil && planned[runID] != action {
				continue
			}
			if err := retireAbandonedRecordLocked(path, rec, now, uncovered[runID]); err != nil {
				return err
			}
			out = append(out, Retirement{RunID: runID, Action: action, Phase: rec.Phase, Counts: rec.Counts})
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	if len(out) > 0 {
		PruneTerminal(cfg, kind)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out, nil
}

// retireAbandonedRecordLocked shares the guarded transition between Doctor and
// Begin. Preserve phase/counts; abandoning ownership never proves completion.
func retireAbandonedRecordLocked(path string, rec Record, now time.Time, retain bool) error {
	if retain {
		stamp := now.UTC().Format(time.RFC3339Nano)
		rec.State = Failed
		rec.HeartbeatAt = stamp
		rec.FinishedAt = stamp
		rec.FailureCode = FailureOwnerAbandoned
		return SaveRecord(path, rec)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// AcknowledgementRecorded / AcknowledgementAlready name how one reviewed
// receipt was handled. "already_acknowledged" keeps a repeated apply honest: it
// reports the original review stamp instead of claiming fresh work.
const (
	AcknowledgementRecorded = "acknowledged"
	AcknowledgementAlready  = "already_acknowledged"
)

// Acknowledgement is one operator-reviewed abandoned receipt. AcknowledgedAt is
// the stamp actually on disk, so a repeated apply reports the first review time.
type Acknowledgement struct {
	RunID          string `json:"run_id"`
	Action         string `json:"action"`
	Phase          string `json:"phase,omitempty"`
	Counts         Counts `json:"counts"`
	AcknowledgedAt string `json:"acknowledged_at"`
}

// acknowledgeableActivity reports whether a classified record is a terminal
// owner_abandoned receipt — the only shape an operator may acknowledge. Corrupt
// records classify with a different failure code, so they fail closed.
func acknowledgeableActivity(a Activity) bool {
	return a.State == Failed && a.FailureCode == FailureOwnerAbandoned && a.FinishedAt != ""
}

// ListUnacknowledgedAbandoned is the read-only planning seam for the Doctor
// acknowledge action (#498). It reports terminal owner_abandoned receipts the
// operator has not reviewed yet — the permanently-red state that no writer,
// rebuild, or age-based prune can ever clear. It never mutates a receipt.
func ListUnacknowledgedAbandoned(cfg config.Config, kind Kind, now time.Time) ([]AbandonedReceipt, error) {
	if err := operationStateRootErr(cfg); err != nil {
		return nil, err
	}
	if !operationKindValid(kind) {
		return nil, fmt.Errorf("invalid operation kind %q", kind)
	}
	dir := filepath.Join(operationRoot(cfg), string(kind))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []AbandonedReceipt
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		runID := strings.TrimSuffix(entry.Name(), ".json")
		rec, err := LoadRecord(filepath.Join(dir, entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			continue
		}
		a := classifyOperationRecord(rec, kind, runID, now, ProcessAlive)
		if !acknowledgeableActivity(a) || a.AcknowledgedAt != "" {
			continue
		}
		out = append(out, AbandonedReceipt{RunID: runID, Phase: rec.Phase, Counts: rec.Counts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out, nil
}

// AcknowledgeAbandoned records an operator's explicit review of the named
// terminal owner_abandoned receipts while holding the per-kind guard.
//
// It never invents completion: State stays Failed and FailureCode, phase, and
// counts are preserved untouched. The only change is the acknowledged_at stamp,
// which stops the receipt reddening health and returns it to bounded terminal
// retention. planned is required and names exact run ids — there is no "all"
// form and no age-based trigger, because the whole point of #498 is that this
// evidence only ever disappears behind a deliberate human decision.
//
// A receipt that is already acknowledged is reported, not re-stamped; a missing,
// non-terminal, differently-failed, or malformed receipt is left untouched and
// omitted, so a caller can verify the requested set against what was achieved.
func AcknowledgeAbandoned(cfg config.Config, kind Kind, now time.Time, planned []string) ([]Acknowledgement, error) {
	if err := operationStateRootErr(cfg); err != nil {
		return nil, err
	}
	if !operationKindValid(kind) {
		return nil, fmt.Errorf("invalid operation kind %q", kind)
	}
	if len(planned) == 0 {
		return nil, errors.New("acknowledging an abandoned receipt requires explicit run ids")
	}
	targets := map[string]bool{}
	for _, runID := range planned {
		if !validOperationToken(runID) {
			return nil, fmt.Errorf("invalid acknowledged run id %q", runID)
		}
		targets[runID] = true
	}
	ids := make([]string, 0, len(targets))
	for runID := range targets {
		ids = append(ids, runID)
	}
	sort.Strings(ids)
	stamp := now.UTC().Format(time.RFC3339Nano)
	var out []Acknowledgement
	err := leasefile.WithGuard(operationGuardPath(cfg, kind), func() error {
		for _, runID := range ids {
			path := Path(cfg, kind, runID)
			rec, err := LoadRecord(path)
			if errors.Is(err, os.ErrNotExist) {
				continue // already removed by bounded retention; nothing to review
			}
			if err != nil {
				continue // malformed evidence is never rewritten
			}
			if !acknowledgeableActivity(classifyOperationRecord(rec, kind, runID, now, ProcessAlive)) {
				continue
			}
			if rec.AcknowledgedAt != "" {
				out = append(out, Acknowledgement{RunID: runID, Action: AcknowledgementAlready,
					Phase: rec.Phase, Counts: rec.Counts, AcknowledgedAt: rec.AcknowledgedAt})
				continue
			}
			rec.AcknowledgedAt = stamp
			if err := SaveRecord(path, rec); err != nil {
				return err
			}
			out = append(out, Acknowledgement{RunID: runID, Action: AcknowledgementRecorded,
				Phase: rec.Phase, Counts: rec.Counts, AcknowledgedAt: stamp})
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	// Acknowledged receipts rejoin bounded terminal history, so this is the
	// terminal-writer transition that is allowed to prune.
	if len(out) > 0 {
		PruneTerminal(cfg, kind)
	}
	return out, nil
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
	// Retain every active/stalled/corrupt/uncovered record plus the newest valid
	// terminal record per kind. Older terminals remain on disk as bounded audit
	// evidence. Ordinary old failures yield to newer successes; uncovered
	// abandonment remains visible because a later run does not prove recovery —
	// until the operator explicitly acknowledges it, which returns the receipt to
	// ordinary bounded terminal history.
	latestTerminal := map[Kind]Activity{}
	var current []Activity
	for _, a := range out {
		if (a.FailureCode != FailureOwnerAbandoned || a.Acknowledged()) && (a.State == Failed || a.State == Completed) && a.FinishedAt != "" {
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
	// Only a terminal owner_abandoned receipt can be acknowledged. Anywhere else
	// the field is incoherent evidence, not a licence to green the check.
	if rec.AcknowledgedAt != "" && (rec.State != Failed || rec.FailureCode != FailureOwnerAbandoned) {
		return bad("incoherent_state")
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
		// A review cannot predate the failure it reviewed, nor come from the future.
		if rec.AcknowledgedAt != "" {
			acknowledged, aerr := time.Parse(time.RFC3339Nano, rec.AcknowledgedAt)
			if aerr != nil || acknowledged.Before(finished) || acknowledged.After(now.Add(time.Minute)) {
				return bad("invalid_timestamp")
			}
		}
	}
	a := Activity{
		Kind: rec.Kind, State: rec.State, RunID: rec.RunID,
		StartedAt: rec.StartedAt, LastHeartbeat: rec.HeartbeatAt, FinishedAt: rec.FinishedAt,
		Phase: sanitizeOperationPhase(rec.Phase), Counts: rec.Counts, FailureCode: rec.FailureCode,
		AcknowledgedAt: rec.AcknowledgedAt,
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
		// Uncovered abandonment is recovery evidence, not bounded completion
		// history. A later successful run must not age it out — only an explicit
		// operator acknowledgement returns it to bounded terminal retention.
		acknowledged := rec.AcknowledgedAt != "" && rec.State == Failed && rec.FailureCode == FailureOwnerAbandoned
		if err == nil && (rec.FailureCode != FailureOwnerAbandoned || acknowledged) && (rec.State == Failed || rec.State == Completed) {
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
