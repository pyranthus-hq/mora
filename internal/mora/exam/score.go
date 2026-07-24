package exam

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ScorerVersion freezes the meaning and shape of every scorecard emitted by this
// package. Version 2 adds direct commitment observability while retaining every
// version-1 metric's value for predictions that carry no commitment substrate.
const ScorerVersion = 2

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

type PredictionOrigin string

const (
	PredictionOriginSurface   PredictionOrigin = "surface"
	PredictionOriginInventory PredictionOrigin = "inventory"
)

type CitationRole string

const (
	CitationRoleOpener     CitationRole = "opener"
	CitationRoleClosure    CitationRole = "closure"
	CitationRoleSupporting CitationRole = "supporting"
)

// PredictionCitation keeps the evidence role and the commitment it supports
// explicit. CommitmentID is required for supporting citations so two labelled
// commitments in one memory remain independently observable.
type PredictionCitation struct {
	MemoryID     string       `json:"memory_id"`
	CommitmentID string       `json:"commitment_id"`
	Role         CitationRole `json:"role"`
}

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

// Prediction is the scorer's typed input. The adapters that build it do no cleaning
// and no interpretation: every transformation belongs to Score, which lives in a
// package that structurally cannot reach the product's cleaners.
type Prediction struct {
	Surface      string           `json:"surface"`
	Origin       PredictionOrigin `json:"origin,omitempty"`
	Text         string           `json:"text"`
	Attendee     string           `json:"attendee,omitempty"`
	AttendeeAtom string           `json:"attendee_atom,omitempty"`
	// CounterpartyLabel is explicit name-grain attribution when the source has no
	// provider identity atom. It never substitutes for a present atom.
	CounterpartyLabel string               `json:"counterparty_label,omitempty"`
	Owner             string               `json:"owner,omitempty"`
	MemoryID          string               `json:"memory_id"`
	SectionKind       string               `json:"section_kind,omitempty"`
	CommitmentID      string               `json:"commitment_id,omitempty"`
	DuplicateOf       string               `json:"duplicate_of,omitempty"`
	Direction         string               `json:"direction"`
	Due               string               `json:"due"`
	Lifecycle         string               `json:"lifecycle"`
	ClosureRef        string               `json:"closure_ref"`
	Citations         []PredictionCitation `json:"citations,omitempty"`
}

// PR carries Defined so an empty prediction set reports N/A, never a flattering
// 1.0. Every gate treats !Defined as failure.
type PR struct {
	Precision     float64            `json:"precision"`
	Recall        float64            `json:"recall"`
	Defined       bool               `json:"defined"`
	RecallByClass map[string]float64 `json:"recall_by_class,omitempty"`
}

// Scorecard is one surface's honest scoreboard: live rows, leak absolutes, eight
// born-red typed rows, and the health of the scorer itself. Every
// numeric field here is registered in RequiredMetrics with at least one sabotage
// case that proves it can move — a field that cannot fail is decoration, and the
// registry meta-test makes adding one a named failure.
type Scorecard struct {
	ScorerVersion int    `json:"scorer_version"`
	Surface       string `json:"surface"`

	// LIVE
	Extraction         PR  `json:"extraction"`
	RecallUncapped     PR  `json:"recall_uncapped"`
	CitationCoverage   PR  `json:"citation_coverage"`
	CitationCorrect    PR  `json:"citation_correct"`
	Counterparty       PR  `json:"counterparty"`
	DedupCrossArtifact int `json:"dedup_cross_artifact"`

	// LEAK / CRITICAL ABSOLUTES — visible-surface display discipline, kept
	// separately from the typed inventory dimensions below.
	ThirdPartyLeaks    int  `json:"third_party_leaks"`
	ClosedLeaks        int  `json:"closed_leaks"`
	DupLeaks           int  `json:"dup_leaks"`
	NonObligationLeaks int  `json:"non_obligation_leaks"`
	CriticalIdentity   int  `json:"critical_identity"`
	CriticalDirection  int  `json:"critical_direction"`
	DirectionScorable  bool `json:"direction_scorable"`

	// BORN-RED: typed-or-unknown. Never averaged into a headline number.
	Owner              PR `json:"owner"`
	Direction          PR `json:"direction"`
	DueTime            PR `json:"due_time"`
	Lifecycle          PR `json:"lifecycle"`
	ClosureLinkage     PR `json:"closure_linkage"`
	CommitmentIdentity PR `json:"commitment_identity"`
	Dedup              PR `json:"dedup"`
	CitationRoles      PR `json:"citation_roles"`

	// HEALTH OF THE SCORER ITSELF
	LooseMatches int `json:"loose_matches"`
	Unmatched    int `json:"unmatched"`
}

func harnessError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidHarness, fmt.Sprintf(format, args...))
}

