package commitment

import (
	"encoding/json"
	"github.com/pyranthus-hq/mora/internal/evidence"
	"github.com/pyranthus-hq/mora/internal/evidencetext"
	"github.com/pyranthus-hq/mora/internal/graph"
	"github.com/pyranthus-hq/mora/internal/identity"
	"github.com/pyranthus-hq/mora/internal/imessage"
	"github.com/pyranthus-hq/mora/internal/memory"
	"github.com/pyranthus-hq/mora/internal/urgency"
	"sort"
	"strings"
)

type GmailMessage struct {
	MessageRef   string   `json:"message_ref"`
	Sender       string   `json:"sender,omitempty"`
	To           []string `json:"to,omitempty"`
	Cc           []string `json:"cc,omitempty"`
	At           string   `json:"at,omitempty"`
	BlockRefs    []string `json:"block_refs,omitempty"`
	AncestorRefs []string `json:"ancestor_refs,omitempty"`
}

func GmailMessages(m memory.Memory) []GmailMessage {
	body, err := json.Marshal(m.Meta["messages"])
	if err != nil {
		return nil
	}
	var messages []GmailMessage
	if json.Unmarshal(body, &messages) != nil {
		return nil
	}
	return messages
}
func GmailBodyParts(m memory.Memory) []string {
	return strings.Split(urgency.StripFromLine(m.Text), "\n\n---\n\n")
}
func GmailAuthoredBlockRef(message GmailMessage, body string) string {
	if len(message.BlockRefs) == 0 || strings.TrimSpace(evidencetext.SenderAuthoredBody(body)) == "" {
		return ""
	}
	return message.BlockRefs[0]
}
func FirstGmailSender(m memory.Memory) string {
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

type Turn struct {
	Self bool
	Body string
}

func ConversationTurns(text string) []Turn {
	var turns []Turn
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "*") {
			continue
		}
		label, body, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(body) == "" {
			continue
		}
		turns = append(turns, Turn{Self: strings.EqualFold(strings.TrimSpace(label), "me"), Body: strings.TrimSpace(body)})
	}
	return turns
}
func SourceOf(m memory.Memory) string {
	if m.Source != "" {
		return m.Source
	}
	if m.Provider != "" {
		return m.Provider
	}
	return m.Type
}

// EvidenceFromMemories projects eligible caller-filtered memories into deterministic lifecycle evidence.
func EvidenceFromMemories(mems []memory.Memory, selfEmails map[string]bool) []Evidence {
	out := []Evidence{}
	self := CanonicalSelf(selfEmails, "")
	for _, m := range mems {
		if m.DeletedAt != "" {
			continue
		}
		at := graph.ValidFrom(m)
		citation, err := evidence.ForMemory(m, SourceOf(m), at)
		if err != nil {
			continue
		}
		counterparty, _ := Counterparty(m, selfEmails)
		keys := CounterpartyKeys(m, counterparty)
		appendEvidence := func(text, messageRef, blockRef, occurredAt string, party Party) {
			evidenceCitation := citation
			if IsConversation(m) && messageRef != "" {
				// Connector admission already proved the message timestamp and the base citation proved identity/source fields.
				evidenceCitation, _ = evidence.ForMemory(m, SourceOf(m), occurredAt)
			}
			for _, segment := range ClosureSegments(text) {
				out = append(out, Evidence{MemoryID: m.ID, MessageRef: messageRef, BlockRef: blockRef, Text: segment, OccurredAt: occurredAt, Party: party, Authored: true, Citation: evidenceCitation, Source: SourceOf(m), CounterpartyKeys: append([]string(nil), keys...)})
			}
		}
		switch {
		case IsGmail(m):
			messages := GmailMessages(m)
			parts := GmailBodyParts(m)
			if len(messages) > 0 && len(messages) == len(parts) {
				for i, message := range messages {
					author := Atom{Kind: AtomAddress, Value: identity.Normalize(AtomAddress, message.Sender)}
					party := PartyUnknown
					switch {
					case EqualAtom(author, self):
						party = PartySelf
					case EqualAtom(author, counterparty):
						party = PartyCounterparty
					}
					if party == PartyUnknown {
						continue
					}
					appendEvidence(parts[i], message.MessageRef, GmailAuthoredBlockRef(message, parts[i]), message.At, party)
				}
				continue
			}
			sender := Atom{Kind: AtomAddress, Value: identity.Normalize(AtomAddress, FirstGmailSender(m))}
			party := PartyUnknown
			switch {
			case EqualAtom(sender, self):
				party = PartySelf
			case EqualAtom(sender, counterparty):
				party = PartyCounterparty
			}
			if party != PartyUnknown && len(parts) > 0 {
				appendEvidence(parts[0], "", "", at, party)
			}
		case IsConversation(m):
			if messages, present := imessage.CommitmentMessages(m); present {
				for _, message := range messages {
					party := PartyCounterparty
					if message.Self {
						party = PartySelf
					}
					appendEvidence(message.Body, message.MessageRef, message.BlockRef, message.At, party)
				}
				continue
			}
			for _, turn := range ConversationTurns(m.Text) {
				party := PartyCounterparty
				if turn.Self {
					party = PartySelf
				}
				appendEvidence(turn.Body, "", "", at, party)
			}
		case m.Provider == "" && (m.Source == "manual" || m.Source == "mcp"):
			appendEvidence(m.Text, "", "", at, PartySelf)
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
