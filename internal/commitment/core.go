// Package commitment owns durable obligation identity and due-date semantics.
package commitment

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Direction string

const (
	DirectionUnknown   Direction = "unknown"
	OwedBySelf         Direction = "owed_by_self"
	OwedByCounterparty Direction = "owed_by_counterparty"
	Open                         = "open"
	Closed                       = "closed"
	Superseded                   = "superseded"
	DueNone                      = "none"
	DueRelative                  = "relative"
	DueExplicitDate              = "explicit_date"
)

type Due struct {
	Kind string `json:"kind"`
	At   string `json:"at,omitempty"`
}

var (
	monthDateRE = regexp.MustCompile(`(?i)\b(january|february|march|april|may|june|july|august|september|october|november|december)\s+([0-9]{1,2})(?:st|nd|rd|th)?(?:,\s*([0-9]{4}))?\b`)
	isoDateRE   = regexp.MustCompile(`\b([0-9]{4})-([0-9]{1,2})-([0-9]{1,2})\b`)
	relativeRE  = regexp.MustCompile(`(?i)\b(today|tomorrow|tonight|this\s+(?:morning|afternoon|evening|week|month)|next\s+(?:monday|tuesday|wednesday|thursday|friday|saturday|sunday|week|month)|monday|tuesday|wednesday|thursday|friday|saturday|sunday|before|after|when|once|until|by\s+the\s+end|in\s+the\s+(?:morning|afternoon|evening)|before\s+(?:breakfast|lunch|dinner)|in\s+[0-9]+\s+(?:minutes?|hours?|days?|weeks?))\b`)
	eventDueRE  = regexp.MustCompile(`(?i)\bfor\s+the\s+(?:[\p{L}\p{N}’'\-]+\s+){0,6}(?:meeting|session|review|walk-through)\b`)
)

func ClassifyDue(text, occurredAt string) Due {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return Due{Kind: DueNone}
	}
	anchor, err := time.Parse(time.RFC3339, strings.TrimSpace(occurredAt))
	if err == nil {
		if date, ok := explicitDue(text, anchor.UTC()); ok {
			return Due{Kind: DueExplicitDate, At: date}
		}
	}
	if relativeRE.MatchString(text) || eventDueRE.MatchString(text) {
		return Due{Kind: DueRelative}
	}
	return Due{Kind: DueNone}
}
func explicitDue(text string, anchor time.Time) (string, bool) {
	year, month, day := 0, time.Month(0), 0
	if match := isoDateRE.FindStringSubmatch(text); len(match) != 0 {
		year, _ = strconv.Atoi(match[1])
		monthValue, _ := strconv.Atoi(match[2])
		month = time.Month(monthValue)
		day, _ = strconv.Atoi(match[3])
	} else if match := monthDateRE.FindStringSubmatch(text); len(match) != 0 {
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
	date := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	if date.Year() != year || date.Month() != month || date.Day() != day {
		return "", false
	}
	return date.Format("2006-01-02"), true
}
func DueValue(due Due) string {
	if due.Kind == DueExplicitDate {
		return due.At
	}
	return due.Kind
}
func ID(messageRef, blockRef string, slot int) string {
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
