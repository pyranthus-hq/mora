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
	if len(acts) != 1 || acts[0].RunID != succeeded.RunID || acts[0].State != operationCompleted {
		t.Fatalf("activities = %+v, want only newest successful terminal", acts)
	}
	if got := aggregateHealthState(Health{Index: indexHealth{State: idxFresh}, Activities: acts}); got != healthHealthy {
		t.Fatalf("aggregate = %q, old failure was not superseded", got)
	}
}
