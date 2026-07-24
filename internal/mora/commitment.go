package mora

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Direction is the shared obligation-direction vocabulary used by every
// product lane. A named type prevents task-ledger and evidence-derived loops
// from drifting into independently invented string values.
type Direction string

const (
	commitDirectionUnknown   Direction = "unknown"
	commitOwedBySelf         Direction = "owed_by_self"
	commitOwedByCounterparty Direction = "owed_by_counterparty"

	commitOpen       = "open"
	commitClosed     = "closed"
	commitSuperseded = "superseded"

	commitDueNone         = "none"
	commitDueRelative     = "relative"
	commitDueExplicitDate = "explicit_date"

	commitClosureNone = "none"

	commitCitationOpener     = "opener"
	commitCitationClosure    = "closure"
	commitCitationSupporting = "supporting"
)

// Commitment is the typed, derived projection of immutable opening evidence.
// It is materialized with the whole-vault index generation and is never written
// into a vault memory.
type Commitment struct {
	ID               string               `json:"id,omitempty"`
	Owner            govAtom              `json:"owner"`
	Counterparty     govAtom              `json:"counterparty"`
	CounterpartyKeys []string             `json:"counterparty_keys,omitempty"`
	Direction        Direction            `json:"direction"`
	Summary          string               `json:"summary"`
	OpenedBy         commitSpan           `json:"opened_by"`
	Due              commitDue            `json:"due"`
	State            string               `json:"state"`
	ClosureRef       string               `json:"closure_ref"`
	SupersededBy     string               `json:"superseded_by,omitempty"`
	StateUncertain   bool                 `json:"state_uncertain,omitempty"`
	Gap              string               `json:"gap,omitempty"`
	Citations        []CommitmentCitation `json:"citations"`
	DuplicateOf      string               `json:"duplicate_of,omitempty"`
}

type commitSpan struct {
	MemoryID     string   `json:"memory_id"`
	MessageRef   string   `json:"message_ref,omitempty"`
	BlockRef     string   `json:"block_ref,omitempty"`
	AncestorRefs []string `json:"ancestor_refs,omitempty"`
	Quote        string   `json:"quote"`
	OccurredAt   string   `json:"occurred_at,omitempty"`
}

type commitDue struct {
	Kind string `json:"kind"`
	At   string `json:"at,omitempty"`
}

var (
	commitMonthDateRE = regexp.MustCompile(`(?i)\b(january|february|march|april|may|june|july|august|september|october|november|december)\s+([0-9]{1,2})(?:st|nd|rd|th)?(?:,\s*([0-9]{4}))?\b`)
	commitISODateRE   = regexp.MustCompile(`\b([0-9]{4})-([0-9]{1,2})-([0-9]{1,2})\b`)
	commitRelativeRE  = regexp.MustCompile(`(?i)\b(today|tomorrow|tonight|this\s+(?:morning|afternoon|evening|week|month)|next\s+(?:monday|tuesday|wednesday|thursday|friday|saturday|sunday|week|month)|monday|tuesday|wednesday|thursday|friday|saturday|sunday|before|after|once|until|by\s+the\s+end|in\s+the\s+(?:morning|afternoon|evening)|before\s+(?:breakfast|lunch|dinner)|in\s+[0-9]+\s+(?:minutes?|hours?|days?|weeks?))\b`)
)

func classifyCommitmentDue(text, occurredAt string) commitDue {
	text = oneLine(text)
	if text == "" {
		return commitDue{Kind: commitDueNone}
	}
	anchor, err := time.Parse(time.RFC3339, strings.TrimSpace(occurredAt))
	if err == nil {
		if date, ok := explicitCommitmentDue(text, anchor.UTC()); ok {
			return commitDue{Kind: commitDueExplicitDate, At: date}
		}
	}
	if commitRelativeRE.MatchString(text) {
		return commitDue{Kind: commitDueRelative}
	}
	return commitDue{Kind: commitDueNone}
}

func explicitCommitmentDue(text string, anchor time.Time) (string, bool) {
	year, month, day := 0, time.Month(0), 0
	if match := commitISODateRE.FindStringSubmatch(text); len(match) != 0 {
		year, _ = strconv.Atoi(match[1])
		monthValue, _ := strconv.Atoi(match[2])
		month = time.Month(monthValue)
		day, _ = strconv.Atoi(match[3])
	} else if match := commitMonthDateRE.FindStringSubmatch(text); len(match) != 0 {
		monthTime, err := time.Parse("January", strings.ToUpper(match[1][:1])+strings.ToLower(match[1][1:]))
		if err != nil {
			return "", false
		}
		month = monthTime.Month()
		day, _ = strconv.Atoi(match[2])
		year = anchor.Year()
		if match[3] != "" {
			year, _ = strconv.Atoi(match[3])
		}
	}
	if year == 0 || month == 0 || day == 0 {
		return "", false
	}

	// The opener supplies a calendar date, not an instant. Preserve exactly that
	// expressiveness; clock-level due extraction requires a separately typed,
	// event-linked capability.
	date := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	if date.Year() != year || date.Month() != month || date.Day() != day {
		return "", false
	}
	return date.Format("2006-01-02"), true
}

func commitDueValue(due commitDue) string {
	if due.Kind == commitDueExplicitDate {
		return due.At
	}
	return due.Kind
}

type commitmentMessageEvidence struct {
	MessageRef   string   `json:"message_ref"`
	Sender       string   `json:"sender,omitempty"`
	To           []string `json:"to,omitempty"`
	Cc           []string `json:"cc,omitempty"`
	At           string   `json:"at,omitempty"`
	BlockRefs    []string `json:"block_refs,omitempty"`
	AncestorRefs []string `json:"ancestor_refs,omitempty"`
}

// CommitmentCitation assigns an evidence role without changing BriefCitation's
// long-standing provenance contract. Closure and duplicate evidence therefore
// add typed citations instead of replacing the opening citation.
type CommitmentCitation struct {
	Citation     BriefCitation `json:"citation"`
	CommitmentID string        `json:"commitment_id,omitempty"`
	Role         string        `json:"role"`
}

type commitmentPartyRole string

