package exam

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Comparison operators for a typed expectation. Every Want below is COMPUTED from
// the ledger (or from the row's own baseline scorecard) by the builder — never a
// literal — so resizing the corpus needs no edit and no invented threshold can hide
// in the gate code.
const (
	OpEq  = "=="
	OpLT  = "<"
	OpGT  = ">"
	OpLTE = "<="
	OpGTE = ">="
)

// epsilon absorbs float representation, nothing else. Every expected value here is a
// small exact fraction; the tolerance exists so an exact assertion cannot be lost to
// the last bit, not so a wrong answer can pass.
const epsilon = 1e-9

type Check struct {
	Metric string
	Op     string
	Want   float64
	Note   string
}

func (c Check) Holds(got float64) bool {
	switch c.Op {
	case OpEq:
		return math.Abs(got-c.Want) <= epsilon
	case OpLT:
		return got < c.Want-epsilon
	case OpGT:
		return got > c.Want+epsilon
	case OpLTE:
		return got <= c.Want+epsilon
	case OpGTE:
		return got >= c.Want-epsilon
	}
	return false
}

// Baseline is the unmutated run an invariance row must byte-match.
type Baseline struct {
	Ledger      Ledger
	Predictions []Prediction
}

// Expectation is typed data, not code, so a red-team row cannot quietly assert
// nothing. HarnessError and Identical are the two rows that assert something other
// than a metric movement: fail-closed, and do-not-move.
type Expectation struct {
	HarnessError bool
	State        RunState
	Identical    *Baseline
	Checks       []Check
}

type RedTeamCase struct {
	Label       string
	Ledger      Ledger
	Predictions []Prediction
	Expect      Expectation
}

// RedTeamInput carries everything a row may transform: the gold world, the synthetic
// red-team world, and the REAL predictions the production engine emits today. Rows
// (e), (g), (j), (n), (q) and (r) mutate the real output, which makes them far
// stronger than any standalone baseline.
type RedTeamInput struct {
	Ledger   Ledger
	Sabotage Ledger
	Meeting  []Prediction
	Daily    []Prediction
}

func (in RedTeamInput) real(surface string) []Prediction {
	if surface == SurfaceDaily {
		return clonePredictions(in.Daily)
	}
	return clonePredictions(in.Meeting)
}

// GateControl binds one production exclusion gate to the named negative-control
// ledger case whose leak it prevents, and to the counter that leak turns red.
// Matrix 1 disables the gate in the source; red-team row (s) proves the disabled
// gate's leak COSTS something scored — without this, a gate could be deleted and
// every number would stay exactly where it was.
type GateControl struct {
	Gate    string
	Case    string
	Counter string
}

func GateControls() []GateControl {
	return []GateControl{
		{"classifyMeetingBriefEvidence", "c/sab-closed", MetricClosedLeaks},
		{"isMeetingNotification", "n/sab-notification", MetricNonObligationLeaks},
		{"assignedToThirdParty", "c/sab-third-party", MetricThirdPartyLeaks},
		{"memoryIsServiceOnly", "n/sab-footer", MetricNonObligationLeaks},
		{"userOwnedOpenLoop", "c/sab-third-party", MetricCriticalDirection},
		{"meetingBriefIsTwoPartyExchange", "n/sab-bystander", MetricNonObligationLeaks},
		{"relationalEvidenceIDs", "n/sab-bystander", MetricNonObligationLeaks},
		{"meetingBriefResolveAttribution", "c/sab-approve", MetricCriticalIdentity},
		{"stripURLs", "n/sab-url", MetricNonObligationLeaks},
		{"unwrapHardWraps", "n/sab-lead-in", MetricNonObligationLeaks},
		{"senderAuthoredBody", "n/sab-quoted", MetricNonObligationLeaks},
		{"stripSpeakerPrefix", "n/sab-self-spoken", MetricNonObligationLeaks},
		{"isForwardedSubject", "n/sab-forwarded", MetricNonObligationLeaks},
		{"isLeadInFragment", "n/sab-lead-in", MetricNonObligationLeaks},
		{"stripNoiseTokens", "n/sab-url", MetricNonObligationLeaks},
		{"gmailActionableAsk", "n/sab-substring", MetricNonObligationLeaks},
		{"containsPhrase", "n/sab-substring", MetricNonObligationLeaks},
	}
}

