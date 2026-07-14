package exam

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	examLedgerPath      = "../eval/obligations-v1/ledger.json"
	sabotageLedgerPath  = "../eval/obligations-v1/sabotage-ledger.json"
	realPredictionsPath = "testdata/real-predictions.json"
)

func loadLedger(t *testing.T, path string) Ledger {
	t.Helper()
	l, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// realPredictions are the predictions the PRODUCTION engine emits on the exam
// corpus. They are a committed fixture because package exam may not import
// internal/mora (Landmine 6). internal/mora/exam_score_test.go regenerates this
// file from the real CLI/MCP call sites and fails by name when it drifts, so the
// red-team rows below really do mutate the real output rather than a hand-written
// stand-in for it.
func realPredictions(t *testing.T, surface string) []Prediction {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(realPredictionsPath))
	if err != nil {
		t.Fatal(err)
	}
	var pinned map[string][]Prediction
	if err := json.Unmarshal(b, &pinned); err != nil {
		t.Fatal(err)
	}
	preds, ok := pinned[surface]
	if !ok {
		t.Fatalf("real prediction fixture has no %q surface", surface)
	}
	return preds
}

func redTeamInput(t *testing.T) RedTeamInput {
	t.Helper()
	return RedTeamInput{
		Ledger:   loadLedger(t, examLedgerPath),
		Sabotage: loadLedger(t, sabotageLedgerPath),
		Meeting:  realPredictions(t, SurfaceMeeting),
		Daily:    realPredictions(t, SurfaceDaily),
	}
}

// TestScorerRedTeam iterates the MANIFEST, not the code. A manifested row with no
// registered baseline is a named failure, so deleting a subtest cannot delete the
// failure — the whole point of the manifest.
func TestScorerRedTeam(t *testing.T) {
	in := redTeamInput(t)
	rows := RedTeamRows()
	for _, id := range RequiredRedTeamRows {
		t.Run(id.Surface+"/"+id.Name, func(t *testing.T) {
			build, ok := rows[id]
			if !ok {
				t.Fatalf("EVAL_BROKEN: no baseline registered for row %s/%s", id.Surface, id.Name)
			}
			cases := build(in)
			if len(cases) == 0 {
				t.Fatalf("EVAL_BROKEN: row %s/%s built no cases", id.Surface, id.Name)
			}
			for _, c := range cases {
				label := c.Label
				if label == "" {
					label = id.Name
				}
				t.Run(label, func(t *testing.T) { assertRedTeamCase(t, id, c) })
			}
		})
	}
}

func assertRedTeamCase(t *testing.T, id RedTeamRowID, c RedTeamCase) {
	t.Helper()
	got, err := scoreRedTeamCase(c, id.Surface)
	if c.Expect.HarnessError {
		if err == nil || !errors.Is(err, ErrInvalidHarness) {
			t.Fatalf("EVAL_BROKEN: row %s must fail closed with INVALID_HARNESS, got err=%v", id.Name, err)
		}
		if RunStateOf(got, err) != StateInvalidHarness {
			t.Fatalf("EVAL_BROKEN: row %s run state = %q, want INVALID_HARNESS", id.Name, RunStateOf(got, err))
		}
		if !reflect.DeepEqual(got, Scorecard{}) {
			t.Fatalf("EVAL_BROKEN: row %s returned a partially usable scorecard on a harness fault", id.Name)
		}
		return
	}
	if err != nil {
		t.Fatalf("EVAL_BROKEN: row %s failed to score: %v", id.Name, err)
	}
	if c.Expect.Identical != nil {
		base, baseErr := Score(c.Expect.Identical.Ledger, c.Expect.Identical.Predictions, id.Surface)
		if baseErr != nil {
			t.Fatalf("EVAL_BROKEN: row %s baseline failed to score: %v", id.Name, baseErr)
		}
		if !reflect.DeepEqual(base, got) {
			t.Fatalf("EVAL_BROKEN: row %s is an INVARIANCE row — the scorecard moved.\n base=%+v\n got =%+v", id.Name, base, got)
		}
	}
	if c.Expect.State != "" && RunStateOf(got, err) != c.Expect.State {
		t.Errorf("EVAL_BROKEN: row %s run state = %q, want %q", id.Name, RunStateOf(got, err), c.Expect.State)
	}
	for _, check := range c.Expect.Checks {
		value, ok := MetricValue(got, check.Metric)
		if !ok {
			t.Fatalf("EVAL_BROKEN: row %s names unknown metric %q", id.Name, check.Metric)
		}
		if !check.Holds(value) {
			t.Errorf("EVAL_BROKEN: row %s: %s = %v, want %s %v (%s)", id.Name, check.Metric, value, check.Op, check.Want, check.Note)
		}
	}
}

