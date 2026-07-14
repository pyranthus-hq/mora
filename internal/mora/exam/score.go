package exam

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// The two surfaces the exam scores. One Scorecard per surface; they are NEVER
// merged into a single number, because averaging a quote-grain meeting brief with
// an artifact-grain recency feed reports a number that describes neither.
const (
	SurfaceMeeting = "meeting"
	SurfaceDaily   = "daily"
)

// The typed vocabulary a Prediction may carry.
//
// The real adapters can only fill what the product payload carries, so every field
// the engine cannot express arrives as Unknown (or ""). That is what keeps the
// born-red rows visibly red instead of accidentally correct. The oracle fills the
// same fields from gold, which proves each row can go green the day the typed
// commitment model supplies the substrate.
const (
	Unknown = "unknown"

	DirectionOwedBySelf         = "owed_by_self"
	DirectionOwedByCounterparty = "owed_by_counterparty"

	// DueNone and DueRelative are the typed GOLD due values for a commitment that
	// carries no instant. They are deliberately distinct from "": "" means the
	// surface could not express a due time at all. Without the distinction the real
	// adapter's empty string would score CORRECT against a no-due obligation and the
	// DueTime row would not be born red — it would be accidentally right.
	DueNone     = "none"
	DueRelative = "relative"

	// ClosureNone is the typed gold value for "this obligation has no closure", for
	// the same reason: "" means unknown, not "no closure".
	ClosureNone = "none"

	LifecycleOpen       = "open"
	LifecycleClosed     = "closed"
	LifecycleSuperseded = "superseded"
)

// meetingOpenLoops is the user-owed lane. A gold commitment owed by a THIRD PARTY
// surfacing here presents someone else's obligation as yours.
const meetingOpenLoops = "open_loops"

// RunState keeps three outcomes apart that an eval must never collapse. A broken
// instrument must never be reported as a bad product, and a bad product must never
// be excused as a broken instrument.
type RunState string

const (
	StateInvalidHarness RunState = "INVALID_HARNESS"
	StateScoredFailure  RunState = "SCORED_FAILURE"
	StatePass           RunState = "PASS"
)

// ErrInvalidHarness tags every fault that makes a run unscorable: an invalid
// ledger, an unknown surface, zero required samples, a missing cited source
// artifact. Score returns it wrapped with the specific cause and a ZERO Scorecard —
// a harness fault is never a low score.
var ErrInvalidHarness = errors.New("INVALID_HARNESS")

// OwnerUnscorable is the refusal the Owner row reports, verbatim. Deriving an owner
// from SectionKind == "open_loops" would restate extraction precision on one
// section; it would not measure ownership.
const OwnerUnscorable = "UNSCORABLE: no commitment-owner field exists on the meeting or daily brief payload"

// Prediction is the scorer's typed input. The adapters that build it do no cleaning
// and no interpretation: every transformation belongs to Score, which lives in a
// package that structurally cannot reach the product's cleaners.
type Prediction struct {
	Surface      string `json:"surface"`
	Text         string `json:"text"`
	Attendee     string `json:"attendee,omitempty"`
	AttendeeAtom string `json:"attendee_atom,omitempty"`
	MemoryID     string `json:"memory_id"`
	SectionKind  string `json:"section_kind,omitempty"`
	Direction    string `json:"direction"`
	Due          string `json:"due"`
	Lifecycle    string `json:"lifecycle"`
	ClosureRef   string `json:"closure_ref"`
}

// PR carries Defined so an empty prediction set reports N/A, never a flattering
// 1.0. Every gate treats !Defined as failure.
type PR struct {
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	Defined   bool    `json:"defined"`
}

// Scorecard is one surface's honest scoreboard: live rows, leak absolutes, four
// born-red rows, one refused row, and the health of the scorer itself. Every
// numeric field here is registered in RequiredMetrics with at least one sabotage
// case that proves it can move — a field that cannot fail is decoration, and the
// registry meta-test makes adding one a named failure.
type Scorecard struct {
	Surface string `json:"surface"`

	// LIVE
	Extraction         PR  `json:"extraction"`
	RecallUncapped     PR  `json:"recall_uncapped"`
	CitationCoverage   PR  `json:"citation_coverage"`
	CitationCorrect    PR  `json:"citation_correct"`
	Counterparty       PR  `json:"counterparty"`
	DedupCrossArtifact int `json:"dedup_cross_artifact"`

	// LEAK / CRITICAL ABSOLUTES — how owner, lifecycle and dedup get measured with
	// no typed product field to read.
	ThirdPartyLeaks    int  `json:"third_party_leaks"`
	ClosedLeaks        int  `json:"closed_leaks"`
	DupLeaks           int  `json:"dup_leaks"`
	NonObligationLeaks int  `json:"non_obligation_leaks"`
	CriticalIdentity   int  `json:"critical_identity"`
	CriticalDirection  int  `json:"critical_direction"`
	DirectionScorable  bool `json:"direction_scorable"`

	// BORN-RED: typed-or-unknown. Never averaged into a headline number; they FAIL
	// the build the day they start passing.
	Direction      PR `json:"direction"`
	DueTime        PR `json:"due_time"`
	Lifecycle      PR `json:"lifecycle"`
	ClosureLinkage PR `json:"closure_linkage"`

	// REFUSED, and reported as such.
	Owner string `json:"owner"`

	// HEALTH OF THE SCORER ITSELF
	LooseMatches int `json:"loose_matches"`
	Unmatched    int `json:"unmatched"`
}