// Oracle is the positive control: predictions derived MECHANICALLY from the ledger,
// with every typed field filled from gold. Without it the negative rows are all
// satisfiable by a scorer that unconditionally fails, and an eval broken in the safe
// direction is still broken. It also proves each born-red row CAN go green.
//
// The oracle surfaces an obligation only when its evidence is sender-authored text
// or the subject line — which is why reclassifying a gold block to quoted_reply (row
// o) makes the obligation genuinely disappear instead of being rigged away.
func Oracle(l Ledger, surface string) []Prediction {
	idx, err := indexLedger(l)
	if err != nil {
		return nil
	}
	gold, err := goldFor(l, idx, surface, sliceAny{})
	if err != nil {
		return nil
	}
	grain, err := grainOf(surface)
	if err != nil {
		return nil
	}
	out := make([]Prediction, 0, len(gold))
	for _, g := range gold {
		if g.BlockKind != "" && g.BlockKind != "authored" {
			continue
		}
		p := Prediction{
			Surface:    grain,
			MemoryID:   g.MemoryID,
			Direction:  g.Direction,
			Due:        g.Due,
			Lifecycle:  g.Lifecycle,
			ClosureRef: g.Closure,
		}
		if grain == SurfaceMeeting {
			p.Text = emitMeetingLine(idx, g)
			p.SectionKind = meetingOpenLoops
			p.Attendee = idx.displayOf(g.Attendee)
			p.AttendeeAtom = idx.atomOf(g.Attendee)
		} else {
			p.Text = emitDailyLine(idx, g)
		}
		out = append(out, p)
	}
	return out
}

// emitMeetingLine reproduces the shape of the product's presentation layer — a
// quoted evidence span — WITHOUT re-implementing its dated prefix. UnwrapEmitted is
// pinned against the real engine's exact bytes by the real-prediction fixture, so
// the oracle does not need to (and must not) grow a second copy of that renderer.
func emitMeetingLine(idx ledgerIndex, g goldItem) string {
	return "· " + idx.displayOf(g.Attendee) + " — “" + g.Quote + "”"
}

func emitDailyLine(idx ledgerIndex, g goldItem) string {
	subject := idx.byMemory[g.MemoryID].Subject
	return "- " + subject + " — " + g.Quote + " (id: " + g.MemoryID + ")\n"
}

func (idx ledgerIndex) displayOf(identity string) string {
	for display, id := range idx.displays {
		if id == identity {
			return display
		}
	}
	return idx.atomValueOf(identity)
}

func (idx ledgerIndex) atomOf(identity string) string {
	value := idx.atomValueOf(identity)
	if strings.HasPrefix(value, "+") {
		return "handle:" + value
	}
	return "address:" + value
}

func (idx ledgerIndex) atomValueOf(identity string) string {
	atoms := make([]string, 0, len(idx.atoms))
	for atom, id := range idx.atoms {
		if id == identity {
			atoms = append(atoms, atom)
		}
	}
	sort.Strings(atoms)
	if len(atoms) == 0 {
		return identity
	}
	return atoms[0]
}