func scoreRedTeamCase(c RedTeamCase, surface string) (Scorecard, error) {
	sc, err := Score(c.Ledger, c.Predictions, surface)
	if err != nil || c.UncappedPredictions == nil {
		return sc, err
	}
	return WithRecallUncapped(sc, c.Ledger, *c.UncappedPredictions, surface)
}

// TestEveryMetricHasASabotageCase is the sensitivity contract: a metric with no
// declared, registered sabotage case is not a metric, it is decoration.
func TestEveryMetricHasASabotageCase(t *testing.T) {
	registered := map[string]bool{}
	for _, id := range RequiredRedTeamRows {
		registered[id.Name] = true
	}
	for _, spec := range RequiredMetrics {
		if len(spec.SabotageCases) == 0 {
			t.Errorf("EVAL_BROKEN: metric %q declares no sabotage case", spec.ID)
			continue
		}
		for _, row := range spec.SabotageCases {
			if !registered[row] {
				t.Errorf("EVAL_BROKEN: metric %q names sabotage row %q, which has no registered baseline", spec.ID, row)
			}
		}
		if spec.ZeroDenominatorPolicy != PolicyNAIsFailure {
			t.Errorf("metric %q zero-denominator policy = %q, want %q", spec.ID, spec.ZeroDenominatorPolicy, PolicyNAIsFailure)
		}
		if spec.InvalidRunPolicy != PolicyHardFail {
			t.Errorf("metric %q invalid-run policy = %q, want %q", spec.ID, spec.InvalidRunPolicy, PolicyHardFail)
		}
		if len(spec.RequiredSlices) == 0 {
			t.Errorf("metric %q declares no required slices; a global average must never hide a collapsed slice", spec.ID)
		}
	}
}

// TestEveryRegisteredSabotageMovesItsMetric executes the sensitivity contract.
// A row name in the registry is not evidence: the row must move the exact
// Scorecard field it claims to sabotage relative to a typed positive control.
func TestEveryRegisteredSabotageMovesItsMetric(t *testing.T) {
	in := redTeamInput(t)
	rows := RedTeamRows()
	for _, spec := range RequiredMetrics {
		for _, rowName := range spec.SabotageCases {
			t.Run(spec.ID+"/"+rowName, func(t *testing.T) {
				moved := false
				for _, id := range RequiredRedTeamRows {
					if id.Name != rowName {
						continue
					}
					build, ok := rows[id]
					if !ok {
						continue
					}
					for _, c := range build(in) {
						if c.Expect.HarnessError || c.Expect.Identical != nil {
							continue
						}
						if spec.ID == MetricRecallUncapped && c.UncappedPredictions == nil {
							// A zero-valued RecallUncapped field is not movement. The row
							// must provide and score a typed uncapped prediction run.
							continue
						}
						got, err := scoreRedTeamCase(c, id.Surface)
						if err != nil {
							t.Fatalf("EVAL_BROKEN: sabotage row %s failed to score: %v", rowName, err)
						}
						baseLedger, basePredictions := sensitivityBaseline(in, id, c)
						base, err := Score(baseLedger, basePredictions, id.Surface)
						if err != nil {
							t.Fatalf("EVAL_BROKEN: sensitivity baseline for %s failed to score: %v", rowName, err)
						}
						if spec.ID == MetricRecallUncapped {
							base, err = WithRecallUncapped(base, baseLedger, basePredictions, id.Surface)
							if err != nil {
								t.Fatalf("EVAL_BROKEN: uncapped sensitivity baseline for %s failed to score: %v", rowName, err)
							}
						}
						baseValue := reflect.ValueOf(base).FieldByName(spec.Field).Interface()
						gotValue := reflect.ValueOf(got).FieldByName(spec.Field).Interface()
						moved = moved || !reflect.DeepEqual(baseValue, gotValue)
					}
				}
				if !moved {
					t.Fatalf("EVAL_BROKEN: metric %q names sabotage row %q, but that row leaves Scorecard.%s unchanged", spec.ID, rowName, spec.Field)
				}
			})
		}
	}
}

