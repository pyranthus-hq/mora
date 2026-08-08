package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var operationTestNow = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

func TestOperationActivityLifecycleAndSanitizedProjection(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	h, err := beginOperation(cfg, operationKindIngest, "fetching", operationTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := heartbeatOperation(cfg, h, "writing", operationCounts{Items: 7, Files: 3}, operationTestNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	acts := operationActivities(cfg, operationTestNow.Add(2*time.Minute), func(pid int) bool { return pid == os.Getpid() })
	if len(acts) != 1 {
		t.Fatalf("activities = %+v", acts)
	}
	a := acts[0]
	if a.Kind != operationKindIngest || a.State != operationRunning || a.RunID != h.runID || a.Phase != "writing" || a.Counts.Items != 7 {
		t.Fatalf("running activity = %+v", a)
	}
	body, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"owner_pid", filepath.Clean(cfg.StateDir), "account", "path"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Fatalf("sanitized activity leaked %q: %s", secret, body)
		}
	}

	if err := finishOperation(cfg, h, operationCompleted, "retired", operationCounts{Items: 7, Files: 3}, "", operationTestNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	acts = operationActivities(cfg, operationTestNow.Add(4*time.Minute), func(int) bool { return false })
	if len(acts) != 1 || acts[0].State != operationCompleted || acts[0].FinishedAt == "" {
		t.Fatalf("completed activities = %+v", acts)
	}
}

func TestOperationActivityLivenessRequiresRunAndHeartbeat(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	write := func(runID string, rec operationRecord) {
		t.Helper()
		path := operationPath(cfg, operationKindIngest, runID)
		if err := saveOperationRecord(path, rec); err != nil {
			t.Fatal(err)
		}
	}
	base := operationRecord{
		SchemaVersion: operationSchemaVersion, Kind: operationKindIngest,
		State: operationRunning, RunID: "op_live", OwnerPID: 4242,
		StartedAt: operationTestNow.Format(time.RFC3339), HeartbeatAt: operationTestNow.Format(time.RFC3339), Phase: "fetching",
	}
	write("op_live", base)
	expired := base
	expired.RunID = "op_expired"
	expired.HeartbeatAt = operationTestNow.Add(-operationHeartbeatTTL - time.Second).Format(time.RFC3339)
	expired.StartedAt = expired.HeartbeatAt
	write("op_expired", expired)
	mismatch := base
	mismatch.RunID = "op_other"
	write("op_path", mismatch)

	acts := operationActivities(cfg, operationTestNow, func(pid int) bool { return pid == 4242 })
	byRun := map[string]operationActivity{}
	for _, a := range acts {
		byRun[a.RunID] = a
	}
	if byRun["op_live"].State != operationRunning {
		t.Fatalf("live = %+v", byRun["op_live"])
	}
	if a := byRun["op_expired"]; a.State != operationStalled || a.FailureCode != "heartbeat_expired" {
		t.Fatalf("expired/PID-reused = %+v", a)
	}
	if a := byRun["op_path"]; a.State != operationFailed || a.FailureCode != "identity_mismatch" {
		t.Fatalf("run mismatch = %+v", a)
	}

	acts = operationActivities(cfg, operationTestNow.Add(time.Minute), func(int) bool { return false })
	for _, a := range acts {
		if a.RunID == "op_live" && (a.State != operationStalled || a.FailureCode != "owner_dead") {
			t.Fatalf("dead owner = %+v", a)
		}
	}
}

func TestOperationReceiptStrictJSONRejectsUnknownAndTrailingValues(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "unknown_field", body: `{"schema_version":1,"kind":"ingest","state":"running","run_id":"op_bad","owner_pid":1,"started_at":"2026-07-01T12:00:00Z","heartbeat_at":"2026-07-01T12:00:00Z","phase":"fetching","counts":{},"future_field":true}`},
		{name: "trailing_value", body: `{"schema_version":1,"kind":"ingest","state":"running","run_id":"op_bad","owner_pid":1,"started_at":"2026-07-01T12:00:00Z","heartbeat_at":"2026-07-01T12:00:00Z","phase":"fetching","counts":{}} {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{StateDir: t.TempDir()}
			path := operationPath(cfg, operationKindIngest, "op_bad")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			acts := operationActivities(cfg, operationTestNow, func(int) bool { return true })
			if len(acts) != 1 || acts[0].State != operationFailed || acts[0].FailureCode != "receipt_invalid" {
				t.Fatalf("activities = %+v", acts)
			}
		})
	}
}

func TestOperationClassifierRejectsInvalidPhaseAndIncoherentStates(t *testing.T) {
	base := operationRecord{
		SchemaVersion: operationSchemaVersion,
		Kind:          operationKindIngest,
		State:         operationRunning,
		RunID:         "op_bad",
		OwnerPID:      1,
		StartedAt:     operationTestNow.Format(time.RFC3339),
		HeartbeatAt:   operationTestNow.Format(time.RFC3339),
		Phase:         "fetching",
	}
	cases := []struct {
		name string
		edit func(*operationRecord)
		code string
	}{
		{name: "invalid_phase", edit: func(r *operationRecord) { r.Phase = "provider/account" }, code: "invalid_phase"},
		{name: "running_with_finished_at", edit: func(r *operationRecord) { r.FinishedAt = operationTestNow.Format(time.RFC3339) }, code: "incoherent_state"},
		{name: "running_with_failure", edit: func(r *operationRecord) { r.FailureCode = "failed" }, code: "incoherent_state"},
		{name: "failed_without_code", edit: func(r *operationRecord) {
			r.State = operationFailed
			r.FinishedAt = operationTestNow.Format(time.RFC3339)
		}, code: "incoherent_state"},
		{name: "completed_with_code", edit: func(r *operationRecord) {
			r.State = operationCompleted
			r.FinishedAt = operationTestNow.Format(time.RFC3339)
			r.FailureCode = "failed"
		}, code: "incoherent_state"},
		{name: "future_completed", edit: func(r *operationRecord) {
			future := operationTestNow.Add(2 * time.Minute).Format(time.RFC3339)
			r.State = operationCompleted
			r.HeartbeatAt = future
			r.FinishedAt = future
		}, code: "invalid_timestamp"},
		{name: "future_failed", edit: func(r *operationRecord) {
			future := operationTestNow.Add(2 * time.Minute).Format(time.RFC3339)
			r.State = operationFailed
			r.HeartbeatAt = future
			r.FinishedAt = future
			r.FailureCode = "injected_failure"
		}, code: "invalid_timestamp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{StateDir: t.TempDir()}
			rec := base
			tc.edit(&rec)
			if err := saveOperationRecord(operationPath(cfg, operationKindIngest, rec.RunID), rec); err != nil {
				t.Fatal(err)
			}
			acts := operationActivities(cfg, operationTestNow, func(int) bool { return true })
			if len(acts) != 1 || acts[0].State != operationFailed || acts[0].FailureCode != tc.code {
				t.Fatalf("activities = %+v, want failed/%s", acts, tc.code)
			}
		})
	}
}

func TestOperationWriterRejectsBackwardTimeWithoutMutation(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	h, err := beginOperation(cfg, operationKindIngest, "fetching", operationTestNow)
	if err != nil {
		t.Fatal(err)
	}
	path := operationPath(cfg, h.kind, h.runID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	past := operationTestNow.Add(-time.Nanosecond)
	if err := heartbeatOperation(cfg, h, "writing", operationCounts{Items: 1}, past); err == nil {
		t.Fatal("backward heartbeat accepted")
	}
	if err := finishOperation(cfg, h, operationCompleted, "retired", operationCounts{}, "", past); err == nil {
		t.Fatal("backward terminal transition accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("rejected backward transition mutated receipt")
	}
}

func TestOperationActivityCorruptFutureSchemaFailsClosedAndReadIsPure(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	dir := filepath.Join(operationRoot(cfg), string(operationKindIndexRebuild))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	corruptPath := filepath.Join(dir, "op_corrupt.json")
	if err := os.WriteFile(corruptPath, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := operationRecord{
		SchemaVersion: operationSchemaVersion + 1, Kind: operationKindIndexRebuild,
		State: operationRunning, RunID: "op_future", OwnerPID: os.Getpid(),
		StartedAt: operationTestNow.Format(time.RFC3339), HeartbeatAt: operationTestNow.Format(time.RFC3339), Phase: "listing",
	}
	if err := saveOperationRecord(filepath.Join(dir, "op_future.json"), future); err != nil {
		t.Fatal(err)
	}

	beforeCorrupt, _ := os.ReadFile(corruptPath)
	futurePath := filepath.Join(dir, "op_future.json")
	beforeFuture, _ := os.ReadFile(futurePath)
	acts := operationActivities(cfg, operationTestNow, func(int) bool { return true })
	afterCorrupt, _ := os.ReadFile(corruptPath)
	afterFuture, _ := os.ReadFile(futurePath)
	if !bytes.Equal(beforeCorrupt, afterCorrupt) || !bytes.Equal(beforeFuture, afterFuture) {
		t.Fatal("read-only classification mutated an operation receipt")
	}
	if len(acts) != 2 {
		t.Fatalf("activities = %+v", acts)
	}
	for _, a := range acts {
		if a.State != operationFailed {
			t.Fatalf("corrupt/future must fail closed: %+v", a)
		}
	}
	if aggregateHealthState(Health{Index: indexHealth{State: idxFresh}, Activities: acts}) != healthUnhealthy {
		t.Fatal("failed operation receipts must fail aggregate health closed")
	}
}

func TestOperationActivityDirtyRunningBannerDoesNotMakeHealthy(t *testing.T) {
	h := Health{
		Index:      indexHealth{State: idxDirty, IndexedAt: "2026-07-01T11:00:00Z", PendingOps: 1},
		Activities: []operationActivity{{Kind: operationKindIndexRebuild, State: operationRunning, RunID: "op_run", Phase: "vectors"}},
	}
	h.State = aggregateHealthState(h)
	if h.State != healthUnhealthy {
		t.Fatalf("state = %q", h.State)
	}
	banner := healthBannerFrom(h)
	for _, want := range []string{"refresh in progress", "serving the last committed snapshot", "remains DIRTY"} {
		if !strings.Contains(banner, want) {
			t.Fatalf("banner %q missing %q", banner, want)
		}
	}
}

func TestDoctorJSONExposesFailedOperationAndStrictFails(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	h, err := beginOperation(cfg, operationKindIndexRebuild, "listing", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := finishOperation(cfg, h, operationFailed, "listing", operationCounts{Errors: 1}, "vault_walk_failed", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := Run(context.Background(), []string{"doctor", "--json", "--strict"}, &out, &out, strings.NewReader("")); err == nil {
		t.Fatalf("doctor strict succeeded with failed activity: %s", out.String())
	}
	var rep struct {
		Healthy    bool                `json:"healthy"`
		Activities []operationActivity `json:"activities"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("doctor JSON: %v\n%s", err, out.String())
	}
	if rep.Healthy || len(rep.Activities) != 1 || rep.Activities[0].State != operationFailed {
		t.Fatalf("doctor report = %+v", rep)
	}
}

