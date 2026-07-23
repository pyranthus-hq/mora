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

	"github.com/pyranthus-hq/mora/internal/mora/exam"
)

const examFlywheelFixturePath = "exam/testdata/flywheel-predictions.json"

type examFlywheelFixture struct {
	Pre                []exam.Prediction `json:"pre"`
	Post               []exam.Prediction `json:"post"`
	PreScorecard       exam.Scorecard    `json:"pre_scorecard"`
	PostScorecard      exam.Scorecard    `json:"post_scorecard"`
	GoldCount          int               `json:"gold_count"`
	FlywheelRecallGain float64           `json:"flywheel_recall_gain"`
}

func runExamEventCLI(t *testing.T, eventID string, at time.Time) MeetingBrief {
	t.Helper()
	body := runExamCLI(t, "brief", "--event-id", eventID, "--at", at.Format(time.RFC3339), "--json")
	var brief MeetingBrief
	if err := json.Unmarshal([]byte(body), &brief); err != nil {
		t.Fatalf("decode event CLI brief: %v\n%s", err, body)
	}
	return brief
}

func meetingGoldCount(ledger exam.Ledger) int {
	count := 0
	for _, commitment := range ledger.Commitments {
		for _, surface := range commitment.ExpectedIn {
			if strings.HasPrefix(surface, exam.SurfaceMeeting+":") {
				count++
				break
			}
		}
	}
	return count
}

func flywheelVerdict(verdicts []exam.Verdict) (exam.Verdict, bool) {
	for _, verdict := range verdicts {
		if verdict.Label == "c/flywheel" {
			return verdict, true
		}
	}
	return exam.Verdict{}, false
}

