package mora

import (
	"fmt"
	"strconv"
	"strings"
)

// extractActivitySearchFlags leaves the existing free-text/scope/limit parser
// intact and collects only the additive, explicitly requested retrieval knobs.
func extractActivitySearchFlags(args []string) ([]string, map[string]any, error) {
	args, source, err := extractSourceFlag(args)
	if err != nil {
		return nil, nil, err
	}
	values := map[string]any{}
	if source != "" {
		values["source"] = source
	}
	var rest, exclusions []string
	for i := 0; i < len(args); i++ {
		name, value, equals := strings.Cut(args[i], "=")
		if name != "--event-since-hours" && name != "--dispositions" {
			rest = append(rest, args[i])
			continue
		}
		if !equals {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("%s requires a value", name)
			}
			i++
			value = args[i]
		}
		if name == "--event-since-hours" {
			if _, exists := values["event_since_hours"]; exists {
				return nil, nil, fmt.Errorf("--event-since-hours may only be specified once")
			}
			hours, err := strconv.Atoi(value)
			if err != nil || hours < 1 || hours > 8784 {
				return nil, nil, fmt.Errorf("event-since-hours must be an integer from 1 to 8784")
			}
			values["event_since_hours"] = hours
		} else {
			mode, disposition, ok := strings.Cut(value, ":")
			if !ok || mode != "exclude" || disposition == "" {
				return nil, nil, fmt.Errorf("--dispositions requires exclude:<disposition>")
			}
			exclusions = append(exclusions, disposition)
		}
	}
	if len(exclusions) > 0 {
		values["exclude_dispositions"] = exclusions
	}
	return rest, values, nil
}