// RunStateOf is the only place the three states are decided. PASS means the product
// got EVERYTHING right on this surface — including every born-red row, so a
// surface that can only emit "unknown" can never vacuously pass.
func RunStateOf(sc Scorecard, err error) RunState {
	if err != nil {
		return StateInvalidHarness
	}
	if sc.ScorerVersion != ScorerVersion {
		return StateScoredFailure
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
	rows := []PR{
		sc.Extraction,
		sc.CitationCoverage,
		sc.CitationCorrect,
		sc.Owner,
		sc.Direction,
		sc.DueTime,
		sc.Lifecycle,
		sc.ClosureLinkage,
		sc.CommitmentIdentity,
		sc.Dedup,
		sc.CitationRoles,
	}
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
		if p.Origin != "" && p.Origin != PredictionOriginSurface && p.Origin != PredictionOriginInventory {
			return Scorecard{}, harnessError("prediction %d declares origin %q", i, p.Origin)
		}
	}
	preds = uniquePredictions(preds)
	idx, err := indexLedger(l)
	if err != nil {
		return Scorecard{}, err
	}
	gold, err := goldFor(l, idx, surface, only)
	if err != nil {
		return Scorecard{}, err
	}
	inventoryGold, err := inventoryGoldFor(l, idx, only)
	if err != nil {
		return Scorecard{}, err
	}
	if grain == SurfaceDaily {
		return scoreDaily(l, idx, gold, inventoryGold, preds), nil
	}
	return scoreMeeting(l, idx, gold, inventoryGold, preds), nil
}

// uniquePredictions makes the prediction list set-like before any metric consumes
// it. Repeating an identical claim cannot create more truth, improve a ratio, or
// multiply an absolute leak/health counter.
func uniquePredictions(preds []Prediction) []Prediction {
	seen := make(map[string]struct{}, len(preds))
	out := make([]Prediction, 0, len(preds))
	for _, p := range preds {
		if isInventoryPrediction(p) && p.CommitmentID == "" {
			// Ref-less counterparty rows have no immutable key that proves two
			// identical-looking claims are the same product fact. Preserve each
			// emitted row: only one may claim a matching gold row, while the rest
			// must remain observable as counterparty precision cost.
			out = append(out, p)
			continue
		}
		keyPrediction := p
		keyPrediction.Citations = append([]PredictionCitation(nil), p.Citations...)
		sort.Slice(keyPrediction.Citations, func(i, j int) bool {
			a, b := keyPrediction.Citations[i], keyPrediction.Citations[j]
			if a.CommitmentID != b.CommitmentID {
				return a.CommitmentID < b.CommitmentID
			}
			if a.Role != b.Role {
				return a.Role < b.Role
			}
			return a.MemoryID < b.MemoryID
		})
		encoded, _ := json.Marshal(keyPrediction)
		key := string(encoded)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, p)
	}
	return out
}

// A genuine inventory row has no rendered text or section. A visible line cannot
// relabel itself as inventory to escape extraction or leak scoring: a text-bearing
// row is always scored as surface output regardless of its declared origin.
func isInventoryPrediction(p Prediction) bool {
	return p.Origin == PredictionOriginInventory && strings.TrimSpace(p.Text) == "" && p.SectionKind == ""
}

func surfacePredictions(preds []Prediction) []Prediction {
	out := make([]Prediction, 0, len(preds))
	for _, p := range preds {
		if !isInventoryPrediction(p) {
			out = append(out, p)
		}
	}
	return out
}

func inventoryPredictions(preds []Prediction) []Prediction {
	out := make([]Prediction, 0, len(preds))
	for _, p := range preds {
		if isInventoryPrediction(p) {
			out = append(out, p)
		}
	}
	return out
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
	ID           string // human-readable ledger label
	CommitmentID string // evidence-derived product identity
	MemoryID     string
	Quote        string
	Attendee     string // identity id of the party who is NOT the user
	Owner        string
	Direction    string
	Due          string
	Lifecycle    string
	Closure      string
	Channel      string
	BlockKind    string
	State        string
	Transition   string
	DuplicateOf  string
}

type commitmentMemoryKey struct {
	CommitmentID string
	MemoryID     string
}

// commitmentStableIDs is the scorer-side oracle for the versioned product ID.
// Identity uses only immutable opening evidence. Person identity is deliberately
// absent, so an alias correction or graph merge cannot churn commitment IDs.
func commitmentStableIDs(l Ledger) map[string]string {
	type located struct {
		label   string
		message string
		block   string
		offset  int
	}
	groups := map[string][]located{}
	for _, c := range l.Commitments {
		a, ok := artifactByID(l, c.OpenedBy.ArtifactID)
		if !ok {
			continue
		}
		messageRef := a.MemoryID + "#subject"
		blockRef := "subject"
		offset := strings.Index(a.Subject, c.OpenedBy.Quote)
		if c.OpenedBy.MessageID != "" {
			messageRef = a.MemoryID + "#" + c.OpenedBy.MessageID
			blockRef = c.OpenedBy.BlockID
			offset = openingOffset(a, c.OpenedBy)
		}
		group := messageRef + "\x00" + blockRef
		groups[group] = append(groups[group], located{
			label: c.ID, message: messageRef, block: blockRef, offset: offset,
		})
	}
	out := make(map[string]string, len(l.Commitments))
	for _, commitments := range groups {
		sort.Slice(commitments, func(i, j int) bool {
			if commitments[i].offset != commitments[j].offset {
				return commitments[i].offset < commitments[j].offset
			}
			return commitments[i].label < commitments[j].label
		})
		for slot, commitment := range commitments {
			out[commitment.label] = stableCommitmentID(commitment.message, commitment.block, slot)
		}
	}
	return out
}

