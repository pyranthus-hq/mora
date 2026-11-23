package imessage

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRenderBodyRetainsFirstFittingSuffix(t *testing.T) {
	messages := []renderMessage{
		{date: localDate(2026, 5, 28, 9, 0), kind: msgSkip},
		{date: localDate(2026, 5, 28, 9, 0), fromMe: true, text: strings.Repeat("界", 300)},
		{date: localDate(2026, 5, 29, 9, 0), fromMe: true, text: "Next day 👋\n\n"},
		{date: localDate(2026, 5, 29, 9, 0), fromMe: true, text: "Same timestamp"},
		{date: localDate(2026, 5, 30, 9, 0), kind: msgSkip},
	}
	full := renderTranscript(messages, nil)
	originalSize := utf8.RuneCountInString(full)
	for budget := 1; budget < originalSize; budget++ {
		start := 0
		for start < len(messages)-1 && utf8.RuneCountInString(withMarker(renderTranscript(messages[start:], nil))) > budget {
			start++
		}
		want := withMarker(renderTranscript(messages[start:], nil))
		body, result := renderBody(messages, nil, budget)
		if body != want || !result.Truncated || result.OriginalSize != originalSize ||
			result.IngestedSize != utf8.RuneCountInString(want) || len(result.retained) != len(messages)-start {
			t.Fatalf("budget %d: expected suffix at %d, got %d retained messages", budget, start, len(result.retained))
		}
	}
}
