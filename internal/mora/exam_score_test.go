package mora

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/mora/exam"
)

// The adapters. They are the ONLY Mora-aware code in the exam, they are test-only,
// and they do no cleaning and no interpretation — they read the payload and stop.
// Every transformation belongs to exam.Score, which lives in a package that
// structurally cannot reach meetingbrief.go's cleaners and therefore cannot score
// the implementation against itself.

func examMeetingPredictions(b MeetingBrief) []exam.Prediction {
	var out []exam.Prediction
	for _, section := range b.Sections {
		for _, line := range section.Lines {
			atom := line.Correction.AttendeeAtom
			out = append(out, exam.Prediction{
				Surface:      exam.SurfaceMeeting,
				Text:         line.Text,
				Attendee:     line.Attendee,
				AttendeeAtom: atom.Kind + ":" + atom.Value,
				MemoryID:     line.Citation.MemoryID(),
				SectionKind:  section.Kind,
				// Nothing on the meeting payload carries a direction, a due time, a
				// lifecycle state or a closure link. The adapter says so in the type
				// rather than guessing from the section a line landed in — the brief
				// collapses self-authored commitments and inbound requests into the
				// same lane, so placement is not direction.
				Direction: exam.Unknown,
				Lifecycle: exam.Unknown,
			})
		}
	}
	return out
}

// digestAllItems walks the urgent shelf first and then every section, in render
// order. Urgent items are LIFTED OUT of their sections, so a scorer that only walked
// Sections would silently miss the items the product considers most important.
func digestAllItems(d Digest) []DigestItem {
	items := append([]DigestItem(nil), d.Urgent...)
	for _, section := range d.Sections {
		items = append(items, section.Items...)
	}
	return items
}

// examDailyPredictions scores the daily surface at ARTIFACT grain: a DigestItem is a
// title+snippet projection of a memory, not an evidence quote, and DigestItem.ID is
// the documented citation.
func examDailyPredictions(d Digest) []exam.Prediction {
	var out []exam.Prediction
	for _, item := range digestAllItems(d) {
		out = append(out, exam.Prediction{
			Surface:   exam.SurfaceDaily,
			Text:      renderDigestItemLine(item),
			MemoryID:  item.ID,
			Direction: exam.Unknown,
			Lifecycle: exam.Unknown,
		})
	}
	return out
}

const examRealPredictionsPath = "exam/testdata/real-predictions.json"

func examSurfaces(t *testing.T) map[string][]exam.Prediction {
	t.Helper()
	cfg, event, at := seedExamHome(t)
	brief, err := buildEventMeetingBrief(context.Background(), cfg, event.EventID, at, 0, meetingBriefDefaultPerGuest)
	if err != nil {
		t.Fatal(err)
	}
	uncapped, err := buildEventMeetingBrief(context.Background(), cfg, event.EventID, at, 0, examUncappedPerGuest)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := buildDigest(cfg, at, briefOpts{sinceHours: examDailyWindowHours, perSourceCap: examDailyPerSourceCap})
	if err != nil {
		t.Fatal(err)
	}
	return map[string][]exam.Prediction{
		exam.SurfaceMeeting: examMeetingPredictions(brief),
		exam.SurfaceDaily:   examDailyPredictions(digest),
		"meeting_uncapped":  examMeetingPredictions(uncapped),
	}
}

const (
	// examUncappedPerGuest raises the ranker's per-guest cap so RecallUncapped
	// measures the extractor rather than meetingBriefDefaultPerGuest.
	examUncappedPerGuest = 25
	// The daily surface is scored through the EXPLICIT WINDOW path. The delta path
	// suppresses the backfill flood on a cold start, so a fresh exam vault scored
	// through it yields zero items and a vacuous scorecard.
	examDailyWindowHours  = 24 * 30
	examDailyPerSourceCap = 50
)