func harnessError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidHarness, fmt.Sprintf(format, args...))
}

// RunStateOf is the only place the three states are decided. PASS means the product
// got EVERYTHING right on this surface — including the four born-red rows, so a
// surface that can only emit "unknown" can never vacuously pass.
func RunStateOf(sc Scorecard, err error) RunState {
	if err != nil {
		return StateInvalidHarness
	}
	counts := []int{
		sc.DedupCrossArtifact, sc.ThirdPartyLeaks, sc.ClosedLeaks, sc.DupLeaks,
		sc.NonObligationLeaks, sc.CriticalIdentity, sc.CriticalDirection,
		sc.LooseMatches, sc.Unmatched,
	}
	for _, n := range counts {
		if n != 0 {
			return StateScoredFailure
		}
	}
	rows := []PR{sc.Extraction, sc.CitationCoverage, sc.CitationCorrect, sc.Direction, sc.DueTime, sc.Lifecycle, sc.ClosureLinkage}
	if sc.Surface == SurfaceMeeting {
		rows = append(rows, sc.Counterparty)
	}
	for _, row := range rows {
		if !row.Defined || row.Precision != 1 || row.Recall != 1 {
			return StateScoredFailure
		}
	}
	return StatePass
}

// Score REFUSES to run over broken ground truth. An eval over an invalid ledger is
// worse than no eval, because its number will be defended. Every validator rule is
// exercised through THIS chokepoint in the red team, not only through Validate.
func Score(l Ledger, preds []Prediction, surface string) (Scorecard, error) {
	return score(l, preds, surface, sliceAny{})
}

// ScoreSlice restricts the gold set to one slice of the corpus so a global average
// can never hide a collapsed slice. Packet E ratchets these floors alongside the
// absolute ones.
func ScoreSlice(l Ledger, preds []Prediction, surface, slice, value string) (Scorecard, error) {
	if !knownSlice(slice) {
		return Scorecard{}, harnessError("unknown slice %q", slice)
	}
	return score(l, preds, surface, sliceAny{slice: slice, value: value})
}

type sliceAny struct{ slice, value string }

func (s sliceAny) keeps(g goldItem) bool {
	if s.slice == "" {
		return true
	}
	return g.slices()[s.slice] == s.value
}

func score(l Ledger, preds []Prediction, surface string, only sliceAny) (Scorecard, error) {
	grain, err := grainOf(surface)
	if err != nil {
		return Scorecard{}, err
	}
	if err := Validate(l); err != nil {
		return Scorecard{}, harnessError("invalid ledger: %v", err)
	}
	for i, p := range preds {
		if p.Surface != grain {
			return Scorecard{}, harnessError("prediction %d declares surface %q, want %q", i, p.Surface, grain)
		}
	}
	idx, err := indexLedger(l)
	if err != nil {
		return Scorecard{}, err
	}
	gold, err := goldFor(l, idx, surface, only)
	if err != nil {
		return Scorecard{}, err
	}
	if grain == SurfaceDaily {
		return scoreDaily(l, idx, gold, preds), nil
	}
	return scoreMeeting(l, idx, gold, preds), nil
}

func grainOf(surface string) (string, error) {
	switch {
	case surface == SurfaceDaily:
		return SurfaceDaily, nil
	case surface == SurfaceMeeting, strings.HasPrefix(surface, SurfaceMeeting+":"):
		return SurfaceMeeting, nil
	}
	return "", harnessError("unknown surface %q", surface)
}

// surfaceCovers reports whether a ledger ExpectedIn entry is on the scored surface.
// "meeting" covers every meeting event; "meeting:<memory-id>" pins one.
func surfaceCovers(surface, expected string) bool {
	if surface == expected {
		return true
	}
	return surface == SurfaceMeeting && strings.HasPrefix(expected, SurfaceMeeting+":")
}

