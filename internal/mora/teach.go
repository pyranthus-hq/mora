package mora

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	teachNotCommitment  = "not_a_commitment"
	teachWrongPerson    = "wrong_person"
	teachWrongDirection = "wrong_direction"
	teachAlreadyClosed  = "already_closed"
	teachDuplicate      = "duplicate"
	teachUseful         = "useful"

	teachMemoryCorrect   = "correct"
	teachMemorySupersede = "supersede"
	teachMemoryRetract   = "retract"

	decisionProvisional = "provisional"
	decisionWorking     = "working"
	decisionStanding    = "standing"

	decisionCurrent     = "current"
	decisionNeedsReview = "needs_review"
)

// DecisionValidity makes the conditions under which a decision remains useful
// first-class data rather than prose an agent can accidentally omit.
func normalizeDecisionValidity(m Memory) *DecisionValidity {
	if m.Type != "decision" {
		return nil
	}
	if m.Decision == nil {
		return &DecisionValidity{
			AsOf:       m.CreatedAt,
			Durability: decisionProvisional,
			Complete:   false,
		}
	}
	d := *m.Decision
	d.AsOf = strings.TrimSpace(d.AsOf)
	d.Durability = strings.TrimSpace(d.Durability)
	d.ReviewBy = strings.TrimSpace(d.ReviewBy)
	d.FlipConditions = cleanStrings(d.FlipConditions)
	d.Complete = validRFC3339(d.AsOf) &&
		validDecisionDurability(d.Durability) &&
		len(d.FlipConditions) > 0 &&
		(d.ReviewBy == "" || validRFC3339(d.ReviewBy))
	return &d
}

func decisionStatusAt(d *DecisionValidity, now time.Time) string {
	if d == nil {
		return ""
	}
	if !d.Complete {
		return decisionNeedsReview
	}
	if d.ReviewBy != "" {
		if review, err := time.Parse(time.RFC3339, d.ReviewBy); err != nil || !review.After(now) {
			return decisionNeedsReview
		}
	}
	return decisionCurrent
}

func decorateDecision(m Memory, now time.Time) Memory {
	m.Decision = normalizeDecisionValidity(m)
	m.DecisionStatus = decisionStatusAt(m.Decision, now)
	return m
}

// memoryMayGovernCommitments reports whether a memory can currently open or
// close an obligation. Ordinary evidence is unaffected. A decision must carry a
// complete, unexpired validity contract: needs-review decisions remain visible
// for review, but never act as current law in commitment or meeting surfaces.
func memoryMayGovernCommitments(m Memory, now time.Time) bool {
	if m.Type != "decision" {
		return true
	}
	return decisionStatusAt(normalizeDecisionValidity(m), now) == decisionCurrent
}

func validRFC3339(raw string) bool {
	_, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	return err == nil
}

func validDecisionDurability(raw string) bool {
	switch raw {
	case decisionProvisional, decisionWorking, decisionStanding:
		return true
	default:
		return false
	}
}

func cleanStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, value := range in {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func teachDecisionValid(decision string) bool {
	switch decision {
	case teachNotCommitment, teachWrongPerson, teachWrongDirection,
		teachAlreadyClosed, teachDuplicate, teachUseful:
		return true
	default:
		return false
	}
}

func teachMemoryDecisionValid(decision string) bool {
	switch decision {
	case teachMemoryCorrect, teachMemorySupersede, teachMemoryRetract:
		return true
	default:
		return false
	}
}

// memoryVisible resolves the authored-memory revision graph. Active retractions
// and replacements hide their target. A replacement is current only while its
// authorizing correction/supersession remains active; undo restores the original
// without leaking both revisions into current-state reads.
func (g governance) memoryVisible(id string) bool {
	hidden := map[string]bool{}
	replacementEver := map[string]bool{}
	activeReplacement := map[string]bool{}
	for _, e := range g.Entries {
		if e.Kind != govKindTeachMemory || e.Action != govActionRecord ||
			!teachMemoryDecisionValid(e.Decision) {
			continue
		}
		if e.ReplacementID != "" {
			replacementEver[e.ReplacementID] = true
		}
		if e.revoked() {
			continue
		}
		if e.TargetID != "" {
			hidden[e.TargetID] = true
		}
		if e.ReplacementID != "" {
			activeReplacement[e.ReplacementID] = true
		}
	}
	if hidden[id] {
		return false
	}
	if replacementEver[id] && !activeReplacement[id] {
		return false
	}
	return true
}

func filterCurrentMemories(g governance, in []Memory) []Memory {
	out := make([]Memory, 0, len(in))
	for _, m := range in {
		if g.memoryVisible(m.ID) {
			out = append(out, m)
		}
	}
	return out
}

func currentMemories(cfg Config, in []Memory, now time.Time) ([]Memory, error) {
	g, err := loadGovernance(cfg)
	if err != nil {
		return nil, err
	}
	out := filterCurrentMemories(g, in)
	for i := range out {
		out[i] = decorateDecision(out[i], now)
	}
	return out, nil
}

func (g governance) teachingEntries() []govEntry {
	var out []govEntry
	for _, e := range g.Entries {
		if e.Kind == govKindTeachCommitment || e.Kind == govKindTeachMemory ||
			e.Kind == govKindEvalConsent {
			out = append(out, e)
		}
	}
	return out
}

func (g governance) evalConsentEnabled() bool {
	enabled := false
	for _, e := range g.Entries {
		if e.revoked() || e.Kind != govKindEvalConsent || e.Action != govActionRecord {
			continue
		}
		switch e.Decision {
		case "enable":
			enabled = true
		case "disable":
			enabled = false
		}
	}
	return enabled
}

func (g governance) activeTeachCommitments() []govEntry {
	var out []govEntry
	for _, e := range g.Entries {
		if e.revoked() || e.Kind != govKindTeachCommitment ||
			e.Action != govActionRecord || !teachDecisionValid(e.Decision) {
			continue
		}
		out = append(out, e)
	}
	return out
}

func teachEntryMatchesCommitment(e govEntry, c Commitment) bool {
	if e.TargetID == "" || e.TargetID != c.OpenedBy.MemoryID {
		return false
	}
	return e.CommitmentID == "" || e.CommitmentID == c.ID
}

// applyTeachCommitments runs after extraction/lifecycle/dedup so human review is
// the final deterministic authority over the derived projection.
func applyTeachCommitments(commitments []Commitment, g governance, cfg Config) []Commitment {
	out := append([]Commitment(nil), commitments...)
	dropped := map[int]bool{}
	for _, e := range g.activeTeachCommitments() {
		for i := range out {
			if !teachEntryMatchesCommitment(e, out[i]) {
				continue
			}
			switch e.Decision {
			case teachNotCommitment:
				dropped[i] = true
			case teachWrongPerson:
				if e.CorrectedAtom != nil {
					out[i].Counterparty = *e.CorrectedAtom
					out[i].CounterpartyKeys = []string{atomPersonID(*e.CorrectedAtom)}
					if out[i].Direction == commitOwedByCounterparty {
						out[i].Owner = *e.CorrectedAtom
					}
				}
			case teachWrongDirection:
				if e.CorrectedDirection == commitOwedBySelf ||
					e.CorrectedDirection == commitOwedByCounterparty {
					out[i].Direction = e.CorrectedDirection
					if e.CorrectedDirection == commitOwedByCounterparty {
						out[i].Owner = out[i].Counterparty
					} else {
						out[i].Owner = canonicalSelfAtom(cfg, "")
					}
				}
			case teachAlreadyClosed:
				out[i].State = commitClosed
				out[i].ClosureRef = "governance:" + e.ID
			case teachDuplicate:
				out[i].DuplicateOf = e.DuplicateOf
			case teachUseful:
				out[i].ReviewedUseful = true
			}
		}
	}
	filtered := make([]Commitment, 0, len(out))
	for i, c := range out {
		if !dropped[i] {
			filtered = append(filtered, c)
		}
	}
	return filtered
}

func validateTeachCommitmentEntry(e govEntry) error {
	if !teachDecisionValid(e.Decision) {
		return fmt.Errorf("unknown commitment decision %q", e.Decision)
	}
	switch e.Decision {
	case teachWrongPerson:
		if e.CorrectedAtom == nil || atomPersonID(*e.CorrectedAtom) == "" {
			return errors.New("wrong_person requires a corrected person atom")
		}
	case teachWrongDirection:
		if e.CorrectedDirection != commitOwedBySelf &&
			e.CorrectedDirection != commitOwedByCounterparty {
			return errors.New("wrong_direction requires owed_by_self or owed_by_counterparty")
		}
	case teachDuplicate:
		if strings.TrimSpace(e.DuplicateOf) == "" {
			return errors.New("duplicate requires duplicate_of")
		}
	}
	return nil
}