// TestExamRealPredictionsPin is what makes the red team's "mutate the REAL output"
// rows honest. package exam cannot import package mora, so the engine's predictions
// reach it as a committed fixture — and this test, which runs the REAL builders, is
// what proves the fixture is still the engine's output. A helper unit test is not a
// call-site guard; this is the guard.
func TestExamRealPredictionsPin(t *testing.T) {
	surfaces := examSurfaces(t)
	pinned := map[string][]exam.Prediction{
		exam.SurfaceMeeting: surfaces[exam.SurfaceMeeting],
		exam.SurfaceDaily:   surfaces[exam.SurfaceDaily],
	}
	want, err := json.MarshalIndent(pinned, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	path := filepath.FromSlash(examRealPredictionsPath)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("EVAL_BROKEN: the red team scores a fixture of the real output, and it is missing: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("EVAL_BROKEN: the engine's output no longer matches the fixture the red team mutates.\n"+
			"Every row (e)/(g)/(j)/(n)/(q)/(r) is now scoring a stale brief.\n"+
			"Re-run: go test ./internal/mora -run TestExamRealPredictionsPin -update\n\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestExamRealEngineScorecard is the measured, honest scoreboard against today's
// engine. It asserts the SHAPE of the truth (which gold items the product finds,
// which labelled negatives it leaks) — never a threshold. Report-only: PR 4's audit
// is what turns a measurement into a floor.
func TestExamRealEngineScorecard(t *testing.T) {
	l := loadExamLedger(t)
	surfaces := examSurfaces(t)

	meeting, err := exam.Score(l, surfaces[exam.SurfaceMeeting], exam.SurfaceMeeting)
	if err != nil {
		t.Fatal(err)
	}
	meeting, err = exam.WithRecallUncapped(meeting, l, surfaces["meeting_uncapped"], exam.SurfaceMeeting)
	if err != nil {
		t.Fatal(err)
	}
	if meeting.LooseMatches != 0 || meeting.Unmatched != 0 {
		t.Fatalf("EVAL_BROKEN: meeting LooseMatches=%d Unmatched=%d, want 0/0 — the ledger or the match predicate is wrong, and the audit does not start",
			meeting.LooseMatches, meeting.Unmatched)
	}
	if meeting.Owner.Defined || meeting.Owner.Recall != 0 {
		t.Errorf("Owner = %+v, want fail-closed until the product payload carries a direct owner", meeting.Owner)
	}
	if exam.RunStateOf(meeting, nil) != exam.StateScoredFailure {
		t.Errorf("meeting run state = %q; a clean first score is a defect report against the ledger, not a win",
			exam.RunStateOf(meeting, nil))
	}
	// The two live defects the corpus catches today. They are ASSERTED, not merely
	// reported: if a change makes them disappear without anyone flipping these
	// expectations, the corpus has stopped exercising the engine.
	if meeting.NonObligationLeaks == 0 {
		t.Error("the engine used to surface a labelled non-obligation as an open loop; if it no longer does, flip this expectation and raise the floor")
	}
	if meeting.ClosedLeaks == 0 {
		t.Error("the engine used to surface a CLOSED obligation as an open loop; if it no longer does, flip this expectation and raise the floor")
	}
	if meeting.CriticalIdentity != 0 {
		t.Errorf("CriticalIdentity = %d, want 0 — Mora GAPS, it does not misattribute (#135)", meeting.CriticalIdentity)
	}
	if meeting.CitationCoverage.Precision != 1 {
		t.Errorf("meeting citation coverage = %v, want 1.0 — the validated path cannot ship an uncited line", meeting.CitationCoverage.Precision)
	}
	if meeting.RecallUncapped.Recall < meeting.Extraction.Recall {
		t.Errorf("RecallUncapped %v is below Recall %v, which is impossible", meeting.RecallUncapped.Recall, meeting.Extraction.Recall)
	}

	daily, err := exam.Score(l, surfaces[exam.SurfaceDaily], exam.SurfaceDaily)
	if err != nil {
		t.Fatal(err)
	}
	if daily.LooseMatches != 0 || daily.Unmatched != 0 {
		t.Fatalf("EVAL_BROKEN: daily LooseMatches=%d Unmatched=%d, want 0/0", daily.LooseMatches, daily.Unmatched)
	}
	if daily.Counterparty.Defined {
		t.Error("Counterparty is scored on the daily surface, which carries no attendee — it must be reported N/A, never folded in")
	}
	for name, row := range map[string]exam.PR{
		"direction": daily.Direction, "due_time": daily.DueTime,
		"lifecycle": daily.Lifecycle, "closure_linkage": daily.ClosureLinkage,
	} {
		if row.Recall != 0 || row.Defined {
			t.Errorf("daily %s = %+v, want born-red — no obligation lane exists on the digest", name, row)
		}
	}
}

// TestExamAuthoredToQuotedDisappearsFromTheRealBrief is red-team row (o) driven
// through the WHOLE machine: re-render the corpus with the gold block wrapped in
// connector-surviving quoted form, re-index, re-run the real brief. The obligation
// must actually disappear — this is senderAuthoredBody's scored consequence, proved
// against the engine rather than simulated in the scorer.
func TestExamAuthoredToQuotedDisappearsFromTheRealBrief(t *testing.T) {
	l := loadExamLedger(t)
	before := examScoreRenderedLedger(t, l)

	quoted := examQuoteGoldBlockBytes(t, l)
	after := examScoreRenderedLedger(t, quoted)

	gold := 0
	for _, c := range l.Commitments {
		for _, in := range c.ExpectedIn {
			if strings.HasPrefix(in, exam.SurfaceMeeting+":") {
				gold++
			}
		}
	}
	want := before.Extraction.Recall - 1/float64(gold)
	if diff := after.Extraction.Recall - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("EVAL_BROKEN: quoting the gold block moved recall to %v, want exactly %v (= %v - 1/%d).\n"+
			"If recall did not fall, senderAuthoredBody is no longer excluding quoted text and the exclusion gate is dead.",
			after.Extraction.Recall, want, before.Extraction.Recall, gold)
	}
	if after.NonObligationLeaks != before.NonObligationLeaks {
		t.Errorf("quoting a gold block changed NonObligationLeaks %d -> %d; it should only remove an obligation",
			before.NonObligationLeaks, after.NonObligationLeaks)
	}
}

// examQuoteGoldBlockBytes wraps the gold block in the form the real Gmail connector
// LEAVES BEHIND (an "-----Original Message-----" preamble), not the form it strips
// (a ">"-prefixed line) — the corpus must stay a fixed point of the real mapper, so
// the mutation has to survive the connector to be worth anything.
func examQuoteGoldBlockBytes(t *testing.T, l exam.Ledger) exam.Ledger {
	t.Helper()
	quoted := exam.QuoteGoldBlock(l, exam.SurfaceMeeting)
	for ai, a := range quoted.Artifacts {
		for mi, m := range a.Messages {
			for bi, b := range m.Body {
				if b.Kind != "quoted_reply" || b.Kind == l.Artifacts[ai].Messages[mi].Body[bi].Kind {
					continue
				}
				quoted.Artifacts[ai].Messages[mi].Body[bi].Text = "-----Original Message-----\n" + b.Text
				return quoted
			}
		}
	}
	t.Fatal("no gold block was reclassified; the mutant is a no-op")
	return quoted
}

func examScoreRenderedLedger(t *testing.T, l exam.Ledger) exam.Scorecard {
	t.Helper()
	files, err := exam.Render(l)
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
	brief, err := buildEventMeetingBrief(context.Background(), cfg, event.EventID, at, 0, meetingBriefDefaultPerGuest)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := exam.Score(l, examMeetingPredictions(brief), exam.SurfaceMeeting)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// TestObligationScoreReport emits the audit's input: one row per surfaced line and
// one per gold obligation the product never found, with the scorer's verdict. It
// asserts DETERMINISM, not quality — the number this produces is not a threshold
// until a human has adjudicated every row of it.
func TestObligationScoreReport(t *testing.T) {
	l := loadExamLedger(t)
	surfaces := examSurfaces(t)
	var rows []string
	for _, surface := range []string{exam.SurfaceDaily, exam.SurfaceMeeting} {
		verdicts, err := exam.Classify(l, surfaces[surface], surface)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range verdicts {
			rows = append(rows, strings.Join([]string{v.Surface, v.Kind, v.Label, v.MemoryID, v.Text}, "\t"))
		}
	}
	report := "surface\tverdict\tledger_case\tmemory_id\ttext\n" + strings.Join(rows, "\n") + "\n"

	path := filepath.Join(examFixtureRoot, "report.golden.tsv")
	if *update {
		if err := os.WriteFile(path, []byte(report), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report golden: %v", err)
	}
	if string(got) != report {
		t.Fatalf("report drift; run go test ./internal/mora -run TestObligationScoreReport -update\ngot:\n%s\nwant:\n%s", got, report)
	}
	second, err := exam.Classify(l, surfaces[exam.SurfaceMeeting], exam.SurfaceMeeting)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(second) != fmt.Sprint(mustClassify(t, l, surfaces[exam.SurfaceMeeting], exam.SurfaceMeeting)) {
		t.Fatal("Classify is not deterministic across runs")
	}
}

func mustClassify(t *testing.T, l exam.Ledger, preds []exam.Prediction, surface string) []exam.Verdict {
	t.Helper()
	v, err := exam.Classify(l, preds, surface)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestUnwrapEmittedHandlesTheRealPresentationLayer pins the ONE piece of production
// formatting the scorer has to understand. The emitted line is a presentation string
// — "~3 days ago · Sam — “<evidence>”" — not the source text, and a scorer that got
// this wrong would report a corpus-wide extraction failure that does not exist.
func TestUnwrapEmittedHandlesTheRealPresentationLayer(t *testing.T) {
	surfaces := examSurfaces(t)
	l := loadExamLedger(t)
	quotes := map[string]bool{}
	for _, c := range l.Commitments {
		quotes[c.OpenedBy.Quote] = true
	}
	for _, n := range l.NonObligations {
		quotes[n.Span.Quote] = true
	}
	for _, p := range surfaces[exam.SurfaceMeeting] {
		if !strings.Contains(p.Text, "“") || !strings.HasSuffix(p.Text, "”") {
			t.Fatalf("emitted line %q no longer carries the dated-historical rail the scorer unwraps", p.Text)
		}
		if !quotes[exam.UnwrapEmitted(p.Text)] {
			t.Errorf("UnwrapEmitted(%q) = %q, which is not a ledger span — the unwrap or the ledger is wrong",
				p.Text, exam.UnwrapEmitted(p.Text))
		}
	}
}
