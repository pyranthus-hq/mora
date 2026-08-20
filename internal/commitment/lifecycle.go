package commitment

import (
	"fmt"
	"github.com/pyranthus-hq/mora/internal/evidence"
	"github.com/pyranthus-hq/mora/internal/identity"
	"sort"
	"strings"
	"time"
	"unicode"
)

type Atom = identity.Atom
type Span struct {
	MemoryID     string   `json:"memory_id"`
	MessageRef   string   `json:"message_ref,omitempty"`
	BlockRef     string   `json:"block_ref,omitempty"`
	AncestorRefs []string `json:"ancestor_refs,omitempty"`
	Quote        string   `json:"quote"`
	OccurredAt   string   `json:"occurred_at,omitempty"`
}
type Item struct {
	ID                string     `json:"id,omitempty"`
	Owner             Atom       `json:"owner"`
	Counterparty      Atom       `json:"counterparty"`
	CounterpartyLabel string     `json:"counterparty_label,omitempty"`
	CounterpartyKeys  []string   `json:"counterparty_keys,omitempty"`
	Direction         Direction  `json:"direction"`
	Summary           string     `json:"summary"`
	OpenedBy          Span       `json:"opened_by"`
	Due               Due        `json:"due"`
	State             string     `json:"state"`
	ClosureRef        string     `json:"closure_ref"`
	SupersededBy      string     `json:"superseded_by,omitempty"`
	StateUncertain    bool       `json:"state_uncertain,omitempty"`
	Gap               string     `json:"gap,omitempty"`
	Citations         []Citation `json:"citations"`
	DuplicateOf       string     `json:"duplicate_of,omitempty"`
	ReviewedUseful    bool       `json:"reviewed_useful,omitempty"`
}

type Party string

const (
	PartyUnknown      Party = ""
	PartySelf         Party = "self"
	PartyCounterparty Party = "counterparty"
)

type Evidence struct {
	MemoryID, MessageRef, BlockRef, Text, OccurredAt string
	Party                                            Party
	Authored                                         bool
	CounterpartyKeys                                 []string
	Citation                                         evidence.Citation
	Source                                           string
	CitationEvidenceRef                              string
}
type LifecycleResult struct {
	Item            Item
	ClosureEvidence int
}
type DedupResult struct {
	Item                      Item
	OriginalIndex             int
	SupportingOriginalIndexes []int
}
type transitionVoice string