func sensitivityBaseline(in RedTeamInput, id RedTeamRowID, c RedTeamCase) (Ledger, []Prediction) {
	switch id.Name {
	case RowOracle:
		return c.Ledger, in.real(id.Surface)
	case RowGateDisableSweep:
		return in.Sabotage, Oracle(in.Sabotage, id.Surface)
	case RowGoldOwnerFlip, RowCitationSpanMove, RowAuthoredToQuoted:
		return in.Ledger, Oracle(in.Ledger, id.Surface)
	default:
		return c.Ledger, Oracle(c.Ledger, id.Surface)
	}
}

func TestDuplicatePredictionsCannotInflateAnyCountedMetric(t *testing.T) {
	l := loadLedger(t, examLedgerPath)
	tests := []struct {
		name    string
		surface string
		preds   []Prediction
		attack  func([]Prediction) []Prediction
	}{
		{name: "meeting true positives", surface: SurfaceMeeting, preds: append(Oracle(l, SurfaceMeeting), Prediction{Surface: SurfaceMeeting, Text: "not in the ledger", MemoryID: Oracle(l, SurfaceMeeting)[0].MemoryID, Direction: Unknown, Lifecycle: Unknown}), attack: duplicateFirstPrediction},
		{name: "meeting loose matches and leaks", surface: SurfaceMeeting, preds: CopyTheInputPredictions(l, SurfaceMeeting), attack: duplicateAllPredictions},
		{name: "daily true positives", surface: SurfaceDaily, preds: append(Oracle(l, SurfaceDaily), Prediction{Surface: SurfaceDaily, Text: "not in the ledger", MemoryID: "missing", Direction: Unknown, Lifecycle: Unknown}), attack: duplicateFirstPrediction},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, err := Score(l, tt.preds, tt.surface)
			if err != nil {
				t.Fatal(err)
			}
			duplicated := tt.attack(tt.preds)
			got, err := Score(l, duplicated, tt.surface)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, base) {
				t.Fatalf("duplicating predictions moved the scorecard:\nbase=%+v\n got=%+v", base, got)
			}
		})
	}
}

func duplicateFirstPrediction(preds []Prediction) []Prediction {
	out := clonePredictions(preds)
	for range 100 {
		out = append(out, preds[0])
	}
	return out
}

func duplicateAllPredictions(preds []Prediction) []Prediction {
	return append(clonePredictions(preds), clonePredictions(preds)...)
}

// TestMetricRegistryCoversEveryScorecardField is the other half of the contract:
// a new scorecard number cannot be added without a MetricSpec, and therefore
// cannot be added without a sabotage case.
func TestMetricRegistryCoversEveryScorecardField(t *testing.T) {
	registered := map[string]bool{}
	for _, spec := range RequiredMetrics {
		if registered[spec.Field] {
			t.Errorf("two metrics claim scorecard field %q", spec.Field)
		}
		registered[spec.Field] = true
	}
	scorecard := reflect.TypeOf(Scorecard{})
	for i := 0; i < scorecard.NumField(); i++ {
		name := scorecard.Field(i).Name
		if nonMetricScorecardFields[name] {
			continue
		}
		if !registered[name] {
			t.Errorf("EVAL_BROKEN: scorecard field %q has no MetricSpec, so nothing proves it can move", name)
		}
	}
	for field := range registered {
		if _, ok := scorecard.FieldByName(field); !ok {
			t.Errorf("metric registry names %q, which is not a scorecard field", field)
		}
	}
	for field := range nonMetricScorecardFields {
		if _, ok := scorecard.FieldByName(field); !ok {
			t.Errorf("the non-metric exclusion list names %q, which is not a scorecard field", field)
		}
	}
}

