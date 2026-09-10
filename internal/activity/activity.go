// Package activity derives connector-neutral activity facts from one memory.
//
// It only reads the supplied memory. Callers provide now so the result is
// deterministic and can use Select for a stable, source-event ordering.
package activity

import (
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
	"github.com/pyranthus-hq/mora/internal/segments"
)

// EventSource identifies the evidence used for EventAt.
type EventSource string

const (
	EventSourceMessageEvidence EventSource = "message_evidence"
	EventSourceOccurredAt      EventSource = "occurred_at"
)

// Participation is the shared evidence-backed conversation DTO.
type Participation = memory.Participation

// Projection is the read-time activity projection for one memory. Automated is
// nil until a later durable-stamp implementation supplies an affirmative basis.
type Projection struct {
	EventAt       *time.Time
	EventSource   EventSource
	Eligible      bool
	Participation *Participation
	Automated     *bool
}

// Result joins a memory with its activity projection. Select orders Results by
// EventAt descending and Memory.ID ascending for equal event instants.
type Result struct {
	Memory     memory.Memory
	Projection Projection
}

// Derive selects activity time and participation without reading sources or
// writing a vault. It supports Gmail, iMessage, WhatsApp, Google Calendar, and
// Apple Calendar. Conversation memories prefer the newest validated retained
// message-evidence timestamp; only when no such timestamp exists do they fall
// back to explicit meta.occurred_at. Calendar memories use occurred_at only.
//
// An unknown, malformed, or future selected time is ineligible. now itself is
// inclusive: an event at now is eligible. CreatedAt is never a fallback.
func Derive(m memory.Memory, now time.Time) Projection {
	p := Projection{}
	provider := providerOf(m)
	if !supported(provider) {
		return p
	}

	if provider == "imessage" || provider == "whatsapp" {
		var rows []segments.Row
		if !m.Truncated {
			rows, _ = segments.Derive(m)
		}
		if len(rows) > 0 {
			p.Participation = participation(rows, explicitGroup(m, provider))
			if at, ok := newestAt(rows); ok {
				p.EventAt, p.EventSource = at, EventSourceMessageEvidence
			}
		}
	} else if provider == "gmail" {
		// Derive validates Gmail's message/block/ref correspondence before any
		// message timestamp is trusted. Its current parent-ref rules remain the
		// authority, so this helper does not broaden historical ref acceptance.
		rows, _ := segments.Derive(m)
		if at, ok := newestAt(rows); ok {
			p.EventAt, p.EventSource = at, EventSourceMessageEvidence
		}
	}

	if p.EventAt == nil {
		if at, ok := occurredAt(m); ok {
			p.EventAt, p.EventSource = at, EventSourceOccurredAt
		}
	}
	if p.EventAt != nil && !p.EventAt.After(now) {
		p.Eligible = true
	}
	return p
}

// Select derives eligible activity rows and returns a new deterministic order.
func Select(memories []memory.Memory, now time.Time) []Result {
	return SelectRange(memories, time.Time{}, now)
}

// SelectRange returns eligible activity rows in the inclusive [from, now] range.
// A zero from disables the lower bound.
func SelectRange(memories []memory.Memory, from, now time.Time) []Result {
	out := make([]Result, 0, len(memories))
	for _, m := range memories {
		p := Derive(m, now)
		if p.Eligible && (from.IsZero() || !p.EventAt.Before(from)) {
			out = append(out, Result{Memory: m, Projection: p})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.Projection.EventAt.Equal(*b.Projection.EventAt) {
			return a.Projection.EventAt.After(*b.Projection.EventAt)
		}
		return a.Memory.ID < b.Memory.ID
	})
	return out
}

func providerOf(m memory.Memory) string {
	for _, value := range []string{m.Provider, m.Type, m.Source} {
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			return value
		}
	}
	return ""
}

func supported(provider string) bool {
	switch provider {
	case "gmail", "imessage", "whatsapp", "calendar", "applecalendar":
		return true
	default:
		return false
	}
}

func occurredAt(m memory.Memory) (*time.Time, bool) {
	if m.Meta == nil {
		return nil, false
	}
	value, ok := m.Meta["occurred_at"].(string)
	if !ok {
		return nil, false
	}
	return parseTime(value)
}

func newestAt(rows []segments.Row) (*time.Time, bool) {
	var newest *time.Time
	var newestRef string
	for _, row := range rows {
		at, ok := parseTime(row.At)
		if !ok {
			continue
		}
		// Ref makes selection stable even when malformed input has equal clocks.
		if newest == nil || at.After(*newest) || (at.Equal(*newest) && row.EvidenceRef < newestRef) {
			newest, newestRef = at, row.EvidenceRef
		}
	}
	return newest, newest != nil
}

func participation(rows []segments.Row, group *bool) *Participation {
	p := &Participation{MessageEvidenceCount: len(rows), IsGroup: group}
	var own int
	var latest *time.Time
	var latestRef string
	var lastOwnRef string
	for _, row := range rows {
		at, ok := parseTime(row.At)
		if !ok { // defensive: segments currently validates these for conversations.
			continue
		}
		if p.LatestSender == "" || latest == nil || at.After(*latest) || (at.Equal(*latest) && row.EvidenceRef < latestRef) {
			p.LatestSender, latest, latestRef = row.Sender, at, row.EvidenceRef
		}
		if segments.Direction(row.BlockRefs) != "outgoing" {
			continue
		}
		own++
		if p.LastOwnAt == nil || at.After(*p.LastOwnAt) || (at.Equal(*p.LastOwnAt) && row.EvidenceRef < lastOwnRef) {
			copy := *at
			p.LastOwnAt = &copy
			lastOwnRef = row.EvidenceRef
		}
	}
	p.OwnShare = float64(own) / float64(p.MessageEvidenceCount)
	return p
}

func explicitGroup(m memory.Memory, provider string) *bool {
	if m.Meta == nil {
		return nil
	}
	if provider == "imessage" {
		if value, ok := m.Meta["is_group"].(bool); ok {
			return &value
		}
		return nil
	}
	if value, ok := m.Meta["chat_kind"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "group":
			v := true
			return &v
		case "direct":
			v := false
			return &v
		}
	}
	return nil
}

func parseTime(value string) (*time.Time, bool) {
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return nil, false
	}
	return &at, true
}
