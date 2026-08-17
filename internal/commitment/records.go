package commitment

import (
	"sort"
	"strings"
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
