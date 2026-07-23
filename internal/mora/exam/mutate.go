package exam

import (
	"sort"
	"strings"
)

// The mutants. Half of them corrupt the GOLD (a ledger the scorer must notice has
// changed); half corrupt the OUTPUT (predictions the scorer must notice are wrong).
// Every one is a pure function: the AST determinism guard proves no clock and no
// PRNG reaches this file, which is what lets the invariance rows promise
// byte-stability across runs.

func cloneLedgerValue(l Ledger) Ledger {
	out := l
	out.People = append([]Identity(nil), l.People...)
	out.Artifacts = make([]Artifact, len(l.Artifacts))
	for i, a := range l.Artifacts {
		a.Participants = append([]string(nil), l.Artifacts[i].Participants...)
		a.Messages = make([]Message, len(l.Artifacts[i].Messages))
		for j, m := range l.Artifacts[i].Messages {
			m.To = append([]string(nil), l.Artifacts[i].Messages[j].To...)
			m.Cc = append([]string(nil), l.Artifacts[i].Messages[j].Cc...)
			m.Body = append([]Block(nil), l.Artifacts[i].Messages[j].Body...)
			a.Messages[j] = m
		}
		out.Artifacts[i] = a
	}
	out.Commitments = make([]Commitment, len(l.Commitments))
	for i, c := range l.Commitments {
		c.ExpectedIn = append([]string(nil), l.Commitments[i].ExpectedIn...)
		c.Transitions = append([]Transition(nil), l.Commitments[i].Transitions...)
		out.Commitments[i] = c
	}
	out.NonObligations = append([]NonObligation(nil), l.NonObligations...)
	return out
}

func clonePredictions(preds []Prediction) []Prediction {
	out := append([]Prediction(nil), preds...)
	for i := range out {
		out[i].Citations = append([]PredictionCitation(nil), preds[i].Citations...)
	}
	return out
}

// firstGoldIndex is the deterministic choice of victim: the lowest ledger id among
// the commitments expected on the surface. Picking "some" gold item at random would
// make every mutant's expected delta unreproducible.
func firstGoldIndex(l Ledger, surface string) int {
	best := -1
	for i, c := range l.Commitments {
		expected := false
		for _, in := range c.ExpectedIn {
			expected = expected || surfaceCovers(surface, in)
		}
		if !expected {
			continue
		}
		if best < 0 || c.ID < l.Commitments[best].ID {
			best = i
		}
	}
	return best
}

// FlipGoldOwner moves one gold obligation from the user to a THIRD PARTY — someone
// neither owed by nor owing the user. It leaves ExpectedIn, so a product that still
// surfaces it is presenting someone else's obligation as yours.
func FlipGoldOwner(l Ledger, surface string) Ledger {
	out := cloneLedgerValue(l)
	i := firstGoldIndex(out, surface)
	if i < 0 {
		return out
	}
	c := out.Commitments[i]
	outsider := ""
	for _, p := range out.People {
		if p.ID != c.Owner && p.ID != c.Counterparty && !p.Service {
			outsider = p.ID
			break
		}
	}
	if outsider == "" {
		return out
	}
	c.Owner = outsider
	c.Direction = DirectionOwedByCounterparty
	c.ExpectedIn = nil
	out.Commitments[i] = c
	return out
}

// MoveGoldSpan shifts one gold citation to a different block IN THE SAME MEMORY. The
// line the product emits is now quoting the wrong sentence of the right thread —
// which must cost extraction precision and earn NO fuzzy credit.
//
// It moves a gold item the product ACTUALLY SURFACES. Moving one the product never
// found would be a mutant that changes no number and proves nothing — the exact
// class of dead test this exam exists to expose.
func MoveGoldSpan(l Ledger, preds []Prediction, surface string) Ledger {
	out := cloneLedgerValue(l)
	idx, err := indexLedger(out)
	if err != nil {
		return out
	}
	for _, i := range goldOrder(out, surface) {
		span := out.Commitments[i].OpenedBy
		memoryID, _, _, err := idx.resolveSpanText(span)
		if err != nil || !surfacedBy(preds, memoryID, span.Quote) {
			continue
		}
		for _, a := range out.Artifacts {
			if a.ID != span.ArtifactID {
				continue
			}
			for _, m := range a.Messages {
				for _, b := range m.Body {
					if m.ID == span.MessageID && b.ID == span.BlockID {
						continue
					}
					out.Commitments[i].OpenedBy = Span{ArtifactID: a.ID, MessageID: m.ID, BlockID: b.ID, Quote: b.Text}
					return out
				}
			}
		}
	}
	return out
}

