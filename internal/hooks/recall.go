package hooks

import (
	"fmt"
	"github.com/pyranthus-hq/mora/internal/memory"
	"strings"
	"time"
	"unicode/utf8"
)

const RecallLimit = 3
const RecallByteLimit = 800

func SkipRecallPrompt(prompt string) bool {
	if utf8.RuneCountInString(prompt) < 12 {
		return true
	}
	if strings.HasPrefix(strings.TrimLeft(prompt, " \t\r\n"), "/") {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(prompt)) {
	case "yes", "no", "ok", "y", "n", "continue", "go", "k":
		return true
	default:
		return false
	}
}
func FormatRecallContext(mems []memory.Memory, threshold float64, now time.Time) string {
	var b strings.Builder
	count := 0
	for _, m := range mems {
		if count >= RecallLimit {
			break
		}
		if m.Score > threshold {
			continue
		}
		line := RecallLine(m, now)
		if line == "" {
			continue
		}
		nextLen := b.Len() + len(line)
		if b.Len() == 0 {
			nextLen += len("[Mora recall]\n")
		} else {
			nextLen++
		}
		if nextLen > RecallByteLimit {
			break
		}
		if b.Len() == 0 {
			b.WriteString("[Mora recall]\n")
		} else {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		count++
	}
	return b.String()
}
func RecallLine(m memory.Memory, now time.Time) string {
	snippet := strings.Join(strings.Fields(m.Text), " ")
	if snippet == "" {
		snippet = strings.TrimSpace(m.Title)
	}
	if snippet == "" {
		return ""
	}
	snippet = ClipRunes(snippet, 180)
	title := strings.TrimSpace(m.Title)
	if title == "" {
		title = m.ID
	}
	provenance := m.Source
	if provenance == "" {
		provenance = "memory"
	}
	if m.Scope != "" {
		provenance += "/" + m.Scope
	}
	return fmt.Sprintf("- %s [%s, age: %s, id: %s]: %s", title, provenance, MemoryAge(m.CreatedAt, now), m.ID, snippet)
}
func ClipRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	var b strings.Builder
	count := 0
	for _, r := range s {
		if count >= limit {
			break
		}
		b.WriteRune(r)
		count++
	}
	return strings.TrimSpace(b.String()) + "..."
}
func MemoryAge(createdAt string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return "unknown"
	}
	if t.After(now) {
		return "in the future"
	}
	days := int(now.Sub(t).Hours() / 24)
	switch days {
	case 0:
		return "today"
	case 1:
		return "1d"
	default:
		return fmt.Sprintf("%dd", days)
	}
}
func PrependBanner(banner, body string) string {
	switch {
	case banner == "":
		return body
	case body == "":
		return banner
	default:
		return banner + "\n" + body
	}
}
