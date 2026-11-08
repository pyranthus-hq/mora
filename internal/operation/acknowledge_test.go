package operation

import (
	"os"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/config"
)

var acknowledgeTestNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// saveAbandoned writes a receipt in the state the uncovered-journal path retains:
// terminal failed with owner_abandoned. That state is permanently red (#498), so
// it is the only shape an operator acknowledgement may touch.
func saveAbandoned(t *testing.T, cfg config.Config, runID string, finished time.Time) Record {
	t.Helper()
	stamp := finished.UTC().Format(time.RFC3339Nano)
	rec := Record{
		SchemaVersion: SchemaVersion, Kind: KindIngest, State: Failed, RunID: runID, OwnerPID: 4242,
		StartedAt: finished.Add(-time.Hour).UTC().Format(time.RFC3339Nano), HeartbeatAt: stamp,
		FinishedAt: stamp, Phase: "awaiting_rebuild", Counts: Counts{Items: 9, Files: 3},
		FailureCode: FailureOwnerAbandoned,
	}
	if err := SaveRecord(Path(cfg, KindIngest, runID), rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestAcknowledgeAbandonedRoundTripPreservesEvidence(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	saveAbandoned(t, cfg, "op_abandoned", acknowledgeTestNow.Add(-2*time.Hour))
	saveAbandoned(t, cfg, "op_untouched", acknowledgeTestNow.Add(-3*time.Hour))

	pending, err := ListUnacknowledgedAbandoned(cfg, KindIngest, acknowledgeTestNow)
	if err != nil || len(pending) != 2 || pending[0].RunID != "op_abandoned" || pending[0].Counts.Items != 9 {
		t.Fatalf("unacknowledged = %+v, %v", pending, err)
	}
	// Listing is a read-only planning seam.
	if rec, err := LoadRecord(Path(cfg, KindIngest, "op_abandoned")); err != nil || rec.AcknowledgedAt != "" {
		t.Fatalf("list mutated receipt: %+v, %v", rec, err)
	}

	acks, err := AcknowledgeAbandoned(cfg, KindIngest, acknowledgeTestNow, []string{"op_abandoned"})
	if err != nil || len(acks) != 1 || acks[0].Action != AcknowledgementRecorded || acks[0].Counts.Items != 9 {
		t.Fatalf("acknowledgement = %+v, %v", acks, err)
	}
	// No invented completion: the failure and its evidence survive verbatim.
	rec, err := LoadRecord(Path(cfg, KindIngest, "op_abandoned"))
	if err != nil || rec.State != Failed || rec.FailureCode != FailureOwnerAbandoned ||
		rec.Phase != "awaiting_rebuild" || rec.Counts != (Counts{Items: 9, Files: 3}) {
		t.Fatalf("acknowledgement rewrote evidence: %+v, %v", rec, err)
	}
	if rec.AcknowledgedAt != acknowledgeTestNow.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("acknowledged_at = %q", rec.AcknowledgedAt)
	}
	// An unnamed receipt is never acknowledged as a side effect.
	if other, err := LoadRecord(Path(cfg, KindIngest, "op_untouched")); err != nil || other.AcknowledgedAt != "" {
		t.Fatalf("unplanned receipt acknowledged: %+v, %v", other, err)
	}

	// Idempotent apply: the second pass reports the ORIGINAL stamp, never re-stamps.
	again, err := AcknowledgeAbandoned(cfg, KindIngest, acknowledgeTestNow.Add(time.Hour), []string{"op_abandoned"})
	if err != nil || len(again) != 1 || again[0].Action != AcknowledgementAlready || again[0].AcknowledgedAt != rec.AcknowledgedAt {
		t.Fatalf("repeat acknowledgement = %+v, %v", again, err)
	}
	if reloaded, err := LoadRecord(Path(cfg, KindIngest, "op_abandoned")); err != nil || reloaded.AcknowledgedAt != rec.AcknowledgedAt {
		t.Fatalf("repeat acknowledgement re-stamped: %+v, %v", reloaded, err)
	}
	pending, err = ListUnacknowledgedAbandoned(cfg, KindIngest, acknowledgeTestNow)
	if err != nil || len(pending) != 1 || pending[0].RunID != "op_untouched" {
		t.Fatalf("after acknowledgement unacknowledged = %+v, %v", pending, err)
	}
}

func TestAcknowledgedAbandonedStopsReddeningAndRejoinsRetention(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	saveAbandoned(t, cfg, "op_reviewed", acknowledgeTestNow.Add(-2*time.Hour))
	saveAbandoned(t, cfg, "op_unreviewed", acknowledgeTestNow.Add(-2*time.Hour))
	if _, err := AcknowledgeAbandoned(cfg, KindIngest, acknowledgeTestNow, []string{"op_reviewed"}); err != nil {
		t.Fatal(err)
	}
	dead := func(int) bool { return false }
	reviewed, unreviewed := false, false
	for _, a := range Activities(cfg, acknowledgeTestNow.Add(time.Minute), dead) {
		switch a.RunID {
		case "op_reviewed":
			reviewed = true
			// Still truthfully failed, but no longer convicting health.
			if a.State != Failed || a.FailureCode != FailureOwnerAbandoned || !a.Acknowledged() {
				t.Fatalf("reviewed activity = %+v", a)
			}
		case "op_unreviewed":
			unreviewed = true
			if a.Acknowledged() {
				t.Fatalf("unreviewed activity acknowledged: %+v", a)
			}
		}
	}
	if !reviewed || !unreviewed {
		t.Fatal("both receipts must stay visible as evidence")
	}

	// Bounded retention: TerminalKeep+1 acknowledged receipts age out, the
	// unacknowledged one never does.
	for i := 0; i <= TerminalKeep; i++ {
		runID := "op_bulk_" + string(rune('a'+i))
		saveAbandoned(t, cfg, runID, acknowledgeTestNow.Add(-time.Duration(i)*time.Minute))
		if _, err := AcknowledgeAbandoned(cfg, KindIngest, acknowledgeTestNow, []string{runID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(Path(cfg, KindIngest, "op_unreviewed")); err != nil {
		t.Fatalf("unacknowledged receipt aged out: %v", err)
	}
	remaining := 0
	dir := Path(cfg, KindIngest, "x")
	entries, err := os.ReadDir(dir[:len(dir)-len("x.json")])
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() { // the per-kind lease guard lives in its own subdirectory
			remaining++
		}
	}
	// 18 acknowledged receipts were written; retention keeps TerminalKeep of them,
	// plus the unacknowledged one that is exempt from aging out.
	if remaining != TerminalKeep+1 {
		t.Fatalf("acknowledged receipts not bounded: %d receipts, want %d", remaining, TerminalKeep+1)
	}
}

// Acknowledging one receipt must never delete an unrelated one from Activities.
// owner_abandoned receipts do not compete for the newest-terminal-per-kind slot,
// acknowledged or not: an acknowledged one is green, so anything it displaced would
// disappear from doctor and stop counting in AggregateState — silencing an older,
// unreviewed ordinary failure nobody ever looked at.
func TestAcknowledgementNeverEvictsAnUnrelatedTerminalReceipt(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	finished := acknowledgeTestNow.Add(-3 * time.Hour)
	stamp := finished.UTC().Format(time.RFC3339Nano)
	ordinary := Record{
		SchemaVersion: SchemaVersion, Kind: KindIngest, State: Failed, RunID: "op_diskfull", OwnerPID: 4242,
		StartedAt: finished.Add(-time.Hour).UTC().Format(time.RFC3339Nano), HeartbeatAt: stamp,
		FinishedAt: stamp, Phase: "ingesting", Counts: Counts{Items: 2, Examined: 5, Materialized: 2},
		FailureCode: "disk_full",
	}
	if err := SaveRecord(Path(cfg, KindIngest, ordinary.RunID), ordinary); err != nil {
		t.Fatal(err)
	}
	// Newer than the ordinary failure, so it would win the slot if it competed.
	saveAbandoned(t, cfg, "op_abandoned", acknowledgeTestNow.Add(-time.Hour))
	if _, err := AcknowledgeAbandoned(cfg, KindIngest, acknowledgeTestNow, []string{"op_abandoned"}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]Activity{}
	for _, a := range Activities(cfg, acknowledgeTestNow.Add(time.Minute), func(int) bool { return false }) {
		seen[a.RunID] = a
	}
	if _, ok := seen["op_diskfull"]; !ok {
		t.Fatalf("acknowledging op_abandoned silenced an unreviewed failure: %+v", seen)
	}
	if seen["op_diskfull"].Acknowledged() {
		t.Fatal("an ordinary failure must never read as acknowledged")
	}
	if !seen["op_abandoned"].Acknowledged() {
		t.Fatalf("acknowledged receipt = %+v", seen["op_abandoned"])
	}
}

// PruneTerminal must read an acknowledgement the same way health does. A stamp
// health rejects as corrupt cannot buy the receipt into bounded retention, or a
// record doctor still shows red would age off disk underneath it.
func TestPruneTerminalIgnoresAnIncoherentAcknowledgement(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	for i := 0; i <= TerminalKeep; i++ {
		saveAbandoned(t, cfg, "op_bulk_"+string(rune('a'+i)), acknowledgeTestNow.Add(-time.Duration(i+2)*time.Hour))
		if _, err := AcknowledgeAbandoned(cfg, KindIngest, acknowledgeTestNow, []string{"op_bulk_" + string(rune('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	// Oldest of all, so retention would drop it first if the stamp counted.
	tampered := saveAbandoned(t, cfg, "op_tampered", acknowledgeTestNow.Add(-24*time.Hour))
	tampered.AcknowledgedAt = "yesterday" // classifyOperationRecord: invalid_timestamp
	path := Path(cfg, KindIngest, tampered.RunID)
	if err := SaveRecord(path, tampered); err != nil {
		t.Fatal(err)
	}
	PruneTerminal(cfg, KindIngest)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a receipt health calls corrupt was pruned as acknowledged history: %v", err)
	}
	acts := Activities(cfg, acknowledgeTestNow, func(int) bool { return false })
	for _, a := range acts {
		if a.RunID == tampered.RunID && (a.FailureCode != "invalid_timestamp" || a.Acknowledged()) {
			t.Fatalf("tampered receipt = %+v", a)
		}
	}
}

func TestAcknowledgeAbandonedRefusesEverythingElse(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	if _, err := AcknowledgeAbandoned(cfg, KindIngest, acknowledgeTestNow, nil); err == nil {
		t.Fatal("acknowledging without explicit run ids must fail: never automatic, never age-based")
	}
	if _, err := AcknowledgeAbandoned(cfg, KindIngest, acknowledgeTestNow, []string{"../escape"}); err == nil {
		t.Fatal("invalid run id accepted")
	}
	if _, err := AcknowledgeAbandoned(config.Config{StateDir: "relative"}, KindIngest, acknowledgeTestNow, []string{"op_x"}); err == nil {
		t.Fatal("relative state dir accepted")
	}
	if _, err := AcknowledgeAbandoned(cfg, Kind("bogus"), acknowledgeTestNow, []string{"op_x"}); err == nil {
		t.Fatal("invalid kind accepted")
	}
	// A missing receipt is benign (bounded retention may have removed it), but a
	// receipt that is not a terminal owner_abandoned failure is left untouched.
	acks, err := AcknowledgeAbandoned(cfg, KindIngest, acknowledgeTestNow, []string{"op_absent"})
	if err != nil || len(acks) != 0 {
		t.Fatalf("missing receipt = %+v, %v", acks, err)
	}
	for _, variant := range []struct {
		name string
		rec  Record
	}{
		{"running", Record{SchemaVersion: SchemaVersion, Kind: KindIngest, State: Running, RunID: "op_live", OwnerPID: os.Getpid(),
			StartedAt: acknowledgeTestNow.Format(time.RFC3339Nano), HeartbeatAt: acknowledgeTestNow.Format(time.RFC3339Nano), Phase: "fetching"}},
		{"ordinary_failure", Record{SchemaVersion: SchemaVersion, Kind: KindIngest, State: Failed, RunID: "op_live", OwnerPID: 1,
			StartedAt: acknowledgeTestNow.Format(time.RFC3339Nano), HeartbeatAt: acknowledgeTestNow.Format(time.RFC3339Nano),
			FinishedAt: acknowledgeTestNow.Format(time.RFC3339Nano), Phase: "fetching", FailureCode: "operation_failed"}},
		{"completed", Record{SchemaVersion: SchemaVersion, Kind: KindIngest, State: Completed, RunID: "op_live", OwnerPID: 1,
			StartedAt: acknowledgeTestNow.Format(time.RFC3339Nano), HeartbeatAt: acknowledgeTestNow.Format(time.RFC3339Nano),
			FinishedAt: acknowledgeTestNow.Format(time.RFC3339Nano), Phase: "journal_retired"}},
	} {
		t.Run(variant.name, func(t *testing.T) {
			path := Path(cfg, KindIngest, variant.rec.RunID)
			if err := SaveRecord(path, variant.rec); err != nil {
				t.Fatal(err)
			}
			if pending, err := ListUnacknowledgedAbandoned(cfg, KindIngest, acknowledgeTestNow); err != nil || len(pending) != 0 {
				t.Fatalf("planned a non-abandoned receipt: %+v, %v", pending, err)
			}
			acks, err := AcknowledgeAbandoned(cfg, KindIngest, acknowledgeTestNow, []string{variant.rec.RunID})
			if err != nil || len(acks) != 0 {
				t.Fatalf("acknowledged a non-abandoned receipt: %+v, %v", acks, err)
			}
			got, err := LoadRecord(path)
			if err != nil || got != variant.rec {
				t.Fatalf("receipt mutated: %+v, %v", got, err)
			}
		})
	}
}

func TestAcknowledgedAtOnlyCoherentOnAbandonedFailure(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	stamp := acknowledgeTestNow.Format(time.RFC3339Nano)
	for _, tc := range []struct {
		name string
		mut  func(*Record)
		want string
	}{
		{"completed_cannot_be_acknowledged", func(r *Record) {
			r.State, r.FailureCode, r.Phase = Completed, "", "journal_retired"
		}, "incoherent_state"},
		{"ordinary_failure_cannot_be_acknowledged", func(r *Record) {
			r.FailureCode = "operation_failed"
		}, "incoherent_state"},
		{"running_cannot_be_acknowledged", func(r *Record) {
			r.State, r.FinishedAt, r.FailureCode, r.Phase = Running, "", "", "fetching"
		}, "incoherent_state"},
		{"review_cannot_predate_the_failure", func(r *Record) {
			r.AcknowledgedAt = acknowledgeTestNow.Add(-4 * time.Hour).Format(time.RFC3339Nano)
		}, "invalid_timestamp"},
		{"review_cannot_come_from_the_future", func(r *Record) {
			r.AcknowledgedAt = acknowledgeTestNow.Add(2 * time.Hour).Format(time.RFC3339Nano)
		}, "invalid_timestamp"},
		{"review_must_parse", func(r *Record) { r.AcknowledgedAt = "yesterday" }, "invalid_timestamp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := saveAbandoned(t, cfg, "op_coherence", acknowledgeTestNow.Add(-time.Hour))
			rec.AcknowledgedAt = stamp
			tc.mut(&rec)
			if err := SaveRecord(Path(cfg, KindIngest, rec.RunID), rec); err != nil {
				t.Fatal(err)
			}
			acts := Activities(cfg, acknowledgeTestNow, func(int) bool { return false })
			if len(acts) != 1 || acts[0].FailureCode != tc.want || acts[0].Acknowledged() {
				t.Fatalf("activity = %+v, want failure code %q", acts, tc.want)
			}
		})
	}
}