// goldItem is one labelled obligation, resolved against the corpus it addresses.
type goldItem struct {
	ID         string
	MemoryID   string
	Quote      string
	Attendee   string // identity id of the party who is NOT the user
	Owner      string
	Direction  string
	Due        string
	Lifecycle  string
	Closure    string
	Channel    string
	BlockKind  string
	State      string
	Transition string
}

func (g goldItem) slices() map[string]string {
	return map[string]string{
		SliceChannel:    g.Channel,
		SliceBlockKind:  g.BlockKind,
		SliceOwner:      g.Owner,
		SliceDirection:  g.Direction,
		SliceState:      g.State,
		SliceTransition: g.Transition,
	}
}

// negativeItem is a labelled span that must NOT reach the user's brief. Surfacing
// one is a false positive AND is separately reported under its archetype — which is
// how owner, lifecycle and dedup get measured without inventing a product field.
type negativeItem struct {
	CaseID          string
	MemoryID        string
	Quote           string
	Archetype       string
	Owner           string
	SectionCritical bool // a third-party obligation in the user-owed lane
}

type ledgerIndex struct {
	byArtifact map[string]Artifact
	byMemory   map[string]Artifact
	atoms      map[string]string // lowercased email or handle -> identity id
	displays   map[string]string // lowercased display name -> identity id
	self       string
}

func indexLedger(l Ledger) (ledgerIndex, error) {
	idx := ledgerIndex{
		byArtifact: map[string]Artifact{},
		byMemory:   map[string]Artifact{},
		atoms:      map[string]string{},
		displays:   map[string]string{},
		self:       l.Self.ID,
	}
	for _, a := range l.Artifacts {
		idx.byArtifact[a.ID] = a
		idx.byMemory[a.MemoryID] = a
	}
	for _, p := range append([]Identity{l.Self}, l.People...) {
		for _, raw := range append(append([]string(nil), p.Emails...), p.Handles...) {
			idx.atoms[strings.ToLower(strings.TrimSpace(raw))] = p.ID
		}
		if p.Display != "" {
			idx.displays[strings.ToLower(strings.TrimSpace(p.Display))] = p.ID
		}
	}
	return idx, nil
}

// resolveSpanText fails CLOSED. A gold citation whose source artifact is gone is a
// broken harness, never a silent recall miss.
func (idx ledgerIndex) resolveSpanText(s Span) (memoryID, blockKind, channel string, err error) {
	a, ok := idx.byArtifact[s.ArtifactID]
	if !ok {
		return "", "", "", harnessError("cited source artifact %q is missing from the ledger", s.ArtifactID)
	}
	if s.MessageID == "" {
		return a.MemoryID, "", a.Channel, nil
	}
	for _, m := range a.Messages {
		if m.ID != s.MessageID {
			continue
		}
		for _, b := range m.Body {
			if b.ID == s.BlockID {
				return a.MemoryID, b.Kind, a.Channel, nil
			}
		}
	}
	return "", "", "", harnessError("cited source block %q/%q is missing from artifact %q", s.MessageID, s.BlockID, s.ArtifactID)
}

func goldFor(l Ledger, idx ledgerIndex, surface string, only sliceAny) ([]goldItem, error) {
	var gold []goldItem
	for _, c := range l.Commitments {
		expected := false
		for _, in := range c.ExpectedIn {
			expected = expected || surfaceCovers(surface, in)
		}
		if !expected {
			continue
		}
		memoryID, blockKind, channel, err := idx.resolveSpanText(c.OpenedBy)
		if err != nil {
			return nil, err
		}
		item := goldItem{
			ID:         c.ID,
			MemoryID:   memoryID,
			Quote:      c.OpenedBy.Quote,
			Attendee:   counterpartOf(c, idx.self),
			Owner:      c.Owner,
			Direction:  c.Direction,
			Due:        goldDue(c),
			Lifecycle:  c.State,
			Closure:    ClosureNone,
			Channel:    channel,
			BlockKind:  blockKind,
			State:      c.State,
			Transition: "none",
		}
		if len(c.Transitions) > 0 {
			last := c.Transitions[len(c.Transitions)-1]
			closureMemory, _, _, err := idx.resolveSpanText(last.Evidence)
			if err != nil {
				return nil, err
			}
			item.Closure = closureMemory
			item.Transition = last.To
		}
		if only.keeps(item) {
			gold = append(gold, item)
		}
	}
	if len(gold) == 0 {
		return nil, harnessError("zero required samples on surface %q", surface)
	}
	sort.Slice(gold, func(i, j int) bool { return gold[i].ID < gold[j].ID })
	return gold, nil
}

// counterpartOf is the person a line about this commitment must be attributed to:
// the party who is NOT the user. A brief line attributed to anyone else is the #135
// wrong-person class.
func counterpartOf(c Commitment, self string) string {
	if c.Owner == self {
		return c.Counterparty
	}
	return c.Owner
}