// nonMetricScorecardFields is the pinned exclusion list. It is deliberately tiny:
// every other field is a number that must prove it moves.
var nonMetricScorecardFields = map[string]bool{
	"Surface": true,
	"Owner":   true,
}

// TestScoreRefusesEveryInvalidLedgerClass drives all twelve validator rules through
// the SCORING CHOKEPOINT, not just the helper. A refusal that lives only in
// Validate is a refusal the scorer can forget to make.
func TestScoreRefusesEveryInvalidLedgerClass(t *testing.T) {
	seen := map[string]bool{}
	oracle := Oracle(loadLedger(t, examLedgerPath), SurfaceMeeting)
	for _, tt := range validatorMutations() {
		t.Run(tt.rule, func(t *testing.T) {
			l := cloneLedger(t, validTestLedger())
			tt.mutate(&l)
			sc, err := Score(l, oracle, SurfaceMeeting)
			if err == nil {
				t.Fatalf("EVAL_BROKEN: Score accepted a ledger broken by rule %q", tt.rule)
			}
			if !errors.Is(err, ErrInvalidHarness) {
				t.Fatalf("Score error = %v, want INVALID_HARNESS", err)
			}
			if !strings.Contains(err.Error(), tt.rule) {
				t.Fatalf("Score error = %v, want the named rule %q", err, tt.rule)
			}
			if !reflect.DeepEqual(sc, Scorecard{}) {
				t.Fatal("Score returned a partially usable scorecard over broken ground truth")
			}
			seen[tt.rule] = true
		})
	}
	for _, rule := range RequiredValidatorRules {
		if !seen[rule] {
			t.Errorf("validator rule %q is never driven through Score", rule)
		}
	}
}

func TestScoreRejectsZeroRequiredSamples(t *testing.T) {
	l := loadLedger(t, examLedgerPath)
	for i := range l.Commitments {
		l.Commitments[i].ExpectedIn = nil
	}
	// Class balance is a ratio over surfaced commitments, so stripping every
	// expectation is itself an invalid ledger; keep the check honest by asserting
	// only that the run is refused, never scored.
	if _, err := Score(l, nil, SurfaceMeeting); !errors.Is(err, ErrInvalidHarness) {
		t.Fatalf("Score over a ledger with zero gold samples = %v, want INVALID_HARNESS", err)
	}
}

func TestScoreRejectsUnknownSurface(t *testing.T) {
	l := loadLedger(t, examLedgerPath)
	if _, err := Score(l, nil, "home"); !errors.Is(err, ErrInvalidHarness) {
		t.Fatalf("Score over an unknown surface = %v, want INVALID_HARNESS", err)
	}
}

func TestScoreRejectsMissingCitedSourceArtifact(t *testing.T) {
	l := loadLedger(t, examLedgerPath)
	gold := goldCommitment(t, l, SurfaceMeeting)
	l.Artifacts = dropArtifact(l.Artifacts, gold.OpenedBy.ArtifactID)
	_, err := Score(l, Oracle(l, SurfaceMeeting), SurfaceMeeting)
	if !errors.Is(err, ErrInvalidHarness) {
		t.Fatalf("Score with a removed cited source = %v, want INVALID_HARNESS", err)
	}
	if !strings.Contains(err.Error(), gold.OpenedBy.ArtifactID) {
		t.Fatalf("Score error = %v, want it to name the missing artifact %q", err, gold.OpenedBy.ArtifactID)
	}
}

