package commitment

import (
	"sort"
	"strings"

	"github.com/pyranthus-hq/mora/internal/evidence"
	"github.com/pyranthus-hq/mora/internal/evidencetext"
	"github.com/pyranthus-hq/mora/internal/graph"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func citationKey(c Citation) string {
	return strings.Join([]string{c.Citation.MemoryID(), c.CommitmentID, c.Role, c.EvidenceRef}, "\x00")
}
func MergeCitations(a, b []Citation) []Citation {
	out := append([]Citation(nil), a...)
	seen := map[string]bool{}
	for _, citation := range out {
		seen[citationKey(citation)] = true
	}
	for _, citation := range b {
		if key := citationKey(citation); !seen[key] {
			seen[key] = true
			out = append(out, citation)
		}
	}
	return out
}
func Unique(in []Record) []Record {
	seen := map[string]int{}
	out := make([]Record, 0, len(in))
	for _, record := range in {
		if record.ID == "" {
			out = append(out, record)
			continue
		}
		if prior, ok := seen[record.ID]; ok {
			out[prior].Citations = MergeCitations(out[prior].Citations, record.Citations)
			continue
		}
		seen[record.ID] = len(out)
		out = append(out, record)
	}
	return out
}
func Deduplicate(records []Record) []Record {
	projected := ProjectDuplicates(records)
	out := make([]Record, 0, len(records))
	for _, p := range projected {
		record := records[p.OriginalIndex]
		record.DuplicateOf = p.Item.DuplicateOf
		for _, support := range p.SupportingOriginalIndexes {
			for _, citation := range records[support].Citations {
				citation.Role = CitationSupporting
				citation.CommitmentID = records[support].ID
				record.Citations = MergeCitations(record.Citations, []Citation{citation})
			}
		}
		out = append(out, record)
	}
	return out
}

func ApplyLifecycle(records []Record, evidence []Evidence) []Record {
	sort.Slice(evidence, func(i, j int) bool {
		if evidence[i].OccurredAt != evidence[j].OccurredAt {
			return evidence[i].OccurredAt < evidence[j].OccurredAt
		}
		if evidence[i].MemoryID != evidence[j].MemoryID {
			return evidence[i].MemoryID < evidence[j].MemoryID
		}
		return evidence[i].Text < evidence[j].Text
	})
	projected := ProjectLifecycle(records, evidence)
	out := append([]Record(nil), records...)
	for i, p := range projected {
		out[i].State = p.Item.State
		out[i].ClosureRef = p.Item.ClosureRef
		out[i].SupersededBy = p.Item.SupersededBy
		out[i].Gap = p.Item.Gap
		if p.ClosureEvidence >= 0 {
			candidate := evidence[p.ClosureEvidence]
			if candidate.Citation.MemoryID() != "" {
				out[i].Citations = MergeCitations(out[i].Citations, []Citation{{Citation: candidate.Citation, CommitmentID: out[i].ID, Role: CitationClosure, EvidenceRef: candidate.CitationEvidenceRef}})
			}
		}
	}
	return out
}

func EqualAtom(a, b Atom) bool {
	return strings.EqualFold(strings.TrimSpace(a.Provider), strings.TrimSpace(b.Provider)) && strings.EqualFold(strings.TrimSpace(a.Kind), strings.TrimSpace(b.Kind)) && strings.EqualFold(strings.TrimSpace(a.Value), strings.TrimSpace(b.Value))
}
func AcceptanceRestatesRequest(existing []Record, candidate Record) (int, bool) {
	lower := strings.ToLower(oneLine(candidate.Summary))
	if !FirstPersonCommitment(lower) {
		return -1, false
	}
	anaphoric := containsAny(lower, []string{"i can take that", "i'll take that", "i will take that", "i can handle that", "i'll handle that", "i will handle that", "i can do that", "i'll do that", "i will do that"})
	for i := len(existing) - 1; i >= 0; i-- {
		opener := existing[i]
		orderedAfter := StrictlyAfter(opener.OpenedBy.OccurredAt, candidate.OpenedBy.OccurredAt) || (opener.OpenedBy.MessageRef == "" && candidate.OpenedBy.MessageRef == "" && opener.OpenedBy.OccurredAt == candidate.OpenedBy.OccurredAt)
		sameDue := opener.Due == candidate.Due || (opener.Due.Kind == DueNone && candidate.Due.Kind != "")
		if opener.OpenedBy.MemoryID != candidate.OpenedBy.MemoryID || (opener.OpenedBy.MessageRef != "" && opener.OpenedBy.MessageRef == candidate.OpenedBy.MessageRef) || !orderedAfter || !acceptanceRequest(strings.ToLower(oneLine(opener.Summary))) || !sameDue || !EqualAtom(opener.Owner, candidate.Owner) || !EqualAtom(opener.Counterparty, candidate.Counterparty) || opener.Direction != candidate.Direction || opener.State != candidate.State || opener.ClosureRef != candidate.ClosureRef {
			continue
		}
		overlap := ObjectOverlap(opener.Summary, candidate.Summary)
		if DedupCandidate(opener, candidate) || (anaphoric && overlap > 0) {
			return i, true
		}
	}
	return -1, false
}
func containsAny(text string, phrases []string) bool {
	for _, phrase := range phrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}
func acceptanceRequest(text string) bool {
	return DirectRequest(text) || containsAny(text, []string{"please bring", "needs your ", "still needs your "})
}

type NamedActor struct {
	Atom Atom
	Name string
}

func ReportedActor(text string, counterparty, self Atom, candidates []NamedActor, selfNames []string) (*Atom, bool) {
	lower := strings.ToLower(oneLine(text))
	matched := []NamedActor{}
	for _, candidate := range candidates {
		fields := strings.Fields(strings.ToLower(strings.TrimSpace(candidate.Name)))
		if len(fields) == 0 {
			continue
		}
		for _, name := range []string{strings.Join(fields, " "), fields[0]} {
			if strings.Contains(lower, name+" said ") || strings.Contains(lower, name+" said,") || strings.Contains(lower, name+" said:") || strings.Contains(lower, name+" will ") || strings.Contains(lower, name+"'ll ") {
				matched = append(matched, candidate)
				break
			}
		}
	}
	if len(matched) == 0 {
		return nil, false
	}
	if len(matched) != 1 {
		return nil, true
	}
	actor := matched[0].Atom
	if EqualAtom(actor, counterparty) || textNamesPerson(lower, selfNames) {
		return &actor, true
	}
	return nil, true
}
func textNamesPerson(lower string, names []string) bool {
	for _, name := range names {
		fields := strings.Fields(strings.ToLower(strings.TrimSpace(name)))
		if len(fields) == 0 {
			continue
		}
		if strings.Contains(lower, strings.Join(fields, " ")) || (len(fields[0]) >= 3 && strings.Contains(lower, fields[0])) {
			return true
		}
	}
	return false
}

func OpenerCitations(m memory.Memory, commitmentID, evidenceRef, occurredAt string) []Citation {
	citationAt := graph.ValidFrom(m)
	if IsIMessage(m) && evidenceRef != "" {
		citationAt = occurredAt
	} else {
		evidenceRef = ""
	}
	citation, err := evidence.ForMemory(m, SourceOf(m), citationAt)
	if err != nil {
		return []Citation{}
	}
	return []Citation{{Citation: citation, CommitmentID: commitmentID, Role: CitationOpener, EvidenceRef: evidenceRef}}
}
func NewRecord(m memory.Memory, summary, messageRef, blockRef, occurredAt string, ancestorRefs []string, slot int, owner, counterparty Atom, direction Direction) Record {
	id := ID(messageRef, blockRef, slot)
	summary = evidencetext.OneLine(summary)
	return Record{ID: id, Owner: owner, Counterparty: counterparty, CounterpartyKeys: CounterpartyKeys(m, counterparty), Direction: direction, Summary: summary, OpenedBy: Span{MemoryID: m.ID, MessageRef: messageRef, BlockRef: blockRef, AncestorRefs: append([]string(nil), ancestorRefs...), Quote: summary, OccurredAt: occurredAt}, Due: ClassifyDue(summary, occurredAt), State: Open, ClosureRef: ClosureNone, Citations: OpenerCitations(m, id, messageRef, occurredAt), DuplicateOf: ""}
}
