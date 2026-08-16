// Package graph compiles memories and explicit governance facts into a deterministic materialized entity graph.
package graph

import "github.com/pyranthus-hq/mora/internal/memory"

type EntityRow struct {
	ID, Kind, DisplayName string
	Aliases               []string
	MentionCount          int
	FirstSeen, LastSeen   string
	Salience              int64
}
type EdgeRow struct{ Src, Rel, Dst, EvidenceID, ValidFrom, ObservedAt, InvalidatedAt string }
type ConfirmedMerge struct{ A, B, GovID string }
type MergeLink struct{ A, B, Signal, Detail string }
type MergeCandidate struct {
	PhoneID, EmailID, Name string
	Echoed                 []string
}
type Result struct {
	Entities   []EntityRow
	Edges      []EdgeRow
	Warnings   []string
	Candidates []MergeCandidate
	Merges     []MergeLink
}
type PersonRef struct{ ID, Identity, Name string }
type Pair struct{ Handle, Name string }
type StructuralEntity struct {
	Name, Kind string
	Count      int
	MemoryIDs  []string
}

func Build(mems []memory.Memory, confirmed []ConfirmedMerge) Result {
	in := make([]confirmedMerge, len(confirmed))
	for i, m := range confirmed {
		in[i] = confirmedMerge(m)
	}
	r := buildGraphResult(mems, in)
	out := Result{Warnings: r.warnings}
	for _, e := range r.entities {
		out.Entities = append(out.Entities, EntityRow(e))
	}
	for _, e := range r.edges {
		out.Edges = append(out.Edges, EdgeRow(e))
	}
	for _, m := range r.candidates {
		out.Candidates = append(out.Candidates, MergeCandidate(m))
	}
	for _, m := range r.merges {
		out.Merges = append(out.Merges, MergeLink(m))
	}
	return out
}
func Compile(mems []memory.Memory) ([]EntityRow, []EdgeRow, []string) {
	r := Build(mems, nil)
	return r.Entities, r.Edges, r.Warnings
}
func ValidFrom(m memory.Memory) string  { return validFromOf(m) }
func PersonID(s string) string          { return personID(s) }
func MailboxKey(s string) string        { return mailboxKey(s) }
func MetaStrings(v any) []string        { return metaStrings(v) }
func MetaNames(v any) map[string]string { return metaNames(v) }
func MetaPairs(v any) []Pair {
	in := metaPairs(v)
	out := make([]Pair, len(in))
	for i, p := range in {
		out[i] = Pair{Handle: p.handle, Name: p.name}
	}
	return out
}
func PersonRefs(m memory.Memory) ([]PersonRef, []string, []string, string) {
	p, s, r, rel := personRefs(m)
	out := make([]PersonRef, len(p))
	for i, v := range p {
		out[i] = PersonRef{ID: v.id, Identity: v.identity, Name: v.name}
	}
	return out, s, r, rel
}
func AggregatePersonSalience(m []memory.Memory) map[string]int64 { return aggregatePersonSalience(m) }
func NormalizeGazName(s string) (string, bool)                   { return normalizeGazName(s) }
func ScanGazetteer(g Gazetteer, text string) []string            { return gazetteerScan(g, text) }
func StructuralEntities(m []memory.Memory) []StructuralEntity {
	in := extractEntities(m)
	out := make([]StructuralEntity, len(in))
	for i, e := range in {
		out[i] = StructuralEntity(e)
	}
	return out
}

const RelMentions = graphRelMentions

func TokenizeWords(s string) []string { return tokenizeWords(s) }
func PersonIdentity(id string) string { return personIdentity(id) }

const MaxParticipantFanout = maxParticipantFanout
const SignalConfirmed = sigConfirmed
const SignalMailbox = sigMailbox
const SignalNameEcho = sigNameEcho