func surfacedBy(preds []Prediction, memoryID, quote string) bool {
	for _, p := range preds {
		if p.MemoryID == memoryID && normalize(UnwrapEmitted(p.Text)) == normalize(quote) {
			return true
		}
	}
	return false
}

// QuoteGoldBlock reclassifies the block carrying a gold obligation as a
// connector-surviving quoted reply. The bytes are otherwise identical — only the
// authorship changes — and the obligation must DISAPPEAR, because sender-authored
// text is the whole basis for attributing it.
func QuoteGoldBlock(l Ledger, surface string) Ledger {
	out := cloneLedgerValue(l)
	for _, i := range goldOrder(out, surface) {
		span := out.Commitments[i].OpenedBy
		if span.BlockID == "" {
			continue
		}
		for ai, a := range out.Artifacts {
			if a.ID != span.ArtifactID {
				continue
			}
			for mi, m := range a.Messages {
				if m.ID != span.MessageID {
					continue
				}
				for bi, b := range m.Body {
					if b.ID != span.BlockID || b.Kind != "authored" {
						continue
					}
					out.Artifacts[ai].Messages[mi].Body[bi].Kind = "quoted_reply"
					return out
				}
			}
		}
	}
	return out
}

// RemoveCitedSource deletes the artifact a gold citation points at. The scorer must
// fail CLOSED with a named harness error — never report a silent recall miss, which
// would read as a product defect when the instrument is the thing that broke.
func RemoveCitedSource(l Ledger, surface string) Ledger {
	out := cloneLedgerValue(l)
	i := firstGoldIndex(out, surface)
	if i < 0 {
		return out
	}
	target := out.Commitments[i].OpenedBy.ArtifactID
	kept := make([]Artifact, 0, len(out.Artifacts))
	for _, a := range out.Artifacts {
		if a.ID != target {
			kept = append(kept, a)
		}
	}
	out.Artifacts = kept
	return out
}

// DuplicateUnrelatedArtifact adds noise that carries no label at all. The scorecard
// must not move by one bit: an eval whose number drifts when an irrelevant memory
// appears is measuring the corpus, not the product.
func DuplicateUnrelatedArtifact(l Ledger) Ledger {
	out := cloneLedgerValue(l)
	labelled := map[string]bool{}
	for _, c := range out.Commitments {
		labelled[c.OpenedBy.ArtifactID] = true
		for _, tr := range c.Transitions {
			labelled[tr.Evidence.ArtifactID] = true
		}
	}
	for _, n := range out.NonObligations {
		labelled[n.Span.ArtifactID] = true
	}
	for _, a := range out.Artifacts {
		if labelled[a.ID] || a.Channel != "gmail" {
			continue
		}
		noise := a
		noise.ID = a.ID + "-noise"
		noise.MemoryID = a.MemoryID + "-noise"
		noise.Subject = a.Subject + " (copy)"
		out.Artifacts = append(out.Artifacts, noise)
		return out
	}
	return out
}

// PermuteArtifacts reverses the artifact order. Independent input order is not a
// signal; a scorecard that moves under it is reading the file system.
func PermuteArtifacts(l Ledger) Ledger {
	out := cloneLedgerValue(l)
	for i, j := 0, len(out.Artifacts)-1; i < j; i, j = i+1, j-1 {
		out.Artifacts[i], out.Artifacts[j] = out.Artifacts[j], out.Artifacts[i]
	}
	for i, j := 0, len(out.Commitments)-1; i < j; i, j = i+1, j-1 {
		out.Commitments[i], out.Commitments[j] = out.Commitments[j], out.Commitments[i]
	}
	for i, j := 0, len(out.NonObligations)-1; i < j; i, j = i+1, j-1 {
		out.NonObligations[i], out.NonObligations[j] = out.NonObligations[j], out.NonObligations[i]
	}
	return out
}

