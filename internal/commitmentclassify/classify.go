// Package commitmentclassify derives provider-neutral commitment records from source memories.
package commitmentclassify

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/pyranthus-hq/mora/internal/commitment"
	"github.com/pyranthus-hq/mora/internal/evidence"
	"github.com/pyranthus-hq/mora/internal/evidencetext"
	"github.com/pyranthus-hq/mora/internal/graph"
	"github.com/pyranthus-hq/mora/internal/identity"
	"github.com/pyranthus-hq/mora/internal/imessage"
	"github.com/pyranthus-hq/mora/internal/meeting"
	"github.com/pyranthus-hq/mora/internal/memory"
)

type Options struct {
	SelfEmails  map[string]bool
	ServiceOnly bool
}

func metaStrings(value any) []string {
	body, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var values []string
	if json.Unmarshal(body, &values) != nil {
		return nil
	}
	return values
}
func participantPairs(value any) []map[string]string {
	body, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var pairs []map[string]string
	if json.Unmarshal(body, &pairs) != nil {
		return nil
	}
	return pairs
}
func reportedActorFor(m memory.Memory, text string, counterparty, self commitment.Atom) (*commitment.Atom, bool) {
	var candidates []commitment.NamedActor
	var selfNames []string
	if commitment.IsIMessage(m) {
		for _, pair := range participantPairs(m.Meta["participants"]) {
			atom := commitment.Atom{Provider: "imessage", Kind: commitment.AtomHandle, Value: identity.Normalize(commitment.AtomHandle, pair["handle"])}
			if commitment.EqualAtom(counterparty, atom) {
				candidates = append(candidates, commitment.NamedActor{Atom: atom, Name: pair["name"]})
			}
		}
	}
	if commitment.IsGmail(m) {
		var names map[string]string
		if body, err := json.Marshal(m.Meta["names"]); err == nil && json.Unmarshal(body, &names) == nil {
			keys := make([]string, 0, len(names))
			for raw := range names {
				keys = append(keys, raw)
			}
			sort.Strings(keys)
			for _, raw := range keys {
				atom := commitment.Atom{Kind: commitment.AtomAddress, Value: identity.Normalize(commitment.AtomAddress, raw)}
				if commitment.EqualAtom(atom, self) {
					selfNames = append(selfNames, names[raw])
					continue
				}
				candidates = append(candidates, commitment.NamedActor{Atom: atom, Name: names[raw]})
			}
		}
	}
	return commitment.ReportedActor(text, counterparty, self, candidates, selfNames)
}

func fulfilledQuotedRequest(m memory.Memory, message commitment.GmailMessage, body string) (delivery, request string, quotedAuthor commitment.Atom, blockRef string, ok bool) {
	var names map[string]string
	if raw, err := json.Marshal(m.Meta["names"]); err != nil || json.Unmarshal(raw, &names) != nil {
		return "", "", commitment.Atom{}, "", false
	}
	delivery, request, author, blockRef, ok := commitment.FulfilledQuotedRequest(body, message.BlockRefs, names)
	if !ok {
		return "", "", commitment.Atom{}, "", false
	}
	return delivery, request, commitment.Atom{Kind: commitment.AtomAddress, Value: identity.Normalize(commitment.AtomAddress, author)}, blockRef, true
}