const (
	commitmentPartyUnknown      commitmentPartyRole = ""
	commitmentPartySelf         commitmentPartyRole = "self"
	commitmentPartyCounterparty commitmentPartyRole = "counterparty"
)

type commitmentEvidence struct {
	MemoryID         string
	MessageRef       string
	BlockRef         string
	Text             string
	OccurredAt       string
	Party            commitmentPartyRole
	Authored         bool
	Citation         BriefCitation
	Source           string
	CounterpartyKeys []string
}

type commitmentSnapshot struct {
	Generation  string
	Commitments []Commitment
}

type commitmentSpeechContext struct {
	Author        govAtom
	Addressee     govAtom
	Self          govAtom
	Counterparty  govAtom
	ReportedActor *govAtom
}

// commitmentID is versioned and length-prefixed exactly like the scorer's
// evidence identity. Person identity is deliberately absent: graph alias merges
// can regroup a commitment without churning its durable anchor.
func commitmentID(messageRef, blockRef string, slot int) string {
	if messageRef == "" || blockRef == "" || slot < 0 {
		return ""
	}
	hash := sha256.New()
	for _, component := range []string{messageRef, blockRef, strconv.Itoa(slot)} {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(component)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(component))
	}
	return "commit:v1:" + hex.EncodeToString(hash.Sum(nil))
}

func atomEqual(a, b govAtom) bool {
	return strings.EqualFold(strings.TrimSpace(a.Provider), strings.TrimSpace(b.Provider)) &&
		strings.EqualFold(strings.TrimSpace(a.Kind), strings.TrimSpace(b.Kind)) &&
		strings.EqualFold(strings.TrimSpace(a.Value), strings.TrimSpace(b.Value))
}

func atomPresent(a govAtom) bool {
	return strings.TrimSpace(a.Kind) != "" && strings.TrimSpace(a.Value) != ""
}

func classifyCommitmentSpeech(text string, speech commitmentSpeechContext) (owner govAtom, direction Direction, ok bool) {
	text = oneLine(text)
	lower := strings.ToLower(text)
	if text == "" || !atomPresent(speech.Self) || !atomPresent(speech.Counterparty) {
		return govAtom{}, "", false
	}

	if speech.ReportedActor != nil && atomPresent(*speech.ReportedActor) {
		owner = *speech.ReportedActor
	} else if directCommitmentRequest(lower) {
		if !atomPresent(speech.Addressee) {
			return govAtom{}, "", false
		}
		owner = speech.Addressee
	} else if firstPersonCommitment(lower) {
		if !atomPresent(speech.Author) || nonActionableAcknowledgement(lower) {
			return govAtom{}, "", false
		}
		owner = speech.Author
	} else {
		return govAtom{}, "", false
	}

	switch {
	case atomEqual(owner, speech.Self):
		return owner, commitOwedBySelf, true
	case atomEqual(owner, speech.Counterparty):
		return owner, commitOwedByCounterparty, true
	default:
		// A third actor is not silently collapsed onto the attendee. The caller may
		// materialize it once it can supply that actor's stable atom.
		return govAtom{}, "", false
	}
}

func directCommitmentRequest(lower string) bool {
	return containsAnyPhrase(lower, directRequestPhrases) ||
		containsAnyPhrase(lower, []string{"please bring", "needs your ", "still needs your "})
}

func firstPersonCommitment(lower string) bool {
	if !containsAnyPhrase(lower, firstPersonCommitmentPhrases) {
		return false
	}
	// "I'd" is either "I would" (a commitment) or "I had" (a report about the
	// past). A past-perfect modifier makes the latter explicit and must not turn a
	// completed action into new future work.
	if containsAnyPhrase(lower, []string{"i'd already ", "i'd previously ", "i'd just "}) {
		return false
	}
	return containsAnyPhrase(lower, []string{
		"send", "share", "review", "confirm", "sign", "bring", "upload", "deliver",
		"call", "follow up", "get back", "organize", "archive", "initial", "choose",
		"return", "introduce", "leave", "export", "provide", "finish", "prepare",
		"add", "post", "text", "count", "hold", "reserve", "log",
	})
}

var userAuthoredPromiseToAnotherRE = regexp.MustCompile(`(?i)\bi told\s+(?:[\p{L}\p{N}_.’'\-]+\s+){1,4}i['’]d\s+`)

func userAuthoredPromiseToAnother(text string) bool {
	lower := strings.ToLower(oneLine(text))
	return userAuthoredPromiseToAnotherRE.MatchString(lower) && firstPersonCommitment(lower)
}

func reportedActorFor(m Memory, text string, counterparty govAtom) *govAtom {
	lower := strings.ToLower(oneLine(text))
	var names []string
	if isIMessageMemory(m) {
		for _, pair := range participantPairs(m.Meta["participants"]) {
			if atomEqual(counterparty, govAtom{Provider: "imessage", Kind: atomHandle, Value: normalizeIdentity(atomHandle, pair["handle"])}) {
				names = append(names, pair["name"])
			}
		}
	}
	if isGmailMemory(m) {
		if raw, ok := m.Meta["names"].(map[string]any); ok {
			if name, ok := raw[counterparty.Value].(string); ok {
				names = append(names, name)
			}
		}
	}
	for _, name := range names {
		fields := strings.Fields(strings.ToLower(strings.TrimSpace(name)))
		if len(fields) == 0 {
			continue
		}
		for _, candidate := range []string{strings.Join(fields, " "), fields[0]} {
			if strings.Contains(lower, candidate+" said ") ||
				strings.Contains(lower, candidate+" will ") ||
				strings.Contains(lower, candidate+"'ll ") {
				actor := counterparty
				return &actor
			}
		}
	}
	return nil
}

func nonActionableAcknowledgement(lower string) bool {
	trimmed := strings.TrimSpace(lower)
	if !strings.HasPrefix(trimmed, "thanks") && !strings.HasPrefix(trimmed, "thank you") &&
		!strings.HasPrefix(trimmed, "yep") && !strings.HasPrefix(trimmed, "okay") {
		return false
	}
	return strings.Contains(trimmed, " meet you ") || strings.Contains(trimmed, " see you ")
}

