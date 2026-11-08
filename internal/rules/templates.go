package rules

import "time"

// The compound-cd template is byte-identical to Steward's rule: the match
// from fixed-signatures-v3.json and the message from hook-rules.json, so a
// Mora-native run measures the same treatment.
const (
	compoundCDMatch   = `^\s*cd\s+[^;&|\n]*?\s*(&&|;)`
	compoundCDMessage = "Use one command per Bash call. Set the working folder through the tool instead of chaining cd."
)

// TemplateNames lists the built-in templates.
var TemplateNames = []string{"compound-cd"}

// Template returns a new rule from a built-in template, stamped with now.
func Template(name string, now time.Time) (Rule, bool) {
	switch name {
	case "compound-cd":
		return Rule{
			ID:        "compound-cd",
			Title:     "One command per Bash call",
			CreatedAt: now.UTC().Format(time.RFC3339),
			Source:    "template:compound-cd",
			Message:   compoundCDMessage,
			Spec:      Spec{Version: 1, Tool: "Bash", Match: compoundCDMatch},
		}, true
	}
	return Rule{}, false
}
