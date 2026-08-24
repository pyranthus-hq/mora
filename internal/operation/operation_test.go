package operation

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/pyranthus-hq/mora/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var operationTestNow = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

func TestOperationActivityLifecycleAndSanitizedProjection(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	h, err := Begin(cfg, KindIngest, "fetching", operationTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := Heartbeat(cfg, h, "writing", Counts{Items: 7, Files: 3, Examined: 10, Materialized: 7, Missing: 3, Errors: 3}, operationTestNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	acts := Activities(cfg, operationTestNow.Add(2*time.Minute), func(pid int) bool { return pid == os.Getpid() })
	if len(acts) != 1 {
		t.Fatalf("activities = %+v", acts)
	}
	a := acts[0]
	if a.Kind != KindIngest || a.State != Running || a.RunID != h.RunID || a.Phase != "writing" || a.Counts.Items != 7 {
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

	if err := Finish(cfg, h, Completed, "retired", Counts{Items: 7, Files: 3, Examined: 10, Materialized: 7, Missing: 3, Errors: 3}, "", operationTestNow.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	acts = Activities(cfg, operationTestNow.Add(4*time.Minute), func(int) bool { return false })
	if len(acts) != 1 || acts[0].State != Completed || acts[0].FinishedAt == "" {
		t.Fatalf("completed activities = %+v", acts)
	}
}
func TestOperationActivityLivenessRequiresRunAndHeartbeat(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	write := func(runID string, rec Record) {
		t.Helper()
		path := Path(cfg, KindIngest, runID)
		if err := SaveRecord(path, rec); err != nil {
			t.Fatal(err)
		}
	}
	base := Record{
		SchemaVersion: SchemaVersion, Kind: KindIngest,
		State: Running, RunID: "op_live", OwnerPID: 4242,
		StartedAt: operationTestNow.Format(time.RFC3339), HeartbeatAt: operationTestNow.Format(time.RFC3339), Phase: "fetching",
	}
	write("op_live", base)
	expired := base
	expired.RunID = "op_expired"
	expired.HeartbeatAt = operationTestNow.Add(-HeartbeatTTL - time.Second).Format(time.RFC3339)
	expired.StartedAt = expired.HeartbeatAt
	write("op_expired", expired)
	mismatch := base
	mismatch.RunID = "op_other"
	write("op_path", mismatch)

	acts := Activities(cfg, operationTestNow, func(pid int) bool { return pid == 4242 })
	byRun := map[string]Activity{}
	for _, a := range acts {
		byRun[a.RunID] = a
	}
	if byRun["op_live"].State != Running {
		t.Fatalf("live = %+v", byRun["op_live"])
	}
	if a := byRun["op_expired"]; a.State != Stalled || a.FailureCode != "heartbeat_expired" {
		t.Fatalf("expired/PID-reused = %+v", a)
	}
	if a := byRun["op_path"]; a.State != Failed || a.FailureCode != "identity_mismatch" {
		t.Fatalf("run mismatch = %+v", a)
	}

	acts = Activities(cfg, operationTestNow.Add(time.Minute), func(int) bool { return false })
	for _, a := range acts {
		if a.RunID == "op_live" && (a.State != Stalled || a.FailureCode != "owner_dead") {
			t.Fatalf("dead owner = %+v", a)
		}
	}
}

func TestBeginPrunesOnlyExpiredDeadOwnerRecords(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	oldProcessAlive := ProcessAlive
	t.Cleanup(func() { ProcessAlive = oldProcessAlive })
	ProcessAlive = func(pid int) bool { return pid == 4343 }

	writeRunning := func(runID string, pid int, heartbeat time.Time) {
		t.Helper()
		rec := Record{
			SchemaVersion: SchemaVersion,
			Kind:          KindIngest,
			State:         Running,
			RunID:         runID,
			OwnerPID:      pid,
			StartedAt:     heartbeat.Add(-time.Minute).Format(time.RFC3339Nano),
			HeartbeatAt:   heartbeat.Format(time.RFC3339Nano),
			Phase:         "awaiting_rebuild",
		}
		if err := SaveRecord(Path(cfg, KindIngest, runID), rec); err != nil {
			t.Fatal(err)
		}
	}

	writeRunning("op_dead_expired", 4242, operationTestNow.Add(-HeartbeatTTL-time.Second))
	writeRunning("op_live_expired", 4343, operationTestNow.Add(-HeartbeatTTL-time.Second))
	writeRunning("op_dead_recent", 4444, operationTestNow.Add(-time.Minute))
	if err := os.WriteFile(Path(cfg, KindIngest, "op_corrupt"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Begin(cfg, KindIngest, "fetching", operationTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Path(cfg, KindIngest, "op_dead_expired")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired dead-owner receipt survived: %v", err)
	}
	for _, runID := range []string{"op_live_expired", "op_dead_recent", "op_corrupt"} {
		if _, err := os.Stat(Path(cfg, KindIngest, runID)); err != nil {
			t.Fatalf("receipt %s was pruned: %v", runID, err)
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
			cfg := config.Config{StateDir: t.TempDir()}
			path := Path(cfg, KindIngest, "op_bad")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			acts := Activities(cfg, operationTestNow, func(int) bool { return true })
			if len(acts) != 1 || acts[0].State != Failed || acts[0].FailureCode != "receipt_invalid" {
				t.Fatalf("activities = %+v", acts)
			}
		})
	}
}
func TestOperationClassifierRejectsInvalidPhaseAndIncoherentStates(t *testing.T) {
	base := Record{
		SchemaVersion: SchemaVersion,
		Kind:          KindIngest,
		State:         Running,
		RunID:         "op_bad",
		OwnerPID:      1,
		StartedAt:     operationTestNow.Format(time.RFC3339),
		HeartbeatAt:   operationTestNow.Format(time.RFC3339),
		Phase:         "fetching",
	}
	cases := []struct {
		name string
		edit func(*Record)
		code string
	}{
		{name: "invalid_phase", edit: func(r *Record) { r.Phase = "provider/account" }, code: "invalid_phase"},
		{name: "running_with_finished_at", edit: func(r *Record) { r.FinishedAt = operationTestNow.Format(time.RFC3339) }, code: "incoherent_state"},
		{name: "running_with_failure", edit: func(r *Record) { r.FailureCode = "failed" }, code: "incoherent_state"},
		{name: "failed_without_code", edit: func(r *Record) {
			r.State = Failed
			r.FinishedAt = operationTestNow.Format(time.RFC3339)
		}, code: "incoherent_state"},
		{name: "completed_with_code", edit: func(r *Record) {
			r.State = Completed
			r.FinishedAt = operationTestNow.Format(time.RFC3339)
			r.FailureCode = "failed"
		}, code: "incoherent_state"},
		{name: "future_completed", edit: func(r *Record) {
			future := operationTestNow.Add(2 * time.Minute).Format(time.RFC3339)
			r.State = Completed
			r.HeartbeatAt = future
			r.FinishedAt = future
		}, code: "invalid_timestamp"},
		{name: "future_failed", edit: func(r *Record) {
			future := operationTestNow.Add(2 * time.Minute).Format(time.RFC3339)
			r.State = Failed
			r.HeartbeatAt = future
			r.FinishedAt = future
			r.FailureCode = "injected_failure"
		}, code: "invalid_timestamp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{StateDir: t.TempDir()}
			rec := base
			tc.edit(&rec)
			if err := SaveRecord(Path(cfg, KindIngest, rec.RunID), rec); err != nil {
				t.Fatal(err)
			}
			acts := Activities(cfg, operationTestNow, func(int) bool { return true })
			if len(acts) != 1 || acts[0].State != Failed || acts[0].FailureCode != tc.code {
				t.Fatalf("activities = %+v, want failed/%s", acts, tc.code)
			}
		})
	}
}
func TestOperationWriterRejectsBackwardTimeWithoutMutation(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	h, err := Begin(cfg, KindIngest, "fetching", operationTestNow)
	if err != nil {
		t.Fatal(err)
	}
	path := Path(cfg, h.Kind, h.RunID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	past := operationTestNow.Add(-time.Nanosecond)
	if err := Heartbeat(cfg, h, "writing", Counts{Items: 1}, past); err == nil {
		t.Fatal("backward heartbeat accepted")
	}
	if err := Finish(cfg, h, Completed, "retired", Counts{}, "", past); err == nil {
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
func TestOperationReceiptRejectsRelativeStateDir(t *testing.T) {
	if _, err := Begin(config.Config{StateDir: "relative"}, KindIngest, "start", operationTestNow); err == nil {
		t.Fatal("relative StateDir accepted")
	}
}
func TestOperationProgressPeriodicHeartbeatUsesInjectedTicker(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	h, err := Begin(cfg, KindIngest, "ingesting", operationTestNow)
	if err != nil {
		t.Fatal(err)
	}
	ticks := make(chan time.Time, 1)
	origTicker := newOperationHeartbeatTicker
	t.Cleanup(func() { newOperationHeartbeatTicker = origTicker })
	newOperationHeartbeatTicker = func(time.Duration) operationHeartbeatTicker {
		return operationHeartbeatTicker{C: ticks, stop: func() {}}
	}
	heartbeatAt := operationTestNow.Add(5 * time.Minute)
	clock := func() time.Time { return heartbeatAt }
	progress := StartProgress(cfg, h, "ingesting", clock)
	ticks <- heartbeatAt
	deadline := time.Now().Add(time.Second)
	for {
		rec, err := LoadRecord(Path(cfg, h.Kind, h.RunID))
		if err == nil && rec.HeartbeatAt == heartbeatAt.Format(time.RFC3339Nano) {
			break
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("periodic heartbeat receipt stayed unreadable: %v", err)
			}
			t.Fatalf("periodic heartbeat did not advance: %+v", rec)
		}
		time.Sleep(time.Millisecond)
	}
	if err := progress.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestWriterValidationAndProgressUpdate(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	if _, err := Begin(cfg, Kind("bad"), "start", operationTestNow); err == nil {
		t.Fatal("invalid kind")
	}
	if _, err := Begin(cfg, KindIngest, "bad phase", operationTestNow); err == nil {
		t.Fatal("invalid phase")
	}
	h, err := Begin(cfg, KindIngest, "start", operationTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := Heartbeat(cfg, h, "bad phase", Counts{}, operationTestNow); err == nil {
		t.Fatal("bad heartbeat phase")
	}
	if err := Heartbeat(cfg, h, "next", Counts{Items: -1}, operationTestNow); err == nil {
		t.Fatal("negative counts")
	}
	if err := Heartbeat(cfg, h, "next", Counts{Examined: 1, Materialized: -1}, operationTestNow); err == nil {
		t.Fatal("negative additive counts")
	}
	if err := Finish(cfg, h, Running, "end", Counts{}, "", operationTestNow); err == nil {
		t.Fatal("nonterminal finish")
	}
	if err := Finish(cfg, h, Completed, "bad phase", Counts{}, "", operationTestNow); err == nil {
		t.Fatal("bad finish phase")
	}
	if err := Finish(cfg, h, Completed, "end", Counts{}, "failure", operationTestNow); err == nil {
		t.Fatal("completed failure code")
	}
	orig := newOperationHeartbeatTicker
	t.Cleanup(func() { newOperationHeartbeatTicker = orig })
	ticks := make(chan time.Time)
	newOperationHeartbeatTicker = func(time.Duration) operationHeartbeatTicker {
		return operationHeartbeatTicker{C: ticks, stop: func() {}}
	}
	progress := StartProgress(cfg, h, "start", func() time.Time { return operationTestNow.Add(time.Minute) })
	if !Active(h.RunID) {
		t.Fatal("progress inactive")
	}
	if err := progress.Update("updated", Counts{Items: 1}); err != nil {
		t.Fatal(err)
	}
	if err := progress.Stop(); err != nil {
		t.Fatal(err)
	}
	if Active(h.RunID) {
		t.Fatal("progress remained active")
	}
	if err := Finish(cfg, h, Failed, "failed", Counts{Errors: 1}, "bad code!", operationTestNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	rec, err := LoadRecord(Path(cfg, h.Kind, h.RunID))
	if err != nil || rec.FailureCode != "operation_failed" {
		t.Fatalf("record=%+v err=%v", rec, err)
	}
}
func TestCompleteAfterCoverage(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	if err := CompleteAfterCoverage(cfg, "bad id!", operationTestNow); err == nil {
		t.Fatal("invalid covered id")
	}
	if err := CompleteAfterCoverage(cfg, "op_missing", operationTestNow); err != nil {
		t.Fatal(err)
	}
	h, err := Begin(cfg, KindIngest, "awaiting_rebuild", operationTestNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := CompleteAfterCoverage(cfg, h.RunID, operationTestNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	rec, err := LoadRecord(Path(cfg, KindIngest, h.RunID))
	if err != nil || rec.State != Completed || rec.Phase != "journal_retired" {
		t.Fatalf("record=%+v err=%v", rec, err)
	}
	if err := CompleteAfterCoverage(cfg, h.RunID, operationTestNow.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
}
func TestActivityAndExportedHelperEdges(t *testing.T) {
	if ValidToken("") || ValidToken(strings.Repeat("x", 129)) || ValidToken("bad!") || !ValidToken("ok_ID-1") {
		t.Fatal("token validation")
	}
	if SanitizePhase(" bad phase ") != "unknown" || SanitizePhase(" good ") != "good" {
		t.Fatal("phase sanitization")
	}
	if Root(config.Config{StateDir: "state"}) != "state/operations" && filepath.ToSlash(Root(config.Config{StateDir: "state"})) != "state/operations" {
		t.Fatal("root")
	}
	acts := Activities(config.Config{StateDir: "relative"}, operationTestNow, nil)
	if len(acts) != 1 || acts[0].FailureCode != "ledger_unreadable" {
		t.Fatalf("activities=%+v", acts)
	}
	if processAlive(-1) {
		t.Fatal("invalid pid alive")
	}
}
func TestPruneTerminalBound(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	for i := 0; i < TerminalKeep+3; i++ {
		at := operationTestNow.Add(time.Duration(i) * time.Minute)
		h, err := Begin(cfg, KindIndexRebuild, "start", at)
		if err != nil {
			t.Fatal(err)
		}
		if err := Finish(cfg, h, Completed, "done", Counts{}, "", at.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(Root(cfg), string(KindIndexRebuild)))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			count++
		}
	}
	if count != TerminalKeep {
		t.Fatalf("terminal receipts=%d want %d", count, TerminalKeep)
	}
}