func canonicalSelfAtom(cfg Config, preferred string) govAtom {
	if p := strings.ToLower(strings.TrimSpace(preferred)); p != "" {
		return govAtom{Kind: atomAddress, Value: normalizeIdentity(atomAddress, p)}
	}
	self := selfEmails(cfg)
	values := make([]string, 0, len(self))
	for value := range self {
		values = append(values, value)
	}
	sort.Strings(values)
	if len(values) > 0 {
		return govAtom{Kind: atomAddress, Value: normalizeIdentity(atomAddress, values[0])}
	}
	// iMessage-only installs have no configured mailbox. This explicit self atom is
	// safer than treating an arbitrary participant as the user.
	return govAtom{Kind: "self", Value: "self"}
}

func commitmentCounterparty(m Memory, cfg Config) (govAtom, bool) {
	self := selfEmails(cfg)
	var candidates []govAtom
	if isGmailMemory(m) {
		seen := map[string]bool{}
		for _, field := range []string{"from", "to", "cc"} {
			for _, raw := range metaStrings(m.Meta[field]) {
				value := strings.ToLower(strings.TrimSpace(raw))
				if value == "" || self[value] || seen[mailboxKey(value)] {
					continue
				}
				seen[mailboxKey(value)] = true
				candidates = append(candidates, govAtom{Kind: atomAddress, Value: normalizeIdentity(atomAddress, value)})
			}
		}
	} else if isIMessageMemory(m) {
		selfTokens := selfNameTokens(self)
		for _, pair := range participantPairs(m.Meta["participants"]) {
			if participantNameIsSelf(pair["name"], selfTokens) {
				continue
			}
			if value := strings.TrimSpace(pair["handle"]); value != "" {
				candidates = append(candidates, govAtom{Provider: "imessage", Kind: atomHandle, Value: normalizeIdentity(atomHandle, value)})
			}
		}
	}
	if len(candidates) != 1 {
		return govAtom{}, false
	}
	return candidates[0], true
}

// participantNameIsSelf handles imported/transcoded iMessage records that list the
// user alongside the other chat participants. The live connector already stores
// only other-party handles. We exclude a listed participant only when every
// meaningful display-name token is independently present in a configured self
// mailbox local-part; a partial/common-name overlap is insufficient.
func participantNameIsSelf(name string, selfTokens map[string]bool) bool {
	fields := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(name)), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	if len(fields) == 0 {
		return false
	}
	for _, field := range fields {
		if len(field) < 2 || !selfTokens[field] {
			return false
		}
	}
	return true
}

