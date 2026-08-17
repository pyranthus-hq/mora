package mora

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	commitmentpkg "github.com/pyranthus-hq/mora/internal/commitment"
	imessagepkg "github.com/pyranthus-hq/mora/internal/imessage"
	meetingpkg "github.com/pyranthus-hq/mora/internal/meeting"
	"sort"
	"strings"
	"time"
)

// Direction is the shared obligation-direction vocabulary used by every
// product lane. A named type prevents task-ledger and evidence-derived loops
// from drifting into independently invented string values.
type Direction = commitmentpkg.Direction

const (
	commitDirectionUnknown   = commitmentpkg.DirectionUnknown
	commitOwedBySelf         = commitmentpkg.OwedBySelf
	commitOwedByCounterparty = commitmentpkg.OwedByCounterparty

	commitOpen       = commitmentpkg.Open
	commitClosed     = commitmentpkg.Closed
	commitSuperseded = commitmentpkg.Superseded

	commitDueNone         = commitmentpkg.DueNone
	commitDueRelative     = commitmentpkg.DueRelative
	commitDueExplicitDate = commitmentpkg.DueExplicitDate

	commitClosureNone = commitmentpkg.ClosureNone

	commitCitationOpener     = commitmentpkg.CitationOpener
	commitCitationClosure    = commitmentpkg.CitationClosure
	commitCitationSupporting = commitmentpkg.CitationSupporting
)

type Commitment = commitmentpkg.Record
type commitSpan = commitmentpkg.Span

type commitDue = commitmentpkg.Due

func classifyCommitmentDue(text, occurredAt string) commitDue {
	return commitmentpkg.ClassifyDue(text, occurredAt)
}

func commitDueValue(due commitDue) string { return commitmentpkg.DueValue(due) }

type commitmentMessageEvidence struct {
	MessageRef   string   `json:"message_ref"`
	Sender       string   `json:"sender,omitempty"`
	To           []string `json:"to,omitempty"`
	Cc           []string `json:"cc,omitempty"`
	At           string   `json:"at,omitempty"`
	BlockRefs    []string `json:"block_refs,omitempty"`
	AncestorRefs []string `json:"ancestor_refs,omitempty"`
}

type CommitmentCitation = commitmentpkg.Citation

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
	Author, Addressee, Self, Counterparty govAtom
	ReportedActor                         *govAtom
}

func commitmentSpeechAtom(a govAtom) commitmentpkg.Atom {
	return a
}
func commitmentGovAtom(a commitmentpkg.Atom) govAtom {
	return a
}
func classifyCommitmentSpeech(text string, speech commitmentSpeechContext) (govAtom, Direction, bool) {
	var reported *commitmentpkg.Atom
	if speech.ReportedActor != nil {
		atom := commitmentSpeechAtom(*speech.ReportedActor)
		reported = &atom
	}
	owner, direction, ok := commitmentpkg.ClassifySpeech(text, commitmentpkg.SpeechContext{Author: commitmentSpeechAtom(speech.Author), Addressee: commitmentSpeechAtom(speech.Addressee), Self: commitmentSpeechAtom(speech.Self), Counterparty: commitmentSpeechAtom(speech.Counterparty), ReportedActor: reported})
	return commitmentGovAtom(owner), direction, ok
}
func userAuthoredPromiseToAnother(text string) bool { return commitmentpkg.ManualPromise(text) }
func userAuthoredPromiseCounterpartyLabel(text string) string {
	return commitmentpkg.ManualPromiseCounterpartyLabel(text)
}

// commitmentID is versioned and length-prefixed exactly like the scorer's
// evidence identity. Person identity is deliberately absent: graph alias merges
// can regroup a commitment without churning its durable anchor.
func commitmentID(messageRef, blockRef string, slot int) string {
	return commitmentpkg.ID(messageRef, blockRef, slot)
}

func atomEqual(a, b govAtom) bool { return commitmentpkg.EqualAtom(a, b) }

func atomPresent(a govAtom) bool {
	return strings.TrimSpace(a.Kind) != "" && strings.TrimSpace(a.Value) != ""
}

// reportedActorFor resolves attributed third-person speech only from stable source
// identities. A participant other than the current thread counterparty may own an
// obligation only when the report also names the user as its beneficiary; otherwise
// the work is between third parties and must be dropped.
func reportedActorFor(m Memory, text string, counterparty, self govAtom) (*govAtom, bool) {
	var candidates []commitmentpkg.NamedActor
	selfNames := []string{}
	if isIMessageMemory(m) {
		for _, pair := range participantPairs(m.Meta["participants"]) {
			atom := govAtom{Provider: "imessage", Kind: atomHandle, Value: normalizeIdentity(atomHandle, pair["handle"])}
			if atomEqual(counterparty, atom) {
				candidates = append(candidates, commitmentpkg.NamedActor{Atom: atom, Name: pair["name"]})
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
				candidates = append(candidates, commitmentpkg.NamedActor{Atom: atom, Name: names[raw]})
			}
		}
	}
	return commitmentpkg.ReportedActor(text, counterparty, self, candidates, selfNames)
}

func canonicalSelfAtom(cfg Config, preferred string) govAtom {
	return commitmentpkg.CanonicalSelf(selfEmails(cfg), preferred)
}

func commitmentCounterparty(m Memory, cfg Config) (govAtom, bool) {
	return commitmentpkg.Counterparty(m, selfEmails(cfg))
}

