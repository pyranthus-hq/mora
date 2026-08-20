package governance

import (
	"github.com/pyranthus-hq/mora/internal/graph"
	"github.com/pyranthus-hq/mora/internal/memory"
	"sort"
	"strings"
)

const (
	ParentStableIDKey   = "governance_parent_stable_id"
	ParentProviderKey   = "governance_parent_provider"
	ParentAtomsKey      = "governance_parent_atoms"
	AtomStableID        = "stable_id"
	AtomHandle          = "handle"
	AtomAddress         = "address"
	AtomHost            = "host"
	KindForget          = "forget"
	KindPrune           = "prune"
	KindSourceScope     = "source_scope"
	KindRedact          = "redact"
	KindMergeConfirm    = "merge_confirm"
	KindArchive         = "archive"
	KindTeachCommitment = "teach_commitment"
	KindTeachMemory     = "teach_memory"
	KindEvalConsent     = "eval_consent"
	ActionSuppress      = "suppress"
	ActionRecord        = "record"
	DecisionConfirm     = "confirm"
	DecisionReject      = "reject"
)

func Revoked(e Entry) bool { return e.RevokedAt != "" }
func ActiveSuppress(g Ledger) []Entry {
	var out []Entry
	for _, e := range g.Entries {
		if Revoked(e) || e.Action != ActionSuppress {
			continue
		}
		switch e.Kind {
		case KindForget, KindPrune, KindSourceScope:
			out = append(out, e)
		}
	}
	return out
}
func CounterpartyAtoms(provider string, meta map[string]any) []Atom {
	if meta == nil {
		return nil
	}
	seen := map[string]Atom{}
	add := func(kind, raw string) {
		v := NormalizeIdentity(kind, raw)
		if v != "" {
			seen[kind+"\x00"+v] = Atom{Provider: provider, Kind: kind, Value: v}
		}
	}
	switch provider {
	case "imessage":
		for _, p := range graph.MetaPairs(meta["participants"]) {
			add(AtomHandle, p.Handle)
		}
	default:
		for _, key := range []string{"from", "to", "cc", "attendees"} {
			for _, a := range graph.MetaStrings(meta[key]) {
				add(AtomAddress, a)
			}
		}
		if org, ok := meta["organizer"].(string); ok {
			add(AtomAddress, org)
		}
	}
	out := make([]Atom, 0, len(seen))
	for _, a := range seen {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Value < out[j].Value
	})
	return out
}
func ParentContext(meta map[string]any) (provider, stableID string, atoms []Atom) {
	if meta == nil {
		return "", "", nil
	}
	provider, _ = meta[ParentProviderKey].(string)
	stableID, _ = meta[ParentStableIDKey].(string)
	add := func(kind, value string) {
		value = NormalizeIdentity(kind, value)
		if (kind == AtomHandle || kind == AtomAddress) && value != "" {
			atoms = append(atoms, Atom{Provider: provider, Kind: kind, Value: value})
		}
	}
	switch rows := meta[ParentAtomsKey].(type) {
	case []map[string]string:
		for _, r := range rows {
			add(r["kind"], r["value"])
		}
	case []any:
		for _, raw := range rows {
			switch r := raw.(type) {
			case map[string]any:
				k, _ := r["kind"].(string)
				v, _ := r["value"].(string)
				add(k, v)
			case map[string]string:
				add(r["kind"], r["value"])
			}
		}
	}
	sort.Slice(atoms, func(i, j int) bool {
		if atoms[i].Kind != atoms[j].Kind {
			return atoms[i].Kind < atoms[j].Kind
		}
		return atoms[i].Value < atoms[j].Value
	})
	dedup := atoms[:0]
	for _, a := range atoms {
		if len(dedup) > 0 && dedup[len(dedup)-1].Kind == a.Kind && dedup[len(dedup)-1].Value == a.Value {
			continue
		}
		dedup = append(dedup, a)
	}
	return provider, stableID, dedup
}
func DecideSuppress(g Ledger, provider, stableID string, meta map[string]any) (bool, string) {
	item := ItemAtom(provider, stableID)
	cps := CounterpartyAtoms(provider, meta)
	sole := len(cps) == 1
	pp, pid, pc := ParentContext(meta)
	psole := len(pc) == 1
	for _, e := range ActiveSuppress(g) {
		a := e.Atom
		switch a.Kind {
		case AtomStableID:
			if ProviderMatches(a.Provider, provider) && a.Value == item.Value {
				return true, e.ID
			}
			if pid != "" && ProviderMatches(a.Provider, pp) && a.Value == pid {
				return true, e.ID
			}
		case AtomHandle, AtomAddress:
			if sole && cps[0].Kind == a.Kind && ProviderMatches(a.Provider, provider) && cps[0].Value == a.Value {
				return true, e.ID
			}
			if psole && pc[0].Kind == a.Kind && ProviderMatches(a.Provider, pp) && pc[0].Value == a.Value {
				return true, e.ID
			}
		}
	}
	return false, ""
}
func Suppresses(g Ledger, m memory.MappedMemory) (bool, string) {
	return DecideSuppress(g, m.Provider, m.StableID, m.Meta)
}
func AtomPersonID(a Atom) string {
	switch a.Kind {
	case AtomHandle, AtomAddress:
		if a.Value != "" {
			return graph.PersonID(a.Value)
		}
	}
	return ""
}
func MergePairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "\x00" + b
}
func BriefLineDecisionKey(a, b Atom) string {
	return a.Provider + "\x00" + a.Value + "\x00" + b.Provider + "\x00" + b.Kind + "\x00" + b.Value
}
func BriefLineDecisions(g Ledger) map[string]string {
	out := map[string]string{}
	for _, e := range g.Entries {
		if Revoked(e) || e.Kind != KindRedact || e.Action != ActionRecord || e.Atom2 == nil {
			continue
		}
		if e.Atom.Kind != AtomStableID || strings.TrimSpace(e.Atom.Value) == "" {
			continue
		}
		a := *e.Atom2
		if a.Kind != AtomHandle && a.Kind != AtomAddress {
			continue
		}
		a.Value = NormalizeIdentity(a.Kind, a.Value)
		if a.Value == "" || (e.Decision != DecisionConfirm && e.Decision != DecisionReject) {
			continue
		}
		out[BriefLineDecisionKey(e.Atom, a)] = e.Decision
	}
	return out
}
func ActiveMergeConfirms(g Ledger) []Entry {
	var out []Entry
	for _, e := range g.Entries {
		if !Revoked(e) && e.Kind == KindMergeConfirm && e.Atom2 != nil {
			out = append(out, e)
		}
	}
	return out
}
func MergeDecisions(g Ledger) (confirmed []graph.ConfirmedMerge, decided map[string]bool) {
	decided = map[string]bool{}
	verdict := map[string]string{}
	ids := map[string][2]string{}
	gov := map[string]string{}
	for _, e := range ActiveMergeConfirms(g) {
		a, b := AtomPersonID(e.Atom), AtomPersonID(*e.Atom2)
		if a == "" || b == "" || a == b {
			continue
		}
		k := MergePairKey(a, b)
		verdict[k] = e.Decision
		ids[k] = [2]string{a, b}
		gov[k] = e.ID
		decided[k] = true
	}
	for k, d := range verdict {
		if d == DecisionConfirm {
			p := ids[k]
			confirmed = append(confirmed, graph.ConfirmedMerge{A: p[0], B: p[1], GovID: gov[k]})
		}
	}
	sort.Slice(confirmed, func(i, j int) bool {
		if confirmed[i].A != confirmed[j].A {
			return confirmed[i].A < confirmed[j].A
		}
		return confirmed[i].B < confirmed[j].B
	})
	return confirmed, decided
}
func teachMemoryDecisionValid(d string) bool {
	return d == "correct" || d == "supersede" || d == "retract"
}
func teachCommitmentDecisionValid(d string) bool {
	return d == "not_a_commitment" || d == "wrong_person" || d == "wrong_direction" || d == "already_closed" || d == "duplicate" || d == "useful"
}
func MemoryVisible(g Ledger, id string) bool {
	hidden := map[string]bool{}
	ever := map[string]bool{}
	active := map[string]bool{}
	for _, e := range g.Entries {
		if e.Kind != KindTeachMemory || e.Action != ActionRecord || !teachMemoryDecisionValid(e.Decision) {
			continue
		}
		if e.ReplacementID != "" {
			ever[e.ReplacementID] = true
		}
		if Revoked(e) {
			continue
		}
		if e.TargetID != "" {
			hidden[e.TargetID] = true
		}
		if e.ReplacementID != "" {
			active[e.ReplacementID] = true
		}
	}
	return !hidden[id] && (!ever[id] || active[id])
}
func TeachingEntries(g Ledger) []Entry {
	var out []Entry
	for _, e := range g.Entries {
		if e.Kind == KindTeachCommitment || e.Kind == KindTeachMemory || e.Kind == KindEvalConsent {
			out = append(out, e)
		}
	}
	return out
}
func EvalConsentEnabled(g Ledger) bool {
	enabled := false
	for _, e := range g.Entries {
		if Revoked(e) || e.Kind != KindEvalConsent || e.Action != ActionRecord {
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
func ActiveTeachCommitments(g Ledger) []Entry {
	var out []Entry
	for _, e := range g.Entries {
		if !Revoked(e) && e.Kind == KindTeachCommitment && e.Action == ActionRecord && teachCommitmentDecisionValid(e.Decision) {
			out = append(out, e)
		}
	}
	return out
}