func commitmentCounterpartyKeys(m Memory, counterparty govAtom) []string {
	seen := map[string]bool{}
	add := func(kind, value string) {
		value = strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
		if value != "" {
			seen[kind+":"+value] = true
		}
	}
	add(counterparty.Kind, normalizeIdentity(counterparty.Kind, counterparty.Value))
	if isGmailMemory(m) {
		var names map[string]string
		if body, err := json.Marshal(m.Meta["names"]); err == nil && json.Unmarshal(body, &names) == nil {
			for raw, name := range names {
				atom := govAtom{Kind: atomAddress, Value: normalizeIdentity(atomAddress, raw)}
				if !atomEqual(atom, counterparty) {
					continue
				}
				add("name", name)
				if fields := strings.Fields(name); len(fields) > 0 {
					add("given", fields[0])
				}
			}
		}
	}
	if isIMessageMemory(m) {
		for _, pair := range participantPairs(m.Meta["participants"]) {
			atom := govAtom{Provider: "imessage", Kind: atomHandle, Value: normalizeIdentity(atomHandle, pair["handle"])}
			if !atomEqual(atom, counterparty) {
				continue
			}
			add("name", pair["name"])
			if fields := strings.Fields(pair["name"]); len(fields) > 0 {
				add("given", fields[0])
			}
		}
	}
	out := make([]string, 0, len(seen))
	for key := range seen {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func participantPairs(value any) []map[string]string {
	b, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var pairs []map[string]string
	if json.Unmarshal(b, &pairs) != nil {
		return nil
	}
	return pairs
}

func gmailCommitmentMessages(m Memory) []commitmentMessageEvidence {
	b, err := json.Marshal(m.Meta["messages"])
	if err != nil {
		return nil
	}
	var messages []commitmentMessageEvidence
	if json.Unmarshal(b, &messages) != nil {
		return nil
	}
	return messages
}

func firstGmailSender(m Memory) string {
	first := strings.TrimSpace(strings.SplitN(m.Text, "\n", 2)[0])
	if !strings.HasPrefix(strings.ToLower(first), "from:") {
		return ""
	}
	header := strings.TrimSpace(first[len("From:"):])
	for _, candidate := range metaStrings(m.Meta["from"]) {
		if strings.Contains(strings.ToLower(header), strings.ToLower(candidate)) {
			return candidate
		}
	}
	return ""
}

func gmailBodyParts(m Memory) []string {
	body := stripFromLine(m.Text)
	return strings.Split(body, "\n\n---\n\n")
}

func gmailAddressee(sender govAtom, to, cc []string, self, counterparty govAtom) govAtom {
	recipients := append(append([]string(nil), to...), cc...)
	seenOther := map[string]bool{}
	hasSelf := false
	hasCounterparty := false
	for _, raw := range recipients {
		value := strings.ToLower(strings.TrimSpace(raw))
		atom := govAtom{Kind: atomAddress, Value: normalizeIdentity(atomAddress, value)}
		switch {
		case atomEqual(atom, self):
			hasSelf = true
		case atomEqual(atom, counterparty):
			hasCounterparty = true
			seenOther[mailboxKey(value)] = true
		case value != "":
			seenOther[mailboxKey(value)] = true
		}
	}
	switch {
	case atomEqual(sender, counterparty) && hasSelf &&
		(len(seenOther) == 0 || (len(seenOther) == 1 && hasCounterparty)):
		return self
	case atomEqual(sender, self) && hasCounterparty && len(seenOther) == 1:
		return counterparty
	default:
		return govAtom{}
	}
}

func commitmentCitation(m Memory, commitmentID string) []CommitmentCitation {
	citation, err := citationForMemory(m, evidenceSource(m), validFromOf(m))
	if err != nil {
		return []CommitmentCitation{}
	}
	return []CommitmentCitation{{
		Citation: citation, CommitmentID: commitmentID, Role: commitCitationOpener,
	}}
}

func classifyCommitments(m Memory, cfg Config) []Commitment {
	if m.DeletedAt != "" || isMeetingNotification(m) || memoryIsServiceOnly(m) {
		return nil
	}
	selfAtom := canonicalSelfAtom(cfg, "")
	newCommitment := func(summary, messageRef, blockRef, occurredAt string, ancestorRefs []string, slot int, owner, counterparty govAtom, direction Direction) Commitment {
		due := classifyCommitmentDue(summary, occurredAt)
		id := commitmentID(messageRef, blockRef, slot)
		return Commitment{
			ID:               id,
			Owner:            owner,
			Counterparty:     counterparty,
			CounterpartyKeys: commitmentCounterpartyKeys(m, counterparty),
			Direction:        direction,
			Summary:          oneLine(summary),
			OpenedBy: commitSpan{
				MemoryID: m.ID, MessageRef: messageRef, BlockRef: blockRef,
				AncestorRefs: append([]string(nil), ancestorRefs...),
				Quote:        oneLine(summary), OccurredAt: occurredAt,
			},
			Due:         due,
			State:       commitOpen,
			ClosureRef:  commitClosureNone,
			Citations:   commitmentCitation(m, id),
			DuplicateOf: "",
		}
	}

	var out []Commitment
	if m.Provider == "" && (m.Source == "manual" || m.Source == "mcp") {
		// obligations-v2: "the user's own clear promise to another person" is
		// owed_by_self. A local authored note proves the owner and direction but
		// carries no source-native counterparty atom, so preserve that field as an
		// honest gap instead of inventing an address or handle from prose.
		for slot, segment := range commitmentSegments(m.Text) {
			if !userAuthoredPromiseToAnother(segment) {
				continue
			}
			out = append(out, newCommitment(
				segment, "", "", validFromOf(m), nil, slot,
				selfAtom, govAtom{}, commitOwedBySelf,
			))
		}
		return uniqueCommitments(out)
	}

	counterparty, ok := commitmentCounterparty(m, cfg)
	if !ok && isGmailMemory(m) {
		// A non-self author is the counterparty for both their own commitment and
		// their request to the user, even when another attendee was copied. This
		// does not guess an addressee: the owner comes from the speech act below.
		sender := strings.ToLower(strings.TrimSpace(firstGmailSender(m)))
		if sender != "" && !selfEmails(cfg)[sender] {
			counterparty = govAtom{Kind: atomAddress, Value: normalizeIdentity(atomAddress, sender)}
			ok = true
		}
	}
	if !ok {
		return nil
	}
	if isIMessageMemory(m) {
		for _, turn := range conversationTurns(m.Text) {
			author, addressee := counterparty, selfAtom
			if turn.Self {
				author, addressee = selfAtom, counterparty
			}
			owner, direction, found := classifyCommitmentSpeech(turn.Body, commitmentSpeechContext{
				Author: author, Addressee: addressee, Self: selfAtom, Counterparty: counterparty,
				ReportedActor: reportedActorFor(m, turn.Body, counterparty),
			})
			if !found {
				continue
			}
			// The connector currently preserves ordered turns but not provider
			// message ids in Meta. Refuse to fabricate a CommitmentID; direction and
			// ownership remain typed, while identity stays explicitly unavailable.
			out = append(out, newCommitment(turn.Body, "", "", validFromOf(m), nil, 0, owner, counterparty, direction))
		}
		return out
	}

	if isGmailMemory(m) {
		messages := gmailCommitmentMessages(m)
		parts := gmailBodyParts(m)
		selfTokens := selfNameTokens(selfEmails(cfg))
		if len(messages) > 0 && len(messages) == len(parts) {
			for i, message := range messages {
				sender := strings.ToLower(strings.TrimSpace(message.Sender))
				author := govAtom{}
				if sender != "" {
					author = govAtom{Kind: atomAddress, Value: normalizeIdentity(atomAddress, sender)}
				}
				if !atomEqual(author, selfAtom) && !atomEqual(author, counterparty) {
					continue
				}
				addressee := gmailAddressee(author, message.To, message.Cc, selfAtom, counterparty)
				blockRef := ""
				if len(message.BlockRefs) == 1 {
					blockRef = message.BlockRefs[0]
				}
				slot := 0
				for _, segment := range commitmentSegments(parts[i]) {
					if assignedToThirdParty(segment, selfTokens) {
						continue
					}
					owner, direction, found := classifyCommitmentSpeech(segment, commitmentSpeechContext{
						Author: author, Addressee: addressee, Self: selfAtom, Counterparty: counterparty,
						ReportedActor: reportedActorFor(m, segment, counterparty),
					})
					if !found {
						continue
					}
					out = append(out, newCommitment(segment, message.MessageRef, blockRef, message.At, message.AncestorRefs, slot, owner, counterparty, direction))
					slot++
				}
			}
		} else {
			// Legacy memories predate PR1's immutable message evidence. Classify the
			// sender-authored first message for backward compatibility, but keep ID
			// empty rather than minting a content- or position-derived fake anchor.
			sender := strings.ToLower(strings.TrimSpace(firstGmailSender(m)))
			author := govAtom{}
			if sender != "" {
				author = govAtom{Kind: atomAddress, Value: normalizeIdentity(atomAddress, sender)}
			}
			addressee := gmailAddressee(author, metaStrings(m.Meta["to"]), metaStrings(m.Meta["cc"]), selfAtom, counterparty)
			first := ""
			if len(parts) > 0 {
				first = parts[0]
			}
			for _, segment := range commitmentSegments(first) {
				if assignedToThirdParty(segment, selfTokens) {
					continue
				}
				owner, direction, found := classifyCommitmentSpeech(segment, commitmentSpeechContext{
					Author: author, Addressee: addressee, Self: selfAtom, Counterparty: counterparty,
					ReportedActor: reportedActorFor(m, segment, counterparty),
				})
				if found {
					out = append(out, newCommitment(segment, "", "", validFromOf(m), nil, 0, owner, counterparty, direction))
				}
			}
		}

		// A sender-authored subject is its own immutable evidence block.
		if !isForwardedSubject(m.Title) && !assignedToThirdParty(m.Title, selfTokens) {
			sender := strings.ToLower(strings.TrimSpace(firstGmailSender(m)))
			author := govAtom{}
			if sender != "" {
				author = govAtom{Kind: atomAddress, Value: normalizeIdentity(atomAddress, sender)}
			}
			addressee := gmailAddressee(author, metaStrings(m.Meta["to"]), metaStrings(m.Meta["cc"]), selfAtom, counterparty)
			if owner, direction, found := classifyCommitmentSpeech(m.Title, commitmentSpeechContext{
				Author: author, Addressee: addressee, Self: selfAtom, Counterparty: counterparty,
				ReportedActor: reportedActorFor(m, m.Title, counterparty),
			}); found {
				messageRef, blockRef := "", ""
				if len(messages) > 0 {
					messageRef, blockRef = messages[0].MessageRef, "subject"
				}
				out = append(out, newCommitment(m.Title, messageRef, blockRef, validFromOf(m), nil, 0, owner, counterparty, direction))
			}
		}
	}
	return uniqueCommitments(out)
}

func commitmentSegments(text string) []string {
	authored := senderAuthoredBody(text)
	if containsAnyPhrase(strings.ToLower(authored), []string{
		"earlier request from the archive", "quoted request", "quoted below",
		"forwarded below", "earlier message below",
	}) {
		return nil
	}
	var out []string
	for _, raw := range meetingBriefEvidenceSegments(authored) {
		segment := stripNoiseTokens(raw)
		if segment == "" || isLeadInFragment(segment) || containsPersonalTrivia(segment) {
			continue
		}
		out = append(out, oneLine(segment))
	}
	return out
}

type conversationTurn struct {
	Self bool
	Body string
}

func conversationTurns(text string) []conversationTurn {
	var turns []conversationTurn
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "*") {
			continue
		}
		label, body, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(body) == "" {
			continue
		}
		turns = append(turns, conversationTurn{
			Self: strings.EqualFold(strings.TrimSpace(label), "me"),
			Body: strings.TrimSpace(body),
		})
	}
	return turns
}