func artifactByID(l Ledger, id string) (Artifact, bool) {
	for _, a := range l.Artifacts {
		if a.ID == id {
			return a, true
		}
	}
	return Artifact{}, false
}

func openingOffset(a Artifact, span Span) int {
	for _, message := range a.Messages {
		if message.ID != span.MessageID {
			continue
		}
		for _, block := range message.Body {
			if block.ID == span.BlockID {
				return strings.Index(block.Text, span.Quote)
			}
		}
	}
	return -1
}

func stableCommitmentID(messageRef, blockRef string, slot int) string {
	hash := sha256.New()
	for _, component := range []string{messageRef, blockRef, strconv.Itoa(slot)} {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(component)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(component))
	}
	return "commit:v1:" + hex.EncodeToString(hash.Sum(nil))
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
// one is a false positive AND is separately reported under its archetype.
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
	labels     map[string]string // unique full/given display labels -> identity id
	self       string
}

func indexLedger(l Ledger) (ledgerIndex, error) {
	idx := ledgerIndex{
		byArtifact: map[string]Artifact{},
		byMemory:   map[string]Artifact{},
		atoms:      map[string]string{},
		displays:   map[string]string{},
		labels:     map[string]string{},
		self:       l.Self.ID,
	}
	for _, a := range l.Artifacts {
		idx.byArtifact[a.ID] = a
		idx.byMemory[a.MemoryID] = a
	}
	labelCandidates := map[string][]string{}
	for _, p := range append([]Identity{l.Self}, l.People...) {
		for _, raw := range append(append([]string(nil), p.Emails...), p.Handles...) {
			idx.atoms[strings.ToLower(strings.TrimSpace(raw))] = p.ID
		}
		if p.Display != "" {
			display := strings.ToLower(strings.TrimSpace(p.Display))
			idx.displays[display] = p.ID
			labelCandidates[display] = append(labelCandidates[display], p.ID)
			if fields := strings.Fields(display); len(fields) > 0 {
				labelCandidates[fields[0]] = append(labelCandidates[fields[0]], p.ID)
			}
		}
	}
	for label, ids := range labelCandidates {
		unique := map[string]bool{}
		for _, id := range ids {
			unique[id] = true
		}
		if len(unique) == 1 {
			for id := range unique {
				idx.labels[label] = id
			}
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
	stableIDs := commitmentStableIDs(l)
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
			ID:           c.ID,
			CommitmentID: stableIDs[c.ID],
			MemoryID:     memoryID,
			Quote:        c.OpenedBy.Quote,
			Attendee:     counterpartOf(c, idx.self),
			Owner:        c.Owner,
			Direction:    c.Direction,
			Due:          goldDue(c),
			Lifecycle:    c.State,
			Closure:      ClosureNone,
			Channel:      channel,
			BlockKind:    blockKind,
			State:        c.State,
			Transition:   "none",
			DuplicateOf:  stableIDs[c.DuplicateOf],
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

// inventoryGoldFor returns the complete commitment inventory, including closed,
// superseded, duplicate, and otherwise non-rendered commitments. Visible surface
// scoring asks "what may be shown?"; inventory scoring asks "what does the product
// know?". Keeping those contracts separate prevents both false closure and leak
// laundering.
func inventoryGoldFor(l Ledger, idx ledgerIndex, only sliceAny) ([]goldItem, error) {
	var gold []goldItem
	stableIDs := commitmentStableIDs(l)
	for _, c := range l.Commitments {
		memoryID, blockKind, channel, err := idx.resolveSpanText(c.OpenedBy)
		if err != nil {
			return nil, err
		}
		item := goldItem{
			ID:           c.ID,
			CommitmentID: stableIDs[c.ID],
			MemoryID:     memoryID,
			Quote:        c.OpenedBy.Quote,
			Attendee:     counterpartOf(c, idx.self),
			Owner:        c.Owner,
			Direction:    c.Direction,
			Due:          goldDue(c),
			Lifecycle:    c.State,
			Closure:      ClosureNone,
			Channel:      channel,
			BlockKind:    blockKind,
			State:        c.State,
			Transition:   "none",
			DuplicateOf:  stableIDs[c.DuplicateOf],
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
		return nil, harnessError("zero commitment-inventory samples")
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

func explicitDueDay(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if date, err := time.Parse("2006-01-02", raw); err == nil {
		return date.Format("2006-01-02"), true
	}
	if instant, err := time.Parse(time.RFC3339, raw); err == nil {
		return instant.Format("2006-01-02"), true
	}
	return "", false
}

func dueForComparison(raw string) string {
	if raw == DueNone || raw == DueRelative {
		return raw
	}
	// A dated phrase can support its calendar day but not the ledger's
	// annotation-time clock. Compare explicit dates at day granularity; clock-level
	// scoring waits for an event-linked due extractor that carries time as a
	// distinct typed capability.
	if day, ok := explicitDueDay(raw); ok {
		return day
	}
	return raw
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

// predictionGroundedInRenderedMemory independently checks the identity join
// against the bytes the corpus renderer gives the product. Presentation is
// unwrapped first; normalized containment then tolerates only whitespace and case.
func predictionGroundedInRenderedMemory(l Ledger, idx ledgerIndex, p Prediction) bool {
	text := normalize(UnwrapEmitted(p.Text))
	a, ok := idx.byMemory[p.MemoryID]
	if text == "" || !ok {
		return false
	}
	ids := map[string]Identity{l.Self.ID: l.Self}
	for _, person := range l.People {
		ids[person.ID] = person
	}
	rendered, _, err := renderArtifact(a, ids, l.Self.ID, l.Version)
	if err != nil {
		return false
	}
	memoryBytes, err := renderFrontmatter(rendered)
	if err != nil {
		return false
	}
	return strings.Contains(normalize(string(memoryBytes)), text)
}

func scoreMeeting(l Ledger, idx ledgerIndex, gold, inventoryGold []goldItem, preds []Prediction) Scorecard {
	negatives, err := negativesFor(l, idx, SurfaceMeeting)
	if err != nil {
		// Unreachable: Validate has already resolved every span. The scorer still
		// refuses to invent a number it cannot compute.
		return Scorecard{ScorerVersion: ScorerVersion, Surface: SurfaceMeeting}
	}
	visible := surfacePredictions(preds)
	inventory := inventoryPredictions(preds)
	universe := spanUniverse(l)
	sc := Scorecard{ScorerVersion: ScorerVersion, Surface: SurfaceMeeting}

	goldHit := make([]bool, len(gold))
	goldAttributed := make([]bool, len(gold))
	exactPreds := 0
	acc := newTypedAccumulator(gold, idx)
	commitments := newCommitmentAccumulator(l, idx, inventoryGold)
	identityGold := make(map[commitmentMemoryKey]int, len(gold))
	if l.Version >= SchemaV3 {
		for i, g := range gold {
			identityGold[commitmentMemoryKey{CommitmentID: g.CommitmentID, MemoryID: g.MemoryID}] = i
		}
	}

	for _, p := range visible {
		commitments.observeVisibleClaim(p)
		if p.Direction != "" && p.Direction != Unknown {
			sc.DirectionScorable = true
		}
		m := matchMeeting(p, gold, negatives, universe)
		matchedGold := m.gold
		if l.Version >= SchemaV3 {
			matchedGold = -1
			i, ok := identityGold[commitmentMemoryKey{CommitmentID: p.CommitmentID, MemoryID: p.MemoryID}]
			if ok && !goldHit[i] && predictionGroundedInRenderedMemory(l, idx, p) {
				matchedGold = i
			}
		}
		if m.loose && matchedGold < 0 {
			sc.LooseMatches++
		}
		if !m.grounded && !m.loose && matchedGold < 0 {
			sc.Unmatched++
		}
		if matchedGold >= 0 {
			exactPreds++
			g := gold[matchedGold]
			goldHit[matchedGold] = true
			if idx.resolveAttendee(p) == g.Attendee {
				goldAttributed[matchedGold] = true
			} else {
				sc.CriticalIdentity++
			}
			if p.Direction != "" && p.Direction != Unknown && p.Direction != g.Direction {
				sc.CriticalDirection++
			}
			acc.observeGold(p, g, matchedGold)
			commitments.observeSurface(p, g)
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
			commitments.observeNegative(p, l, idx)
		}
	}
	for _, p := range inventory {
		commitments.observeInventory(p)
	}

	sc.Extraction = ratio(exactPreds, len(visible), countTrue(goldHit), len(gold))
	sc.CitationCoverage = coverage(preds, idx)
	sc.CitationCorrect = citationCorrectness(l, preds, gold, idx)
	if l.Version >= SchemaV3 && len(inventory) > 0 {
		sc.Counterparty = inventoryCounterparty(inventoryGold, inventory, idx)
	} else {
		sc.Counterparty = perClassPR(gold, goldHit, goldAttributed, visible, idx)
	}
	sc.DedupCrossArtifact = dedupCrossArtifact(l, idx, visible)
	sc.Owner = acc.owner()
	sc.Direction = acc.direction()
	sc.DueTime = acc.due()
	sc.Lifecycle = commitments.lifecycle()
	sc.ClosureLinkage = commitments.closure()
	sc.CommitmentIdentity = commitments.identity()
	sc.Dedup = commitments.dedup()
	sc.CitationRoles = commitments.citationRoles()
	return sc
}

// resolveAttendee maps the product's attendee string back onto a ledger identity.
// The stable atom is authoritative (it is the governance key); the display string is
// only a fallback, because the product renders a person with no display name as a
// bare address.
func (idx ledgerIndex) resolveAttendee(p Prediction) string {
	if strings.TrimSpace(p.AttendeeAtom) != "" {
		_, value, ok := strings.Cut(p.AttendeeAtom, ":")
		if !ok {
			return ""
		}
		if id, found := idx.atoms[strings.ToLower(strings.TrimSpace(value))]; found {
			return id
		}
		return ""
	}
	if label := strings.ToLower(strings.TrimSpace(p.CounterpartyLabel)); label != "" {
		return idx.labels[label]
	}
	key := strings.ToLower(strings.TrimSpace(p.Attendee))
	if id, found := idx.atoms[key]; found {
		return id
	}
	return idx.displays[key]
}

func scoreDaily(l Ledger, idx ledgerIndex, gold, inventoryGold []goldItem, preds []Prediction) Scorecard {
	visible := surfacePredictions(preds)
	inventory := inventoryPredictions(preds)
	sc := Scorecard{ScorerVersion: ScorerVersion, Surface: SurfaceDaily}
	goldMemories := map[string]int{}
	identityGold := make(map[commitmentMemoryKey]int, len(gold))
	goldByMemory := make(map[string][]int, len(gold))
	for i, g := range gold {
		goldMemories[g.MemoryID] = i
		if l.Version >= SchemaV3 {
			identityGold[commitmentMemoryKey{CommitmentID: g.CommitmentID, MemoryID: g.MemoryID}] = i
			goldByMemory[g.MemoryID] = append(goldByMemory[g.MemoryID], i)
		}
	}
	goldHit := make([]bool, len(gold))
	exactPreds := 0
	acc := newTypedAccumulator(gold, idx)
	commitments := newCommitmentAccumulator(l, idx, inventoryGold)
	for _, p := range visible {
		commitments.observeVisibleClaim(p)
		if p.Direction != "" && p.Direction != Unknown {
			sc.DirectionScorable = true
		}
		if _, ok := idx.byMemory[p.MemoryID]; !ok {
			sc.Unmatched++
			continue
		}
		i := -1
		if l.Version >= SchemaV3 {
			if p.CommitmentID != "" {
				candidate, ok := identityGold[commitmentMemoryKey{CommitmentID: p.CommitmentID, MemoryID: p.MemoryID}]
				if ok && !goldHit[candidate] {
					i = candidate
				}
			} else if candidates := goldByMemory[p.MemoryID]; len(candidates) == 1 && !goldHit[candidates[0]] {
				// The current daily adapter has no commitment ID. Preserve its
				// unambiguous artifact-grain behavior, but refuse to choose among
				// multiple commitments in one memory.
				i = candidates[0]
			}
		} else if candidate, ok := goldMemories[p.MemoryID]; ok {
			i = candidate
		}
		if i < 0 {
			continue
		}
		exactPreds++
		goldHit[i] = true
		acc.observeGold(p, gold[i], i)
		commitments.observeSurface(p, gold[i])
	}
	for _, p := range inventory {
		commitments.observeInventory(p)
	}
	sc.Extraction = ratio(exactPreds, len(visible), countTrue(goldHit), len(gold))
	sc.CitationCoverage = coverage(preds, idx)
	sc.CitationCorrect = dailyCitationCorrectness(l, preds, idx)
	sc.DedupCrossArtifact = dedupCrossArtifact(l, idx, visible)
	sc.Owner = acc.owner()
	sc.Direction = acc.direction()
	sc.DueTime = acc.due()
	sc.Lifecycle = commitments.lifecycle()
	sc.ClosureLinkage = commitments.closure()
	sc.CommitmentIdentity = commitments.identity()
	sc.Dedup = commitments.dedup()
	sc.CitationRoles = commitments.citationRoles()
	if l.Version >= SchemaV3 && len(inventory) > 0 {
		sc.Counterparty = inventoryCounterparty(inventoryGold, inventory, idx)
	}
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
	covered, total := 0, 0
	for _, p := range preds {
		if isInventoryPrediction(p) {
			// ID-less inventory rows exist only for the globally-required,
			// ref-independent counterparty metric. They make no citation claim.
			if p.CommitmentID == "" {
				continue
			}
			if len(p.Citations) == 0 {
				total++
				continue
			}
			for _, citation := range p.Citations {
				total++
				if _, ok := idx.byMemory[citation.MemoryID]; ok {
					covered++
				}
			}
			continue
		}
		total++
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
	return sameSidedPR(covered, total)
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
func citationCorrectness(l Ledger, preds []Prediction, gold []goldItem, idx ledgerIndex) PR {
	correct, cited := 0, 0
	for _, p := range preds {
		if isInventoryPrediction(p) {
			required := requiredCitationSet(l, idx, p.CommitmentID)
			for _, citation := range p.Citations {
				cited++
				// Citation correctness answers whether the cited memory carries
				// evidence for the commitment. Role correctness is deliberately a
				// separate row.
				for key := range required {
					if key.MemoryID == citation.MemoryID && key.CommitmentID == citation.CommitmentID {
						correct++
						break
					}
				}
			}
			continue
		}
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
func dailyCitationCorrectness(l Ledger, preds []Prediction, idx ledgerIndex) PR {
	correct, cited := 0, 0
	for _, p := range preds {
		if isInventoryPrediction(p) {
			required := requiredCitationSet(l, idx, p.CommitmentID)
			for _, citation := range p.Citations {
				cited++
				for key := range required {
					if key.MemoryID == citation.MemoryID && key.CommitmentID == citation.CommitmentID {
						correct++
						break
					}
				}
			}
			continue
		}
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
		Precision:     macroMean(correctByClass, predByClass),
		Recall:        macroMean(hitByClass, goldByClass),
		Defined:       true,
		RecallByClass: classRecall(hitByClass, goldByClass),
	}
}

func inventoryCounterparty(gold []goldItem, preds []Prediction, idx ledgerIndex) PR {
	goldByClass, hitByClass := map[string]int{}, map[string]int{}
	predByClass, correctByClass := map[string]int{}, map[string]int{}
	byID := map[string]goldItem{}
	byMemory := map[string][]goldItem{}
	for _, g := range gold {
		goldByClass[g.Attendee]++
		byID[g.CommitmentID] = g
		byMemory[g.MemoryID] = append(byMemory[g.MemoryID], g)
	}
	// An ID-less row may never claim gold already addressed by an ID-bearing row,
	// even when that row carries the wrong counterparty. Identity-bearing evidence
	// keeps its existing authoritative join.
	reserved := map[string]bool{}
	for _, p := range preds {
		if p.CommitmentID != "" {
			if g, ok := byID[p.CommitmentID]; ok {
				reserved[g.ID] = true
			}
		}
	}
	for _, p := range preds {
		if p.CommitmentID == "" {
			continue
		}
		g, ok := byID[p.CommitmentID]
		if !ok {
			continue
		}
		class := idx.resolveAttendee(p)
		predByClass[class]++
		if class == g.Attendee {
			correctByClass[class]++
			hitByClass[g.Attendee]++
		}
	}
	hitGold := map[string]bool{}
	for _, g := range gold {
		if hitByClass[g.Attendee] > 0 {
			// The per-class count alone cannot identify a gold row. Seed exact hits
			// from the authoritative ID pass instead.
			for _, p := range preds {
				if p.CommitmentID == g.CommitmentID && idx.resolveAttendee(p) == g.Attendee {
					hitGold[g.ID] = true
					break
				}
			}
		}
	}
	for _, p := range preds {
		if p.CommitmentID != "" {
			continue
		}
		class := idx.resolveAttendee(p)
		predByClass[class]++
		var candidates []goldItem
		for _, g := range byMemory[p.MemoryID] {
			if reserved[g.ID] || hitGold[g.ID] || g.Attendee != class {
				continue
			}
			candidates = append(candidates, g)
		}
		if len(candidates) != 1 {
			continue
		}
		g := candidates[0]
		correctByClass[class]++
		hitByClass[g.Attendee]++
		hitGold[g.ID] = true
	}
	if len(predByClass) == 0 || len(goldByClass) == 0 {
		return PR{}
	}
	return PR{
		Precision:     macroMean(correctByClass, predByClass),
		Recall:        macroMean(hitByClass, goldByClass),
		Defined:       true,
		RecallByClass: classRecall(hitByClass, goldByClass),
	}
}

func classRecall(numerators, denominators map[string]int) map[string]float64 {
	if len(denominators) == 0 {
		return nil
	}
	out := make(map[string]float64, len(denominators))
	for class, denominator := range denominators {
		out[class] = float64(numerators[class]) / float64(denominator)
	}
	return out
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

type citationKey struct {
	MemoryID     string
	CommitmentID string
	Role         CitationRole
}

func requiredCitationSet(l Ledger, idx ledgerIndex, commitmentID string) map[citationKey]bool {
	out := map[citationKey]bool{}
	stableIDs := commitmentStableIDs(l)
	for _, c := range l.Commitments {
		if stableIDs[c.ID] != commitmentID {
			continue
		}
		memoryID, _, _, err := idx.resolveSpanText(c.OpenedBy)
		if err == nil {
			out[citationKey{MemoryID: memoryID, CommitmentID: stableIDs[c.ID], Role: CitationRoleOpener}] = true
		}
		for _, transition := range c.Transitions {
			closureMemory, _, _, err := idx.resolveSpanText(transition.Evidence)
			if err == nil {
				out[citationKey{MemoryID: closureMemory, CommitmentID: stableIDs[c.ID], Role: CitationRoleClosure}] = true
			}
		}
		for _, copy := range l.Commitments {
			if copy.DuplicateOf != c.ID {
				continue
			}
			copyMemory, _, _, err := idx.resolveSpanText(copy.OpenedBy)
			if err == nil {
				out[citationKey{MemoryID: copyMemory, CommitmentID: stableIDs[copy.ID], Role: CitationRoleSupporting}] = true
			}
		}
		break
	}
	return out
}

// commitmentAccumulator scores the complete, materialized commitment inventory.
// Inventory rows measure product knowledge; visible rows measure display
// discipline. A closed visible row can therefore hurt lifecycle precision and a
// leak counter, but only a genuine origin=inventory row can complete inventory
// recall.
type commitmentAccumulator struct {
	l    Ledger
	idx  ledgerIndex
	gold []goldItem
	byID map[string]goldItem

	identityExpressed, identityCorrect int
	identityHit                        map[string]bool

	lifecycleExpressed, lifecycleCorrect int
	lifecycleHit                         map[string]bool

	closureExpressed, closureCorrect int
	closureHit                       map[string]bool

	dedupExpressed, dedupCorrect int
	dedupHit                     map[string]bool
	surfaceByCommitment          map[string]int

	citationExpressed, citationCorrect int
	citationHit                        map[citationKey]bool
	requiredCitations                  map[citationKey]bool
}

func newCommitmentAccumulator(l Ledger, idx ledgerIndex, gold []goldItem) *commitmentAccumulator {
	a := &commitmentAccumulator{
		l:                   l,
		idx:                 idx,
		gold:                gold,
		byID:                map[string]goldItem{},
		identityHit:         map[string]bool{},
		lifecycleHit:        map[string]bool{},
		closureHit:          map[string]bool{},
		dedupHit:            map[string]bool{},
		surfaceByCommitment: map[string]int{},
		citationHit:         map[citationKey]bool{},
		requiredCitations:   map[citationKey]bool{},
	}
	for _, g := range gold {
		a.byID[g.CommitmentID] = g
		for key := range requiredCitationSet(l, idx, g.CommitmentID) {
			a.requiredCitations[key] = true
		}
	}
	return a
}

func (a *commitmentAccumulator) observeSurface(p Prediction, g goldItem) {
	a.observe(p, g, true)
}

func (a *commitmentAccumulator) observeVisibleClaim(p Prediction) {
	if p.CommitmentID != "" {
		a.surfaceByCommitment[p.CommitmentID]++
		declared, known := a.byID[p.CommitmentID]
		if (known && declared.DuplicateOf != "") || a.surfaceByCommitment[p.CommitmentID] > 1 {
			// A copy or a second canonical claim surfaced as another visible
			// obligation. This is wrong even when it shares one memory with the
			// canonical and the quote matcher would otherwise credit both.
			a.dedupExpressed++
		}
	}
}

func (a *commitmentAccumulator) observeInventory(p Prediction) {
	g, ok := a.byID[p.CommitmentID]
	a.observe(p, g, ok)
}

func (a *commitmentAccumulator) observeNegative(p Prediction, l Ledger, idx ledgerIndex) {
	stableIDs := commitmentStableIDs(l)
	for _, c := range l.Commitments {
		memoryID, blockKind, channel, err := idx.resolveSpanText(c.OpenedBy)
		if err != nil || memoryID != p.MemoryID || normalize(c.OpenedBy.Quote) != normalize(UnwrapEmitted(p.Text)) {
			continue
		}
		g := goldItem{
			ID:           c.ID,
			CommitmentID: stableIDs[c.ID],
			MemoryID:     memoryID,
			Quote:        c.OpenedBy.Quote,
			Attendee:     counterpartOf(c, idx.self),
			Owner:        c.Owner,
			Direction:    c.Direction,
			Due:          goldDue(c),
			Lifecycle:    c.State,
			Closure:      ClosureNone,
			Channel:      channel,
			BlockKind:    blockKind,
			State:        c.State,
			DuplicateOf:  stableIDs[c.DuplicateOf],
		}
		if len(c.Transitions) > 0 {
			g.Closure, _, _, _ = idx.resolveSpanText(c.Transitions[len(c.Transitions)-1].Evidence)
		}
		a.observe(p, g, false)
		return
	}
}

func (a *commitmentAccumulator) observe(p Prediction, g goldItem, allowRecall bool) {
	if p.CommitmentID != "" {
		a.identityExpressed++
		if p.CommitmentID == g.CommitmentID && g.ID != "" {
			a.identityCorrect++
			if allowRecall {
				a.identityHit[g.ID] = true
			}
		}
	}
	if p.Lifecycle != "" && p.Lifecycle != Unknown {
		a.lifecycleExpressed++
		if p.Lifecycle == g.Lifecycle && g.ID != "" {
			a.lifecycleCorrect++
			if allowRecall {
				a.lifecycleHit[g.ID] = true
			}
		}
	}
	if p.ClosureRef != "" {
		a.closureExpressed++
		if p.ClosureRef == g.Closure && g.ID != "" {
			a.closureCorrect++
			if allowRecall {
				a.closureHit[g.ID] = true
			}
		}
	}
	if p.DuplicateOf != "" {
		a.dedupExpressed++
		if p.DuplicateOf == g.DuplicateOf && g.DuplicateOf != "" {
			a.dedupCorrect++
			if allowRecall {
				a.dedupHit[g.ID] = true
			}
		}
	}
	required := requiredCitationSet(a.l, a.idx, g.CommitmentID)
	for _, citation := range p.Citations {
		a.citationExpressed++
		key := citationKey(citation)
		if required[key] {
			a.citationCorrect++
			if allowRecall {
				a.citationHit[key] = true
			}
		}
	}
}

func (a *commitmentAccumulator) identity() PR {
	return typedPR(len(a.identityHit), len(a.gold), a.identityCorrect, a.identityExpressed)
}

func (a *commitmentAccumulator) lifecycle() PR {
	out := typedPR(len(a.lifecycleHit), len(a.gold), a.lifecycleCorrect, a.lifecycleExpressed)
	goldByClass, hitByClass := map[string]int{}, map[string]int{}
	for _, g := range a.gold {
		goldByClass[g.Lifecycle]++
		if a.lifecycleHit[g.ID] {
			hitByClass[g.Lifecycle]++
		}
	}
	out.RecallByClass = classRecall(hitByClass, goldByClass)
	return out
}

func (a *commitmentAccumulator) closure() PR {
	return typedPR(len(a.closureHit), len(a.gold), a.closureCorrect, a.closureExpressed)
}

func (a *commitmentAccumulator) dedup() PR {
	total := 0
	for _, g := range a.gold {
		if g.DuplicateOf != "" {
			total++
		}
	}
	return typedPR(len(a.dedupHit), total, a.dedupCorrect, a.dedupExpressed)
}

func (a *commitmentAccumulator) citationRoles() PR {
	out := typedPR(len(a.citationHit), len(a.requiredCitations), a.citationCorrect, a.citationExpressed)
	goldByClass, hitByClass := map[string]int{}, map[string]int{}
	for key := range a.requiredCitations {
		role := string(key.Role)
		goldByClass[role]++
		if a.citationHit[key] {
			hitByClass[role]++
		}
	}
	out.RecallByClass = classRecall(hitByClass, goldByClass)
	return out
}

// typedAccumulator scores the visible-surface typed rows. Recall runs over the gold set;
// precision runs over the predictions that ACTUALLY EXPRESSED a value, so a surface
// emitting only "unknown" reports N/A — a failure — instead of a vacuous 1.0.
type typedAccumulator struct {
	gold []goldItem
	idx  ledgerIndex

	ownerHit  map[string]int
	ownerGold map[string]int
	ownerOK   map[string]int
	ownerPred map[string]int

	directionHit  map[string]int
	directionGold map[string]int
	directionOK   map[string]int
	directionPred map[string]int

	dueHit, dueCorrect, duePred   int
	dueHitByClass, dueGoldByClass map[string]int
}

func newTypedAccumulator(gold []goldItem, idx ledgerIndex) *typedAccumulator {
	acc := &typedAccumulator{
		gold:           gold,
		idx:            idx,
		ownerHit:       map[string]int{},
		ownerGold:      map[string]int{},
		ownerOK:        map[string]int{},
		ownerPred:      map[string]int{},
		directionHit:   map[string]int{},
		directionGold:  map[string]int{},
		directionOK:    map[string]int{},
		directionPred:  map[string]int{},
		dueHitByClass:  map[string]int{},
		dueGoldByClass: map[string]int{},
	}
	for _, g := range gold {
		acc.ownerGold[g.Owner]++
		acc.directionGold[g.Direction]++
		acc.dueGoldByClass[g.Due]++
	}
	return acc
}

func (a *typedAccumulator) observeGold(p Prediction, g goldItem, index int) {
	if p.Owner != "" && p.Owner != Unknown {
		owner := a.resolveOwner(p.Owner)
		a.ownerPred[owner]++
		if owner == g.Owner {
			a.ownerOK[owner]++
			a.ownerHit[g.Owner]++
		}
	}
	if p.Direction != "" && p.Direction != Unknown {
		a.directionPred[p.Direction]++
		if p.Direction == g.Direction {
			a.directionOK[p.Direction]++
			a.directionHit[g.Direction]++
		}
	}
	if p.Due != "" {
		a.duePred++
		if dueForComparison(p.Due) == dueForComparison(g.Due) {
			a.dueCorrect++
			a.dueHit++
			a.dueHitByClass[g.Due]++
		}
	}
}

func (a *typedAccumulator) resolveOwner(owner string) string {
	owner = strings.ToLower(strings.TrimSpace(owner))
	if owner == "self:self" {
		return a.idx.self
	}
	if _, value, ok := strings.Cut(owner, ":"); ok {
		if identity, found := a.idx.atoms[strings.ToLower(strings.TrimSpace(value))]; found {
			return identity
		}
	}
	return owner
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
			if dueForComparison(p.Due) == dueForComparison(goldDue(c)) {
				a.dueCorrect++
			}
		}
		return
	}
}

func (a *typedAccumulator) owner() PR {
	out := PR{
		Recall:        macroMean(a.ownerHit, a.ownerGold),
		RecallByClass: classRecall(a.ownerHit, a.ownerGold),
	}
	if len(a.ownerPred) > 0 {
		out.Precision = macroMean(a.ownerOK, a.ownerPred)
		out.Defined = true
	}
	return out
}

func (a *typedAccumulator) direction() PR {
	out := PR{
		Recall:        macroMean(a.directionHit, a.directionGold),
		RecallByClass: classRecall(a.directionHit, a.directionGold),
	}
	if len(a.directionPred) > 0 {
		out.Precision = macroMean(a.directionOK, a.directionPred)
		out.Defined = true
	}
	return out
}

func (a *typedAccumulator) due() PR {
	out := typedPR(a.dueHit, len(a.gold), a.dueCorrect, a.duePred)
	out.RecallByClass = classRecall(a.dueHitByClass, a.dueGoldByClass)
	return out
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
	for _, p := range surfacePredictions(preds) {
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