func seedRenderedExamLedger(t *testing.T, ledger exam.Ledger) (examEventFixture, time.Time) {
	t.Helper()
	files, err := exam.Render(ledger)
	if err != nil {
		t.Fatal(err)
	}
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	event, at := loadExamEvent(t)
	if err := saveSources(cfg, []Source{{Name: "gmail", Type: "gmail", Email: "alex@example.com", Enabled: ptr(true), CreatedAt: "2026-07-01T00:00:00Z"}}); err != nil {
		t.Fatal(err)
	}
	for rel, body := range files {
		path := filepath.Join(cfg.VaultDir, filepath.FromSlash(strings.TrimPrefix(rel, "vault/")))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	return event, at
}

// TestExamCorrectionFlywheel establishes both graph states through real CLI
// surfaces over the same corpus. The only state change is the durable governance
// entry written by `mora merge confirm`, which rebuilds the graph itself.
func TestExamCorrectionFlywheel(t *testing.T) {
	_, event, at := seedExamHome(t)
	pinExamSurfaceClocks(t, at)
	ledger := loadExamLedger(t)
	gold := meetingGoldCount(ledger)
	if gold == 0 {
		t.Fatal("flywheel surface has no gold commitments")
	}

	prePredictions := examMeetingPredictions(runExamEventCLI(t, event.EventID, at))
	preScorecard := scoreExamSurface(t, ledger, prePredictions, exam.SurfaceMeeting)
	preVerdicts, err := exam.Classify(ledger, prePredictions, exam.SurfaceMeeting)
	if err != nil {
		t.Fatal(err)
	}
	preFlywheel, ok := flywheelVerdict(preVerdicts)
	if !ok {
		t.Fatal("EVAL_BROKEN: pre-merge report has no c/flywheel verdict")
	}
	if preFlywheel.Kind == exam.VerdictTruePositive {
		t.Fatal("EVAL_BROKEN: c/flywheel already scores correctly before merge; the confirmation is doing no work")
	}
	if preFlywheel.Kind != exam.VerdictGoldMiss {
		t.Fatalf("pre-merge c/flywheel verdict = %q, want identity-gap gold_miss", preFlywheel.Kind)
	}
	if preScorecard.CriticalIdentity != 0 {
		t.Fatalf("pre-merge CriticalIdentity = %d, want 0: Mora must gap rather than misattribute", preScorecard.CriticalIdentity)
	}

	runExamCLI(t, "merge", "confirm", "--handle", "+15550100137", "--email", "dana@example.net")

	postPredictions := examMeetingPredictions(runExamEventCLI(t, event.EventID, at))
	postScorecard := scoreExamSurface(t, ledger, postPredictions, exam.SurfaceMeeting)
	postVerdicts, err := exam.Classify(ledger, postPredictions, exam.SurfaceMeeting)
	if err != nil {
		t.Fatal(err)
	}
	postFlywheel, ok := flywheelVerdict(postVerdicts)
	if !ok || postFlywheel.Kind != exam.VerdictTruePositive {
		t.Fatalf("post-merge c/flywheel verdict = %+v, want true_positive\npredictions=%+v", postFlywheel, postPredictions)
	}
	if postFlywheel.MemoryID != "imessage_chat/exam-flywheel" {
		t.Fatalf("post-merge c/flywheel citation = %q, want iMessage evidence", postFlywheel.MemoryID)
	}
	if postScorecard.CriticalIdentity != 0 || postScorecard.CitationCorrect.Precision != 1 {
		t.Fatalf("post-merge attribution/grounding = CriticalIdentity %d, CitationCorrect %+v", postScorecard.CriticalIdentity, postScorecard.CitationCorrect)
	}
	if postScorecard.Owner.Precision != 1 ||
		postScorecard.Direction.Precision != 1 ||
		!postScorecard.DirectionScorable || postScorecard.CriticalDirection != 0 {
		t.Fatalf("post-merge owner/direction = Owner %+v Direction %+v scorable=%v CriticalDirection=%d, want typed precision 1.0, true, 0",
			postScorecard.Owner, postScorecard.Direction, postScorecard.DirectionScorable, postScorecard.CriticalDirection)
	}

	wantGain := 1 / float64(gold)
	gotGain := postScorecard.Extraction.Recall - preScorecard.Extraction.Recall
	if !(exam.Check{Op: exam.OpEq, Want: wantGain}).Holds(gotGain) {
		t.Fatalf("flywheel recall gain = %v, want exactly 1/%d = %v", gotGain, gold, wantGain)
	}

	fixture := examFlywheelFixture{
		Pre:                prePredictions,
		Post:               postPredictions,
		PreScorecard:       preScorecard,
		PostScorecard:      postScorecard,
		GoldCount:          gold,
		FlywheelRecallGain: wantGain,
	}
	assertExamFlywheelFixture(t, fixture)
}

// TestExamConversationCommitmentNotLastIsKnownRed keeps #156's former known-red
// scenario as a permanent regression contract: every turn is classified under
// its own speaker even when a later acknowledgement ends the conversation.
func TestExamConversationCommitmentNotLastIsKnownRed(t *testing.T) {
	const (
		wantRED = false
		issue   = "https://github.com/pyranthus-hq/mora/issues/156"
		expires = "2026-10-14"
	)
	ledger := loadExamLedger(t)
	found := false
	for i := range ledger.Artifacts {
		artifact := &ledger.Artifacts[i]
		if artifact.ID != "a/imessage-flywheel" {
			continue
		}
		if len(artifact.Messages) != 2 {
			t.Fatalf("flywheel conversation has %d messages, want 2", len(artifact.Messages))
		}
		artifact.Messages[0], artifact.Messages[1] = artifact.Messages[1], artifact.Messages[0]
		artifact.Messages[0].At = "2026-07-13T19:00:00Z"
		artifact.Messages[1].At = "2026-07-13T19:05:00Z"
		found = true
		break
	}
	if !found {
		t.Fatal("flywheel conversation not found")
	}
	event, at := seedRenderedExamLedger(t, ledger)
	pinExamSurfaceClocks(t, at)
	runExamCLI(t, "merge", "confirm", "--handle", "+15550100137", "--email", "dana@example.net")
	preds := examMeetingPredictions(runExamEventCLI(t, event.EventID, at))
	verdicts, err := exam.Classify(ledger, preds, exam.SurfaceMeeting)
	if err != nil {
		t.Fatal(err)
	}
	verdict, ok := flywheelVerdict(verdicts)
	if !ok {
		t.Fatal("EVAL_BROKEN: non-last commitment has no scorer verdict")
	}
	passes := verdict.Kind == exam.VerdictTruePositive
	if wantRED {
		if passes {
			t.Fatalf("FIXED: iMessage earlier-turn commitment now surfaces; flip wantRED:false and close %s", issue)
		}
		if verdict.Kind != exam.VerdictGoldMiss {
			t.Fatalf("known-red earlier-turn verdict = %q, want gold_miss", verdict.Kind)
		}
		t.Logf("known RED through %s: commitment-not-last remains a miss; tracked by %s", expires, issue)
		return
	}
	if !passes {
		t.Fatalf("earlier-turn commitment verdict = %q, want true_positive", verdict.Kind)
	}
	var typed exam.Prediction
	for _, prediction := range preds {
		if prediction.MemoryID == "imessage_chat/exam-flywheel" {
			typed = prediction
			break
		}
	}
	if typed.Direction != exam.DirectionOwedByCounterparty || typed.Owner != "address:dana@example.net" {
		t.Fatalf("earlier-turn owner/direction = %q/%q, want counterparty Dana", typed.Owner, typed.Direction)
	}
}

func assertExamFlywheelFixture(t *testing.T, got examFlywheelFixture) {
	t.Helper()
	want, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	path := filepath.FromSlash(examFlywheelFixturePath)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read flywheel fixture: %v", err)
	}
	if !bytes.Equal(committed, want) {
		t.Fatalf("flywheel typed scorecards drifted; inspect the graph change, then run go test ./internal/mora -run TestExamCorrectionFlywheel -update\n got:\n%s\nwant:\n%s", committed, want)
	}
}