func uniqueCommitments(in []Commitment) []Commitment {
	seen := map[string]int{}
	out := make([]Commitment, 0, len(in))
	for _, commitment := range in {
		// An immutable evidence id is strong provenance. Text is deliberately not:
		// two separately requested copies of the same sentence are two obligations.
		if commitment.ID == "" {
			out = append(out, commitment)
			continue
		}
		if prior, ok := seen[commitment.ID]; ok {
			out[prior].Citations = mergeCommitmentCitations(out[prior].Citations, commitment.Citations)
			continue
		}
		seen[commitment.ID] = len(out)
		out = append(out, commitment)
	}
	return out
}

func mergeCommitmentCitations(a, b []CommitmentCitation) []CommitmentCitation {
	out := append([]CommitmentCitation(nil), a...)
	seen := map[string]bool{}
	for _, citation := range out {
		seen[commitmentCitationKey(citation)] = true
	}
	for _, citation := range b {
		if key := commitmentCitationKey(citation); !seen[key] {
			seen[key] = true
			out = append(out, citation)
		}
	}
	return out
}

func commitmentCitationKey(citation CommitmentCitation) string {
	return strings.Join([]string{
		citation.Citation.MemoryID(), citation.CommitmentID, citation.Role,
	}, "\x00")
}

func commitmentEvidenceFromMemories(mems []Memory, cfg Config) []commitmentEvidence {
	var out []commitmentEvidence
	self := canonicalSelfAtom(cfg, "")
	for _, m := range mems {
		if m.DeletedAt != "" || isMeetingNotification(m) || memoryIsServiceOnly(m) {
			continue
		}
		citation, err := citationForMemory(m, evidenceSource(m), validFromOf(m))
		if err != nil {
			continue
		}
		counterparty, _ := commitmentCounterparty(m, cfg)
		counterpartyKeys := commitmentCounterpartyKeys(m, counterparty)
		appendEvidence := func(text, messageRef, blockRef, at string, party commitmentPartyRole) {
			for _, segment := range closureEvidenceSegments(text) {
				out = append(out, commitmentEvidence{
					MemoryID: m.ID, MessageRef: messageRef, BlockRef: blockRef,
					Text: segment, OccurredAt: at, Party: party, Authored: true,
					Citation: citation, Source: evidenceSource(m),
					CounterpartyKeys: append([]string(nil), counterpartyKeys...),
				})
			}
		}

		switch {
		case isGmailMemory(m):
			messages := gmailCommitmentMessages(m)
			parts := gmailBodyParts(m)
			if len(messages) > 0 && len(messages) == len(parts) {
				for i, message := range messages {
					author := govAtom{Kind: atomAddress, Value: normalizeIdentity(atomAddress, message.Sender)}
					party := commitmentPartyUnknown
					switch {
					case atomEqual(author, self):
						party = commitmentPartySelf
					case atomEqual(author, counterparty):
						party = commitmentPartyCounterparty
					}
					if party == commitmentPartyUnknown {
						continue
					}
					blockRef := ""
					if len(message.BlockRefs) == 1 {
						blockRef = message.BlockRefs[0]
					}
					appendEvidence(parts[i], message.MessageRef, blockRef, message.At, party)
				}
				continue
			}
			// Legacy Gmail only proves the first sender. Later body parts lack
			// per-message authors and times, so they cannot drive a state change.
			sender := govAtom{Kind: atomAddress, Value: normalizeIdentity(atomAddress, firstGmailSender(m))}
			party := commitmentPartyUnknown
			switch {
			case atomEqual(sender, self):
				party = commitmentPartySelf
			case atomEqual(sender, counterparty):
				party = commitmentPartyCounterparty
			}
			if party != commitmentPartyUnknown && len(parts) > 0 {
				appendEvidence(parts[0], "", "", validFromOf(m), party)
			}
		case isIMessageMemory(m):
			for _, turn := range conversationTurns(m.Text) {
				party := commitmentPartyCounterparty
				if turn.Self {
					party = commitmentPartySelf
				}
				appendEvidence(turn.Body, "", "", validFromOf(m), party)
			}
		case m.Provider == "" && (m.Source == "manual" || m.Source == "mcp"):
			appendEvidence(m.Text, "", "", validFromOf(m), commitmentPartySelf)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OccurredAt != out[j].OccurredAt {
			return out[i].OccurredAt < out[j].OccurredAt
		}
		if out[i].MemoryID != out[j].MemoryID {
			return out[i].MemoryID < out[j].MemoryID
		}
		return out[i].Text < out[j].Text
	})
	return out
}

