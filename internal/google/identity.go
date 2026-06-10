package google

import (
	"net/mail"
	"sort"
	"strings"
)

// addrSet accumulates normalized email identities and their display-name aliases
// across one or more address headers. Addresses are lowercased and deduped;
// display names are kept per address so the entity graph (S4) can accrete them as
// aliases. Output lists are sorted for deterministic Meta (byte-stable graph).
type addrSet struct {
	addrs map[string]struct{}
	names map[string]string // lowercased addr -> first non-empty display name
}

func newAddrSet() *addrSet {
	return &addrSet{addrs: map[string]struct{}{}, names: map[string]string{}}
}

// addHeader parses one RFC 5322 address-list header (From/To/Cc) with net/mail so
// quoted display names, comma-separated lists, and comments survive. net/mail's
// ParseAddressList is all-or-nothing, so on a malformed header fall back to
// per-address parsing — a single bad address never drops the rest of the list.
func (s *addrSet) addHeader(raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	if list, err := mail.ParseAddressList(raw); err == nil {
		for _, a := range list {
			s.add(a.Address, a.Name)
		}
		return
	}
	// Best-effort fallback (ParseAddressList is all-or-nothing): split on commas
	// that are NOT inside a quoted display name or angle-bracket address, so a
	// comma in `"Doe, Jane" <jane@x.com>` doesn't sever a valid address when some
	// OTHER address in the list is malformed.
	for _, part := range splitAddrList(raw) {
		if a, err := mail.ParseAddress(strings.TrimSpace(part)); err == nil {
			s.add(a.Address, a.Name)
		}
	}
}

// splitAddrList splits a raw address-list header on top-level commas, treating a
// comma inside "..." (quoted name) or <...> (addr-spec) as literal.
func splitAddrList(raw string) []string {
	var parts []string
	var cur strings.Builder
	inQuote, inAngle := false, false
	for _, r := range raw {
		switch r {
		case '"':
			inQuote = !inQuote
		case '<':
			if !inQuote {
				inAngle = true
			}
		case '>':
			if !inQuote {
				inAngle = false
			}
		case ',':
			if !inQuote && !inAngle {
				parts = append(parts, cur.String())
				cur.Reset()
				continue
			}
		}
		cur.WriteRune(r)
	}
	if strings.TrimSpace(cur.String()) != "" {
		parts = append(parts, cur.String())
	}
	return parts
}

// add records one already-split address (used directly for Calendar attendees,
// whose emails arrive pre-parsed).
func (s *addrSet) add(addr, name string) {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if addr == "" {
		return
	}
	s.addrs[addr] = struct{}{}
	if name = strings.TrimSpace(name); name != "" {
		if _, ok := s.names[addr]; !ok {
			s.names[addr] = name
		}
	}
}

func (s *addrSet) empty() bool { return len(s.addrs) == 0 }

// list returns the sorted, deduped lowercased addresses.
func (s *addrSet) list() []string {
	out := make([]string, 0, len(s.addrs))
	for a := range s.addrs {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// putIdentities writes from/to/cc-style address lists and a merged names map into
// a Meta map, omitting empty lists and an empty names map so semantically-empty
// identity data never becomes hash material or a pointless meta line.
func putAddrList(meta map[string]any, key string, s *addrSet) {
	if !s.empty() {
		meta[key] = s.list()
	}
}

// mergeNames folds each set's display-name aliases into one names map on Meta,
// added only when non-empty.
func mergeNames(meta map[string]any, sets ...*addrSet) {
	names := map[string]string{}
	for _, s := range sets {
		for addr, n := range s.names {
			if _, ok := names[addr]; !ok {
				names[addr] = n
			}
		}
	}
	if len(names) > 0 {
		meta["names"] = names
	}
}