// EveryQuestionPredictions is the extractor that mistakes a question mark for an
// obligation. It drowns in the ledger's labelled non-obligations, which is the whole
// point: a corpus with no distractor mass cannot tell this baseline from a good one.
func EveryQuestionPredictions(l Ledger, surface string) []Prediction {
	grain, err := grainOf(surface)
	if err != nil || grain != SurfaceMeeting {
		return nil
	}
	idx, _ := indexLedger(l)
	var out []Prediction
	for _, a := range l.Artifacts {
		for _, m := range a.Messages {
			for _, b := range m.Body {
				for _, sentence := range sentences(b.Text) {
					if !strings.HasSuffix(sentence, "?") {
						continue
					}
					out = append(out, Prediction{
						Surface:      SurfaceMeeting,
						Text:         "· — “" + sentence + "”",
						MemoryID:     a.MemoryID,
						SectionKind:  meetingOpenLoops,
						Attendee:     idx.displayOf(m.From),
						AttendeeAtom: idx.atomOf(m.From),
						Direction:    Unknown,
						Lifecycle:    Unknown,
					})
				}
			}
		}
	}
	return out
}

// CopyTheInputPredictions emits the corpus back at the scorer at BOTH grains it
// could plausibly claim — every memory's whole body, and every block — each
// correctly cited. That is exactly the extractor a containment-based scorer cannot
// refuse: the whole body "contains" every gold quote. It must fail on precision AND
// register loose matches, or the match predicate has absorbed the brittleness it was
// supposed to report.
func CopyTheInputPredictions(l Ledger, surface string) []Prediction {
	grain, err := grainOf(surface)
	if err != nil || grain != SurfaceMeeting {
		return nil
	}
	idx, _ := indexLedger(l)
	seen := map[string]bool{}
	var out []Prediction
	emit := func(memoryID, text, from string) {
		key := memoryID + "\x00" + normalize(text)
		if text == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, Prediction{
			Surface:      SurfaceMeeting,
			Text:         "· — “" + text + "”",
			MemoryID:     memoryID,
			SectionKind:  meetingOpenLoops,
			Attendee:     idx.displayOf(from),
			AttendeeAtom: idx.atomOf(from),
			Direction:    Unknown,
			Lifecycle:    Unknown,
		})
	}
	for _, a := range l.Artifacts {
		var body []string
		for _, m := range a.Messages {
			for _, b := range m.Body {
				body = append(body, b.Text)
				emit(a.MemoryID, b.Text, m.From)
			}
		}
		emit(a.MemoryID, a.Subject, a.Messages[0].From)
		emit(a.MemoryID, strings.Join(body, "\n\n"), a.Messages[0].From)
	}
	return out
}

// SyntheticGibberishPredictions is the frozen-incident baseline's synthetic twin. It
// is rendered from the RED-TEAM ledger — a self-contained world that re-creates every
// defect class with invented personas — because the committed frozen fixture carries
// real names on a public repo and the exam never reads it.
func SyntheticGibberishPredictions(sabotage Ledger, surface string) []Prediction {
	grain, err := grainOf(surface)
	if err != nil {
		return nil
	}
	idx, _ := indexLedger(sabotage)
	var out []Prediction
	for _, a := range sabotage.Artifacts {
		for _, m := range a.Messages {
			for _, b := range m.Body {
				p := Prediction{
					Surface:      grain,
					Text:         "· — “" + b.Text + "”",
					MemoryID:     a.MemoryID,
					SectionKind:  meetingOpenLoops,
					Attendee:     idx.displayOf(m.From),
					AttendeeAtom: idx.atomOf(m.From),
					Direction:    Unknown,
					Lifecycle:    Unknown,
				}
				if grain == SurfaceDaily {
					p.Text = "- " + a.Subject + " — " + b.Text + " (id: " + a.MemoryID + ")\n"
					p.SectionKind, p.Attendee, p.AttendeeAtom = "", "", ""
				}
				out = append(out, p)
			}
		}
	}
	return out
}