func closureEvidenceSegments(text string) []string {
	authored := senderAuthoredBody(text)
	if authored == "" {
		return nil
	}
	var out []string
	for _, raw := range meetingBriefEvidenceSegments(authored) {
		segment := oneLine(stripNoiseTokens(raw))
		if segment != "" && !isLeadInFragment(segment) {
			out = append(out, segment)
		}
	}
	return out
}

func applyCommitmentLifecycle(commitments []Commitment, evidence []commitmentEvidence) []Commitment {
	out := append([]Commitment(nil), commitments...)
	sort.Slice(evidence, func(i, j int) bool {
		if evidence[i].OccurredAt != evidence[j].OccurredAt {
			return evidence[i].OccurredAt < evidence[j].OccurredAt
		}
		if evidence[i].MemoryID != evidence[j].MemoryID {
			return evidence[i].MemoryID < evidence[j].MemoryID
		}
		return evidence[i].Text < evidence[j].Text
	})
	for _, candidate := range evidence {
		transition, voice := commitmentTransition(candidate.Text)
		if transition == "" || !candidate.Authored {
			continue
		}
		bestScore := 0
		var best []int
		for i := range out {
			if out[i].State != commitOpen {
				continue
			}
			score := commitmentClosureScore(out[i], candidate, transition, voice)
			switch {
			case score > bestScore:
				bestScore, best = score, []int{i}
			case score > 0 && score == bestScore:
				best = append(best, i)
			}
		}
		if bestScore == 0 {
			continue
		}
		if len(best) != 1 {
			gap := fmt.Sprintf("Ambiguous state evidence %s fits %d open commitments; state left open.", candidate.MemoryID, len(best))
			for _, i := range best {
				out[i].Gap = gap
			}
			continue
		}
		i := best[0]
		switch transition {
		case commitSuperseded:
			out[i].State = commitSuperseded
			out[i].SupersededBy = candidate.MemoryID
		case commitClosed:
			out[i].State = commitClosed
			out[i].ClosureRef = candidate.MemoryID
			if candidate.Citation.MemoryID() != "" {
				out[i].Citations = mergeCommitmentCitations(out[i].Citations, []CommitmentCitation{{
					Citation: candidate.Citation, CommitmentID: out[i].ID, Role: commitCitationClosure,
				}})
			}
		}
	}
	return out
}

type commitmentTransitionVoice string

const (
	commitmentVoiceDelivery commitmentTransitionVoice = "delivery"
	commitmentVoiceAck      commitmentTransitionVoice = "ack"
	commitmentVoiceEither   commitmentTransitionVoice = "either"
)

func commitmentTransition(text string) (string, commitmentTransitionVoice) {
	lower := strings.ToLower(oneLine(text))
	if lower == "" || commitmentLooksQuestion(lower) ||
		containsAnyPhrase(lower, []string{
			"haven't ", "have not ", "hasn't ", "has not ", "didn't ", "did not ",
			"not sent", "not delivered", "not uploaded", "not finished", "never sent",
			"will send", "will deliver", "will upload", "going to send", "plan to send",
			"intend to send", "can send", "could send", "should send",
			"is staged", "was staged", "staged for", "queued for", "ready to send",
		}) {
		return "", commitmentVoiceEither
	}
	if containsAnyPhrase(lower, []string{
		"instead", "replaced by", "new deadline", "deadline moved", "deadline changed",
		"moved to monday", "moved to tuesday", "moved to wednesday",
		"moved to thursday", "moved to friday",
	}) {
		return commitSuperseded, commitmentVoiceEither
	}
	if containsAnyPhrase(lower, []string{"no longer needed", "cancelled", "canceled"}) {
		return commitClosed, commitmentVoiceEither
	}
	if containsAnyPhrase(lower, []string{
		"got it", "got the ", "received ", "i found ", "we found ", "opens correctly", "arrived",
	}) {
		return commitClosed, commitmentVoiceAck
	}
	if containsAnyPhrase(lower, []string{
		"i sent ", "we sent ", "sent the ", "sent it", "i delivered ", "we delivered ",
		"was delivered", "i attached ", "we attached ", "attached the ", "i uploaded ",
		"we uploaded ", "completed ", "finished ", "all set",
	}) || lower == "done" || strings.HasPrefix(lower, "done ") || strings.Contains(lower, " done ") {
		return commitClosed, commitmentVoiceDelivery
	}
	return "", commitmentVoiceEither
}