const (
	voiceDelivery      transitionVoice = "delivery"
	voiceAck           transitionVoice = "ack"
	voiceAttendanceAck transitionVoice = "attendance_ack"
	voiceEither        transitionVoice = "either"
)

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
func containsAnyPhrase(s string, phrases []string) bool {
	for _, p := range phrases {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}
func transition(text string) (string, transitionVoice) {
	lower := strings.ToLower(oneLine(text))
	if lower == "" || looksQuestion(lower) || containsAnyPhrase(lower, []string{"haven't ", "have not ", "hasn't ", "has not ", "didn't ", "did not ", "not sent", "not delivered", "not uploaded", "not finished", "never sent", "will send", "will deliver", "will upload", "going to send", "plan to send", "intend to send", "can send", "could send", "should send", "is staged", "was staged", "staged for", "queued for", "ready to send"}) {
		return "", voiceEither
	}
	if containsAnyPhrase(lower, []string{"instead", "replaced by", "new deadline", "deadline moved", "deadline changed", "moved to monday", "moved to tuesday", "moved to wednesday", "moved to thursday", "moved to friday"}) {
		return Superseded, voiceEither
	}
	if containsAnyPhrase(lower, []string{"no longer needed", "cancelled", "canceled"}) {
		return Closed, voiceEither
	}
	if containsAnyPhrase(lower, []string{"thanks for coming", "thank you for coming", "thanks for joining", "thank you for joining"}) {
		// Attendance/joining acknowledgements are generic thread noise: they close a
		// commitment only when the event/object it names actually overlaps, even in
		// the same memory, so an unrelated "thanks for coming" can't sweep the thread.
		return Closed, voiceAttendanceAck
	}
	if containsAnyPhrase(lower, []string{"got it", "got the ", "received ", "i found ", "we found ", "opens correctly", "arrived"}) {
		return Closed, voiceAck
	}
	if containsAnyPhrase(lower, []string{"i sent ", "we sent ", "sent the ", "sent it", "i delivered ", "we delivered ", "was delivered", "i attached ", "we attached ", "attached the ", "i uploaded ", "we uploaded ", "completed ", "finished ", "all set", "as promised",
		// Colloquial self-confirmation: a same-object "already" report is delivery
		// evidence, not a fresh promise, even without one of the verbs above.
		"already sent", "already delivered", "already uploaded", "already attached",
		"already did", "already done", "already finished",
		"already handled", "already took care of", "took care of it already",
		"alr sent", "alr delivered", "alr uploaded", "alr attached",
		"alr did", "alr done", "alr finished", "alr handled"}) || lower == "done" || strings.HasPrefix(lower, "done ") || strings.Contains(lower, " done ") {
		return Closed, voiceDelivery
	}
	return "", voiceEither
}
func looksQuestion(lower string) bool {
	if strings.Contains(lower, "?") {
		return true
	}
	trimmed := strings.TrimSpace(lower)
	for _, p := range []string{"did you ", "did they ", "have you ", "has it ", "has the ", "is it ", "is the ", "was it ", "was the ", "when will ", "will you ", "can you ", "could you "} {
		if strings.HasPrefix(trimmed, p) {
			return true
		}
	}
	return false
}
func strictlyAfter(opened, evidence string) bool {
	o, oe := time.Parse(time.RFC3339, strings.TrimSpace(opened))
	e, ee := time.Parse(time.RFC3339, strings.TrimSpace(evidence))
	return oe == nil && ee == nil && e.After(o)
}
func counterpartyLinked(keys, evidenceKeys []string, text string) bool {
	set := map[string]bool{}
	for _, k := range evidenceKeys {
		set[strings.ToLower(strings.TrimSpace(k))] = true
	}
	lower := strings.ToLower(oneLine(text))
	for _, k := range keys {
		n := strings.ToLower(strings.TrimSpace(k))
		if set[n] {
			return true
		}
		kind, value, ok := strings.Cut(n, ":")
		if ok && (kind == "name" || kind == "given") && len(value) >= 3 && strings.Contains(lower, value) {
			return true
		}
	}
	return false
}

var objectStopwords = map[string]bool{"a": true, "an": true, "and": true, "are": true, "at": true, "before": true, "by": true, "can": true, "could": true, "did": true, "do": true, "for": true, "from": true, "got": true, "have": true, "i": true, "in": true, "is": true, "it": true, "me": true, "my": true, "now": true, "of": true, "on": true, "please": true, "send": true, "sent": true, "share": true, "shared": true, "the": true, "them": true, "this": true, "to": true, "upload": true, "uploaded": true, "we": true, "will": true, "with": true, "you": true, "your": true, "delivered": true, "attached": true, "completed": true, "finished": true, "instead": true, "deadline": true, "moved": true}

func objectTokens(text string) map[string]bool {
	out := map[string]bool{}
	for _, raw := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		token := strings.TrimSpace(raw)
		if len(token) < 3 || objectStopwords[token] {
			continue
		}
		if len(token) > 4 && strings.HasSuffix(token, "s") {
			token = strings.TrimSuffix(token, "s")
		}
		out[token] = true
	}
	return out
}
func objectOverlap(a, b string) int {
	left, right := objectTokens(a), objectTokens(b)
	n := 0
	for t := range left {
		if right[t] {
			n++
		}
	}
	return n
}
func closureScore(c Item, e Evidence, tr string, voice transitionVoice) int {
	if e.Party == PartyUnknown || !strictlyAfter(c.OpenedBy.OccurredAt, e.OccurredAt) {
		return 0
	}
	if c.OpenedBy.MemoryID != e.MemoryID && !counterpartyLinked(c.CounterpartyKeys, e.CounterpartyKeys, e.Text) {
		return 0
	}
	owner := PartyCounterparty
	if c.Direction == OwedBySelf {
		owner = PartySelf
	}
	if voice == voiceDelivery && e.Party != owner {
		return 0
	}
	if (voice == voiceAck || voice == voiceAttendanceAck) && e.Party == owner {
		return 0
	}
	overlap := objectOverlap(c.Summary+" "+c.OpenedBy.Quote, e.Text)
	same := c.OpenedBy.MemoryID == e.MemoryID
	if overlap == 0 {
		// Attendance/joining acknowledgements are broad thread noise (any
		// commitment can share a MemoryID with a "thanks for coming"), so they
		// never get the same-memory pass that delivery/receipt evidence gets.
		if voice == voiceAttendanceAck || !same {
			return 0
		}
	}
	score := overlap * 10
	if same {
		score++
	}
	if tr == Superseded {
		score++
	}
	return score
}
func ProjectLifecycle(items []Item, evidence []Evidence) []LifecycleResult {
	out := make([]LifecycleResult, len(items))
	for i, v := range items {
		out[i] = LifecycleResult{Item: v, ClosureEvidence: -1}
	}
	type indexed struct {
		Evidence
		index int
	}
	ev := make([]indexed, len(evidence))
	for i, v := range evidence {
		ev[i] = indexed{v, i}
	}
	sort.Slice(ev, func(i, j int) bool {
		if ev[i].OccurredAt != ev[j].OccurredAt {
			return ev[i].OccurredAt < ev[j].OccurredAt
		}
		if ev[i].MemoryID != ev[j].MemoryID {
			return ev[i].MemoryID < ev[j].MemoryID
		}
		return ev[i].Text < ev[j].Text
	})
	for _, candidate := range ev {
		tr, voice := transition(candidate.Text)
		if tr == "" || !candidate.Authored {
			continue
		}
		bestScore := 0
		var best []int
		for i := range out {
			if out[i].Item.State != Open {
				continue
			}
			score := closureScore(out[i].Item, candidate.Evidence, tr, voice)
			if score > bestScore {
				bestScore, best = score, []int{i}
			} else if score > 0 && score == bestScore {
				best = append(best, i)
			}
		}
		if bestScore == 0 {
			continue
		}
		if len(best) != 1 {
			gap := fmt.Sprintf("Ambiguous state evidence %s fits %d open commitments; state left open.", candidate.MemoryID, len(best))
			for _, i := range best {
				out[i].Item.Gap = gap
			}
			continue
		}
		i := best[0]
		if tr == Superseded {
			out[i].Item.State = Superseded
			out[i].Item.SupersededBy = candidate.MemoryID
		} else {
			out[i].Item.State = Closed
			out[i].Item.ClosureRef = candidate.MemoryID
			out[i].ClosureEvidence = candidate.index
		}
	}
	return out
}
func atomEqual(a, b Atom) bool {
	return strings.EqualFold(strings.TrimSpace(a.Provider), strings.TrimSpace(b.Provider)) && strings.EqualFold(strings.TrimSpace(a.Kind), strings.TrimSpace(b.Kind)) && strings.EqualFold(strings.TrimSpace(a.Value), strings.TrimSpace(b.Value))
}
func evidenceLess(a, b Item) bool {
	if a.OpenedBy.OccurredAt != b.OpenedBy.OccurredAt {
		at, ae := time.Parse(time.RFC3339, a.OpenedBy.OccurredAt)
		bt, be := time.Parse(time.RFC3339, b.OpenedBy.OccurredAt)
		if ae == nil && be == nil && !at.Equal(bt) {
			return at.Before(bt)
		}
		return a.OpenedBy.OccurredAt < b.OpenedBy.OccurredAt
	}
	if a.OpenedBy.MessageRef != b.OpenedBy.MessageRef {
		return a.OpenedBy.MessageRef < b.OpenedBy.MessageRef
	}
	if a.OpenedBy.BlockRef != b.OpenedBy.BlockRef {
		return a.OpenedBy.BlockRef < b.OpenedBy.BlockRef
	}
	return a.ID < b.ID
}
func dedupCandidate(a, b Item) bool {
	if strings.EqualFold(oneLine(a.Summary), oneLine(b.Summary)) {
		return true
	}
	left, right := objectTokens(a.Summary), objectTokens(b.Summary)
	if len(left) == 0 || len(right) == 0 {
		return false
	}
	smaller := len(left)
	if len(right) < smaller {
		smaller = len(right)
	}
	return objectOverlap(a.Summary, b.Summary)*2 >= smaller
}
func containsFold(values []string, want string) bool {
	if want == "" {
		return false
	}
	for _, v := range values {
		if strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}
func strongProvenance(a, b Item) bool {
	if a.OpenedBy.MessageRef != "" && a.OpenedBy.MessageRef == b.OpenedBy.MessageRef && a.OpenedBy.BlockRef != "" && a.OpenedBy.BlockRef == b.OpenedBy.BlockRef {
		return true
	}
	return containsFold(a.OpenedBy.AncestorRefs, b.OpenedBy.MessageRef) || containsFold(b.OpenedBy.AncestorRefs, a.OpenedBy.MessageRef)
}
func keySetsOverlap(a, b []string) bool {
	seen := map[string]bool{}
	for _, k := range a {
		seen[strings.ToLower(strings.TrimSpace(k))] = true
	}
	for _, k := range b {
		if seen[strings.ToLower(strings.TrimSpace(k))] {
			return true
		}
	}
	return false
}
func sameLifecycle(a, b Item) bool {
	sameCounterparty := atomEqual(a.Counterparty, b.Counterparty) || keySetsOverlap(a.CounterpartyKeys, b.CounterpartyKeys)
	sameOwner := atomEqual(a.Owner, b.Owner) || (a.Direction == OwedByCounterparty && b.Direction == OwedByCounterparty && sameCounterparty)
	return sameOwner && sameCounterparty && a.Direction == b.Direction && a.Due == b.Due && a.State == b.State && a.ClosureRef == b.ClosureRef
}
func ProjectDuplicates(items []Item) []DedupResult {
	out := make([]DedupResult, len(items))
	for i, v := range items {
		out[i] = DedupResult{Item: v, OriginalIndex: i}
	}
	sort.SliceStable(out, func(i, j int) bool { return evidenceLess(out[i].Item, out[j].Item) })
	for i := range out {
		if out[i].Item.ID == "" || out[i].Item.DuplicateOf != "" {
			continue
		}
		for j := i + 1; j < len(out); j++ {
			if out[j].Item.ID == "" || out[j].Item.DuplicateOf != "" || !dedupCandidate(out[i].Item, out[j].Item) || !strongProvenance(out[i].Item, out[j].Item) || !sameLifecycle(out[i].Item, out[j].Item) {
				continue
			}
			out[j].Item.DuplicateOf = out[i].Item.ID
			out[i].SupportingOriginalIndexes = append(out[i].SupportingOriginalIndexes, out[j].OriginalIndex)
		}
	}
	return out
}

// TransitionVoice describes whose authored evidence can prove a state transition.
type TransitionVoice = transitionVoice

const (
	VoiceDelivery      = voiceDelivery
	VoiceAck           = voiceAck
	VoiceAttendanceAck = voiceAttendanceAck
	VoiceEither        = voiceEither
)

func Transition(text string) (string, TransitionVoice)     { return transition(text) }
func StrictlyAfter(opened, evidence string) bool           { return strictlyAfter(opened, evidence) }
func ObjectOverlap(a, b string) int                        { return objectOverlap(a, b) }
func DedupCandidate(a, b Item) bool                        { return dedupCandidate(a, b) }
func EvidenceLess(a, b Item) bool                          { return evidenceLess(a, b) }
func ContainsStringFold(values []string, want string) bool { return containsFold(values, want) }
