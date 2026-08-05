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
	ID           string  `json:"id,omitempty"`
	Owner        govAtom `json:"owner"`
	Counterparty govAtom `json:"counterparty"`
	// CounterpartyLabel is explicit name-grain attribution, not identity. It is
	// used only when authored text names an addressee but the source supplies no
	// provider atom. It never enters the entity graph or merge machinery.
	CounterpartyLabel string               `json:"counterparty_label,omitempty"`
	CounterpartyKeys  []string             `json:"counterparty_keys,omitempty"`
	Direction         Direction            `json:"direction"`
	Summary           string               `json:"summary"`
	OpenedBy          commitSpan           `json:"opened_by"`
	Due               commitDue            `json:"due"`
	State             string               `json:"state"`
	ClosureRef        string               `json:"closure_ref"`
	SupersededBy      string               `json:"superseded_by,omitempty"`
	StateUncertain    bool                 `json:"state_uncertain,omitempty"`
	Gap               string               `json:"gap,omitempty"`
	Citations         []CommitmentCitation `json:"citations"`
	DuplicateOf       string               `json:"duplicate_of,omitempty"`
	ReviewedUseful    bool                 `json:"reviewed_useful,omitempty"`
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
	commitRelativeRE  = regexp.MustCompile(`(?i)\b(today|tomorrow|tonight|this\s+(?:morning|afternoon|evening|week|month)|next\s+(?:monday|tuesday|wednesday|thursday|friday|saturday|sunday|week|month)|monday|tuesday|wednesday|thursday|friday|saturday|sunday|before|after|when|once|until|by\s+the\s+end|in\s+the\s+(?:morning|afternoon|evening)|before\s+(?:breakfast|lunch|dinner)|in\s+[0-9]+\s+(?:minutes?|hours?|days?|weeks?))\b`)
	commitEventDueRE  = regexp.MustCompile(`(?i)\bfor\s+the\s+(?:[\p{L}\p{N}’'\-]+\s+){0,6}(?:meeting|session|review|walk-through)\b`)
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
	if commitRelativeRE.MatchString(text) || commitEventDueRE.MatchString(text) {
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
	EvidenceRef  string        `json:"evidence_ref,omitempty"`
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

var userAuthoredPromiseToAnotherRE = regexp.MustCompile(`(?i)\bi told\s+((?:[\p{L}\p{N}_.’'\-]+\s+){0,3}[\p{L}\p{N}_.’'\-]+)\s+i(?:['’]d|\s+would)\s+`)

func userAuthoredPromiseToAnother(text string) bool {
	lower := strings.ToLower(oneLine(text))
	if !userAuthoredPromiseToAnotherRE.MatchString(lower) ||
		containsAnyPhrase(lower, []string{" i would have ", " i would already ", " i would previously "}) {
		return false
	}
	return firstPersonCommitment(strings.Replace(lower, " i would ", " i will ", 1))
}

func userAuthoredPromiseCounterpartyLabel(text string) string {
	match := userAuthoredPromiseToAnotherRE.FindStringSubmatch(oneLine(text))
	if len(match) != 2 {
		return ""
	}
	return strings.Join(strings.Fields(match[1]), " ")
}

// reportedActorFor resolves attributed third-person speech only from stable source
// identities. A participant other than the current thread counterparty may own an
// obligation only when the report also names the user as its beneficiary; otherwise
// the work is between third parties and must be dropped.
func reportedActorFor(m Memory, text string, counterparty, self govAtom) (*govAtom, bool) {
	lower := strings.ToLower(oneLine(text))
	type namedAtom struct {
		atom govAtom
		name string
	}
	var candidates []namedAtom
	selfNames := []string{}
	if isIMessageMemory(m) {
		for _, pair := range participantPairs(m.Meta["participants"]) {
			atom := govAtom{Provider: "imessage", Kind: atomHandle, Value: normalizeIdentity(atomHandle, pair["handle"])}
			if atomEqual(counterparty, atom) {
				candidates = append(candidates, namedAtom{atom: atom, name: pair["name"]})
			}
		}
	}
	if isGmailMemory(m) {
		var names map[string]string
		if body, err := json.Marshal(m.Meta["names"]); err == nil && json.Unmarshal(body, &names) == nil {
			keys := make([]string, 0, len(names))
			for raw := range names {
				keys = append(keys, raw)
			}
			sort.Strings(keys)
			for _, raw := range keys {
				atom := govAtom{Kind: atomAddress, Value: normalizeIdentity(atomAddress, raw)}
				if atomEqual(atom, self) {
					selfNames = append(selfNames, names[raw])
					continue
				}
				candidates = append(candidates, namedAtom{atom: atom, name: names[raw]})
			}
		}
	}
	var matched []namedAtom
	for _, candidate := range candidates {
		fields := strings.Fields(strings.ToLower(strings.TrimSpace(candidate.name)))
		if len(fields) == 0 {
			continue
		}
		for _, name := range []string{strings.Join(fields, " "), fields[0]} {
			if strings.Contains(lower, name+" said ") ||
				strings.Contains(lower, name+" said,") ||
				strings.Contains(lower, name+" said:") ||
				strings.Contains(lower, name+" will ") ||
				strings.Contains(lower, name+"'ll ") {
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
	actor := matched[0].atom
	if atomEqual(actor, counterparty) || textNamesPerson(lower, selfNames) {
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
		if strings.Contains(lower, strings.Join(fields, " ")) ||
			(len(fields[0]) >= 3 && strings.Contains(lower, fields[0])) {
			return true
		}
	}
	return false
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

type imessageCommitmentMessage struct {
	MessageRef string
	BlockRef   string
	Body       string
	At         string
	Party      commitmentPartyRole
}

// imessageCommitmentMessages returns message-grain lifecycle evidence only when
// the whole metadata set passes the same fail-closed validation as the search
// projection. The second return value says that message_evidence was present.
// Present but malformed metadata must not fall back to transcript guesses.
func imessageCommitmentMessages(m Memory) ([]imessageCommitmentMessage, bool) {
	if _, present := m.Meta["message_evidence"]; !present {
		if _, schemaPresent := m.Meta["message_evidence_schema"]; schemaPresent {
			return nil, true
		}
		return nil, false
	}
	if fmt.Sprint(m.Meta["message_evidence_schema"]) != "1" {
		return nil, true
	}
	if _, hasDiagnostics := m.Meta["message_evidence_diagnostics"]; hasDiagnostics {
		return nil, true
	}
	messageCount, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(m.Meta["message_count"])))
	if err != nil || messageCount < 1 {
		return nil, true
	}
	rows, diagnostic := deriveIMessageSegments(m)
	if diagnostic != nil {
		return nil, true
	}
	if (!m.Truncated && len(rows) != messageCount) || (m.Truncated && len(rows) > messageCount) ||
		!imessageEvidenceCoversRenderedBody(m.Text, rows) {
		return nil, true
	}
	messages := make([]imessageCommitmentMessage, 0, len(rows))
	var lastAt time.Time
	for _, row := range rows {
		at, err := time.Parse(time.RFC3339, row.At)
		if err != nil || (!lastAt.IsZero() && at.Before(lastAt)) {
			return nil, true
		}
		lastAt = at
		trimmed := strings.TrimSpace(row.Text)
		if strings.HasPrefix(trimmed, "*") && strings.HasSuffix(trimmed, "*") {
			// System events have stable message identity but no authored party.
			continue
		}
		direction := imessageDirection(row.BlockRefs)
		body, ok := trustedIMessageAuthoredBody(row, direction)
		if !ok {
			return nil, true
		}
		party := commitmentPartyCounterparty
		if direction == "outgoing" {
			party = commitmentPartySelf
		}
		messages = append(messages, imessageCommitmentMessage{
			MessageRef: row.EvidenceRef,
			// One iMessage evidence_ref names one connector-visible authored
			// block. Keep the block identity content-independent, like Gmail.
			BlockRef: "body",
			Body:     body,
			At:       row.At,
			Party:    party,
		})
	}
	return messages, true
}

func imessageEvidenceCoversRenderedBody(body string, rows []gmailSegmentRow) bool {
	cursor := 0
	for _, row := range rows {
		start, end, ok := imessageEvidenceByteRange(row.BlockRefs)
		if !ok || start < cursor || end > len(body) || !imessageStructuralGap(body[cursor:start]) {
			return false
		}
		cursor = end
	}
	return imessageStructuralGap(body[cursor:])
}

func imessageEvidenceByteRange(refs []string) (int, int, bool) {
	for _, ref := range refs {
		raw, ok := strings.CutPrefix(ref, "bytes:")
		if !ok {
			continue
		}
		startRaw, endRaw, ok := strings.Cut(raw, "-")
		if !ok {
			return 0, 0, false
		}
		start, startErr := strconv.Atoi(startRaw)
		end, endErr := strconv.Atoi(endRaw)
		return start, end, startErr == nil && endErr == nil && start >= 0 && end > start
	}
	return 0, 0, false
}

func imessageStructuralGap(gap string) bool {
	for _, line := range strings.Split(gap, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "> ") {
			continue
		}
		return false
	}
	return true
}

// trustedIMessageAuthoredBody removes exactly one rendered sender prefix. The
// explicit direction and sender metadata must agree with the visible block.
func trustedIMessageAuthoredBody(row gmailSegmentRow, direction string) (string, bool) {
	if direction != "incoming" && direction != "outgoing" {
		return "", false
	}
	firstLine, rest, _ := strings.Cut(strings.TrimSpace(row.Text), "\n")
	label, firstBody, ok := strings.Cut(firstLine, ":")
	if !ok {
		return "", false
	}
	label = strings.TrimSpace(label)
	if direction == "outgoing" {
		if !strings.EqualFold(label, "me") || !strings.EqualFold(strings.TrimSpace(row.Sender), "me") {
			return "", false
		}
	} else if strings.EqualFold(label, "me") || !strings.EqualFold(label, strings.TrimSpace(row.Sender)) {
		return "", false
	}
	body := strings.TrimSpace(firstBody + "\n" + rest)
	if body == "" {
		return "", false
	}
	return body, true
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

// gmailAuthoredBlockRef binds a sender-authored prefix to the first ordered block
// ref. The Gmail renderer preserves block order. When later footer, quoted, or
// forwarded blocks exist, senderAuthoredBody removes them before classification;
// therefore the first ref remains the only evidence-derived ref we can assign.
// A message with no authored prefix stays ID-less.
func gmailAuthoredBlockRef(message commitmentMessageEvidence, body string) string {
	if len(message.BlockRefs) == 0 || strings.TrimSpace(senderAuthoredBody(body)) == "" {
		return ""
	}
	return message.BlockRefs[0]
}

// gmailFulfilledQuotedRequest recognizes the contract's one exceptional quoted
// opening: an authored delivery followed by the earlier request it fulfills. The
// two ordered block refs are required so the opening and closure remain grounded;
// a quote without authored fulfillment returns no obligation.
func gmailFulfilledQuotedRequest(m Memory, message commitmentMessageEvidence, body string) (delivery, request string, quotedAuthor govAtom, blockRef string, ok bool) {
	if len(message.BlockRefs) != 2 {
		return "", "", govAtom{}, "", false
	}
	lines := strings.Split(body, "\n")
	attribution := -1
	for i, line := range lines {
		if quotedReplyLine.MatchString(line) {
			attribution = i
			break
		}
	}
	if attribution < 0 {
		return "", "", govAtom{}, "", false
	}
	delivery = oneLine(senderAuthoredBody(strings.Join(lines[:attribution], "\n")))
	transition, voice := commitmentTransition(delivery)
	if transition != commitClosed || voice != commitmentVoiceDelivery {
		return "", "", govAtom{}, "", false
	}

	attributionLine := strings.ToLower(strings.TrimSpace(lines[attribution]))
	var names map[string]string
	if body, err := json.Marshal(m.Meta["names"]); err != nil || json.Unmarshal(body, &names) != nil {
		return "", "", govAtom{}, "", false
	}
	var authors []govAtom
	keys := make([]string, 0, len(names))
	for raw := range names {
		keys = append(keys, raw)
	}
	sort.Strings(keys)
	for _, raw := range keys {
		name := strings.ToLower(strings.TrimSpace(names[raw]))
		fields := strings.Fields(name)
		if name == "" || (!strings.Contains(attributionLine, name) &&
			(len(fields) == 0 || len(fields[0]) < 3 || !strings.Contains(attributionLine, fields[0]))) {
			continue
		}
		authors = append(authors, govAtom{Kind: atomAddress, Value: normalizeIdentity(atomAddress, raw)})
	}
	if len(authors) != 1 {
		return "", "", govAtom{}, "", false
	}

	var quotedLines []string
	for _, line := range lines[attribution+1:] {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, ">") {
			if trimmed != "" {
				break
			}
			continue
		}
		quotedLines = append(quotedLines, strings.TrimSpace(strings.TrimPrefix(trimmed, ">")))
	}
	segments := commitmentSegments(strings.Join(quotedLines, "\n"))
	if len(segments) != 1 || !directCommitmentRequest(strings.ToLower(oneLine(segments[0]))) {
		return "", "", govAtom{}, "", false
	}
	return delivery, segments[0], authors[0], message.BlockRefs[1], true
}

// acceptanceRestatesRequest implements the contract rule that accepting an
// existing request does not create extra work. It is intentionally narrower than
// general dedup: same artifact, later message, same typed parties/direction/due,
// a direct-request opener, and either strong object overlap or an explicit
// anaphoric acceptance with corroborating object overlap.
func acceptanceRestatesRequest(existing []Commitment, candidate Commitment) (int, bool) {
	lower := strings.ToLower(oneLine(candidate.Summary))
	if !firstPersonCommitment(lower) {
		return -1, false
	}
	anaphoricAcceptance := containsAnyPhrase(lower, []string{
		"i can take that", "i'll take that", "i will take that",
		"i can handle that", "i'll handle that", "i will handle that",
		"i can do that", "i'll do that", "i will do that",
	})
	for i := len(existing) - 1; i >= 0; i-- {
		opener := existing[i]
		orderedAfter := strictlyAfter(opener.OpenedBy.OccurredAt, candidate.OpenedBy.OccurredAt) ||
			(opener.OpenedBy.MessageRef == "" && candidate.OpenedBy.MessageRef == "" &&
				opener.OpenedBy.OccurredAt == candidate.OpenedBy.OccurredAt)
		sameDueInstance := opener.Due == candidate.Due ||
			(opener.Due.Kind == commitDueNone && candidate.Due.Kind != "")
		if opener.OpenedBy.MemoryID != candidate.OpenedBy.MemoryID ||
			(opener.OpenedBy.MessageRef != "" && opener.OpenedBy.MessageRef == candidate.OpenedBy.MessageRef) ||
			!orderedAfter ||
			!directCommitmentRequest(strings.ToLower(oneLine(opener.Summary))) ||
			!sameDueInstance ||
			!atomEqual(opener.Owner, candidate.Owner) ||
			!atomEqual(opener.Counterparty, candidate.Counterparty) ||
			opener.Direction != candidate.Direction ||
			opener.State != candidate.State ||
			opener.ClosureRef != candidate.ClosureRef {
			continue
		}
		overlap := commitmentObjectOverlap(opener.Summary, candidate.Summary)
		if commitmentDedupCandidate(opener, candidate) || (anaphoricAcceptance && overlap > 0) {
			return i, true
		}
	}
	return -1, false
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

func commitmentCitation(m Memory, commitmentID, evidenceRef, occurredAt string) []CommitmentCitation {
	citationAt := validFromOf(m)
	if isIMessageMemory(m) && evidenceRef != "" {
		citationAt = occurredAt
	} else {
		evidenceRef = ""
	}
	citation, err := citationForMemory(m, evidenceSource(m), citationAt)
	if err != nil {
		return []CommitmentCitation{}
	}
	return []CommitmentCitation{{
		Citation: citation, CommitmentID: commitmentID, Role: commitCitationOpener, EvidenceRef: evidenceRef,
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
			Citations:   commitmentCitation(m, id, messageRef, occurredAt),
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
			out[len(out)-1].CounterpartyLabel = userAuthoredPromiseCounterpartyLabel(segment)
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
		if messages, present := imessageCommitmentMessages(m); present {
			for _, message := range messages {
				author, addressee := counterparty, selfAtom
				if message.Party == commitmentPartySelf {
					author, addressee = selfAtom, counterparty
				}
				reportedActor, attributed := reportedActorFor(m, message.Body, counterparty, selfAtom)
				if attributed && reportedActor == nil {
					continue
				}
				speechCounterparty := counterparty
				if reportedActor != nil {
					speechCounterparty = *reportedActor
				}
				owner, direction, found := classifyCommitmentSpeech(message.Body, commitmentSpeechContext{
					Author: author, Addressee: addressee, Self: selfAtom, Counterparty: speechCounterparty,
					ReportedActor: reportedActor,
				})
				if !found {
					continue
				}
				candidate := newCommitment(message.Body, message.MessageRef, message.BlockRef, message.At, nil, 0, owner, speechCounterparty, direction)
				if prior, accepted := acceptanceRestatesRequest(out, candidate); accepted {
					if out[prior].Due.Kind == commitDueNone && candidate.Due.Kind != commitDueNone {
						out[prior].Due = candidate.Due
					}
					continue
				}
				out = append(out, candidate)
			}
			return out
		}
		for _, turn := range conversationTurns(m.Text) {
			author, addressee := counterparty, selfAtom
			if turn.Self {
				author, addressee = selfAtom, counterparty
			}
			reportedActor, attributed := reportedActorFor(m, turn.Body, counterparty, selfAtom)
			if attributed && reportedActor == nil {
				continue
			}
			speechCounterparty := counterparty
			if reportedActor != nil {
				speechCounterparty = *reportedActor
			}
			owner, direction, found := classifyCommitmentSpeech(turn.Body, commitmentSpeechContext{
				Author: author, Addressee: addressee, Self: selfAtom, Counterparty: speechCounterparty,
				ReportedActor: reportedActor,
			})
			if !found {
				continue
			}
			// Legacy memories have no provider message ids. Refuse to fabricate a
			// CommitmentID; direction and ownership remain typed.
			candidate := newCommitment(turn.Body, "", "", validFromOf(m), nil, 0, owner, speechCounterparty, direction)
			if prior, accepted := acceptanceRestatesRequest(out, candidate); accepted {
				if out[prior].Due.Kind == commitDueNone && candidate.Due.Kind != commitDueNone {
					out[prior].Due = candidate.Due
				}
				continue
			}
			out = append(out, candidate)
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
				blockRef := gmailAuthoredBlockRef(message, parts[i])
				slot := 0
				for _, segment := range commitmentSegments(parts[i]) {
					if assignedToThirdParty(segment, selfTokens) {
						continue
					}
					reportedActor, attributed := reportedActorFor(m, segment, counterparty, selfAtom)
					if attributed && reportedActor == nil {
						continue
					}
					speechCounterparty := counterparty
					if reportedActor != nil {
						speechCounterparty = *reportedActor
					}
					owner, direction, found := classifyCommitmentSpeech(segment, commitmentSpeechContext{
						Author: author, Addressee: addressee, Self: selfAtom, Counterparty: speechCounterparty,
						ReportedActor: reportedActor,
					})
					if !found {
						continue
					}
					candidate := newCommitment(segment, message.MessageRef, blockRef, message.At, message.AncestorRefs, slot, owner, speechCounterparty, direction)
					if prior, accepted := acceptanceRestatesRequest(out, candidate); accepted {
						if out[prior].Due.Kind == commitDueNone && candidate.Due.Kind != commitDueNone {
							out[prior].Due = candidate.Due
						}
						continue
					}
					out = append(out, candidate)
					slot++
				}
				delivery, request, quotedAuthor, quotedBlockRef, fulfilled := gmailFulfilledQuotedRequest(m, message, parts[i])
				if fulfilled {
					speechCounterparty := quotedAuthor
					if atomEqual(quotedAuthor, selfAtom) {
						speechCounterparty = author
					}
					owner, direction, found := classifyCommitmentSpeech(request, commitmentSpeechContext{
						Author: quotedAuthor, Addressee: author, Self: selfAtom, Counterparty: speechCounterparty,
					})
					if found && atomEqual(owner, author) && commitmentObjectOverlap(request, delivery) > 0 {
						candidate := newCommitment(
							request, message.MessageRef, quotedBlockRef, message.At,
							message.AncestorRefs, 0, owner, speechCounterparty, direction,
						)
						candidate.State = commitClosed
						candidate.ClosureRef = m.ID
						if citation, err := citationForMemory(m, evidenceSource(m), validFromOf(m)); err == nil {
							candidate.Citations = mergeCommitmentCitations(candidate.Citations, []CommitmentCitation{{
								Citation: citation, CommitmentID: candidate.ID, Role: commitCitationClosure,
							}})
						}
						out = append(out, candidate)
					}
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
				reportedActor, attributed := reportedActorFor(m, segment, counterparty, selfAtom)
				if attributed && reportedActor == nil {
					continue
				}
				speechCounterparty := counterparty
				if reportedActor != nil {
					speechCounterparty = *reportedActor
				}
				owner, direction, found := classifyCommitmentSpeech(segment, commitmentSpeechContext{
					Author: author, Addressee: addressee, Self: selfAtom, Counterparty: speechCounterparty,
					ReportedActor: reportedActor,
				})
				if found {
					out = append(out, newCommitment(segment, "", "", validFromOf(m), nil, 0, owner, speechCounterparty, direction))
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
			reportedActor, attributed := reportedActorFor(m, m.Title, counterparty, selfAtom)
			if attributed && reportedActor == nil {
				return uniqueCommitments(out)
			}
			speechCounterparty := counterparty
			if reportedActor != nil {
				speechCounterparty = *reportedActor
			}
			if owner, direction, found := classifyCommitmentSpeech(m.Title, commitmentSpeechContext{
				Author: author, Addressee: addressee, Self: selfAtom, Counterparty: speechCounterparty,
				ReportedActor: reportedActor,
			}); found {
				messageRef, blockRef := "", ""
				if len(messages) > 0 {
					messageRef, blockRef = messages[0].MessageRef, "subject"
				}
				out = append(out, newCommitment(m.Title, messageRef, blockRef, validFromOf(m), nil, 0, owner, speechCounterparty, direction))
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
		citation.Citation.MemoryID(), citation.CommitmentID, citation.Role, citation.EvidenceRef,
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
			evidenceCitation := citation
			if isIMessageMemory(m) && messageRef != "" {
				var exactErr error
				evidenceCitation, exactErr = citationForMemory(m, evidenceSource(m), at)
				if exactErr != nil {
					return
				}
			}
			for _, segment := range closureEvidenceSegments(text) {
				out = append(out, commitmentEvidence{
					MemoryID: m.ID, MessageRef: messageRef, BlockRef: blockRef,
					Text: segment, OccurredAt: at, Party: party, Authored: true,
					Citation: evidenceCitation, Source: evidenceSource(m),
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
					blockRef := gmailAuthoredBlockRef(message, parts[i])
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
			if messages, present := imessageCommitmentMessages(m); present {
				for _, message := range messages {
					appendEvidence(message.Body, message.MessageRef, message.BlockRef, message.At, message.Party)
				}
				continue
			}
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
				stableEvidenceRef := ""
				if strings.HasPrefix(candidate.MemoryID, "imessage_chat/") &&
					strings.HasPrefix(candidate.MessageRef, candidate.MemoryID+"#") {
					stableEvidenceRef = candidate.MessageRef
				}
				out[i].Citations = mergeCommitmentCitations(out[i].Citations, []CommitmentCitation{{
					Citation: candidate.Citation, CommitmentID: out[i].ID, Role: commitCitationClosure,
					EvidenceRef: stableEvidenceRef,
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
	// Decisions with incomplete or expired validity remain queryable as
	// needs_review, but cannot open an obligation or supply lifecycle evidence
	// that closes one. Filter both paths together so a stale decision cannot act
	// as current law through either side of the lifecycle projection.
	authority := make([]Memory, 0, len(mems))
	for _, m := range mems {
		if memoryMayGovernCommitments(m, now) {
			authority = append(authority, m)
		}
	}
	var commitments []Commitment
	for _, m := range authority {
		commitments = append(commitments, classifyCommitments(m, cfg)...)
	}
	commitments = applyCommitmentLifecycle(commitments, commitmentEvidenceFromMemories(authority, cfg))
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
	line.CounterpartyLabel = commitment.CounterpartyLabel
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
	governance, err := loadGovernance(cfg)
	if err != nil {
		return err
	}
	commitments := applyTeachCommitments(materializeCommitments(mems, cfg, now), governance, cfg)
	for i, commitment := range commitments {
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

func readCommitmentInventory(ctx context.Context, cfg Config, at time.Time) (map[string][]Commitment, error) {
	inventory, _, err := readCommitmentInventoryWithMemories(ctx, cfg, at)
	return inventory, err
}

// readCommitmentInventoryWithMemories returns the current typed inventory and
// the opening memories it validated in the same vault pass. Callers that need
// both must use this helper instead of resolving each memory id separately,
// which would rescan the whole vault once per commitment-bearing memory.
func readCommitmentInventoryWithMemories(ctx context.Context, cfg Config, at time.Time) (map[string][]Commitment, map[string]Memory, error) {
	snapshot, err := readCommitmentSnapshot(ctx, cfg)
	if err != nil {
		return nil, nil, err
	}
	governance, err := loadGovernance(cfg)
	if err != nil {
		return nil, nil, err
	}
	wanted := make(map[string]bool, len(snapshot.Commitments))
	for _, commitment := range snapshot.Commitments {
		wanted[commitment.OpenedBy.MemoryID] = true
	}
	memories := make(map[string]Memory, len(wanted))
	files, err := allMemoryFiles(cfg)
	if err != nil {
		return nil, nil, err
	}
	for _, path := range files {
		m, perr := parseMemory(path)
		if perr == nil && wanted[m.ID] {
			memories[m.ID] = m
		}
	}
	out := map[string][]Commitment{}
	for _, commitment := range snapshot.Commitments {
		// The vault is truth and the index is derived. Refuse a stale row when its
		// evidence is no longer current or no longer readable. Re-evaluating
		// decision validity at the caller's surface clock also prevents a
		// commitment built before review_by from remaining authoritative after it
		// expires without another rebuild.
		m, ok := memories[commitment.OpenedBy.MemoryID]
		if !ok || !governance.memoryVisible(m.ID) ||
			!memoryMayGovernCommitments(m, at) {
			continue
		}
		out[commitment.OpenedBy.MemoryID] = append(out[commitment.OpenedBy.MemoryID], commitment)
	}
	return out, memories, nil
}
