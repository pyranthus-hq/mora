package commitment

import (
	"encoding/json"
	"github.com/pyranthus-hq/mora/internal/identity"
	"github.com/pyranthus-hq/mora/internal/memory"
	"sort"
	"strings"
	"unicode"
)

const (
	AtomAddress = "address"
	AtomHandle  = "handle"
)

func CanonicalSelf(self map[string]bool, preferred string) Atom {
	if p := strings.ToLower(strings.TrimSpace(preferred)); p != "" {
		return Atom{Kind: AtomAddress, Value: identity.Normalize(AtomAddress, p)}
	}
	values := make([]string, 0, len(self))
	for value := range self {
		values = append(values, value)
	}
	sort.Strings(values)
	if len(values) > 0 {
		return Atom{Kind: AtomAddress, Value: identity.Normalize(AtomAddress, values[0])}
	}
	return Atom{Kind: "self", Value: "self"}
}
func IsGmail(m memory.Memory) bool {
	return strings.EqualFold(m.Provider, "gmail") || strings.Contains(strings.ToLower(m.ProviderID), "gmail")
}
func IsIMessage(m memory.Memory) bool {
	return strings.EqualFold(m.Provider, "imessage") || strings.Contains(strings.ToLower(m.ProviderID), "imessage")
}

// IsWhatsApp reports whether provider identity identifies a WhatsApp memory.
func IsWhatsApp(m memory.Memory) bool { return strings.EqualFold(m.Provider, "whatsapp") }

// IsConversation reports whether m is a chat-transcript memory (iMessage or
// WhatsApp). Both mint participants/handle metadata, so counterparty
// resolution treats them identically; the atom carries the memory's own
// provider so governance keys never collapse the two channels.
func IsConversation(m memory.Memory) bool { return IsIMessage(m) || IsWhatsApp(m) }
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
func ParticipantNameIsSelf(name string, selfTokens map[string]bool) bool {
	fields := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(name)), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
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
func Counterparty(m memory.Memory, self map[string]bool) (Atom, bool) {
	candidates := []Atom{}
	if IsGmail(m) {
		seen := map[string]bool{}
		for _, field := range []string{"from", "to", "cc"} {
			for _, raw := range metaStrings(m.Meta[field]) {
				value := strings.ToLower(strings.TrimSpace(raw))
				if value == "" || self[value] || seen[identity.MailboxKey(value)] {
					continue
				}
				seen[identity.MailboxKey(value)] = true
				candidates = append(candidates, Atom{Kind: AtomAddress, Value: identity.Normalize(AtomAddress, value)})
			}
		}
	} else if IsConversation(m) {
		selfTokens := identity.SelfNameTokens(self)
		for _, pair := range participantPairs(m.Meta["participants"]) {
			if ParticipantNameIsSelf(pair["name"], selfTokens) {
				continue
			}
			if value := strings.TrimSpace(pair["handle"]); value != "" {
				candidates = append(candidates, Atom{Provider: m.Provider, Kind: AtomHandle, Value: identity.Normalize(AtomHandle, value)})
			}
		}
	}
	if len(candidates) != 1 {
		return Atom{}, false
	}
	return candidates[0], true
}
func CounterpartyKeys(m memory.Memory, counterparty Atom) []string {
	seen := map[string]bool{}
	add := func(kind, value string) {
		value = strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
		if value != "" {
			seen[kind+":"+value] = true
		}
	}
	add(counterparty.Kind, identity.Normalize(counterparty.Kind, counterparty.Value))
	if IsGmail(m) {
		var names map[string]string
		if body, err := json.Marshal(m.Meta["names"]); err == nil && json.Unmarshal(body, &names) == nil {
			for raw, name := range names {
				atom := Atom{Kind: AtomAddress, Value: identity.Normalize(AtomAddress, raw)}
				if !EqualAtom(atom, counterparty) {
					continue
				}
				add("name", name)
				if fields := strings.Fields(name); len(fields) > 0 {
					add("given", fields[0])
				}
			}
		}
	}
	if IsConversation(m) {
		for _, pair := range participantPairs(m.Meta["participants"]) {
			atom := Atom{Provider: m.Provider, Kind: AtomHandle, Value: identity.Normalize(AtomHandle, pair["handle"])}
			if !EqualAtom(atom, counterparty) {
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

// GmailThreadCounterparty uses immutable per-message authorship to establish the
// correspondent for a thread. Recipient lists can contain relay Reply-To aliases
// and observers; a single distinct non-self sender is stronger evidence than
// guessing among those recipients. Multiple non-self senders remain ambiguous.
func GmailThreadCounterparty(m memory.Memory, self map[string]bool) (Atom, bool) {
	seen := map[string]Atom{}
	for _, message := range GmailMessages(m) {
		value := strings.ToLower(strings.TrimSpace(message.Sender))
		if value == "" || self[value] {
			continue
		}
		key := identity.MailboxKey(value)
		seen[key] = Atom{Kind: AtomAddress, Value: identity.Normalize(AtomAddress, value)}
	}
	if len(seen) != 1 {
		return Atom{}, false
	}
	for _, counterparty := range seen {
		return counterparty, true
	}
	return Atom{}, false
}
func GmailAddressee(sender Atom, to, cc []string, self, counterparty Atom, threadCounterparty bool) Atom {
	recipients := append(append([]string(nil), to...), cc...)
	seenOther := map[string]bool{}
	seenOtherTo := map[string]bool{}
	hasSelf, hasCounterparty := false, false
	for i, raw := range recipients {
		value := strings.ToLower(strings.TrimSpace(raw))
		atom := Atom{Kind: AtomAddress, Value: identity.Normalize(AtomAddress, value)}
		switch {
		case EqualAtom(atom, self):
			hasSelf = true
		case EqualAtom(atom, counterparty):
			hasCounterparty = true
			seenOther[identity.MailboxKey(value)] = true
		case value != "":
			seenOther[identity.MailboxKey(value)] = true
		}
		if i < len(to) && value != "" && !EqualAtom(atom, self) {
			seenOtherTo[identity.MailboxKey(value)] = true
		}
	}
	switch {
	case EqualAtom(sender, counterparty) && hasSelf && (len(seenOther) == 0 || (len(seenOther) == 1 && hasCounterparty)):
		return self
	case EqualAtom(sender, self) && hasCounterparty && len(seenOther) == 1:
		return counterparty
	case EqualAtom(sender, self) && threadCounterparty && !hasCounterparty && len(seenOtherTo) == 1:
		// A single primary recipient on a reply to a thread with one proven
		// correspondent may be that provider's opaque Reply-To alias. CCs are
		// observers and never select the owner; multiple primary recipients fail.
		return counterparty
	default:
		return Atom{}
	}
}