func goldDue(c Commitment) string {
	switch c.DueKind {
	case "explicit_date":
		return c.DueAt
	case "relative":
		return DueRelative
	default:
		return DueNone
	}
}

func negativesFor(l Ledger, idx ledgerIndex, surface string) ([]negativeItem, error) {
	var out []negativeItem
	for _, c := range l.Commitments {
		expected := false
		for _, in := range c.ExpectedIn {
			expected = expected || surfaceCovers(surface, in)
		}
		if expected {
			continue
		}
		memoryID, _, _, err := idx.resolveSpanText(c.OpenedBy)
		if err != nil {
			return nil, err
		}
		thirdParty := c.Owner != idx.self && c.Counterparty != idx.self
		item := negativeItem{
			CaseID:          c.ID,
			MemoryID:        memoryID,
			Quote:           c.OpenedBy.Quote,
			Archetype:       ArchetypeUnexpected,
			Owner:           c.Owner,
			SectionCritical: thirdParty,
		}
		switch {
		case thirdParty:
			item.Archetype = ArchetypeThirdParty
		case c.DuplicateOf != "":
			item.Archetype = ArchetypeDuplicate
		case c.State == LifecycleClosed:
			item.Archetype = ArchetypeClosed
		}
		out = append(out, item)
	}
	for _, n := range l.NonObligations {
		memoryID, _, _, err := idx.resolveSpanText(n.Span)
		if err != nil {
			return nil, err
		}
		out = append(out, negativeItem{CaseID: n.ID, MemoryID: memoryID, Quote: n.Span.Quote, Archetype: ArchetypeNonObligation})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CaseID < out[j].CaseID })
	return out, nil
}

// spanUniverse is EVERY labelled span in the ledger — gold, negative, and closure
// evidence alike. Unmatched is measured against this universe and not against the
// gold set, because a surfaced line that hits a labelled NON-obligation is a false
// positive, not a hallucination. Collapsing the two is what makes an engineer
// "fix" a nonzero Unmatched by loosening the match predicate into containment —
// which destroys the instrument.
func spanUniverse(l Ledger) []string {
	var out []string
	for _, c := range l.Commitments {
		out = append(out, c.OpenedBy.Quote)
		for _, tr := range c.Transitions {
			out = append(out, tr.Evidence.Quote)
		}
	}
	for _, n := range l.NonObligations {
		out = append(out, n.Span.Quote)
	}
	return out
}

