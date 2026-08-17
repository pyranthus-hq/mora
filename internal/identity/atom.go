package identity

import "strings"

// Atom is a stable provider identity used by governance and derived projections.
type Atom struct {
	Provider string `json:"provider,omitempty"`
	Kind     string `json:"kind"`
	Value    string `json:"value"`
}

func Normalize(kind, raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	if kind == "address" || strings.Contains(v, "@") {
		return strings.ToLower(v)
	}
	return v
}
func MailboxKey(addr string) string {
	addr = strings.ToLower(strings.TrimSpace(addr))
	at := strings.LastIndexByte(addr, '@')
	if at < 0 {
		return addr
	}
	local, host := addr[:at], addr[at+1:]
	if host == "gmail.com" || host == "googlemail.com" {
		if i := strings.IndexByte(local, '+'); i >= 0 {
			local = local[:i]
		}
		local = strings.ReplaceAll(local, ".", "")
		host = "gmail.com"
	}
	return local + "@" + host
}

func SelfNameTokens(self map[string]bool) map[string]bool {
	out := map[string]bool{}
	for addr := range self {
		local, _, found := strings.Cut(addr, "@")
		if !found {
			local = addr
		}
		for _, part := range strings.FieldsFunc(local, func(r rune) bool { return r == '.' || r == '_' || r == '-' || r == '+' }) {
			if len(part) >= 2 {
				out[strings.ToLower(part)] = true
			}
		}
	}
	return out
}