func Classify(m memory.Memory, opts Options) []commitment.Record {
	if m.DeletedAt != "" || meeting.IsMeetingNotification(m) || opts.ServiceOnly {
		return nil
	}
	self := commitment.CanonicalSelf(opts.SelfEmails, "")
	makeRecord := func(summary, messageRef, blockRef, occurredAt string, ancestorRefs []string, slot int, owner, counterparty commitment.Atom, direction commitment.Direction) commitment.Record {
		return commitment.NewRecord(m, summary, messageRef, blockRef, occurredAt, ancestorRefs, slot, owner, counterparty, direction)
	}
	var out []commitment.Record
	if m.Provider == "" && (m.Source == "manual" || m.Source == "mcp") {
		for slot, segment := range commitment.Segments(m.Text) {
			if !commitment.ManualPromise(segment) {
				continue
			}
			out = append(out, makeRecord(segment, "", "", graph.ValidFrom(m), nil, slot, self, commitment.Atom{}, commitment.OwedBySelf))
			out[len(out)-1].CounterpartyLabel = commitment.ManualPromiseCounterpartyLabel(segment)
		}
		return commitment.Unique(out)
	}
	counterparty, ok := commitment.Counterparty(m, opts.SelfEmails)
	if !ok && commitment.IsGmail(m) {
		sender := strings.ToLower(strings.TrimSpace(commitment.FirstGmailSender(m)))
		if sender != "" && !opts.SelfEmails[sender] {
			counterparty = commitment.Atom{Kind: commitment.AtomAddress, Value: identity.Normalize(commitment.AtomAddress, sender)}
			ok = true
		}
	}
	if !ok {
		return nil
	}
	if commitment.IsIMessage(m) {
		if messages, present := imessage.CommitmentMessages(m); present {
			for _, message := range messages {
				author, addressee := counterparty, self
				if message.Self {
					author, addressee = self, counterparty
				}
				reported, attributed := reportedActorFor(m, message.Body, counterparty, self)
				if attributed && reported == nil {
					continue
				}
				speechCounterparty := counterparty
				if reported != nil {
					speechCounterparty = *reported
				}
				owner, direction, found := commitment.ClassifySpeech(message.Body, commitment.SpeechContext{Author: author, Addressee: addressee, Self: self, Counterparty: speechCounterparty, ReportedActor: reported})
				if !found {
					continue
				}
				candidate := makeRecord(message.Body, message.MessageRef, message.BlockRef, message.At, nil, 0, owner, speechCounterparty, direction)
				if prior, accepted := commitment.AcceptanceRestatesRequest(out, candidate); accepted {
					if out[prior].Due.Kind == commitment.DueNone && candidate.Due.Kind != commitment.DueNone {
						out[prior].Due = candidate.Due
					}
					continue
				}
				out = append(out, candidate)
			}
			return out
		}
		for _, turn := range commitment.ConversationTurns(m.Text) {
			author, addressee := counterparty, self
			if turn.Self {
				author, addressee = self, counterparty
			}
			reported, attributed := reportedActorFor(m, turn.Body, counterparty, self)
			if attributed && reported == nil {
				continue
			}
			speechCounterparty := counterparty
			if reported != nil {
				speechCounterparty = *reported
			}
			owner, direction, found := commitment.ClassifySpeech(turn.Body, commitment.SpeechContext{Author: author, Addressee: addressee, Self: self, Counterparty: speechCounterparty, ReportedActor: reported})
			if !found {
				continue
			}
			candidate := makeRecord(turn.Body, "", "", graph.ValidFrom(m), nil, 0, owner, speechCounterparty, direction)
			if prior, accepted := commitment.AcceptanceRestatesRequest(out, candidate); accepted {
				if out[prior].Due.Kind == commitment.DueNone && candidate.Due.Kind != commitment.DueNone {
					out[prior].Due = candidate.Due
				}
				continue
			}
			out = append(out, candidate)
		}
		return out
	}
	if commitment.IsGmail(m) {
		messages := commitment.GmailMessages(m)
		parts := commitment.GmailBodyParts(m)
		selfTokens := identity.SelfNameTokens(opts.SelfEmails)
		if len(messages) > 0 && len(messages) == len(parts) {
			for i, message := range messages {
				sender := strings.ToLower(strings.TrimSpace(message.Sender))
				author := commitment.Atom{}
				if sender != "" {
					author = commitment.Atom{Kind: commitment.AtomAddress, Value: identity.Normalize(commitment.AtomAddress, sender)}
				}
				if !commitment.EqualAtom(author, self) && !commitment.EqualAtom(author, counterparty) {
					continue
				}
				addressee := commitment.GmailAddressee(author, message.To, message.Cc, self, counterparty)
				blockRef := commitment.GmailAuthoredBlockRef(message, parts[i])
				slot := 0
				for _, segment := range commitment.Segments(parts[i]) {
					if meeting.AssignedToThirdParty(segment, selfTokens) {
						continue
					}
					reported, attributed := reportedActorFor(m, segment, counterparty, self)
					if attributed && reported == nil {
						continue
					}
					speechCounterparty := counterparty
					if reported != nil {
						speechCounterparty = *reported
					}
					owner, direction, found := commitment.ClassifySpeech(segment, commitment.SpeechContext{Author: author, Addressee: addressee, Self: self, Counterparty: speechCounterparty, ReportedActor: reported})
					if !found {
						continue
					}
					candidate := makeRecord(segment, message.MessageRef, blockRef, message.At, message.AncestorRefs, slot, owner, speechCounterparty, direction)
					if prior, accepted := commitment.AcceptanceRestatesRequest(out, candidate); accepted {
						if out[prior].Due.Kind == commitment.DueNone && candidate.Due.Kind != commitment.DueNone {
							out[prior].Due = candidate.Due
						}
						continue
					}
					out = append(out, candidate)
					slot++
				}
				delivery, request, quotedAuthorRaw, quotedBlockRef, fulfilled := fulfilledQuotedRequest(m, message, parts[i])
				if fulfilled {
					quotedAuthor := quotedAuthorRaw
					speechCounterparty := quotedAuthor
					if commitment.EqualAtom(quotedAuthor, self) {
						speechCounterparty = author
					}
					owner, direction, found := commitment.ClassifySpeech(request, commitment.SpeechContext{Author: quotedAuthor, Addressee: author, Self: self, Counterparty: speechCounterparty})
					if found && commitment.EqualAtom(owner, author) && commitment.ObjectOverlap(request, delivery) > 0 {
						candidate := makeRecord(request, message.MessageRef, quotedBlockRef, message.At, message.AncestorRefs, 0, owner, speechCounterparty, direction)
						candidate.State = commitment.Closed
						candidate.ClosureRef = m.ID
						if citation, err := evidence.ForMemory(m, commitment.SourceOf(m), graph.ValidFrom(m)); err == nil {
							candidate.Citations = commitment.MergeCitations(candidate.Citations, []commitment.Citation{{Citation: citation, CommitmentID: candidate.ID, Role: commitment.CitationClosure}})
						}
						out = append(out, candidate)
					}
				}
			}
		} else {
			sender := strings.ToLower(strings.TrimSpace(commitment.FirstGmailSender(m)))
			author := commitment.Atom{}
			if sender != "" {
				author = commitment.Atom{Kind: commitment.AtomAddress, Value: identity.Normalize(commitment.AtomAddress, sender)}
			}
			addressee := commitment.GmailAddressee(author, metaStrings(m.Meta["to"]), metaStrings(m.Meta["cc"]), self, counterparty)
			first := ""
			if len(parts) > 0 {
				first = parts[0]
			}
			for _, segment := range commitment.Segments(first) {
				if meeting.AssignedToThirdParty(segment, selfTokens) {
					continue
				}
				reported, attributed := reportedActorFor(m, segment, counterparty, self)
				if attributed && reported == nil {
					continue
				}
				speechCounterparty := counterparty
				if reported != nil {
					speechCounterparty = *reported
				}
				owner, direction, found := commitment.ClassifySpeech(segment, commitment.SpeechContext{Author: author, Addressee: addressee, Self: self, Counterparty: speechCounterparty, ReportedActor: reported})
				if found {
					out = append(out, makeRecord(segment, "", "", graph.ValidFrom(m), nil, 0, owner, speechCounterparty, direction))
				}
			}
		}
		if !evidencetext.IsForwardedSubject(m.Title) && !meeting.AssignedToThirdParty(m.Title, selfTokens) {
			sender := strings.ToLower(strings.TrimSpace(commitment.FirstGmailSender(m)))
			author := commitment.Atom{}
			if sender != "" {
				author = commitment.Atom{Kind: commitment.AtomAddress, Value: identity.Normalize(commitment.AtomAddress, sender)}
			}
			addressee := commitment.GmailAddressee(author, metaStrings(m.Meta["to"]), metaStrings(m.Meta["cc"]), self, counterparty)
			reported, attributed := reportedActorFor(m, m.Title, counterparty, self)
			if attributed && reported == nil {
				return commitment.Unique(out)
			}
			speechCounterparty := counterparty
			if reported != nil {
				speechCounterparty = *reported
			}
			if owner, direction, found := commitment.ClassifySpeech(m.Title, commitment.SpeechContext{Author: author, Addressee: addressee, Self: self, Counterparty: speechCounterparty, ReportedActor: reported}); found {
				messageRef, blockRef := "", ""
				if len(messages) > 0 {
					messageRef, blockRef = messages[0].MessageRef, "subject"
				}
				out = append(out, makeRecord(m.Title, messageRef, blockRef, graph.ValidFrom(m), nil, 0, owner, speechCounterparty, direction))
			}
		}
	}
	return commitment.Unique(out)
}
