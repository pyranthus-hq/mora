package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/genericutil"
	"github.com/pyranthus-hq/mora/internal/memory"
	"github.com/pyranthus-hq/mora/internal/operation"
)

func TestDoctorAmbiguousSQLiteFailureIsCauseUnverified(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	source := Source{Name: "applecalendar", Type: "applecalendar", Enabled: genericutil.Ptr(true)}
	if err := saveSources(cfg, []Source{source}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	setDoctorClock(t, now)
	if err := memory.SaveStatus(syncStatusPathFor(cfg, source), &memory.SyncStatus{
		Source: source.Name, LastAttemptAt: now.Format(time.RFC3339),
		LastError: "unable to open database file: out of memory (14)", ErrorCode: errCodeConnectorUnclassified, ErrorCount: 1,
	}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := cmdDoctor(testCtx(t), []string{"--json"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	var report doctorReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, diagnosis := range report.Diagnosis {
		if diagnosis.Subject == "source:applecalendar" {
			found = diagnosis.Code == "cause_unverified" && diagnosis.ErrorCode == errCodeConnectorUnclassified
		}
		if diagnosis.Code == "permission_missing" {
			t.Fatalf("ambiguous failure claimed permission state: %+v", diagnosis)
		}
	}
	if !found || strings.Contains(strings.ToLower(output.String()), "full disk access") {
		t.Fatalf("report did not preserve uncertainty: %s", output.String())
	}
	if report.Observed == nil || report.Diagnosis == nil || report.RepairPlan == nil || report.Verification == nil {
		t.Fatalf("doctor additive fields must be non-null: %+v", report)
	}
}

func TestDoctorRepairDryRunIsExactAndDoesNotMutate(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	tokenDir := filepath.Join(cfg.ConfigDir, "tokens")
	if err := os.Remove(tokenDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tokenDir); !os.IsNotExist(err) {
		t.Fatalf("token dir unexpectedly exists before repair: %v", err)
	}

	var output bytes.Buffer
	if err := cmdDoctor(testCtx(t), []string{"--repair", "--dry-run", "--json"}, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	var report doctorReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, action := range report.RepairPlan {
		if action.ID == "create_token_dir" {
			found = action.Mutation == "mkdir" && action.Target == tokenDir && action.Safe && action.ApprovalRequired
		}
	}
	if !found || !report.Repairable || len(report.Verification) != 0 {
		t.Fatalf("dry-run did not return the exact unapplied plan: %+v", report)
	}
	if _, err := os.Stat(tokenDir); !os.IsNotExist(err) {
		t.Fatalf("dry-run mutated token dir: %v", err)
	}
}

func TestDoctorRepairRequiresApproval(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	tokenDir := filepath.Join(cfg.ConfigDir, "tokens")
	if err := os.Remove(tokenDir); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := cmdDoctor(testCtx(t), []string{"--repair", "--json"}, &output, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "refusing to repair without --yes") {
		t.Fatalf("repair without approval error = %v", err)
	}
	if _, err := os.Stat(tokenDir); !os.IsNotExist(err) {
		t.Fatalf("unapproved repair mutated token dir: %v", err)
	}
}

func TestDoctorRepairApplyVerifiesAndIsIdempotent(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	tokenDir := filepath.Join(cfg.ConfigDir, "tokens")
	if err := os.Remove(tokenDir); err != nil {
		t.Fatal(err)
	}

	var first bytes.Buffer
	if err := cmdDoctor(testCtx(t), []string{"--repair", "--yes", "--json"}, &first, io.Discard); err != nil {
		t.Fatal(err)
	}
	var report doctorReport
	if err := json.Unmarshal(first.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	verified := false
	for _, result := range report.Verification {
		if result.ActionID == "create_token_dir" {
			verified = result.Before == "failed" && result.After == "passed" && result.Verified
		}
	}
	if !verified {
		t.Fatalf("applied repair lacks before/after verification: %+v", report.Verification)
	}
	if info, err := os.Stat(tokenDir); err != nil || !info.IsDir() {
		t.Fatalf("approved repair did not create token dir: %v", err)
	}

	var second bytes.Buffer
	if err := cmdDoctor(testCtx(t), []string{"--repair", "--yes", "--json"}, &second, io.Discard); err != nil {
		t.Fatal(err)
	}
	var rerun doctorReport
	if err := json.Unmarshal(second.Bytes(), &rerun); err != nil {
		t.Fatal(err)
	}
	for _, action := range rerun.RepairPlan {
		if action.ID == "create_token_dir" {
			t.Fatalf("idempotent rerun proposed completed repair: %+v", rerun.RepairPlan)
		}
	}
}

func TestDoctorUnsafeRepairIsProposalOnly(t *testing.T) {
	cfg := Config{VaultDir: filepath.Join(string(filepath.Separator), "vault")}
	tokenDir := filepath.Join(cfg.VaultDir, "tokens")
	plan := planDoctorRepairs([]doctorCheck{{Name: "tokens_disjoint_from_vault", OK: false, Critical: true}}, cfg, tokenDir, time.Now())
	if len(plan) != 1 || plan[0].ID != "relocate_token_dir" || plan[0].Safe || !plan[0].ApprovalRequired {
		t.Fatalf("unsafe relocation plan = %+v", plan)
	}
	verification, err := applyDoctorRepairs(context.Background(), cfg, plan)
	if err != nil || len(verification) != 0 {
		t.Fatalf("unsafe proposal was executed: verification=%+v err=%v", verification, err)
	}
}

func TestDoctorRetireAbandonedIngestDryRunApplyAndIdempotent(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	setDoctorClock(t, now)

	oldAlive := operationProcessAlive
	t.Cleanup(func() { operationProcessAlive = oldAlive })
	operationProcessAlive = func(int) bool { return false }

	orphan := operationRecord{
		SchemaVersion: operationSchemaVersion,
		Kind:          operationKindIngest,
		State:         operationRunning,
		RunID:         "op_doctor_orphan",
		OwnerPID:      4242,
		StartedAt:     now.Add(-time.Hour).Format(time.RFC3339Nano),
		HeartbeatAt:   now.Add(-operationHeartbeatTTL - time.Second).Format(time.RFC3339Nano),
		Phase:         "awaiting_rebuild",
		Counts:        operationCounts{Items: 4, Files: 1},
	}
	if err := saveOperationRecord(operation.Path(cfg, operationKindIngest, orphan.RunID), orphan); err != nil {
		t.Fatal(err)
	}
	uncoveredFile := filepath.Join(t.TempDir(), "still-here.md")
	if err := os.WriteFile(uncoveredFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	journalDir := filepath.Join(cfg.StateDir, "ingest", "filesystem")
	if err := os.MkdirAll(journalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	uncovered := operationRecord{
		SchemaVersion: operationSchemaVersion,
		Kind:          operationKindIngest,
		State:         operationRunning,
		RunID:         "op_doctor_uncovered",
		OwnerPID:      4343,
		StartedAt:     now.Add(-time.Hour).Format(time.RFC3339Nano),
		HeartbeatAt:   now.Add(-operationHeartbeatTTL - time.Second).Format(time.RFC3339Nano),
		Phase:         "awaiting_rebuild",
		Counts:        operationCounts{Items: 7},
	}
	if err := saveOperationRecord(operation.Path(cfg, operationKindIngest, uncovered.RunID), uncovered); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(journalDir, "journal.log"), []byte("run op_doctor_uncovered "+now.Format(time.RFC3339)+"\n"+uncoveredFile+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var dry bytes.Buffer
	if err := cmdDoctor(testCtx(t), []string{"--repair", "--dry-run", "--json"}, &dry, io.Discard); err != nil {
		t.Fatal(err)
	}
	var dryReport doctorReport
	if err := json.Unmarshal(dry.Bytes(), &dryReport); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, action := range dryReport.RepairPlan {
		if action.ID == "retire_abandoned_ingest" {
			found = action.Safe && action.ApprovalRequired && action.Mutation == "retire_dead_owner_receipts" &&
				strings.Contains(action.Target, "op_doctor_orphan") && strings.Contains(action.Target, "op_doctor_uncovered")
		}
	}
	if !found || !dryReport.Repairable || len(dryReport.Verification) != 0 {
		t.Fatalf("dry-run plan = %+v", dryReport.RepairPlan)
	}
	if _, err := os.Stat(operation.Path(cfg, operationKindIngest, orphan.RunID)); err != nil {
		t.Fatalf("dry-run mutated orphan: %v", err)
	}

	var applyOut bytes.Buffer
	if err := cmdDoctor(testCtx(t), []string{"--repair", "--yes", "--json"}, &applyOut, io.Discard); err != nil {
		t.Fatal(err)
	}
	var applied doctorReport
	if err := json.Unmarshal(applyOut.Bytes(), &applied); err != nil {
		t.Fatal(err)
	}
	verified := false
	for _, result := range applied.Verification {
		if result.ActionID == "retire_abandoned_ingest" {
			verified = result.Before == "failed" && result.After == "passed" && result.Verified
		}
	}
	if !verified {
		t.Fatalf("verification = %+v", applied.Verification)
	}
	if _, err := os.Stat(operation.Path(cfg, operationKindIngest, orphan.RunID)); !os.IsNotExist(err) {
		t.Fatalf("journal-absent orphan not removed: %v", err)
	}
	rec, err := operation.LoadRecord(operation.Path(cfg, operationKindIngest, uncovered.RunID))
	if err != nil || rec.State != operationFailed || rec.FailureCode != operation.FailureOwnerAbandoned || rec.Counts.Items != 7 {
		t.Fatalf("uncovered receipt = %+v err=%v", rec, err)
	}
	if _, err := os.Stat(filepath.Join(journalDir, "journal.log")); err != nil {
		t.Fatalf("uncovered journal erased: %v", err)
	}

	var rerunOut bytes.Buffer
	if err := cmdDoctor(testCtx(t), []string{"--repair", "--yes", "--json"}, &rerunOut, io.Discard); err != nil {
		t.Fatal(err)
	}
	var rerun doctorReport
	if err := json.Unmarshal(rerunOut.Bytes(), &rerun); err != nil {
		t.Fatal(err)
	}
	for _, action := range rerun.RepairPlan {
		if action.ID == "retire_abandoned_ingest" {
			t.Fatalf("idempotent rerun still planned retire: %+v", rerun.RepairPlan)
		}
	}
}

func TestDoctorReadOnlyDoesNotRetireAbandonedIngest(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 9, 29, 15, 0, 0, 0, time.UTC)
	setDoctorClock(t, now)
	oldAlive := operationProcessAlive
	t.Cleanup(func() { operationProcessAlive = oldAlive })
	operationProcessAlive = func(int) bool { return false }

	rec := operationRecord{
		SchemaVersion: operationSchemaVersion,
		Kind:          operationKindIngest,
		State:         operationRunning,
		RunID:         "op_readonly",
		OwnerPID:      4242,
		StartedAt:     now.Add(-time.Hour).Format(time.RFC3339Nano),
		HeartbeatAt:   now.Add(-operationHeartbeatTTL - time.Second).Format(time.RFC3339Nano),
		Phase:         "awaiting_rebuild",
	}
	path := operation.Path(cfg, operationKindIngest, rec.RunID)
	if err := saveOperationRecord(path, rec); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := cmdDoctor(testCtx(t), []string{"--json"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("read-only doctor mutated orphan receipt")
	}
}
