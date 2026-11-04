package corrections

import "github.com/pyranthus-hq/mora/internal/sessions"

// Correction kinds.
const (
	KindRejected    = "rejected"
	KindInterrupted = "interrupted"
)

// Reasons a denial is not a correction. Each is its JSON name in not_counted.
const (
	CancelledSiblings     = "cancelled_siblings"
	BlockedByRule         = "blocked_by_rule"
	BlockedByAutoMode     = "blocked_by_auto_mode"
	InterruptedInSubagent = "interrupted_in_subagent"
	InterruptedNotHuman   = "interrupted_not_human"
	UnknownDenialKinds    = "unknown_denial_kinds"
)

// Classify is section 3.7 of the spec as a pure function, for the rows that
// reach a Denial. It reads typed fields only. It returns a kind for a correction
// and a notCounted reason for anything else, never both.
func Classify(d sessions.Denial) (kind, notCounted string) {
	switch d.Kind {
	case "user-rejected":
		return KindRejected, ""
	case "interrupted":
		switch {
		case d.Subagent:
			return "", InterruptedInSubagent
		case d.Shutdown || d.NextOrigin == "peer" || d.NextOrigin == "task_notification":
			return "", InterruptedNotHuman
		}
		return KindInterrupted, ""
	case "cancelled":
		return "", CancelledSiblings
	case "permission-rule":
		return "", BlockedByRule
	case "automode-blocked", "automode-unavailable", "automode-parsing-error":
		return "", BlockedByAutoMode
	}
	return "", UnknownDenialKinds
}