// TestOwnerIsReportedUnscorable pins the refusal. Deriving an owner from
// SectionKind == "open_loops" restates extraction precision on one section; it does
// not measure ownership, and pretending otherwise is how an exam lies.
func TestOwnerIsReportedUnscorable(t *testing.T) {
	l := loadLedger(t, examLedgerPath)
	sc, err := Score(l, Oracle(l, SurfaceMeeting), SurfaceMeeting)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Owner != OwnerUnscorable {
		t.Fatalf("Owner = %q, want the UNSCORABLE refusal", sc.Owner)
	}
}

// TestRealEngineIsGroundedInTheLedger is the precondition Packet E's audit refuses
// to start without: every line the product surfaces is a ledger span, matched
// EXACTLY. A nonzero LooseMatches means the predicate is absorbing brittleness; a
// nonzero Unmatched means the product cited a memory that does not carry the text.
func TestRealEngineIsGroundedInTheLedger(t *testing.T) {
	l := loadLedger(t, examLedgerPath)
	for _, surface := range []string{SurfaceMeeting, SurfaceDaily} {
		sc, err := Score(l, realPredictions(t, surface), surface)
		if err != nil {
			t.Fatal(err)
		}
		if sc.LooseMatches != 0 {
			t.Errorf("%s LooseMatches = %d, want 0 — the ledger or the match predicate is wrong", surface, sc.LooseMatches)
		}
		if sc.Unmatched != 0 {
			t.Errorf("%s Unmatched = %d, want 0 — the product cited a memory that does not carry the line", surface, sc.Unmatched)
		}
	}
}

// TestRealEngineBornRedRowsAreRed proves the born-red rows are red BY MEASUREMENT,
// not by a hard-coded zero. The oracle (row k) proves the same rows can go green.
func TestRealEngineBornRedRowsAreRed(t *testing.T) {
	l := loadLedger(t, examLedgerPath)
	sc, err := Score(l, realPredictions(t, SurfaceMeeting), SurfaceMeeting)
	if err != nil {
		t.Fatal(err)
	}
	if sc.DirectionScorable {
		t.Error("DirectionScorable = true, but no production payload carries a direction (Finding R3)")
	}
	born := map[string]PR{
		MetricDirection:      sc.Direction,
		MetricDueTime:        sc.DueTime,
		MetricLifecycle:      sc.Lifecycle,
		MetricClosureLinkage: sc.ClosureLinkage,
	}
	for name, row := range born {
		if row.Recall != 0 {
			t.Errorf("%s recall = %v, want 0 — the engine cannot express it, so it must be visibly red", name, row.Recall)
		}
		if row.Defined {
			t.Errorf("%s precision is Defined, but the engine emits only %q", name, Unknown)
		}
	}
}

