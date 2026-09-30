package mora

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/operation"
)

// saveAbandonedIngestReceipt writes the permanently-red state #498 retains: a
// terminal failed / owner_abandoned ingest receipt. No writer, rebuild, or prune
// can clear it, so Doctor must both explain it and offer a way out.
//
// Counts carry the shape a real ingest writer produces (ingest.go:1260): Items
// mirrors Materialized, Examined is the walk, and Files is NEVER set — it is an
// index_rebuild field. Hand-writing Files here is what let guidance read it and
// still pass; keep it unset so the test fails if that drifts back.
func saveAbandonedIngestReceipt(t *testing.T, cfg Config, runID string, now time.Time) operationRecord {
	t.Helper()
	stamp := now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	rec := operationRecord{
		SchemaVersion: operationSchemaVersion, Kind: operationKindIngest, State: operationFailed,
		RunID: runID, OwnerPID: 4242,
		StartedAt: now.Add(-3 * time.Hour).Format(time.RFC3339Nano), HeartbeatAt: stamp, FinishedAt: stamp,
		Phase: "awaiting_rebuild", Counts: operationCounts{Items: 9, Examined: 12, Materialized: 9},
		FailureCode: operation.FailureOwnerAbandoned,
	}
	if err := saveOperationRecord(operation.Path(cfg, operationKindIngest, runID), rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// The red check must name the cause, the data impact, and the exact resolving
// command — the convention every other permanently-red condition in doctor.go
// already follows. A bare field dump leaves the operator with no action at all.
func TestDoctorGuidesRecoveryForAbandonedIngestReceipt(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 9, 29, 15, 0, 0, 0, time.UTC)
	setDoctorClock(t, now)
	saveAbandonedIngestReceipt(t, cfg, "op_later_run", now)

	var out bytes.Buffer
	if err := cmdDoctor(testCtx(t), nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "warn operation_healthy:ingest:op_later_run") {
		t.Fatalf("abandoned receipt did not redden its check:\n%s", text)
	}
	for _, want := range []string{
		"owner process died mid-run",                         // cause
		"are in the vault but no",                            // data impact
		"9 item(s) this run had already published",           // data impact, from the count ingest really writes
		"`mora index rebuild`",                               // index what landed
		"`mora sync <source-type>`",                          // refetch what never arrived (a type, not an instance)
		"`mora doctor --repair --dry-run --json`",            // preview (--repair requires --json)
		"`mora doctor --repair --yes --json` to acknowledge", // resolve
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("recovery guidance missing %q:\n%s", want, text)
		}
	}
	// The receipt was retained BECAUSE work landed uncovered. Never report 0.
	if strings.Contains(text, "0 item(s)") || strings.Contains(text, "file(s) this run") {
		t.Fatalf("guidance reported a zero/index_rebuild count:\n%s", text)
	}
	// Read-only doctor prints guidance and mutates nothing.
	rec, err := operation.LoadRecord(operation.Path(cfg, operationKindIngest, "op_later_run"))
	if err != nil || rec.AcknowledgedAt != "" || rec.State != operationFailed ||
		rec.FailureCode != operation.FailureOwnerAbandoned {
		t.Fatalf("read-only doctor mutated the receipt: %+v, %v", rec, err)
	}
}

// The common abandonment shape has NO counts at all: the "ingesting" heartbeat
// writes operationCounts{} (ingest.go:1236) and the owner dies before the
// "awaiting_rebuild" heartbeat reports real ones. Retention still proves work
// landed — UncoveredRunIDs needs an uncovered journal path line — so the guidance
// must say the count is unknown, never that zero items were published.
func TestDoctorGuidanceNeverClaimsZeroPublishedItems(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 9, 29, 15, 0, 0, 0, time.UTC)
	setDoctorClock(t, now)
	rec := saveAbandonedIngestReceipt(t, cfg, "op_no_counts", now)
	rec.Phase, rec.Counts = "ingesting", operationCounts{}
	if err := saveOperationRecord(operation.Path(cfg, operationKindIngest, rec.RunID), rec); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := cmdDoctor(testCtx(t), nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "warn operation_healthy:ingest:op_no_counts") {
		t.Fatalf("countless abandoned receipt did not redden its check:\n%s", text)
	}
	for _, want := range []string{
		"died before it reported counts",
		"whatever it had already published is in",
		"`mora index rebuild`",
		"`mora doctor --repair --yes --json` to acknowledge",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("recovery guidance missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "0 item(s)") {
		t.Fatalf("guidance told the operator nothing landed:\n%s", text)
	}
}

