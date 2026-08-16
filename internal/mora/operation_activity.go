package mora

import (
	"github.com/pyranthus-hq/mora/internal/operation"
	"time"
)

const (
	operationSchemaVersion = operation.SchemaVersion
	operationHeartbeatTTL  = operation.HeartbeatTTL
	operationTerminalKeep  = operation.TerminalKeep
)

type operationKind = operation.Kind

const (
	operationKindIngest       = operation.KindIngest
	operationKindIndexRebuild = operation.KindIndexRebuild
)

type operationState = operation.State

const (
	operationRunning   = operation.Running
	operationStalled   = operation.Stalled
	operationFailed    = operation.Failed
	operationCompleted = operation.Completed
)

type operationCounts = operation.Counts
type operationRecord = operation.Record
type operationActivity = operation.Activity
type operationHandle = operation.Handle
type operationLiveness = operation.Liveness
type operationProgress = operation.Progress

var operationClock = time.Now
var operationProcessAlive operationLiveness = processAlive

func operationRoot(cfg Config) string   { return operation.Root(cfg) }
func validOperationToken(s string) bool { return operation.ValidToken(s) }

func processAlive(pid int) bool { return operation.ProcessAlive(pid) }

func beginOperation(cfg Config, kind operationKind, phase string, now time.Time) (operationHandle, error) {
	return operation.Begin(cfg, kind, phase, now)
}

func finishOperation(cfg Config, h operationHandle, state operationState, phase string, counts operationCounts, failureCode string, now time.Time) error {
	return operation.Finish(cfg, h, state, phase, counts, failureCode, now)
}
func saveOperationRecord(path string, rec operationRecord) error {
	return operation.SaveRecord(path, rec)
}

func operationActivities(cfg Config, now time.Time, live operationLiveness) []operationActivity {
	return operation.Activities(cfg, now, live)
}
func startOperationProgress(cfg Config, h operationHandle, phase string) *operationProgress {
	return operation.StartProgress(cfg, h, phase, operationClock)
}
func completeOperationAfterCoverage(cfg Config, runID string, now time.Time) error {
	return operation.CompleteAfterCoverage(cfg, runID, now)
}

func operationProgressActive(runID string) bool { return operation.Active(runID) }