// TestMatchIsExactNotContainment guards THE PREDICATE, which is the whole exam.
//
// It exists because the copy-the-input row alone does not. That row asserts
// LooseMatches > 0, and on this corpus that assertion is satisfied INCIDENTALLY —
// two thread subjects happen to be substrings of gold quotes — so loosening the
// predicate from equality to containment left the row green while silently
// crediting every padded line as a true positive. A gate that passes for a reason
// unrelated to the thing it is guarding is not a gate.
//
// So this asserts the predicate directly, in both directions: a line that merely
// CONTAINS a gold quote is never credited, and neither is a line the gold quote
// contains. Both are recorded as loose matches. Brittleness is reported, never
// absorbed — otherwise an extractor that emits the whole memory body "contains"
// every gold quote and scores perfectly.
func TestMatchIsExactNotContainment(t *testing.T) {
	l := loadLedger(t, examLedgerPath)
	oracle := Oracle(l, SurfaceMeeting)
	base, err := Score(l, oracle, SurfaceMeeting)
	if err != nil {
		t.Fatal(err)
	}
	if base.Extraction.Precision != 1 || base.LooseMatches != 0 {
		t.Fatalf("the oracle is not a clean baseline: precision=%v loose=%d", base.Extraction.Precision, base.LooseMatches)
	}
	quote := UnwrapEmitted(oracle[0].Text)

	for name, text := range map[string]string{
		"padded (the copy-the-input attack)": quote + " And an unrelated extra sentence.",
		"truncated (a fragment of the gold)": quote[:len(quote)/2],
	} {
		attack := clonePredictions(oracle)
		attack[0].Text = "· x — “" + text + "”"

		got, err := Score(l, attack, SurfaceMeeting)
		if err != nil {
			t.Fatal(err)
		}
		if got.Extraction.Precision >= base.Extraction.Precision {
			t.Errorf("%s: precision = %v, want BELOW %v — the predicate credited a containment hit",
				name, got.Extraction.Precision, base.Extraction.Precision)
		}
		if got.Extraction.Recall >= base.Extraction.Recall {
			t.Errorf("%s: recall = %v, want BELOW %v — the gold item was credited to a line that does not quote it",
				name, got.Extraction.Recall, base.Extraction.Recall)
		}
		if got.LooseMatches != 1 {
			t.Errorf("%s: LooseMatches = %d, want exactly 1 — brittleness is REPORTED, never absorbed", name, got.LooseMatches)
		}

		verdicts, err := Classify(l, attack, SurfaceMeeting)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range verdicts {
			if v.Kind == VerdictTruePositive && strings.Contains(v.Text, "unrelated extra sentence") {
				t.Errorf("%s: the padded line was credited as a TRUE POSITIVE", name)
			}
		}
	}
}

// TestUnknownNeverScoresCorrect is the test that keeps the typed sentinels honest.
// The gold ledger has obligations with NO due time and NO closure. If the scorer
// treated the adapters' "" as "correctly reports no due time", a surface that cannot
// express a due time at all would score CORRECT on them — the born-red rows would be
// accidentally green and the exam would be lying in the flattering direction.
func TestUnknownNeverScoresCorrect(t *testing.T) {
	l := loadLedger(t, examLedgerPath)
	oracle := Oracle(l, SurfaceMeeting)

	silent := clonePredictions(oracle)
	for i := range silent {
		silent[i].Due, silent[i].ClosureRef = "", ""
		silent[i].Direction, silent[i].Lifecycle = Unknown, Unknown
	}
	sc, err := Score(l, silent, SurfaceMeeting)
	if err != nil {
		t.Fatal(err)
	}
	for name, row := range map[string]PR{
		MetricDueTime: sc.DueTime, MetricClosureLinkage: sc.ClosureLinkage,
		MetricDirection: sc.Direction, MetricLifecycle: sc.Lifecycle,
	} {
		if row.Recall != 0 {
			t.Errorf("%s recall = %v for a surface that expressed NOTHING; silence is not a correct answer", name, row.Recall)
		}
		if row.Defined {
			t.Errorf("%s reported a DEFINED precision over zero expressed values, which must be N/A", name)
		}
	}
	if sc.DirectionScorable {
		t.Error("DirectionScorable = true when every prediction was unknown")
	}

	// And the converse: the same predictions with the TYPED gold values must score
	// perfectly, which is what proves the rows are red because the PRODUCT is silent
	// and not because the scorer cannot pass them.
	typed, err := Score(l, oracle, SurfaceMeeting)
	if err != nil {
		t.Fatal(err)
	}
	if typed.DueTime.Recall != 1 || typed.ClosureLinkage.Recall != 1 {
		t.Fatalf("the oracle could not pass the typed rows: due=%+v closure=%+v", typed.DueTime, typed.ClosureLinkage)
	}
}