func TestOperationActivityNewerSuccessSupersedesOldFailure(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	failed, err := beginOperation(cfg, operationKindIndexRebuild, "listing", operationTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := finishOperation(cfg, failed, operationFailed, "listing", operationCounts{Errors: 1}, "walk_failed", operationTestNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	succeeded, err := beginOperation(cfg, operationKindIndexRebuild, "listing", operationTestNow.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := finishOperation(cfg, succeeded, operationCompleted, "retired", operationCounts{Files: 2}, "", operationTestNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	acts := operationActivities(cfg, operationTestNow.Add(4*time.Minute), func(int) bool { return false })
	if len(acts) != 1 || acts[0].RunID != succeeded.runID || acts[0].State != operationCompleted {
		t.Fatalf("activities = %+v, want only newest successful terminal", acts)
	}
	if got := aggregateHealthState(Health{Index: indexHealth{State: idxFresh}, Activities: acts}); got != healthHealthy {
		t.Fatalf("aggregate = %q, old failure was not superseded", got)
	}
}

func TestOperationReceiptRejectsRelativeStateDir(t *testing.T) {
	if _, err := beginOperation(Config{StateDir: "relative"}, operationKindIngest, "start", operationTestNow); err == nil {
		t.Fatal("relative StateDir accepted")
	}
}