func TestDoctorAcknowledgeAbandonedIngestRoundTrip(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 9, 29, 15, 0, 0, 0, time.UTC)
	setDoctorClock(t, now)
	path := operation.Path(cfg, operationKindIngest, "op_reviewed")
	saveAbandonedIngestReceipt(t, cfg, "op_reviewed", now)

	var dry bytes.Buffer
	if err := cmdDoctor(testCtx(t), []string{"--repair", "--dry-run", "--json"}, &dry, io.Discard); err != nil {
		t.Fatal(err)
	}
	var dryReport doctorReport
	if err := json.Unmarshal(dry.Bytes(), &dryReport); err != nil {
		t.Fatal(err)
	}
	planned := false
	for _, action := range dryReport.RepairPlan {
		if action.ID == "acknowledge_abandoned_ingest" {
			planned = action.Safe && action.ApprovalRequired &&
				action.Mutation == "acknowledge_abandoned_receipts" && action.Target == "op_reviewed"
		}
	}
	if !planned || !dryReport.Repairable || len(dryReport.Verification) != 0 {
		t.Fatalf("dry-run plan = %+v", dryReport.RepairPlan)
	}
	if rec, err := operation.LoadRecord(path); err != nil || rec.AcknowledgedAt != "" {
		t.Fatalf("dry-run acknowledged the receipt: %+v, %v", rec, err)
	}

	var applied bytes.Buffer
	if err := cmdDoctor(testCtx(t), []string{"--repair", "--yes", "--json"}, &applied, io.Discard); err != nil {
		t.Fatal(err)
	}
	var appliedReport doctorReport
	if err := json.Unmarshal(applied.Bytes(), &appliedReport); err != nil {
		t.Fatal(err)
	}
	verified := false
	for _, result := range appliedReport.Verification {
		if result.ActionID == "acknowledge_abandoned_ingest" {
			verified = result.Before == "failed" && result.After == "passed" && result.Verified &&
				len(result.Acknowledgements) == 1 &&
				result.Acknowledgements[0].Action == operation.AcknowledgementRecorded &&
				result.Acknowledgements[0].RunID == "op_reviewed"
		}
	}
	if !verified {
		t.Fatalf("verification = %+v", appliedReport.Verification)
	}
	// Evidence survives: no invented completion, counts and failure code intact.
	rec, err := operation.LoadRecord(path)
	if err != nil || rec.State != operationFailed || rec.FailureCode != operation.FailureOwnerAbandoned ||
		rec.Counts.Items != 9 || rec.AcknowledgedAt == "" {
		t.Fatalf("acknowledged receipt = %+v, %v", rec, err)
	}

	// The acknowledged receipt no longer reddens health, and doctor says why.
	var after bytes.Buffer
	if err := cmdDoctor(testCtx(t), nil, &after, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after.String(), "ok   operation_healthy:ingest:op_reviewed") {
		t.Fatalf("acknowledged receipt still red:\n%s", after.String())
	}
	if !strings.Contains(after.String(), "acknowledged "+rec.AcknowledgedAt+" by an operator") {
		t.Fatalf("acknowledgement not disclosed:\n%s", after.String())
	}

	// Idempotent: a second --repair --yes plans nothing and changes nothing.
	var rerun bytes.Buffer
	if err := cmdDoctor(testCtx(t), []string{"--repair", "--yes", "--json"}, &rerun, io.Discard); err != nil {
		t.Fatal(err)
	}
	var rerunReport doctorReport
	if err := json.Unmarshal(rerun.Bytes(), &rerunReport); err != nil {
		t.Fatal(err)
	}
	for _, action := range rerunReport.RepairPlan {
		if action.ID == "acknowledge_abandoned_ingest" {
			t.Fatalf("idempotent rerun still planned an acknowledgement: %+v", rerunReport.RepairPlan)
		}
	}
	if again, err := operation.LoadRecord(path); err != nil || again.AcknowledgedAt != rec.AcknowledgedAt {
		t.Fatalf("rerun re-stamped the receipt: %+v, %v", again, err)
	}
}

