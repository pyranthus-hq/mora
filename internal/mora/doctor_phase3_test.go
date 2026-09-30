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
				action.Target == "op_doctor_orphan=failed_uncovered,op_doctor_uncovered=failed_uncovered"
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
	if rec, err := operation.LoadRecord(operation.Path(cfg, operationKindIngest, orphan.RunID)); err != nil || rec.State != operationFailed {
		t.Fatalf("ambiguous orphan not retained: %+v, %v", rec, err)
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

func TestDoctorRetirementReviewRegressions(t *testing.T) {
	for _, journalHeader := range []string{"op_earlier_run", ""} {
		t.Run("ambiguous_"+journalHeader, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			cfg := mustConfig(t)
			now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
			setDoctorClock(t, now)
			oldAlive := operationProcessAlive
			t.Cleanup(func() { operationProcessAlive = oldAlive })
			operationProcessAlive = func(int) bool { return false }
			rec := operationRecord{SchemaVersion: operationSchemaVersion, Kind: operationKindIngest, State: operationRunning, RunID: "op_later_run", OwnerPID: 4242, StartedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), HeartbeatAt: now.Add(-time.Hour).Format(time.RFC3339Nano), Phase: "awaiting_rebuild", Counts: operationCounts{Items: 9}}
			path := operation.Path(cfg, operationKindIngest, rec.RunID)
			if err := saveOperationRecord(path, rec); err != nil {
				t.Fatal(err)
			}
			published := filepath.Join(t.TempDir(), "published.md")
			if err := os.WriteFile(published, []byte("x"), 0600); err != nil {
				t.Fatal(err)
			}
			journal := filepath.Join(cfg.StateDir, "ingest", "filesystem", "journal.log")
			if err := os.MkdirAll(filepath.Dir(journal), 0700); err != nil {
				t.Fatal(err)
			}
			body := published + "\n"
			if journalHeader != "" {
				body = "run " + journalHeader + " now\n" + body
			}
			if err := os.WriteFile(journal, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			plan := planDoctorRepairs(nil, cfg, "", now)
			if len(plan) != 1 || plan[0].Target != "op_later_run=failed_uncovered" {
				t.Errorf("preview does not disclose retention: %+v", plan)
			}
			results, err := applyDoctorRepairs(context.Background(), cfg, plan)
			if err != nil {
				t.Fatal(err)
			}
			got, err := operation.LoadRecord(path)
			if err != nil || got.State != operationFailed || got.Counts.Items != 9 {
				t.Fatalf("ambiguous receipt lost: %+v, %v; verification=%+v", got, err, results)
			}
		})
	}
	t.Run("terminal_transition_is_not_retirement", func(t *testing.T) {
		withTempHome(t)
		run(t, "init")
		cfg := mustConfig(t)
		now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
		setDoctorClock(t, now)
		oldAlive := operationProcessAlive
		t.Cleanup(func() { operationProcessAlive = oldAlive })
		operationProcessAlive = func(int) bool { return false }
		rec := operationRecord{SchemaVersion: operationSchemaVersion, Kind: operationKindIngest, State: operationRunning, RunID: "op_changed", OwnerPID: 4242, StartedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), HeartbeatAt: now.Add(-time.Hour).Format(time.RFC3339Nano), Phase: "awaiting_rebuild"}
		path := operation.Path(cfg, operationKindIngest, rec.RunID)
		if err := saveOperationRecord(path, rec); err != nil {
			t.Fatal(err)
		}
		plan := planDoctorRepairs(nil, cfg, "", now)
		if len(plan) != 1 || plan[0].Target != "op_changed=removed" {
			t.Errorf("preview does not disclose removal: %+v", plan)
		}
		rec.State = operationCompleted
		rec.Phase = "journal_retired"
		rec.FinishedAt = now.Format(time.RFC3339Nano)
		if err := saveOperationRecord(path, rec); err != nil {
			t.Fatal(err)
		}
		results, err := applyDoctorRepairs(context.Background(), cfg, plan)
		if err == nil || len(results) != 1 || results[0].Verified {
			t.Fatalf("claimed work not performed: %+v, %v", results, err)
		}
	})
}

func TestDoctorRetirementPlanScopeAndRebuildOrder(t *testing.T) {
	for _, scenario := range []string{"remove", "evidence_changed", "rebuild"} {
		t.Run(scenario, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			cfg := mustConfig(t)
			now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
			setDoctorClock(t, now)
			oldAlive := operationProcessAlive
			t.Cleanup(func() { operationProcessAlive = oldAlive })
			operationProcessAlive = func(int) bool { return false }
			rec := operationRecord{SchemaVersion: operationSchemaVersion, Kind: operationKindIngest, State: operationRunning, RunID: "op_planned", OwnerPID: 4242, StartedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), HeartbeatAt: now.Add(-time.Hour).Format(time.RFC3339Nano), Phase: "awaiting_rebuild", Counts: operationCounts{Items: 3}}
			path := operation.Path(cfg, operationKindIngest, rec.RunID)
			if err := saveOperationRecord(path, rec); err != nil {
				t.Fatal(err)
			}
			publish := func() {
				t.Helper()
				file := filepath.Join(cfg.VaultDir, "review-fixture.md")
				if err := os.WriteFile(file, []byte("---\nid: review-fixture\ntitle: Review fixture\ntype: note\n---\nFixture body.\n"), 0600); err != nil {
					t.Fatal(err)
				}
				journal := filepath.Join(cfg.StateDir, "ingest", "filesystem", "journal.log")
				if err := os.MkdirAll(filepath.Dir(journal), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(journal, []byte("run op_planned now\n"+file+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var checks []doctorCheck
			if scenario == "rebuild" {
				publish()
				checks = []doctorCheck{{Name: "index_fresh", OK: false}}
			}
			plan := planDoctorRepairs(checks, cfg, "", now)
			if scenario == "evidence_changed" {
				publish()
			}
			if scenario == "rebuild" && (len(plan) != 2 || plan[0].ID != "retire_abandoned_ingest" || plan[1].ID != "rebuild_index") {
				t.Fatalf("unsafe ordering: %+v", plan)
			}
			// A newly discovered abandoned receipt was not approved by this plan.
			rec.RunID = "op_unplanned"
			unplanned := operation.Path(cfg, operationKindIngest, rec.RunID)
			if err := saveOperationRecord(unplanned, rec); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(unplanned)
			if err != nil {
				t.Fatal(err)
			}
			results, err := applyDoctorRepairs(context.Background(), cfg, plan)
			if scenario == "evidence_changed" {
				if err == nil || results[0].Verified || !strings.Contains(results[0].Detail, "skipped op_planned") {
					t.Fatalf("changed evidence accepted: %+v, %v", results, err)
				}
				got, err := operation.LoadRecord(path)
				if err != nil || got.State != operationRunning {
					t.Fatalf("changed plan mutated receipt: %+v, %v", got, err)
				}
			} else {
				if err != nil || !results[0].Verified || len(results[0].Retirements) != 1 {
					t.Fatalf("retirement not verified: %+v, %v", results, err)
				}
				if scenario == "remove" {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("receipt not removed: %v", err)
					}
				} else {
					got, err := operation.LoadRecord(path)
					if err != nil || got.State != operationFailed || got.FailureCode != operation.FailureOwnerAbandoned {
						t.Fatalf("rebuild replaced retirement: %+v, %v", got, err)
					}
				}
			}
			after, err := os.ReadFile(unplanned)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("unplanned receipt mutated: %s, %v", after, err)
			}
		})
	}
}