// TestZeroPredictionsReportNA is Finding 6 made mechanical: precision over zero
// predictions is N/A, never a flattering 1.0, and N/A is a failure.
func TestZeroPredictionsReportNA(t *testing.T) {
	l := loadLedger(t, examLedgerPath)
	sc, err := Score(l, nil, SurfaceMeeting)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Extraction.Defined {
		t.Fatal("an empty prediction set reported a DEFINED precision")
	}
	if sc.Extraction.Precision != 0 || sc.Extraction.Recall != 0 {
		t.Fatalf("empty extraction = %+v, want zero values with Defined=false", sc.Extraction)
	}
	if RunStateOf(sc, nil) != StateScoredFailure {
		t.Fatalf("an empty brief run state = %q, want SCORED_FAILURE", RunStateOf(sc, nil))
	}
}

func TestScoreIsOrderIndependent(t *testing.T) {
	l := loadLedger(t, examLedgerPath)
	preds := realPredictions(t, SurfaceMeeting)
	base, err := Score(l, preds, SurfaceMeeting)
	if err != nil {
		t.Fatal(err)
	}
	permuted, err := Score(PermuteArtifacts(l), ReversePredictions(preds), SurfaceMeeting)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(base, permuted) {
		t.Fatalf("scorecard moved under an input permutation:\n base=%+v\n got =%+v", base, permuted)
	}
}

// TestSliceScoresAreLoadBearing keeps RequiredSlices from being decoration: a
// per-slice score really is computable, so Packet E's per-slice floors have
// something to ratchet and a collapsed channel cannot hide inside a global average.
func TestSliceScoresAreLoadBearing(t *testing.T) {
	l := loadLedger(t, examLedgerPath)
	preds := realPredictions(t, SurfaceMeeting)
	gmail, err := ScoreSlice(l, preds, SurfaceMeeting, SliceChannel, "gmail")
	if err != nil {
		t.Fatal(err)
	}
	imessage, err := ScoreSlice(l, preds, SurfaceMeeting, SliceChannel, "imessage")
	if err != nil {
		t.Fatal(err)
	}
	if gmail.Extraction.Recall <= imessage.Extraction.Recall {
		t.Fatalf("gmail recall %v is not above imessage recall %v; the corpus's known identity gap is in imessage, so a slice score that cannot see it is broken",
			gmail.Extraction.Recall, imessage.Extraction.Recall)
	}
	if _, err := ScoreSlice(l, preds, SurfaceMeeting, "colour", "green"); !errors.Is(err, ErrInvalidHarness) {
		t.Fatalf("ScoreSlice over an unknown slice = %v, want INVALID_HARNESS", err)
	}
}

// TestGateControlsCoverEverySweptGate binds Matrix 1 to a scored consequence: each
// production exclusion gate names a negative-control case whose leak it prevents,
// and red-team row (s) proves each of those leaks moves a named counter.
func TestGateControlsCoverEverySweptGate(t *testing.T) {
	sab := loadLedger(t, sabotageLedgerPath)
	seen := map[string]bool{}
	for _, control := range GateControls() {
		if seen[control.Gate] {
			t.Errorf("gate %q is registered twice", control.Gate)
		}
		seen[control.Gate] = true
		if !caseExists(sab, control.Case) {
			t.Errorf("gate %q names control case %q, which is not in the red-team ledger", control.Gate, control.Case)
		}
		if _, ok := MetricValue(Scorecard{}, control.Counter); !ok {
			t.Errorf("gate %q names counter %q, which is not a scored metric", control.Gate, control.Counter)
		}
	}
	for _, gate := range ProductionExclusionGates {
		if !seen[gate] {
			t.Errorf("production gate %q has no negative control, so disabling it would cost nothing", gate)
		}
	}
	if len(GateControls()) != len(ProductionExclusionGates) {
		t.Fatalf("gate controls = %d, swept gates = %d", len(GateControls()), len(ProductionExclusionGates))
	}
}

func TestSabotageLedgerIsAValidSyntheticWorld(t *testing.T) {
	sab := loadLedger(t, sabotageLedgerPath)
	if err := Validate(sab); err != nil {
		t.Fatalf("the red-team ledger is not a valid world: %v", err)
	}
	if err := Lint(sab); err != nil {
		t.Fatalf("the red-team ledger leaks a real identity: %v", err)
	}
	classes := map[string]bool{}
	for _, n := range sab.NonObligations {
		classes[n.Class] = true
	}
	for _, want := range []string{"footer", "marketing", "notification", "url_shard", "self_spoken", "lead_in", "bystander", "trivia"} {
		if !classes[want] {
			t.Errorf("the red-team ledger has no %q defect class", want)
		}
	}
}

