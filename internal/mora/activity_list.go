package mora

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/pyranthus-hq/mora/internal/activity"
	"github.com/pyranthus-hq/mora/internal/memory"
)

const maxActivityHours = 24 * 366

// parseActivityHours accepts the canonical new event window and the list-only
// alias since_hours. Search's existing since_hours retains its write-time meaning.
func parseActivityHours(args map[string]any, allowAlias bool, now time.Time) (int, error) {
	value, exists := args["event_since_hours"]
	if alias, ok := args["since_hours"]; ok && allowAlias {
		if exists {
			return 0, fmt.Errorf("event_since_hours and since_hours cannot be combined on list_memory")
		}
		value, exists = alias, true
	}
	if !exists {
		return 0, nil
	}
	return boundedActivityInt(value, maxActivityHours, "event_since_hours")
}

// boundedActivityInt validates JSON numbers without accepting numeric strings,
// truncating fractions, or converting an overflowing number to an integer.
func boundedActivityInt(value any, maximum int, name string) (int, error) {
	var number float64
	switch v := value.(type) {
	case float64:
		number = v
	case int:
		number = float64(v)
	case int64:
		number = float64(v)
	case json.Number:
		var err error
		number, err = v.Float64()
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer from 1 to %d", name, maximum)
		}
	default:
		return 0, fmt.Errorf("%s must be an integer from 1 to %d", name, maximum)
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number < 1 || number > float64(maximum) || math.Trunc(number) != number {
		return 0, fmt.Errorf("%s must be an integer from 1 to %d", name, maximum)
	}
	return int(number), nil
}

func selectActivityEvents(items []Memory, now time.Time, hours, limit int) []Memory {
	selected := activity.SelectRange(items, now.Add(-time.Duration(hours)*time.Hour), now)
	if limit > 0 && len(selected) > limit {
		selected = selected[:limit]
	}
	out := make([]Memory, 0, len(selected))
	for _, row := range selected {
		m := row.Memory
		m.EventAt = row.Projection.EventAt.UTC().Format(time.RFC3339Nano)
		m.Participation = row.Projection.Participation
		m.Automated = &memory.NullableBool{Value: row.Projection.Automated}
		out = append(out, m)
	}
	return out
}

// listActivityMemories keeps source eligibility ahead of the event sort/limit.
// The default path deliberately remains the legacy write-time browse contract.
func listActivityMemories(cfg Config, scope string, limit int, filter searchFilters, hours int, now time.Time) ([]Memory, error) {
	readLimit := limit
	if hours > 0 {
		if limit < 1 || limit > 1000 {
			return nil, fmt.Errorf("event list limit must be 1-1000")
		}
		readLimit = 0
	}
	rows, err := listMemories(cfg, scope, readLimit, filter)
	if err != nil || hours == 0 {
		return rows, err
	}
	return recentSourceEvents(rows, now, hours, limit), nil
}