func commitmentLooksQuestion(lower string) bool {
	if strings.Contains(lower, "?") {
		return true
	}
	trimmed := strings.TrimSpace(lower)
	for _, prefix := range []string{
		"did you ", "did they ", "have you ", "has it ", "has the ", "is it ",
		"is the ", "was it ", "was the ", "when will ", "will you ", "can you ",
		"could you ",
	} {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

func commitmentClosureScore(commitment Commitment, evidence commitmentEvidence, transition string, voice commitmentTransitionVoice) int {
	if evidence.Party == commitmentPartyUnknown || !strictlyAfter(commitment.OpenedBy.OccurredAt, evidence.OccurredAt) {
		return 0
	}
	if commitment.OpenedBy.MemoryID != evidence.MemoryID &&
		!commitmentCounterpartyLinked(commitment.CounterpartyKeys, evidence.CounterpartyKeys, evidence.Text) {
		return 0
	}
	ownerParty := commitmentPartyCounterparty
	if commitment.Direction == commitOwedBySelf {
		ownerParty = commitmentPartySelf
	}
	switch voice {
	case commitmentVoiceDelivery:
		if evidence.Party != ownerParty {
			return 0
		}
	case commitmentVoiceAck:
		if evidence.Party == ownerParty {
			return 0
		}
	}
	overlap := commitmentObjectOverlap(commitment.Summary+" "+commitment.OpenedBy.Quote, evidence.Text)
	sameMemory := commitment.OpenedBy.MemoryID == evidence.MemoryID
	if overlap == 0 && !sameMemory {
		return 0
	}
	score := overlap * 10
	if sameMemory {
		score++
	}
	if transition == commitSuperseded {
		score++
	}
	return score
}

func commitmentCounterpartyLinked(commitmentKeys, evidenceKeys []string, evidenceText string) bool {
	evidenceSet := map[string]bool{}
	for _, key := range evidenceKeys {
		evidenceSet[strings.ToLower(strings.TrimSpace(key))] = true
	}
	lowerText := strings.ToLower(oneLine(evidenceText))
	for _, key := range commitmentKeys {
		normalized := strings.ToLower(strings.TrimSpace(key))
		if evidenceSet[normalized] {
			return true
		}
		kind, value, ok := strings.Cut(normalized, ":")
		if ok && (kind == "name" || kind == "given") && len(value) >= 3 &&
			strings.Contains(lowerText, value) {
			return true
		}
	}
	return false
}

func strictlyAfter(openedAt, evidenceAt string) bool {
	open, openErr := time.Parse(time.RFC3339, strings.TrimSpace(openedAt))
	evidence, evidenceErr := time.Parse(time.RFC3339, strings.TrimSpace(evidenceAt))
	return openErr == nil && evidenceErr == nil && evidence.After(open)
}

var commitmentObjectStopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "at": true, "before": true,
	"by": true, "can": true, "could": true, "did": true, "do": true, "for": true,
	"from": true, "got": true, "have": true, "i": true, "in": true, "is": true,
	"it": true, "me": true, "my": true, "now": true, "of": true, "on": true,
	"please": true, "send": true, "sent": true, "share": true, "shared": true,
	"the": true, "them": true, "this": true, "to": true, "upload": true,
	"uploaded": true, "we": true, "will": true, "with": true, "you": true,
	"your": true, "delivered": true, "attached": true, "completed": true,
	"finished": true, "instead": true, "deadline": true, "moved": true,
}

func commitmentObjectTokens(text string) map[string]bool {
	out := map[string]bool{}
	for _, raw := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		token := strings.TrimSpace(raw)
		if len(token) < 3 || commitmentObjectStopwords[token] {
			continue
		}
		if len(token) > 4 && strings.HasSuffix(token, "s") {
			token = strings.TrimSuffix(token, "s")
		}
		out[token] = true
	}
	return out
}

func commitmentObjectOverlap(a, b string) int {
	left, right := commitmentObjectTokens(a), commitmentObjectTokens(b)
	n := 0
	for token := range left {
		if right[token] {
			n++
		}
	}
	return n
}

func deduplicateCommitments(commitments []Commitment) []Commitment {
	out := append([]Commitment(nil), commitments...)
	sort.SliceStable(out, func(i, j int) bool {
		return commitmentEvidenceLess(out[i], out[j])
	})
	for i := range out {
		if out[i].ID == "" || out[i].DuplicateOf != "" {
			continue
		}
		for j := i + 1; j < len(out); j++ {
			if out[j].ID == "" || out[j].DuplicateOf != "" ||
				!commitmentDedupCandidate(out[i], out[j]) ||
				!commitmentStrongProvenance(out[i], out[j]) ||
				!commitmentSameLifecycleInstance(out[i], out[j]) {
				continue
			}
			out[j].DuplicateOf = out[i].ID
			for _, citation := range out[j].Citations {
				citation.Role = commitCitationSupporting
				citation.CommitmentID = out[j].ID
				out[i].Citations = mergeCommitmentCitations(out[i].Citations, []CommitmentCitation{citation})
			}
		}
	}
	return out
}

