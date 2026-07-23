package mora

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	commitOwedBySelf         = "owed_by_self"
	commitOwedByCounterparty = "owed_by_counterparty"

	commitOpen       = "open"
	commitClosed     = "closed"
	commitSuperseded = "superseded"

	commitDueNone         = "none"
	commitDueRelative     = "relative"
	commitDueExplicitDate = "explicit_date"

	commitClosureNone = "none"
)

// Commitment is the typed, derived projection of immutable opening evidence.
// It is materialized with the whole-vault index generation and is never written
// into a vault memory.
type Commitment struct {
	ID           string          `json:"id,omitempty"`
	Owner        govAtom         `json:"owner"`
	Counterparty govAtom         `json:"counterparty"`
	Direction    string          `json:"direction"`
	Summary      string          `json:"summary"`
	OpenedBy     commitSpan      `json:"opened_by"`
	Due          commitDue       `json:"due"`
	State        string          `json:"state"`
	ClosureRef   string          `json:"closure_ref"`
	Citations    []BriefCitation `json:"citations"`
	DuplicateOf  string          `json:"duplicate_of,omitempty"`
}

type commitSpan struct {
	MemoryID   string `json:"memory_id"`
	MessageRef string `json:"message_ref,omitempty"`
	BlockRef   string `json:"block_ref,omitempty"`
	Quote      string `json:"quote"`
	OccurredAt string `json:"occurred_at,omitempty"`
}

type commitDue struct {
	Kind string `json:"kind"`
	At   string `json:"at,omitempty"`
}

type commitmentMessageEvidence struct {
	MessageRef string   `json:"message_ref"`
	Sender     string   `json:"sender,omitempty"`
	To         []string `json:"to,omitempty"`
	Cc         []string `json:"cc,omitempty"`
	At         string   `json:"at,omitempty"`
	BlockRefs  []string `json:"block_refs,omitempty"`
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

func classifyCommitmentSpeech(text string, speech commitmentSpeechContext) (owner govAtom, direction string, ok bool) {
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
	return containsAnyPhrase(lower, []string{
		"send", "share", "review", "confirm", "sign", "bring", "upload", "deliver",
		"call", "follow up", "get back", "organize", "archive", "initial", "choose",
		"return", "introduce", "leave", "export", "provide", "finish", "prepare",
	})
}

func reportedCommitment(lower string) bool {
	return strings.Contains(lower, " said he'll ") ||
		strings.Contains(lower, " said she'll ") ||
		strings.Contains(lower, " said they'll ") ||
		strings.Contains(lower, " said he will ") ||
		strings.Contains(lower, " said she will ") ||
		strings.Contains(lower, " said they will ")
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
		for _, pair := range participantPairs(m.Meta["participants"]) {
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

func commitmentCitation(m Memory) []BriefCitation {
	citation, err := citationForMemory(m, evidenceSource(m), validFromOf(m))
	if err != nil {
		return []BriefCitation{}
	}
	return []BriefCitation{citation}
}

func classifyCommitments(m Memory, cfg Config) []Commitment {
	if m.DeletedAt != "" || isMeetingNotification(m) || memoryIsServiceOnly(m) {
		return nil
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
	selfAtom := canonicalSelfAtom(cfg, "")
	citations := commitmentCitation(m)
	newCommitment := func(summary, messageRef, blockRef, occurredAt string, slot int, owner govAtom, direction string) Commitment {
		return Commitment{
			ID:           commitmentID(messageRef, blockRef, slot),
			Owner:        owner,
			Counterparty: counterparty,
			Direction:    direction,
			Summary:      oneLine(summary),
			OpenedBy: commitSpan{
				MemoryID: m.ID, MessageRef: messageRef, BlockRef: blockRef,
				Quote: oneLine(summary), OccurredAt: occurredAt,
			},
			Due:         commitDue{Kind: commitDueNone},
			State:       commitOpen,
			ClosureRef:  commitClosureNone,
			Citations:   citations,
			DuplicateOf: "",
		}
	}

	var out []Commitment
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
			out = append(out, newCommitment(turn.Body, "", "", "", 0, owner, direction))
		}
		return out
	}

	if isGmailMemory(m) {
		messages := gmailCommitmentMessages(m)
		parts := gmailBodyParts(m)
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
					owner, direction, found := classifyCommitmentSpeech(segment, commitmentSpeechContext{
						Author: author, Addressee: addressee, Self: selfAtom, Counterparty: counterparty,
						ReportedActor: reportedActorFor(m, segment, counterparty),
					})
					if !found {
						continue
					}
					out = append(out, newCommitment(segment, message.MessageRef, blockRef, message.At, slot, owner, direction))
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
				owner, direction, found := classifyCommitmentSpeech(segment, commitmentSpeechContext{
					Author: author, Addressee: addressee, Self: selfAtom, Counterparty: counterparty,
					ReportedActor: reportedActorFor(m, segment, counterparty),
				})
				if found {
					out = append(out, newCommitment(segment, "", "", "", 0, owner, direction))
				}
			}
		}

		// A sender-authored subject is its own immutable evidence block.
		if !isForwardedSubject(m.Title) {
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
				out = append(out, newCommitment(m.Title, messageRef, blockRef, validFromOf(m), 0, owner, direction))
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
	seen := map[string]bool{}
	out := make([]Commitment, 0, len(in))
	for _, commitment := range in {
		key := commitment.ID
		if key == "" {
			key = strings.Join([]string{
				commitment.OpenedBy.MemoryID,
				strings.ToLower(commitment.Summary),
				commitment.Direction,
				commitment.Owner.Kind,
				commitment.Owner.Value,
			}, "\x00")
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, commitment)
	}
	return out
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
}

func writeCommitments(ctx context.Context, tx *sql.Tx, generation string, mems []Memory, cfg Config) error {
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO commitments (generation, row_key, commitment_id, memory_id, payload)
		 VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, m := range mems {
		for i, commitment := range classifyCommitments(m, cfg) {
			payload, err := json.Marshal(commitment)
			if err != nil {
				return err
			}
			rowKey := commitment.ID
			if rowKey == "" {
				sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s", m.ID, i, commitment.Direction, commitment.Summary)))
				rowKey = "legacy:" + hex.EncodeToString(sum[:])
			}
			if _, err := stmt.ExecContext(ctx, generation, rowKey, nullStr(commitment.ID), m.ID, string(payload)); err != nil {
				return err
			}
		}
	}
	return nil
}

func readCommitmentInventory(ctx context.Context, cfg Config) (map[string][]Commitment, error) {
	db, err := ensureIndexDB(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var generation string
	if err := db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key='commitments_generation'`).Scan(&generation); err != nil {
		return nil, fmt.Errorf("read commitment generation: %w", err)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT memory_id, payload FROM commitments WHERE generation=? ORDER BY memory_id, row_key`, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Commitment{}
	for rows.Next() {
		var memoryID, payload string
		if err := rows.Scan(&memoryID, &payload); err != nil {
			return nil, err
		}
		var commitment Commitment
		if err := json.Unmarshal([]byte(payload), &commitment); err != nil {
			return nil, fmt.Errorf("decode commitment %s: %w", memoryID, err)
		}
		out[memoryID] = append(out[memoryID], commitment)
	}
	return out, rows.Err()
}