// participantNameIsSelf handles imported/transcoded iMessage records that list the
// user alongside the other chat participants. The live connector already stores
// only other-party handles. We exclude a listed participant only when every
// meaningful display-name token is independently present in a configured self
// mailbox local-part; a partial/common-name overlap is insufficient.

func commitmentCounterpartyKeys(m Memory, counterparty govAtom) []string {
	return commitmentpkg.CounterpartyKeys(m, counterparty)
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
	parsed, present := imessagepkg.CommitmentMessages(m)
	messages := make([]imessageCommitmentMessage, 0, len(parsed))
	for _, message := range parsed {
		party := commitmentPartyCounterparty
		if message.Self {
			party = commitmentPartySelf
		}
		messages = append(messages, imessageCommitmentMessage{MessageRef: message.MessageRef, BlockRef: message.BlockRef, Body: message.Body, At: message.At, Party: party})
	}
	return messages, present
}

// trustedIMessageAuthoredBody removes exactly one rendered sender prefix. The
// explicit direction and sender metadata must agree with the visible block.

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
	var names map[string]string
	if raw, err := json.Marshal(m.Meta["names"]); err != nil || json.Unmarshal(raw, &names) != nil {
		return "", "", govAtom{}, "", false
	}
	delivery, request, author, blockRef, ok := commitmentpkg.FulfilledQuotedRequest(body, message.BlockRefs, names)
	if !ok {
		return "", "", govAtom{}, "", false
	}
	return delivery, request, govAtom{Kind: atomAddress, Value: normalizeIdentity(atomAddress, author)}, blockRef, true
}

// acceptanceRestatesRequest implements the contract rule that accepting an
// existing request does not create extra work. It is intentionally narrower than
// general dedup: same artifact, later message, same typed parties/direction/due,
// a direct-request opener, and either strong object overlap or an explicit
// anaphoric acceptance with corroborating object overlap.
func acceptanceRestatesRequest(existing []Commitment, candidate Commitment) (int, bool) {
	return commitmentpkg.AcceptanceRestatesRequest(existing, candidate)
}

func gmailAddressee(sender govAtom, to, cc []string, self, counterparty govAtom) govAtom {
	return commitmentpkg.GmailAddressee(sender, to, cc, self, counterparty)
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
	if m.DeletedAt != "" || meetingpkg.IsMeetingNotification(m) || memoryIsServiceOnly(m) {
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
		selfTokens := meetingpkg.SelfNameTokens(selfEmails(cfg))
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
					if meetingpkg.AssignedToThirdParty(segment, selfTokens) {
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
				if meetingpkg.AssignedToThirdParty(segment, selfTokens) {
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
		if !isForwardedSubject(m.Title) && !meetingpkg.AssignedToThirdParty(m.Title, selfTokens) {
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

func commitmentSegments(text string) []string { return commitmentpkg.Segments(text) }

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

func uniqueCommitments(in []Commitment) []Commitment { return commitmentpkg.Unique(in) }

func mergeCommitmentCitations(a, b []CommitmentCitation) []CommitmentCitation {
	return commitmentpkg.MergeCitations(a, b)
}

func commitmentEvidenceFromMemories(mems []Memory, cfg Config) []commitmentEvidence {
	eligible := make([]Memory, 0, len(mems))
	for _, m := range mems {
		if m.DeletedAt == "" && !meetingpkg.IsMeetingNotification(m) && !memoryIsServiceOnly(m) {
			eligible = append(eligible, m)
		}
	}
	projected := commitmentpkg.EvidenceFromMemories(eligible, selfEmails(cfg))
	out := make([]commitmentEvidence, len(projected))
	for i, e := range projected {
		out[i] = commitmentEvidence{MemoryID: e.MemoryID, MessageRef: e.MessageRef, BlockRef: e.BlockRef, Text: e.Text, OccurredAt: e.OccurredAt, Party: commitmentPartyRole(e.Party), Authored: e.Authored, Citation: e.Citation, Source: e.Source, CounterpartyKeys: e.CounterpartyKeys}
	}
	return out
}

func commitmentObjectOverlap(a, b string) int     { return commitmentpkg.ObjectOverlap(a, b) }
func commitmentEvidenceLess(a, b Commitment) bool { return commitmentpkg.EvidenceLess(a, b) }
func containsStringFold(values []string, want string) bool {
	return commitmentpkg.ContainsStringFold(values, want)
}
func commitmentLifecycleEvidence(e commitmentEvidence) commitmentpkg.Evidence {
	stableEvidenceRef := ""
	if strings.HasPrefix(e.MemoryID, "imessage_chat/") && strings.HasPrefix(e.MessageRef, e.MemoryID+"#") {
		stableEvidenceRef = e.MessageRef
	}
	return commitmentpkg.Evidence{MemoryID: e.MemoryID, MessageRef: e.MessageRef, Text: e.Text, OccurredAt: e.OccurredAt, Party: commitmentpkg.Party(e.Party), Authored: e.Authored, CounterpartyKeys: e.CounterpartyKeys, Citation: e.Citation, CitationEvidenceRef: stableEvidenceRef}
}
func applyCommitmentLifecycle(commitments []Commitment, evidence []commitmentEvidence) []Commitment {
	ev := make([]commitmentpkg.Evidence, len(evidence))
	for i, e := range evidence {
		ev[i] = commitmentLifecycleEvidence(e)
	}
	return commitmentpkg.ApplyLifecycle(commitments, ev)
}

func deduplicateCommitments(commitments []Commitment) []Commitment {
	return commitmentpkg.Deduplicate(commitments)
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
