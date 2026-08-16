// Package health owns deterministic source freshness classification and rendering.
package health

import (
	"fmt"
	"strings"
	"time"
)

const (
	Never         = "never"
	Failed        = "failed"
	Stale         = "stale"
	Fresh         = "fresh"
	UnreadableKey = "sources_config"
)
const (
	GoogleThreshold = 24 * time.Hour
	LocalThreshold  = 48 * time.Hour
	BannerErrorCap  = 200
)

type Source struct {
	Key           string `json:"key"`
	State         string `json:"state"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	AgeHours      int    `json:"age_hours"`
	LastError     string `json:"last_error,omitempty"`
}
type Status struct {
	LastSuccessAt string
	LastError     string
	ErrorCount    int
}

func Threshold(sourceType string) time.Duration {
	switch sourceType {
	case "gmail", "calendar", "applecalendar", "github":
		return GoogleThreshold
	default:
		return LocalThreshold
	}
}
func Classify(key, sourceType string, st *Status, now time.Time) Source {
	h := Source{Key: key}
	if st == nil || st.LastSuccessAt == "" {
		h.State = Never
		if st != nil {
			h.LastError = st.LastError
		}
		return h
	}
	h.LastSuccessAt = st.LastSuccessAt
	h.LastError = st.LastError
	t, err := time.Parse(time.RFC3339, st.LastSuccessAt)
	if err != nil {
		h.State = Never
		return h
	}
	age := now.Sub(t)
	if age < 0 {
		age = 0
	}
	h.AgeHours = int(age / time.Hour)
	switch {
	case st.LastError != "" || st.ErrorCount > 0:
		h.State = Failed
	case age > Threshold(sourceType):
		h.State = Stale
	default:
		h.State = Fresh
	}
	return h
}
func StateRank(state string) int {
	switch state {
	case Failed:
		return 0
	case Never:
		return 1
	case Stale:
		return 2
	default:
		return 3
	}
}
func Worst(sources []Source) *Source {
	var worst *Source
	worstRank := 0
	for i := range sources {
		h := &sources[i]
		if h.State == Fresh {
			continue
		}
		rank := StateRank(h.State)
		if worst == nil || rank < worstRank || (rank == worstRank && h.AgeHours > worst.AgeHours) {
			worst = h
			worstRank = rank
		}
	}
	return worst
}
func Banner(sources []Source) string {
	worst := Worst(sources)
	if worst == nil {
		return ""
	}
	return BannerLine(*worst)
}
func SanitizeError(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > BannerErrorCap {
		return strings.TrimSpace(string(r[:BannerErrorCap])) + "…"
	}
	return s
}
func BannerLine(h Source) string {
	var detail string
	if h.State == Never {
		detail = h.Key + " — never synced"
	} else {
		detail = fmt.Sprintf("%s — no successful sync for %dh", h.Key, h.AgeHours)
	}
	if h.LastError != "" {
		detail += fmt.Sprintf(" (%s)", SanitizeError(h.LastError))
	}
	return fmt.Sprintf("🔴 MORA HEALTH: %s. Run: mora doctor", detail)
}