// A retirement and an acknowledgement never collapse into one approval: the
// receipts this run turns terminal must be reviewed before they can be cleared.
func TestDoctorNeverAcknowledgesAReceiptItJustRetired(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 9, 29, 15, 0, 0, 0, time.UTC)
	setDoctorClock(t, now)
	oldAlive := operationProcessAlive
	t.Cleanup(func() { operationProcessAlive = oldAlive })
	operationProcessAlive = func(int) bool { return false }

	orphan := operationRecord{
		SchemaVersion: operationSchemaVersion, Kind: operationKindIngest, State: operationRunning,
		RunID: "op_orphan", OwnerPID: 4242,
		StartedAt:   now.Add(-time.Hour).Format(time.RFC3339Nano),
		HeartbeatAt: now.Add(-operationHeartbeatTTL - time.Second).Format(time.RFC3339Nano),
		Phase:       "awaiting_rebuild", Counts: operationCounts{Items: 4, Examined: 6, Materialized: 4},
	}
	path := operation.Path(cfg, operationKindIngest, orphan.RunID)
	if err := saveOperationRecord(path, orphan); err != nil {
		t.Fatal(err)
	}
	// An uncovered journal path is what makes the retirement RETAIN the receipt as
	// terminal failed / owner_abandoned instead of removing it.
	published := filepath.Join(cfg.VaultDir, "orphan-fixture.md")
	if err := os.WriteFile(published, []byte("---\nid: orphan-fixture\ntitle: Orphan\ntype: note\n---\nBody.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(cfg.StateDir, "ingest", "filesystem", "journal.log")
	if err := os.MkdirAll(filepath.Dir(journal), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, []byte("run op_orphan "+now.Format(time.RFC3339)+"\n"+published+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := planDoctorRepairs(nil, cfg, "", now)
	for _, action := range plan {
		if action.ID == "acknowledge_abandoned_ingest" {
			t.Fatalf("planned an acknowledgement for a still-running receipt: %+v", plan)
		}
	}
	if _, err := applyDoctorRepairs(testCtx(t), cfg, plan); err != nil {
		t.Fatal(err)
	}
	rec, err := operation.LoadRecord(path)
	if err != nil || rec.State != operationFailed || rec.FailureCode != operation.FailureOwnerAbandoned {
		t.Fatalf("retirement = %+v, %v", rec, err)
	}
	if rec.AcknowledgedAt != "" {
		t.Fatal("the same run both retired and acknowledged the receipt")
	}
	// It becomes acknowledgeable only on the NEXT plan, after the operator can see it.
	next := false
	for _, action := range planDoctorRepairs(nil, cfg, "", now) {
		if action.ID == "acknowledge_abandoned_ingest" && action.Target == "op_orphan" {
			next = true
		}
	}
	if !next {
		t.Fatal("retained receipt never became acknowledgeable")
	}
}

// Read-only surfaces must not mutate, and must not be able to reach the
// acknowledge path at all without --repair and an explicit approval.
func TestDoctorAcknowledgeRequiresRepairApproval(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 9, 29, 15, 0, 0, 0, time.UTC)
	setDoctorClock(t, now)
	path := operation.Path(cfg, operationKindIngest, "op_guarded")
	saveAbandonedIngestReceipt(t, cfg, "op_guarded", now)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"--json"}, {"--strict"}, {"--strict", "--json"}, {"--pulse"}, {"--repair", "--dry-run", "--json"}} {
		// --strict/--pulse exit non-zero on an unhealthy vault; the mutation check is the point.
		_ = cmdDoctor(testCtx(t), args, io.Discard, io.Discard)
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("doctor %v mutated the receipt: %s, %v", args, after, err)
		}
	}
	// The other read-only surfaces that consume the same activities: the health
	// rollup every status/banner caller goes through, and `mora sync status`.
	if h := healthOf(cfg, now); h.State == "" {
		t.Fatal("healthOf returned no state")
	}
	_ = cmdSync(testCtx(t), []string{"status"}, io.Discard, io.Discard)
	afterReads, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, afterReads) {
		t.Fatalf("a read-only health/status surface mutated the receipt: %s, %v", afterReads, err)
	}
	if err := cmdDoctor(testCtx(t), []string{"--repair", "--json"}, io.Discard, io.Discard); err == nil {
		t.Fatal("--repair without --yes must refuse")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("refused repair mutated the receipt: %s, %v", after, err)
	}
}
