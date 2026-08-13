package registry

import (
	"strings"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func HasConfiguredFilesystemSource(sources []memory.Source) bool {
	for _, s := range sources {
		if s.Type == "filesystem" && s.Path != "" {
			return true
		}
	}
	return false
}

func ContainsType(types []string, t string) bool {
	for _, x := range types {
		if x == t {
			return true
		}
	}
	return false
}

func WithoutTypes(types []string, drop ...string) []string {
	out := types[:0:0]
	for _, x := range types {
		if !ContainsType(drop, x) {
			out = append(out, x)
		}
	}
	return out
}

func ParseCSVList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if t := strings.TrimSpace(part); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func ValidAccountLabel(label string) bool {
	for _, r := range label {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return label != ""
}

func GoogleSourceNames(account string) (gmail, calendar string) {
	if account == "" {
		return "gmail", "calendar"
	}
	return "gmail-" + account, "calendar-" + account
}

func GoogleAccountForEmail(sources []memory.Source, email string) (label string, found bool) {
	if email == "" {
		return "", false
	}
	for _, s := range sources {
		if (s.Type == "gmail" || s.Type == "calendar") && s.Email != "" && strings.EqualFold(s.Email, email) {
			return s.Account, true
		}
	}
	return "", false
}