func ReversePredictions(preds []Prediction) []Prediction {
	out := clonePredictions(preds)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func goldOrder(l Ledger, surface string) []int {
	var order []int
	for i, c := range l.Commitments {
		for _, in := range c.ExpectedIn {
			if surfaceCovers(surface, in) {
				order = append(order, i)
				break
			}
		}
	}
	sort.Slice(order, func(a, b int) bool { return l.Commitments[order[a]].ID < l.Commitments[order[b]].ID })
	return order
}

// FlipIdentities swaps the attendee of two REAL brief lines that were attributed to
// different people. This is the #135 class, reproduced on live output.
func FlipIdentities(l Ledger, preds []Prediction) []Prediction {
	out := clonePredictions(preds)
	first := -1
	for i, p := range out {
		if p.AttendeeAtom == "" {
			continue
		}
		if first < 0 {
			first = i
			continue
		}
		if out[first].AttendeeAtom == p.AttendeeAtom {
			continue
		}
		out[first].Attendee, out[i].Attendee = out[i].Attendee, out[first].Attendee
		out[first].AttendeeAtom, out[i].AttendeeAtom = out[i].AttendeeAtom, out[first].AttendeeAtom
		return out
	}
	return out
}

// FlipOneDirection reverses a single TYPED direction. Section movement is never used
// as a direction signal: the meeting brief collapses self-authored commitments and
// inbound requests into the same lane, so a moved line proves nothing about
// direction and everything about placement.
func FlipOneDirection(preds []Prediction) []Prediction {
	out := clonePredictions(preds)
	for i, p := range out {
		switch p.Direction {
		case DirectionOwedBySelf:
			out[i].Direction = DirectionOwedByCounterparty
			return out
		case DirectionOwedByCounterparty:
			out[i].Direction = DirectionOwedBySelf
			return out
		}
	}
	return out
}

func ConstantDirection(preds []Prediction, direction string) []Prediction {
	out := clonePredictions(preds)
	for i := range out {
		out[i].Direction = direction
	}
	return out
}

func FlipOneOwner(preds []Prediction) []Prediction {
	out := clonePredictions(preds)
	for i, p := range out {
		if isInventoryPrediction(p) || p.Owner == "" || p.Owner == Unknown {
			continue
		}
		out[i].Owner = "p/not-the-owner"
		return out
	}
	return out
}

func FlipOneCommitmentIdentity(preds []Prediction) []Prediction {
	out := clonePredictions(preds)
	for i, p := range out {
		if !isInventoryPrediction(p) || p.CommitmentID == "" {
			continue
		}
		out[i].CommitmentID = "commit:wrong"
		return out
	}
	return out
}

func FlipOneDuplicatePointer(preds []Prediction) []Prediction {
	out := clonePredictions(preds)
	for i, p := range out {
		if !isInventoryPrediction(p) || p.DuplicateOf == "" {
			continue
		}
		out[i].DuplicateOf = "commit:wrong"
		return out
	}
	return out
}

func FlipOneCitationRole(preds []Prediction) []Prediction {
	out := clonePredictions(preds)
	for i, p := range out {
		if !isInventoryPrediction(p) {
			continue
		}
		for j, citation := range p.Citations {
			if citation.Role == CitationRoleOpener {
				out[i].Citations[j].Role = CitationRoleClosure
				return out
			}
		}
	}
	return out
}

func FlipOneInventoryLifecycle(preds []Prediction) []Prediction {
	out := clonePredictions(preds)
	for i, p := range out {
		if !isInventoryPrediction(p) {
			continue
		}
		switch p.Lifecycle {
		case LifecycleOpen:
			out[i].Lifecycle = LifecycleClosed
			return out
		case LifecycleClosed, LifecycleSuperseded:
			out[i].Lifecycle = LifecycleOpen
			return out
		}
	}
	return out
}

// RepointOneCitation points a correct line at the wrong memory. The line's TEXT is
// untouched, so extraction can still find it — which is exactly what forces citation
// correctness to be its own row rather than a restatement of extraction.
func RepointOneCitation(l Ledger, preds []Prediction, surface string) []Prediction {
	out := clonePredictions(preds)
	idx, err := indexLedger(l)
	if err != nil {
		return out
	}
	gold, err := goldFor(l, idx, surface, sliceAny{})
	if err != nil {
		return out
	}
	for i, p := range out {
		for _, g := range gold {
			if g.MemoryID != p.MemoryID || normalize(g.Quote) != normalize(UnwrapEmitted(p.Text)) {
				continue
			}
			for _, a := range l.Artifacts {
				if a.MemoryID != p.MemoryID {
					out[i].MemoryID = a.MemoryID
					return out
				}
			}
		}
	}
	return out
}

// BlankAndRepointDailyCitations breaks the daily surface's two citation rows
// independently: one item loses its id (a COVERAGE miss), another keeps an id that
// names the wrong memory (a CORRECTNESS miss). If one mutation moved both rows, the
// rows would be one row wearing two hats.
func BlankAndRepointDailyCitations(l Ledger, preds []Prediction) []Prediction {
	out := clonePredictions(preds)
	if len(out) < 2 {
		return out
	}
	out[0].MemoryID = ""
	for _, a := range l.Artifacts {
		if a.MemoryID != out[1].MemoryID && !strings.Contains(normalize(out[1].Text), normalize(a.Subject)) {
			out[1].MemoryID = a.MemoryID
			break
		}
	}
	return out
}

// SurfaceClosedAsOpen presents a SETTLED obligation as current, in the active lane,
// with an invented deadline and a claim that it has no closure. Every one of those is
// separately wrong, and the scorecard must say so in a separate place.
func SurfaceClosedAsOpen(l Ledger, preds []Prediction, surface string) []Prediction {
	out := clonePredictions(preds)
	idx, err := indexLedger(l)
	if err != nil {
		return out
	}
	for _, c := range l.Commitments {
		if c.State != LifecycleClosed || len(c.ExpectedIn) != 0 {
			continue
		}
		memoryID, _, _, err := idx.resolveSpanText(c.OpenedBy)
		if err != nil {
			continue
		}
		out = append(out, Prediction{
			Surface:      SurfaceMeeting,
			Text:         "· " + idx.displayOf(counterpartOf(c, idx.self)) + " — “" + c.OpenedBy.Quote + "”",
			Attendee:     idx.displayOf(counterpartOf(c, idx.self)),
			AttendeeAtom: idx.atomOf(counterpartOf(c, idx.self)),
			MemoryID:     memoryID,
			SectionKind:  meetingOpenLoops,
			Direction:    c.Direction,
			Due:          l.AsOf,
			Lifecycle:    LifecycleOpen,
			ClosureRef:   ClosureNone,
		})
		return out
	}
	return out
}

// InjectControlLeak surfaces the one negative-control case a production gate is what
// stops. For an attribution gate the "leak" is a misattribution of a gold line
// rather than an extra line — the failure that gate prevents is a wrong person, not
// an extra row.
func InjectControlLeak(l Ledger, preds []Prediction, control GateControl) ([]Prediction, bool) {
	idx, err := indexLedger(l)
	if err != nil {
		return nil, false
	}
	if control.Counter == MetricCriticalIdentity {
		return misattributeCase(l, idx, preds, control.Case)
	}
	for _, n := range l.NonObligations {
		if n.ID != control.Case {
			continue
		}
		memoryID, _, _, err := idx.resolveSpanText(n.Span)
		if err != nil {
			return nil, false
		}
		return append(clonePredictions(preds), leakPrediction(idx, memoryID, n.Span.Quote, l.Self.ID)), true
	}
	for _, c := range l.Commitments {
		if c.ID != control.Case || len(c.ExpectedIn) != 0 {
			continue
		}
		memoryID, _, _, err := idx.resolveSpanText(c.OpenedBy)
		if err != nil {
			return nil, false
		}
		return append(clonePredictions(preds), leakPrediction(idx, memoryID, c.OpenedBy.Quote, counterpartOf(c, idx.self))), true
	}
	return nil, false
}

func leakPrediction(idx ledgerIndex, memoryID, quote, attendee string) Prediction {
	return Prediction{
		Surface:      SurfaceMeeting,
		Text:         "· " + idx.displayOf(attendee) + " — “" + quote + "”",
		Attendee:     idx.displayOf(attendee),
		AttendeeAtom: idx.atomOf(attendee),
		MemoryID:     memoryID,
		SectionKind:  meetingOpenLoops,
		Direction:    Unknown,
		Lifecycle:    Unknown,
	}
}

func misattributeCase(l Ledger, idx ledgerIndex, preds []Prediction, caseID string) ([]Prediction, bool) {
	var target Commitment
	for _, c := range l.Commitments {
		if c.ID == caseID {
			target = c
		}
	}
	if target.ID == "" {
		return nil, false
	}
	memoryID, _, _, err := idx.resolveSpanText(target.OpenedBy)
	if err != nil {
		return nil, false
	}
	wrong := ""
	for _, p := range l.People {
		if p.ID != counterpartOf(target, idx.self) && !p.Service {
			wrong = p.ID
			break
		}
	}
	if wrong == "" {
		return nil, false
	}
	out := clonePredictions(preds)
	for i, p := range out {
		if p.MemoryID != memoryID || normalize(UnwrapEmitted(p.Text)) != normalize(target.OpenedBy.Quote) {
			continue
		}
		out[i].Attendee = idx.displayOf(wrong)
		out[i].AttendeeAtom = idx.atomOf(wrong)
		return out, true
	}
	return nil, false
}
