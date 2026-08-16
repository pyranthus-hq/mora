package mora

import (
	graphpkg "github.com/pyranthus-hq/mora/internal/graph"
)

const (
	graphRelMentions     = graphpkg.RelMentions
	maxParticipantFanout = graphpkg.MaxParticipantFanout
	sigConfirmed         = graphpkg.SignalConfirmed
	sigMailbox           = graphpkg.SignalMailbox
	sigNameEcho          = graphpkg.SignalNameEcho
)

type graphEntity struct {
	ID, Kind, DisplayName string
	Aliases               []string
	MentionCount          int
	FirstSeen, LastSeen   string
	Salience              int64
}
type graphEdge struct{ Src, Rel, Dst, EvidenceID, ValidFrom, ObservedAt, InvalidatedAt string }
type confirmedMerge struct{ A, B, GovID string }
type mergeLink struct{ A, B, Signal, Detail string }
type mergeCandidate struct {
	PhoneID, EmailID, Name string
	Echoed                 []string
}
type graphResult struct {
	entities   []graphEntity
	edges      []graphEdge
	warnings   []string
	candidates []mergeCandidate
	merges     []mergeLink
}
type personRef struct{ id, identity, name string }

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func validFromOf(m Memory) string       { return graphpkg.ValidFrom(m) }
func personID(s string) string          { return graphpkg.PersonID(s) }
func mailboxKey(s string) string        { return graphpkg.MailboxKey(s) }
func metaStrings(v any) []string        { return graphpkg.MetaStrings(v) }
func metaNames(v any) map[string]string { return graphpkg.MetaNames(v) }
func personRefs(m Memory) ([]personRef, []string, []string, string) {
	p, s, r, rel := graphpkg.PersonRefs(m)
	out := make([]personRef, len(p))
	for i, v := range p {
		out[i] = personRef{id: v.ID, identity: v.Identity, name: v.Name}
	}
	return out, s, r, rel
}
func buildGraph(m []Memory) ([]graphEntity, []graphEdge, []string) {
	r := buildGraphResult(m, nil)
	return r.entities, r.edges, r.warnings
}
func buildGraphResult(m []Memory, c []confirmedMerge) graphResult {
	in := make([]graphpkg.ConfirmedMerge, len(c))
	for i, v := range c {
		in[i] = graphpkg.ConfirmedMerge{A: v.A, B: v.B, GovID: v.GovID}
	}
	r := graphpkg.Build(m, in)
	out := graphResult{warnings: r.Warnings}
	for _, e := range r.Entities {
		out.entities = append(out.entities, graphEntity{ID: e.ID, Kind: e.Kind, DisplayName: e.DisplayName, Aliases: e.Aliases, MentionCount: e.MentionCount, FirstSeen: e.FirstSeen, LastSeen: e.LastSeen, Salience: e.Salience})
	}
	for _, e := range r.Edges {
		out.edges = append(out.edges, graphEdge{Src: e.Src, Rel: e.Rel, Dst: e.Dst, EvidenceID: e.EvidenceID, ValidFrom: e.ValidFrom, ObservedAt: e.ObservedAt, InvalidatedAt: e.InvalidatedAt})
	}
	for _, v := range r.Candidates {
		out.candidates = append(out.candidates, mergeCandidate{PhoneID: v.PhoneID, EmailID: v.EmailID, Name: v.Name, Echoed: v.Echoed})
	}
	for _, v := range r.Merges {
		out.merges = append(out.merges, mergeLink{A: v.A, B: v.B, Signal: v.Signal, Detail: v.Detail})
	}
	return out
}
func personIdentity(id string) string { return graphpkg.PersonIdentity(id) }