func sentences(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		var current strings.Builder
		for _, r := range line {
			current.WriteRune(r)
			if r == '.' || r == '?' || r == '!' {
				if s := strings.TrimSpace(current.String()); s != "" {
					out = append(out, s)
				}
				current.Reset()
			}
		}
		if s := strings.TrimSpace(current.String()); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// RedTeamRows is the registry the manifest iterates. A manifested row with no entry
// here is a named failure, so deleting a subtest cannot delete the failure.
func RedTeamRows() map[RedTeamRowID]func(RedTeamInput) []RedTeamCase {
	return map[RedTeamRowID]func(RedTeamInput) []RedTeamCase{
		{SurfaceMeeting, RowSyntheticGibberish}:  rowSyntheticGibberish,
		{SurfaceMeeting, RowEmptyBrief}:          rowEmpty(SurfaceMeeting),
		{SurfaceMeeting, RowEveryQuestion}:       rowEveryQuestion,
		{SurfaceMeeting, RowCopyTheInput}:        rowCopyTheInput,
		{SurfaceMeeting, RowIdentityFlip}:        rowIdentityFlip,
		{SurfaceMeeting, RowDirectionFlip}:       rowDirectionFlip,
		{SurfaceMeeting, RowUnsupportedCitation}: rowUnsupportedCitation,
		{SurfaceMeeting, RowConstantClassifier}:  rowConstantClassifier,
		{SurfaceDaily, RowDailyEmpty}:            rowEmpty(SurfaceDaily),
		{SurfaceDaily, RowDailyCitation}:         rowDailyCitation,
		{SurfaceMeeting, RowOracle}:              rowOracle(SurfaceMeeting),
		{SurfaceDaily, RowOracle}:                rowOracle(SurfaceDaily),
		{SurfaceMeeting, RowClosedAsOpen}:        rowClosedAsOpen,
		{SurfaceMeeting, RowGoldOwnerFlip}:       rowGoldOwnerFlip,
		{SurfaceMeeting, RowCitationSpanMove}:    rowCitationSpanMove,
		{SurfaceMeeting, RowAuthoredToQuoted}:    rowAuthoredToQuoted,
		{SurfaceMeeting, RowRemovedSource}:       rowRemovedSource(SurfaceMeeting),
		{SurfaceDaily, RowRemovedSource}:         rowRemovedSource(SurfaceDaily),
		{SurfaceMeeting, RowDuplicateNoise}:      rowDuplicateNoise(SurfaceMeeting),
		{SurfaceDaily, RowDuplicateNoise}:        rowDuplicateNoise(SurfaceDaily),
		{SurfaceMeeting, RowInputOrder}:          rowInputOrder(SurfaceMeeting),
		{SurfaceDaily, RowInputOrder}:            rowInputOrder(SurfaceDaily),
		{SurfaceMeeting, RowGateDisableSweep}:    rowGateDisableSweep,
	}
}

func mustScore(l Ledger, preds []Prediction, surface string) Scorecard {
	sc, err := Score(l, preds, surface)
	if err != nil {
		return Scorecard{}
	}
	return sc
}

func goldCount(l Ledger, surface string) int {
	idx, err := indexLedger(l)
	if err != nil {
		return 0
	}
	gold, err := goldFor(l, idx, surface, sliceAny{})
	if err != nil {
		return 0
	}
	return len(gold)
}

func rowSyntheticGibberish(in RedTeamInput) []RedTeamCase {
	preds := SyntheticGibberishPredictions(in.Sabotage, SurfaceMeeting)
	return []RedTeamCase{{
		Ledger:      in.Ledger,
		Predictions: preds,
		Expect: Expectation{
			State: StateScoredFailure,
			Checks: []Check{
				{MetricExtraction + ".precision", OpEq, 0, "junk cannot earn a true positive"},
				{MetricExtraction + ".recall", OpEq, 0, "junk cannot find a gold obligation"},
				{MetricUnmatched, OpEq, float64(len(preds)), "every junk line is grounded in nothing"},
				{MetricExtraction + ".defined", OpEq, 1, "the run happened; it just scored zero"},
			},
		},
	}}
}

func rowEmpty(surface string) func(RedTeamInput) []RedTeamCase {
	return func(in RedTeamInput) []RedTeamCase {
		return []RedTeamCase{{
			Ledger:      in.Ledger,
			Predictions: nil,
			Expect: Expectation{
				State: StateScoredFailure,
				Checks: []Check{
					{MetricExtraction + ".recall", OpEq, 0, "an empty brief finds nothing"},
					{MetricExtraction + ".defined", OpEq, 0, "precision over zero predictions is N/A, never 1.0"},
					{MetricCitationCoverage + ".defined", OpEq, 0, "coverage over zero predictions is N/A"},
				},
			},
		}}
	}
}

func rowEveryQuestion(in RedTeamInput) []RedTeamCase {
	oracle := mustScore(in.Ledger, Oracle(in.Ledger, SurfaceMeeting), SurfaceMeeting)
	return []RedTeamCase{{
		Ledger:      in.Ledger,
		Predictions: EveryQuestionPredictions(in.Ledger, SurfaceMeeting),
		Expect: Expectation{
			State: StateScoredFailure,
			Checks: []Check{
				{MetricExtraction + ".precision", OpLT, oracle.Extraction.Precision, "a question mark is not an obligation"},
				{MetricNonObligationLeaks, OpGT, 0, "it drowns in the ledger's labelled non-obligations"},
			},
		},
	}}
}

func rowCopyTheInput(in RedTeamInput) []RedTeamCase {
	oracle := mustScore(in.Ledger, Oracle(in.Ledger, SurfaceMeeting), SurfaceMeeting)
	return []RedTeamCase{{
		Ledger:      in.Ledger,
		Predictions: CopyTheInputPredictions(in.Ledger, SurfaceMeeting),
		Expect: Expectation{
			State: StateScoredFailure,
			Checks: []Check{
				{MetricExtraction + ".precision", OpLT, oracle.Extraction.Precision, "copying the corpus is not extraction"},
				{MetricLooseMatches, OpGT, 0, "it cannot pass by containment — the brittleness is REPORTED"},
				{MetricNonObligationLeaks, OpGT, 0, "it emits the labelled non-obligations too"},
				{MetricDupLeaks, OpGT, 0, "it emits both halves of the duplicate pair"},
				{MetricDedupCrossArtifact, OpGT, 0, "the duplicate pair surfaces from two different memories"},
			},
		},
	}}
}

func rowIdentityFlip(in RedTeamInput) []RedTeamCase {
	real := in.real(SurfaceMeeting)
	base := mustScore(in.Ledger, real, SurfaceMeeting)
	return []RedTeamCase{{
		Ledger:      in.Ledger,
		Predictions: FlipIdentities(in.Ledger, real),
		Expect: Expectation{
			State: StateScoredFailure,
			Checks: []Check{
				{MetricCriticalIdentity, OpGT, 0, "a line attributed to the wrong person is a sev-1"},
				{MetricCounterparty + ".recall", OpLT, base.Counterparty.Recall, "per-class recall must see the swap"},
			},
		},
	}}
}

func rowDirectionFlip(in RedTeamInput) []RedTeamCase {
	oracle := Oracle(in.Ledger, SurfaceMeeting)
	base := mustScore(in.Ledger, oracle, SurfaceMeeting)
	return []RedTeamCase{{
		Ledger:      in.Ledger,
		Predictions: FlipOneDirection(oracle),
		Expect: Expectation{
			State: StateScoredFailure,
			Checks: []Check{
				{MetricDirection + ".recall", OpLT, base.Direction.Recall, "a wrong TYPED direction must cost per-class recall"},
				{MetricCriticalDirection, OpGT, 0, "and must be counted as a critical failure"},
			},
		},
	}}
}

func rowUnsupportedCitation(in RedTeamInput) []RedTeamCase {
	real := in.real(SurfaceMeeting)
	base := mustScore(in.Ledger, real, SurfaceMeeting)
	return []RedTeamCase{{
		Ledger:      in.Ledger,
		Predictions: RepointOneCitation(in.Ledger, real, SurfaceMeeting),
		Expect: Expectation{
			State: StateScoredFailure,
			Checks: []Check{
				{MetricCitationCorrect + ".precision", OpLT, base.CitationCorrect.Precision, "an unsupported citation is TRUTH-04's named failure"},
			},
		},
	}}
}

func rowConstantClassifier(in RedTeamInput) []RedTeamCase {
	oracle := Oracle(in.Ledger, SurfaceMeeting)
	base := mustScore(in.Ledger, oracle, SurfaceMeeting)
	return []RedTeamCase{{
		Ledger:      in.Ledger,
		Predictions: ConstantDirection(oracle, DirectionOwedBySelf),
		Expect: Expectation{
			State: StateScoredFailure,
			Checks: []Check{
				{MetricDirection + ".recall", OpLT, base.Direction.Recall, "per-class recall is why a constant classifier cannot score well"},
				{MetricDirectionScorable, OpEq, 1, "it DID express a direction — it just expressed the same one every time"},
			},
		},
	}}
}

func rowDailyCitation(in RedTeamInput) []RedTeamCase {
	real := in.real(SurfaceDaily)
	base := mustScore(in.Ledger, real, SurfaceDaily)
	return []RedTeamCase{{
		Ledger:      in.Ledger,
		Predictions: BlankAndRepointDailyCitations(in.Ledger, real),
		Expect: Expectation{
			State: StateScoredFailure,
			Checks: []Check{
				{MetricCitationCoverage + ".precision", OpLT, base.CitationCoverage.Precision, "a blanked id is a coverage miss"},
				{MetricCitationCorrect + ".precision", OpLT, base.CitationCorrect.Precision, "a repointed id is a correctness miss — the two rows fail independently"},
			},
		},
	}}
}

func rowOracle(surface string) func(RedTeamInput) []RedTeamCase {
	return func(in RedTeamInput) []RedTeamCase {
		checks := []Check{
			{MetricExtraction + ".precision", OpEq, 1, "the positive control must be perfect, or every negative row is satisfiable by a scorer that always fails"},
			{MetricExtraction + ".recall", OpEq, 1, ""},
			{MetricCitationCoverage + ".precision", OpEq, 1, ""},
			{MetricCitationCorrect + ".precision", OpEq, 1, ""},
			{MetricDirection + ".recall", OpEq, 1, "BORN-RED rows must be able to go GREEN when the substrate exists"},
			{MetricDueTime + ".recall", OpEq, 1, ""},
			{MetricLifecycle + ".recall", OpEq, 1, ""},
			{MetricClosureLinkage + ".recall", OpEq, 1, ""},
			{MetricDirectionScorable, OpEq, 1, ""},
			{MetricThirdPartyLeaks, OpEq, 0, ""},
			{MetricClosedLeaks, OpEq, 0, ""},
			{MetricDupLeaks, OpEq, 0, ""},
			{MetricNonObligationLeaks, OpEq, 0, ""},
			{MetricCriticalIdentity, OpEq, 0, ""},
			{MetricCriticalDirection, OpEq, 0, ""},
			{MetricLooseMatches, OpEq, 0, ""},
			{MetricUnmatched, OpEq, 0, ""},
			{MetricDedupCrossArtifact, OpEq, 0, ""},
		}
		if surface == SurfaceMeeting {
			checks = append(checks, Check{MetricCounterparty + ".recall", OpEq, 1, ""})
		}
		return []RedTeamCase{{
			Ledger:      in.Ledger,
			Predictions: Oracle(in.Ledger, surface),
			Expect:      Expectation{State: StatePass, Checks: checks},
		}}
	}
}

func rowClosedAsOpen(in RedTeamInput) []RedTeamCase {
	oracle := Oracle(in.Ledger, SurfaceMeeting)
	base := mustScore(in.Ledger, oracle, SurfaceMeeting)
	return []RedTeamCase{{
		Ledger:      in.Ledger,
		Predictions: SurfaceClosedAsOpen(in.Ledger, oracle, SurfaceMeeting),
		Expect: Expectation{
			State: StateScoredFailure,
			Checks: []Check{
				{MetricClosedLeaks, OpEq, 1, "a settled obligation presented as current is consequential on its own"},
				{MetricLifecycle + ".precision", OpLT, base.Lifecycle.Precision, ""},
				{MetricClosureLinkage + ".precision", OpLT, base.ClosureLinkage.Precision, ""},
				{MetricDueTime + ".precision", OpLT, base.DueTime.Precision, ""},
			},
		},
	}}
}

func rowGoldOwnerFlip(in RedTeamInput) []RedTeamCase {
	oracle := Oracle(in.Ledger, SurfaceMeeting)
	return []RedTeamCase{{
		Ledger:      FlipGoldOwner(in.Ledger, SurfaceMeeting),
		Predictions: oracle,
		Expect: Expectation{
			State: StateScoredFailure,
			Checks: []Check{
				{MetricThirdPartyLeaks, OpEq, 1, "the item left ExpectedIn; surfacing it presents someone else's obligation as yours"},
				{MetricCriticalDirection, OpGT, 0, "and it is presented in the user-owed lane"},
			},
		},
	}}
}

func rowCitationSpanMove(in RedTeamInput) []RedTeamCase {
	real := in.real(SurfaceMeeting)
	base := mustScore(in.Ledger, real, SurfaceMeeting)
	moved := MoveGoldSpan(in.Ledger, real, SurfaceMeeting)
	want := base.Extraction.Precision - 1/float64(len(real))
	return []RedTeamCase{{
		Ledger:      moved,
		Predictions: real,
		Expect: Expectation{
			State: StateScoredFailure,
			Checks: []Check{
				{MetricExtraction + ".precision", OpEq, want, "exactly one line stops matching — precision falls by 1/P"},
				{MetricLooseMatches, OpEq, 0, "and it gets NO fuzzy credit for being nearby"},
				{MetricCitationCorrect + ".precision", OpEq, base.CitationCorrect.Precision, "grounding and extraction are not the same row wearing two hats"},
			},
		},
	}}
}

func rowAuthoredToQuoted(in RedTeamInput) []RedTeamCase {
	base := mustScore(in.Ledger, Oracle(in.Ledger, SurfaceMeeting), SurfaceMeeting)
	quoted := QuoteGoldBlock(in.Ledger, SurfaceMeeting)
	gold := goldCount(quoted, SurfaceMeeting)
	return []RedTeamCase{{
		Ledger:      quoted,
		Predictions: Oracle(quoted, SurfaceMeeting),
		Expect: Expectation{
			State: StateScoredFailure,
			Checks: []Check{
				{MetricExtraction + ".recall", OpEq, base.Extraction.Recall - 1/float64(gold), "the obligation must DISAPPEAR — recall falls by exactly 1/G"},
				{MetricExtraction + ".precision", OpEq, base.Extraction.Precision, "and precision is NOT credited for the disappearance"},
				{MetricNonObligationLeaks, OpEq, 0, ""},
			},
		},
	}}
}

func rowRemovedSource(surface string) func(RedTeamInput) []RedTeamCase {
	return func(in RedTeamInput) []RedTeamCase {
		return []RedTeamCase{{
			Ledger:      RemoveCitedSource(in.Ledger, surface),
			Predictions: in.real(surface),
			Expect:      Expectation{HarnessError: true},
		}}
	}
}

func rowDuplicateNoise(surface string) func(RedTeamInput) []RedTeamCase {
	return func(in RedTeamInput) []RedTeamCase {
		real := in.real(surface)
		return []RedTeamCase{{
			Ledger:      DuplicateUnrelatedArtifact(in.Ledger),
			Predictions: real,
			Expect: Expectation{
				Identical: &Baseline{Ledger: in.Ledger, Predictions: real},
			},
		}}
	}
}

func rowInputOrder(surface string) func(RedTeamInput) []RedTeamCase {
	return func(in RedTeamInput) []RedTeamCase {
		real := in.real(surface)
		return []RedTeamCase{{
			Ledger:      PermuteArtifacts(in.Ledger),
			Predictions: ReversePredictions(real),
			Expect: Expectation{
				Identical: &Baseline{Ledger: in.Ledger, Predictions: real},
			},
		}}
	}
}

// rowGateDisableSweep is the row that binds Matrix 1 to a SCORED consequence. For
// each of the seventeen production exclusion gates it takes the red-team world's
// oracle — which leaks nothing — and injects the one leak that gate is what stops.
// A gate whose disablement moves no number is not protecting anything, and this row
// is what says so, by name, per gate.
func rowGateDisableSweep(in RedTeamInput) []RedTeamCase {
	oracle := Oracle(in.Sabotage, SurfaceMeeting)
	cases := make([]RedTeamCase, 0, len(ProductionExclusionGates))
	for _, control := range GateControls() {
		preds, ok := InjectControlLeak(in.Sabotage, oracle, control)
		if !ok {
			// A gate with no injectable control is a HOLE, and a hole must be a
			// named failure, never a silent pass.
			cases = append(cases, RedTeamCase{
				Label:       control.Gate,
				Ledger:      in.Sabotage,
				Predictions: oracle,
				Expect: Expectation{Checks: []Check{{
					Metric: control.Counter, Op: OpEq, Want: math.NaN(),
					Note: fmt.Sprintf("gate %q names control case %q, which does not resolve in the red-team ledger", control.Gate, control.Case),
				}}},
			})
			continue
		}
		cases = append(cases, RedTeamCase{
			Label:       control.Gate,
			Ledger:      in.Sabotage,
			Predictions: preds,
			Expect: Expectation{
				State: StateScoredFailure,
				Checks: []Check{{
					Metric: control.Counter, Op: OpEq, Want: 1,
					Note: fmt.Sprintf("disabling %s leaks %s, and that must COST something scored", control.Gate, control.Case),
				}},
			},
		})
	}
	return cases
}

// MetricValue is the one place a metric id becomes a number. A red-team row that
// names an unknown metric is a named failure, not a silently skipped assertion.
func MetricValue(sc Scorecard, key string) (float64, bool) {
	prs := map[string]PR{
		MetricExtraction:       sc.Extraction,
		MetricRecallUncapped:   sc.RecallUncapped,
		MetricCitationCoverage: sc.CitationCoverage,
		MetricCitationCorrect:  sc.CitationCorrect,
		MetricCounterparty:     sc.Counterparty,
		MetricDirection:        sc.Direction,
		MetricDueTime:          sc.DueTime,
		MetricLifecycle:        sc.Lifecycle,
		MetricClosureLinkage:   sc.ClosureLinkage,
	}
	if id, part, ok := strings.Cut(key, "."); ok {
		row, known := prs[id]
		if !known {
			return 0, false
		}
		switch part {
		case "precision":
			return row.Precision, true
		case "recall":
			return row.Recall, true
		case "defined":
			return boolValue(row.Defined), true
		}
		return 0, false
	}
	counts := map[string]int{
		MetricDedupCrossArtifact: sc.DedupCrossArtifact,
		MetricThirdPartyLeaks:    sc.ThirdPartyLeaks,
		MetricClosedLeaks:        sc.ClosedLeaks,
		MetricDupLeaks:           sc.DupLeaks,
		MetricNonObligationLeaks: sc.NonObligationLeaks,
		MetricCriticalIdentity:   sc.CriticalIdentity,
		MetricCriticalDirection:  sc.CriticalDirection,
		MetricLooseMatches:       sc.LooseMatches,
		MetricUnmatched:          sc.Unmatched,
	}
	if n, ok := counts[key]; ok {
		return float64(n), true
	}
	if key == MetricDirectionScorable {
		return boolValue(sc.DirectionScorable), true
	}
	return 0, false
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
