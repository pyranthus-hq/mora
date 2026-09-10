// Package disposition selects explicit, local authored corrections without
// changing target visibility or interpreting the correction's prose.
package disposition

import (
	"fmt"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// Key includes scope so a wide read cannot attach another scope's correction.
type Key struct{ Scope, ID string }

func Valid(value string) bool {
	switch value {
	case "not-context", "keep", "done", "outdated":
		return true
	}
	return false
}

// ValidateFields permits target-only notes, but never an untargeted disposition.
// The empty pair is the legacy ordinary-write path.
func ValidateFields(target, value string) error {
	if target != "" && strings.TrimSpace(target) != target {
		return fmt.Errorf("target must be an exact memory id")
	}
	if value != "" && !Valid(value) {
		return fmt.Errorf("invalid disposition %q: expected not-context, keep, done, or outdated", value)
	}
	if value != "" && target == "" {
		return fmt.Errorf("disposition requires target")
	}
	return nil
}

// ValidateTarget is called before publication and again at proposal approval.
// Target must be a visible local record supplied by the caller's governed read.
func ValidateTarget(correction, target memory.Memory) error {
	id, ok := correction.Meta["target"].(string)
	if !ok || id == "" {
		return fmt.Errorf("correction requires target")
	}
	if target.ID != id || target.DeletedAt != "" || target.Owner != "" {
		return fmt.Errorf("target must be an existing local memory")
	}
	if correction.Scope != target.Scope {
		return fmt.Errorf("target must be in the correction's scope")
	}
	return nil
}

// Project consumes visible vault records only. Pending proposals are not vault
// records and must never be supplied. Connector/provider metadata cannot become
// authored correction authority. Target-only notes do not clear older verdicts.
// Equal instants choose the lexically larger correction ID, independent of order.
func Project(records []memory.Memory, now time.Time) map[Key]memory.Disposition {
	targets := make(map[Key]memory.Memory, len(records))
	for _, m := range records {
		if m.Owner == "" && m.DeletedAt == "" && m.ID != "" {
			targets[Key{m.Scope, m.ID}] = m
		}
	}
	out := map[Key]memory.Disposition{}
	times := map[Key]time.Time{}
	for _, c := range records {
		if c.Type != "correction" || c.Provider != "" || c.ProviderID != "" || !IsLocalAuthored(c) || c.Owner != "" || c.DeletedAt != "" || c.ID == "" {
			continue
		}
		id, targetOK := c.Meta["target"].(string)
		value, valueOK := c.Meta["disposition"].(string)
		if !targetOK || !valueOK || id == "" || !Valid(value) || ValidateFields(id, value) != nil {
			continue
		}
		key := Key{c.Scope, id}
		target, exists := targets[key]
		if !exists || ValidateTarget(c, target) != nil {
			continue
		}
		at, err := time.Parse(time.RFC3339, c.CreatedAt)
		if err != nil || at.After(now) {
			continue
		}
		targetAt, err := time.Parse(time.RFC3339, target.CreatedAt)
		if err != nil || at.Before(targetAt) {
			continue
		}
		previous, exists := out[key]
		if exists && (at.Before(times[key]) || at.Equal(times[key]) && c.ID <= previous.CorrectionID) {
			continue
		}
		out[key] = memory.Disposition{Value: value, CorrectionID: c.ID, At: c.CreatedAt}
		times[key] = at
	}
	return out
}

// IsLocalAuthored rejects connector-shaped records from correction authority.
// Ordinary write sources are unconstrained; this predicate is only for typed corrections.
func IsLocalAuthored(m memory.Memory) bool {
	if m.Provider != "" || m.ProviderID != "" || m.Owner != "" {
		return false
	}
	source := strings.ToLower(strings.TrimSpace(m.Source))
	family, _, _ := strings.Cut(source, ":")
	switch family {
	case "gmail", "imessage", "whatsapp", "calendar", "applecalendar", "applecal", "github":
		return false
	default:
		return true
	}
}