func commitmentEvidenceLess(a, b Commitment) bool {
	if a.OpenedBy.OccurredAt != b.OpenedBy.OccurredAt {
		aTime, aErr := time.Parse(time.RFC3339, a.OpenedBy.OccurredAt)
		bTime, bErr := time.Parse(time.RFC3339, b.OpenedBy.OccurredAt)
		if aErr == nil && bErr == nil && !aTime.Equal(bTime) {
			return aTime.Before(bTime)
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

func commitmentDedupCandidate(a, b Commitment) bool {
	if strings.EqualFold(oneLine(a.Summary), oneLine(b.Summary)) {
		return true
	}
	left, right := commitmentObjectTokens(a.Summary), commitmentObjectTokens(b.Summary)
	if len(left) == 0 || len(right) == 0 {
		return false
	}
	overlap := commitmentObjectOverlap(a.Summary, b.Summary)
	smaller := len(left)
	if len(right) < smaller {
		smaller = len(right)
	}
	return overlap*2 >= smaller
}

func commitmentStrongProvenance(a, b Commitment) bool {
	if a.OpenedBy.MessageRef != "" && a.OpenedBy.MessageRef == b.OpenedBy.MessageRef &&
		a.OpenedBy.BlockRef != "" && a.OpenedBy.BlockRef == b.OpenedBy.BlockRef {
		return true
	}
	return containsStringFold(a.OpenedBy.AncestorRefs, b.OpenedBy.MessageRef) ||
		containsStringFold(b.OpenedBy.AncestorRefs, a.OpenedBy.MessageRef)
}

func containsStringFold(values []string, want string) bool {
	if want == "" {
		return false
	}
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

func commitmentSameLifecycleInstance(a, b Commitment) bool {
	sameCounterparty := atomEqual(a.Counterparty, b.Counterparty) ||
		commitmentKeySetsOverlap(a.CounterpartyKeys, b.CounterpartyKeys)
	sameOwner := atomEqual(a.Owner, b.Owner) ||
		(a.Direction == commitOwedByCounterparty && b.Direction == commitOwedByCounterparty && sameCounterparty)
	return sameOwner && sameCounterparty &&
		a.Direction == b.Direction && a.Due == b.Due && a.State == b.State &&
		a.ClosureRef == b.ClosureRef
}

func commitmentKeySetsOverlap(a, b []string) bool {
	seen := map[string]bool{}
	for _, key := range a {
		seen[strings.ToLower(strings.TrimSpace(key))] = true
	}
	for _, key := range b {
		if seen[strings.ToLower(strings.TrimSpace(key))] {
			return true
		}
	}
	return false
}

func materializeCommitments(mems []Memory, cfg Config, now time.Time) []Commitment {
	var commitments []Commitment
	for _, m := range mems {
		commitments = append(commitments, classifyCommitments(m, cfg)...)
	}
	commitments = applyCommitmentLifecycle(commitments, commitmentEvidenceFromMemories(mems, cfg))
	commitments = deduplicateCommitments(commitments)
	if commitmentSnapshotUncertain(cfg, now) {
		for i := range commitments {
			commitments[i].StateUncertain = true
			if commitments[i].Gap == "" {
				commitments[i].Gap = "One or more required sources are stale or unavailable; lifecycle state is uncertain."
			}
		}
	}
	sort.SliceStable(commitments, func(i, j int) bool {
		if commitments[i].OpenedBy.MemoryID != commitments[j].OpenedBy.MemoryID {
			return commitments[i].OpenedBy.MemoryID < commitments[j].OpenedBy.MemoryID
		}
		return commitmentEvidenceLess(commitments[i], commitments[j])
	})
	return commitments
}

func commitmentGenerationOf(manifestLines []string, cfg Config, now time.Time) string {
	lines := append([]string(nil), manifestLines...)
	health, _ := json.Marshal(sourceHealthAll(cfg, now))
	lines = append(lines,
		"commitment_snapshot_at  "+now.UTC().Format(time.RFC3339Nano),
		"commitment_source_health  "+string(health),
	)
	return manifestDigestOf(lines)
}

func commitmentSnapshotUncertain(cfg Config, now time.Time) bool {
	for _, health := range sourceHealthAll(cfg, now) {
		if health.State != healthFresh {
			return true
		}
	}
	return false
}

func meetingCommitmentFor(candidates []Commitment, attendee govAtom, aliases []string, excerpt string) (Commitment, bool) {
	samePerson := func(atom govAtom) bool {
		if atomEqual(atom, attendee) {
			return true
		}
		for _, alias := range aliases {
			if strings.EqualFold(normalizeIdentity(atom.Kind, atom.Value), normalizeIdentity(atom.Kind, alias)) {
				return true
			}
		}
		return false
	}
	var matched []Commitment
	for _, commitment := range candidates {
		if !samePerson(commitment.Counterparty) {
			continue
		}
		if atomEqual(commitment.Owner, commitment.Counterparty) {
			commitment.Owner = attendee
		}
		commitment.Counterparty = attendee
		if excerpt != "" && strings.EqualFold(oneLine(commitment.Summary), oneLine(excerpt)) {
			return commitment, true
		}
		matched = append(matched, commitment)
	}
	if excerpt == "" && len(matched) == 1 {
		return matched[0], true
	}
	// Multiple independently anchored openings in one artifact are not collapsed
	// into a single line by guesswork. A later PR can render them as separate
	// commitments; this one refuses ambiguous attribution.
	return Commitment{}, false
}

func attachCommitment(line *CitedBriefLine, commitment Commitment) {
	line.Direction = commitment.Direction
	line.Owner = commitment.Owner
	line.Counterparty = commitment.Counterparty
	line.CommitmentID = commitment.ID
	line.DueAt = commitDueValue(commitment.Due)
	line.Lifecycle = commitment.State
	line.ClosureRef = commitment.ClosureRef
	line.DuplicateOf = commitment.DuplicateOf
	line.StateUncertain = commitment.StateUncertain
	line.CommitmentCitations = append([]CommitmentCitation(nil), commitment.Citations...)
}

func writeCommitments(ctx context.Context, tx *sql.Tx, generation string, mems []Memory, cfg Config, now time.Time) error {
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO commitments (generation, row_key, commitment_id, memory_id, payload)
		 VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i, commitment := range materializeCommitments(mems, cfg, now) {
		payload, err := json.Marshal(commitment)
		if err != nil {
			return err
		}
		rowKey := commitment.ID
		if rowKey == "" {
			sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s", commitment.OpenedBy.MemoryID, i, commitment.Direction, commitment.Summary)))
			rowKey = "legacy:" + hex.EncodeToString(sum[:])
		}
		if _, err := stmt.ExecContext(ctx, generation, rowKey, nullStr(commitment.ID), commitment.OpenedBy.MemoryID, string(payload)); err != nil {
			return err
		}
	}
	return nil
}

func readCommitmentSnapshot(ctx context.Context, cfg Config) (commitmentSnapshot, error) {
	db, err := ensureIndexDB(ctx, cfg)
	if err != nil {
		return commitmentSnapshot{}, err
	}
	defer db.Close()
	var generation string
	if err := db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key='commitments_generation'`).Scan(&generation); err != nil {
		return commitmentSnapshot{}, fmt.Errorf("read commitment generation: %w", err)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT memory_id, payload FROM commitments WHERE generation=? ORDER BY memory_id, row_key`, generation)
	if err != nil {
		return commitmentSnapshot{}, err
	}
	defer rows.Close()
	out := commitmentSnapshot{Generation: generation, Commitments: []Commitment{}}
	for rows.Next() {
		var memoryID, payload string
		if err := rows.Scan(&memoryID, &payload); err != nil {
			return commitmentSnapshot{}, err
		}
		var commitment Commitment
		if err := json.Unmarshal([]byte(payload), &commitment); err != nil {
			return commitmentSnapshot{}, fmt.Errorf("decode commitment %s: %w", memoryID, err)
		}
		out.Commitments = append(out.Commitments, commitment)
	}
	return out, rows.Err()
}

func readCommitmentInventory(ctx context.Context, cfg Config) (map[string][]Commitment, error) {
	snapshot, err := readCommitmentSnapshot(ctx, cfg)
	if err != nil {
		return nil, err
	}
	out := map[string][]Commitment{}
	for _, commitment := range snapshot.Commitments {
		out[commitment.OpenedBy.MemoryID] = append(out[commitment.OpenedBy.MemoryID], commitment)
	}
	return out, nil
}