// normalize collapses whitespace and lowercases — AND NOTHING ELSE. Every further
// leniency is a place the scorer starts grading itself.
func normalize(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// UnwrapEmitted strips the meeting brief's presentation layer — the
// "~N days ago · Name — “" prefix and the trailing "”" — to recover the quoted
// evidence. The emitted line is a presentation string, not the source text.
func UnwrapEmitted(text string) string {
	open := strings.Index(text, "“")
	if open < 0 || !strings.HasSuffix(text, "”") {
		return text
	}
	return text[open+len("“") : len(text)-len("”")]
}

type meetingMatch struct {
	gold     int // index into gold, or -1
	negative int // index into negatives, or -1
	loose    bool
	grounded bool // the text hits SOME labelled span in the universe
}

// matchMeeting is the ONLY join between the gold ledger and a surfaced meeting line:
// the memory id is a hard gate, and the text must be EXACTLY equal after
// normalization. A hit that needs containment is recorded as a loose match and never
// credited — brittleness is reported, never absorbed. Each line matches at most one
// gold item: longest quote wins, ties by ledger id.
func matchMeeting(p Prediction, gold []goldItem, negatives []negativeItem, universe []string) meetingMatch {
	text := normalize(UnwrapEmitted(p.Text))
	out := meetingMatch{gold: -1, negative: -1}
	for i, g := range gold {
		if g.MemoryID != p.MemoryID || normalize(g.Quote) != text {
			continue
		}
		if out.gold < 0 || len(gold[out.gold].Quote) < len(g.Quote) {
			out.gold = i
		}
	}
	if out.gold >= 0 {
		out.grounded = true
		return out
	}
	for i, n := range negatives {
		if n.MemoryID != p.MemoryID || normalize(n.Quote) != text {
			continue
		}
		if out.negative < 0 || len(negatives[out.negative].Quote) < len(n.Quote) {
			out.negative = i
		}
	}
	for _, g := range gold {
		if g.MemoryID != p.MemoryID {
			continue
		}
		quote := normalize(g.Quote)
		if quote != "" && text != quote && (strings.Contains(text, quote) || strings.Contains(quote, text)) {
			out.loose = true
		}
	}
	for _, quote := range universe {
		if normalize(quote) == text {
			out.grounded = true
			break
		}
	}
	return out
}

func scoreMeeting(l Ledger, idx ledgerIndex, gold []goldItem, preds []Prediction) Scorecard {
	negatives, err := negativesFor(l, idx, SurfaceMeeting)
	if err != nil {
		// Unreachable: Validate has already resolved every span. The scorer still
		// refuses to invent a number it cannot compute.
		return Scorecard{Surface: SurfaceMeeting, Owner: OwnerUnscorable}
	}
	universe := spanUniverse(l)
	sc := Scorecard{Surface: SurfaceMeeting, Owner: OwnerUnscorable}

	goldHit := make([]bool, len(gold))
	goldAttributed := make([]bool, len(gold))
	exactPreds := 0
	acc := newTypedAccumulator(gold)

	for _, p := range preds {
		if p.Direction != "" && p.Direction != Unknown {
			sc.DirectionScorable = true
		}
		m := matchMeeting(p, gold, negatives, universe)
		if m.loose && m.gold < 0 {
			sc.LooseMatches++
		}
		if !m.grounded && !m.loose {
			sc.Unmatched++
		}
		if m.gold >= 0 {
			exactPreds++
			g := gold[m.gold]
			goldHit[m.gold] = true
			if idx.resolveAttendee(p) == g.Attendee {
				goldAttributed[m.gold] = true
			} else {
				sc.CriticalIdentity++
			}
			if p.Direction != "" && p.Direction != Unknown && p.Direction != g.Direction {
				sc.CriticalDirection++
			}
			acc.observeGold(p, g, m.gold)
			continue
		}
		if m.negative >= 0 {
			n := negatives[m.negative]
			switch n.Archetype {
			case ArchetypeNonObligation:
				sc.NonObligationLeaks++
			case ArchetypeThirdParty:
				sc.ThirdPartyLeaks++
			case ArchetypeClosed:
				sc.ClosedLeaks++
			case ArchetypeDuplicate:
				sc.DupLeaks++
			}
			if n.SectionCritical && p.SectionKind == meetingOpenLoops {
				sc.CriticalDirection++
			}
			acc.observeNegative(p, l, idx)
		}
	}

	sc.Extraction = ratio(exactPreds, len(preds), countTrue(goldHit), len(gold))
	sc.CitationCoverage = coverage(preds, idx)
	sc.CitationCorrect = citationCorrectness(preds, gold, idx)
	sc.Counterparty = perClassPR(gold, goldHit, goldAttributed, preds, idx)
	sc.DedupCrossArtifact = dedupCrossArtifact(l, idx, preds)
	sc.Direction = acc.direction()
	sc.DueTime = acc.due()
	sc.Lifecycle = acc.lifecycle()
	sc.ClosureLinkage = acc.closure()
	return sc
}

// resolveAttendee maps the product's attendee string back onto a ledger identity.
// The stable atom is authoritative (it is the governance key); the display string is
// only a fallback, because the product renders a person with no display name as a
// bare address.
func (idx ledgerIndex) resolveAttendee(p Prediction) string {
	if _, value, ok := strings.Cut(p.AttendeeAtom, ":"); ok {
		if id, found := idx.atoms[strings.ToLower(strings.TrimSpace(value))]; found {
			return id
		}
	}
	key := strings.ToLower(strings.TrimSpace(p.Attendee))
	if id, found := idx.atoms[key]; found {
		return id
	}
	return idx.displays[key]
}

func scoreDaily(l Ledger, idx ledgerIndex, gold []goldItem, preds []Prediction) Scorecard {
	sc := Scorecard{Surface: SurfaceDaily, Owner: OwnerUnscorable}
	goldMemories := map[string]int{}
	for i, g := range gold {
		goldMemories[g.MemoryID] = i
	}
	goldHit := make([]bool, len(gold))
	exactPreds := 0
	acc := newTypedAccumulator(gold)
	for _, p := range preds {
		if p.Direction != "" && p.Direction != Unknown {
			sc.DirectionScorable = true
		}
		if _, ok := idx.byMemory[p.MemoryID]; !ok {
			sc.Unmatched++
			continue
		}
		i, ok := goldMemories[p.MemoryID]
		if !ok {
			continue
		}
		exactPreds++
		goldHit[i] = true
		acc.observeGold(p, gold[i], i)
	}
	sc.Extraction = ratio(exactPreds, len(preds), countTrue(goldHit), len(gold))
	sc.CitationCoverage = coverage(preds, idx)
	sc.CitationCorrect = dailyCitationCorrectness(preds, idx)
	sc.DedupCrossArtifact = dedupCrossArtifact(l, idx, preds)
	sc.Direction = acc.direction()
	sc.DueTime = acc.due()
	sc.Lifecycle = acc.lifecycle()
	sc.ClosureLinkage = acc.closure()
	// Counterparty is N/A on the daily surface: DigestItem carries no attendee. It
	// is REPORTED as undefined, never folded into another row.
	// Leak absolutes are meeting-only for a structural reason: a leak is an
	// obligation CLAIM about the wrong item, and the daily brief makes no obligation
	// claim at all (it has no obligation lane — that absence is its own quarantined
	// row). Surfacing a memory in a recency feed is a precision cost, which
	// Extraction already reports; it is not a leak.
	return sc
}

func ratio(exactPreds, totalPreds, goldHit, totalGold int) PR {
	out := PR{Defined: totalPreds > 0}
	if totalPreds > 0 {
		out.Precision = float64(exactPreds) / float64(totalPreds)
	}
	if totalGold > 0 {
		out.Recall = float64(goldHit) / float64(totalGold)
	}
	return out
}

func coverage(preds []Prediction, idx ledgerIndex) PR {
	covered := 0
	for _, p := range preds {
		if p.MemoryID == "" {
			continue
		}
		if _, ok := idx.byMemory[p.MemoryID]; ok {
			covered++
		}
	}
	// Coverage has ONE denominator — the surfaced set — so both halves of the PR
	// carry it. The gates read a single number ("coverage == 1.0") and the ratchet
	// treats every row uniformly.
	return sameSidedPR(covered, len(preds))
}

func sameSidedPR(numerator, denominator int) PR {
	if denominator == 0 {
		return PR{}
	}
	value := float64(numerator) / float64(denominator)
	return PR{Precision: value, Recall: value, Defined: true}
}

// citationCorrectness joins on TEXT ALONE and then asks whether the citation points
// at the right memory. It deliberately does not reuse the extraction match, whose
// memory id is a hard gate: if it did, repointing a citation would silently become
// an extraction miss and the grounding row could never fail on its own.
func citationCorrectness(preds []Prediction, gold []goldItem, idx ledgerIndex) PR {
	correct, cited := 0, 0
	for _, p := range preds {
		text := normalize(UnwrapEmitted(p.Text))
		best := -1
		for i, g := range gold {
			if normalize(g.Quote) != text {
				continue
			}
			if best < 0 || len(gold[best].Quote) < len(g.Quote) {
				best = i
			}
		}
		if best < 0 {
			continue
		}
		cited++
		if p.MemoryID == gold[best].MemoryID {
			correct++
		}
	}
	return sameSidedPR(correct, cited)
}

// dailyCitationCorrectness works at ARTIFACT grain: a DigestItem is a title+snippet
// projection of a memory, so the citation is correct exactly when the memory it
// names is the memory the item is about.
func dailyCitationCorrectness(preds []Prediction, idx ledgerIndex) PR {
	correct, cited := 0, 0
	for _, p := range preds {
		a, ok := idx.byMemory[p.MemoryID]
		if !ok {
			continue
		}
		cited++
		if strings.Contains(normalize(p.Text), normalize(a.Subject)) {
			correct++
		}
	}
	return sameSidedPR(correct, cited)
}

// perClassPR reports PER-CLASS recall, macro-averaged, never accuracy. On a corpus
// with a class skew, accuracy rewards a constant classifier; a macro average over
// classes does not.
func perClassPR(gold []goldItem, hit, attributed []bool, preds []Prediction, idx ledgerIndex) PR {
	goldByClass, hitByClass := map[string]int{}, map[string]int{}
	for i, g := range gold {
		goldByClass[g.Attendee]++
		if hit[i] && attributed[i] {
			hitByClass[g.Attendee]++
		}
	}
	predByClass, correctByClass := map[string]int{}, map[string]int{}
	universe := map[string]goldItem{}
	for _, g := range gold {
		universe[g.MemoryID+"\x00"+normalize(g.Quote)] = g
	}
	scored := 0
	for _, p := range preds {
		g, ok := universe[p.MemoryID+"\x00"+normalize(UnwrapEmitted(p.Text))]
		if !ok {
			continue
		}
		scored++
		class := idx.resolveAttendee(p)
		predByClass[class]++
		if class == g.Attendee {
			correctByClass[class]++
		}
	}
	if scored == 0 || len(goldByClass) == 0 {
		return PR{}
	}
	return PR{
		Precision: macroMean(correctByClass, predByClass),
		Recall:    macroMean(hitByClass, goldByClass),
		Defined:   true,
	}
}

func macroMean(numerators, denominators map[string]int) float64 {
	if len(denominators) == 0 {
		return 0
	}
	keys := make([]string, 0, len(denominators))
	for key := range denominators {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	total := 0.0
	for _, key := range keys {
		total += float64(numerators[key]) / float64(denominators[key])
	}
	return total / float64(len(keys))
}

// dedupCrossArtifact is falsifiable where a per-memory dedup counter is not: the
// engine collapses each memory to at most one line BY CONSTRUCTION, so only a
// DuplicateOf pair living in two DIFFERENT artifacts can ever both surface.
func dedupCrossArtifact(l Ledger, idx ledgerIndex, preds []Prediction) int {
	surfaced := map[string]bool{}
	for _, p := range preds {
		surfaced[p.MemoryID] = true
	}
	pairs := 0
	for _, c := range l.Commitments {
		if c.DuplicateOf == "" {
			continue
		}
		copyMemory, _, _, err := idx.resolveSpanText(c.OpenedBy)
		if err != nil {
			continue
		}
		for _, canonical := range l.Commitments {
			if canonical.ID != c.DuplicateOf {
				continue
			}
			canonicalMemory, _, _, err := idx.resolveSpanText(canonical.OpenedBy)
			if err != nil || canonicalMemory == copyMemory {
				continue
			}
			if surfaced[copyMemory] && surfaced[canonicalMemory] {
				pairs++
			}
		}
	}
	return pairs
}

func countTrue(flags []bool) int {
	n := 0
	for _, f := range flags {
		if f {
			n++
		}
	}
	return n
}

// typedAccumulator scores the four born-red rows. Recall runs over the gold set;
// precision runs over the predictions that ACTUALLY EXPRESSED a value, so a surface
// emitting only "unknown" reports N/A — a failure — instead of a vacuous 1.0.
type typedAccumulator struct {
	gold []goldItem

	directionHit  map[string]int
	directionGold map[string]int
	directionOK   map[string]int
	directionPred map[string]int

	dueHit, dueCorrect, duePred                   int
	lifecycleHit, lifecycleCorrect, lifecyclePred int
	closureHit, closureCorrect, closurePred       int
}

func newTypedAccumulator(gold []goldItem) *typedAccumulator {
	acc := &typedAccumulator{
		gold:          gold,
		directionHit:  map[string]int{},
		directionGold: map[string]int{},
		directionOK:   map[string]int{},
		directionPred: map[string]int{},
	}
	for _, g := range gold {
		acc.directionGold[g.Direction]++
	}
	return acc
}

func (a *typedAccumulator) observeGold(p Prediction, g goldItem, index int) {
	if p.Direction != "" && p.Direction != Unknown {
		a.directionPred[p.Direction]++
		if p.Direction == g.Direction {
			a.directionOK[p.Direction]++
			a.directionHit[g.Direction]++
		}
	}
	if p.Due != "" {
		a.duePred++
		if p.Due == g.Due {
			a.dueCorrect++
			a.dueHit++
		}
	}
	if p.Lifecycle != "" && p.Lifecycle != Unknown {
		a.lifecyclePred++
		if p.Lifecycle == g.Lifecycle {
			a.lifecycleCorrect++
			a.lifecycleHit++
		}
	}
	if p.ClosureRef != "" {
		a.closurePred++
		if p.ClosureRef == g.Closure {
			a.closureCorrect++
			a.closureHit++
		}
	}
}

// observeNegative scores the typed rows over a prediction that hit a labelled span
// which must NOT surface. It cannot contribute recall — the item is not gold — but a
// wrong typed claim about it is still a precision failure, which is what makes the
// closed-as-open mutant move Lifecycle and ClosureLinkage rather than only a leak.
func (a *typedAccumulator) observeNegative(p Prediction, l Ledger, idx ledgerIndex) {
	for _, c := range l.Commitments {
		memoryID, _, _, err := idx.resolveSpanText(c.OpenedBy)
		if err != nil || memoryID != p.MemoryID || normalize(c.OpenedBy.Quote) != normalize(UnwrapEmitted(p.Text)) {
			continue
		}
		if p.Direction != "" && p.Direction != Unknown {
			a.directionPred[p.Direction]++
			if p.Direction == c.Direction {
				a.directionOK[p.Direction]++
			}
		}
		if p.Due != "" {
			a.duePred++
			if p.Due == goldDue(c) {
				a.dueCorrect++
			}
		}
		if p.Lifecycle != "" && p.Lifecycle != Unknown {
			a.lifecyclePred++
			if p.Lifecycle == c.State {
				a.lifecycleCorrect++
			}
		}
		if p.ClosureRef != "" {
			a.closurePred++
			want := ClosureNone
			if len(c.Transitions) > 0 {
				want, _, _, _ = idx.resolveSpanText(c.Transitions[len(c.Transitions)-1].Evidence)
			}
			if p.ClosureRef == want {
				a.closureCorrect++
			}
		}
		return
	}
}

func (a *typedAccumulator) direction() PR {
	out := PR{Recall: macroMean(a.directionHit, a.directionGold)}
	if len(a.directionPred) > 0 {
		out.Precision = macroMean(a.directionOK, a.directionPred)
		out.Defined = true
	}
	return out
}

func (a *typedAccumulator) due() PR { return typedPR(a.dueHit, len(a.gold), a.dueCorrect, a.duePred) }

func (a *typedAccumulator) lifecycle() PR {
	return typedPR(a.lifecycleHit, len(a.gold), a.lifecycleCorrect, a.lifecyclePred)
}

func (a *typedAccumulator) closure() PR {
	return typedPR(a.closureHit, len(a.gold), a.closureCorrect, a.closurePred)
}

func typedPR(hit, gold, correct, expressed int) PR {
	out := PR{}
	if gold > 0 {
		out.Recall = float64(hit) / float64(gold)
	}
	if expressed > 0 {
		out.Precision = float64(correct) / float64(expressed)
		out.Defined = true
	}
	return out
}

// Verdict is the scorer's judgment on ONE line, and one gold item it never found.
// It is the audit's input: a human adjudicates these rows against the corpus and the
// obligation contract, with the verdict column hidden.
type Verdict struct {
	Surface  string
	Kind     string
	MemoryID string
	Label    string // the gold or negative case the line hit, "" when it hit nothing
	Text     string
}

const (
	VerdictTruePositive  = "true_positive"
	VerdictFalsePositive = "false_positive"
	VerdictLooseMatch    = "loose_match"
	VerdictUnmatched     = "unmatched"
	VerdictGoldMiss      = "gold_miss"
	verdictLeakPrefix    = "leak_"
)

// Classify is Score's per-line explanation, computed by the same predicate. Anything
// that disagrees with the Scorecard here is a bug in one of the two, which is exactly
// why they share matchMeeting rather than each having their own idea of a match.
func Classify(l Ledger, preds []Prediction, surface string) ([]Verdict, error) {
	grain, err := grainOf(surface)
	if err != nil {
		return nil, err
	}
	if err := Validate(l); err != nil {
		return nil, harnessError("invalid ledger: %v", err)
	}
	idx, err := indexLedger(l)
	if err != nil {
		return nil, err
	}
	gold, err := goldFor(l, idx, surface, sliceAny{})
	if err != nil {
		return nil, err
	}
	negatives, err := negativesFor(l, idx, grain)
	if err != nil {
		return nil, err
	}
	universe := spanUniverse(l)
	hit := make([]bool, len(gold))
	var out []Verdict
	for _, p := range preds {
		v := Verdict{Surface: grain, MemoryID: p.MemoryID, Text: oneLineText(p.Text)}
		if grain == SurfaceDaily {
			// A daily item that is not gold is a plain FALSE POSITIVE, never a
			// "leak": the digest is a recency feed that makes no obligation claim,
			// so it cannot present the wrong obligation as yours. Extraction
			// precision already reports the cost, and calling it a leak would invent
			// a sev-1 the surface is structurally incapable of committing.
			v.Kind = VerdictUnmatched
			if _, ok := idx.byMemory[p.MemoryID]; ok {
				v.Kind = VerdictFalsePositive
			}
			for i, g := range gold {
				if g.MemoryID == p.MemoryID {
					v.Kind, v.Label, hit[i] = VerdictTruePositive, g.ID, true
				}
			}
			out = append(out, v)
			continue
		}
		m := matchMeeting(p, gold, negatives, universe)
		switch {
		case m.gold >= 0:
			v.Kind, v.Label = VerdictTruePositive, gold[m.gold].ID
			hit[m.gold] = true
		case m.negative >= 0:
			v.Kind, v.Label = verdictLeakPrefix+negatives[m.negative].Archetype, negatives[m.negative].CaseID
		case m.loose:
			v.Kind = VerdictLooseMatch
		default:
			v.Kind = VerdictUnmatched
		}
		out = append(out, v)
	}
	for i, g := range gold {
		if hit[i] {
			continue
		}
		out = append(out, Verdict{Surface: grain, Kind: VerdictGoldMiss, MemoryID: g.MemoryID, Label: g.ID, Text: oneLineText(g.Quote)})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.MemoryID != b.MemoryID {
			return a.MemoryID < b.MemoryID
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		return a.Text < b.Text
	})
	return out, nil
}

func oneLineText(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// WithRecallUncapped folds a second run of the SAME surface at a higher per-attendee
// cap into the scorecard, so the ranker's cap is a visible NUMBER instead of a
// hidden confound in the extraction row (Finding 7).
func WithRecallUncapped(sc Scorecard, l Ledger, uncapped []Prediction, surface string) (Scorecard, error) {
	other, err := Score(l, uncapped, surface)
	if err != nil {
		return Scorecard{}, err
	}
	sc.RecallUncapped = other.Extraction
	return sc, nil
}