// TestRedTeamManifestIsComplete makes shrinking the manifest a named failure, and
// keeps the flywheel row (t) — which needs the two-state graph run — honestly out
// of this PR rather than silently registered and never exercised.
func TestRedTeamManifestIsComplete(t *testing.T) {
	rows := RedTeamRows()
	if len(rows) != len(RequiredRedTeamRows) {
		t.Fatalf("registered baselines = %d, manifested rows = %d", len(rows), len(RequiredRedTeamRows))
	}
	names := map[string]bool{}
	for _, id := range RequiredRedTeamRows {
		names[id.Name] = true
		if _, ok := rows[id]; !ok {
			t.Errorf("EVAL_BROKEN: no baseline registered for row %s/%s", id.Surface, id.Name)
		}
	}
	for _, want := range []string{
		RowSyntheticGibberish, RowEmptyBrief, RowEveryQuestion, RowCopyTheInput, RowIdentityFlip,
		RowDirectionFlip, RowUnsupportedCitation, RowConstantClassifier, RowDailyEmpty, RowDailyCitation,
		RowOracle, RowClosedAsOpen, RowGoldOwnerFlip, RowCitationSpanMove, RowAuthoredToQuoted,
		RowRemovedSource, RowDuplicateNoise, RowInputOrder, RowGateDisableSweep,
	} {
		if !names[want] {
			t.Errorf("EVAL_BROKEN: red-team row %q vanished from the manifest", want)
		}
	}
}

// TestSyntheticGibberishCarriesEveryDefectSignature is the frozen-incident table's
// synthetic twin. The exam never reads the committed frozen-incident tree — it carries
// real names on a public repo — so the defect SIGNATURES are re-asserted here against
// the synthetic world instead.
func TestSyntheticGibberishCarriesEveryDefectSignature(t *testing.T) {
	in := redTeamInput(t)
	cases := RedTeamRows()[RedTeamRowID{Surface: SurfaceMeeting, Name: RowSyntheticGibberish}](in)
	var body strings.Builder
	for _, p := range cases[0].Predictions {
		body.WriteString(p.Text)
		body.WriteString("\n")
	}
	junk := body.String()
	for _, signature := range []struct{ pattern, defectClass string }{
		{"https://", "url_shard"},
		{"-----Original Message-----", "marketing"},
		{"Please do not reply", "footer"},
		{"has accepted", "notification"},
		{"Earlier note from the archive:", "trivia"},
		{"independent", "substring_trap"},
		{"As discussed,", "lead_in"},
		{"Yes, see you there.", "self_spoken"},
		{"parking plan", "bystander"},
	} {
		if !strings.Contains(junk, signature.pattern) {
			t.Errorf("EVAL_BROKEN: the synthetic gibberish baseline lost the %s signature %q", signature.defectClass, signature.pattern)
		}
	}
}

func caseExists(l Ledger, id string) bool {
	for _, c := range l.Commitments {
		if c.ID == id {
			return true
		}
	}
	for _, n := range l.NonObligations {
		if n.ID == id {
			return true
		}
	}
	return false
}

func goldCommitment(t *testing.T, l Ledger, surface string) Commitment {
	t.Helper()
	for _, c := range l.Commitments {
		for _, in := range c.ExpectedIn {
			if surfaceCovers(surface, in) {
				return c
			}
		}
	}
	t.Fatalf("ledger has no gold commitment on surface %q", surface)
	return Commitment{}
}

func dropArtifact(artifacts []Artifact, id string) []Artifact {
	out := make([]Artifact, 0, len(artifacts))
	for _, a := range artifacts {
		if a.ID != id {
			out = append(out, a)
		}
	}
	return out
}
