package mora

import (
	"context"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"strings"
	"testing"
	"time"
)

func TestContextMemoryRoutesCurrentStateAndOpenLoopQuestions(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	oldClock := briefClock
	briefClock = func() time.Time { return now }
	t.Cleanup(func() { briefClock = oldClock })
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "self@example.com",
		Enabled: genericutil.Ptr(true), CreatedAt: now.Add(-24 * time.Hour).Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	seedSyncStatus(t, cfg, "gmail", now.Add(-30*time.Minute))

	memories := []Memory{
		{
			ID: "project-alpha-recent", Scope: "project:alpha", Type: "decision",
			Title: "Alpha launch path changed", Source: "mcp",
			CreatedAt: now.Add(-2 * time.Hour).Format(time.RFC3339),
			Text:      "We moved the Alpha launch to the signed app path after the review.",
			Decision:  &DecisionValidity{AsOf: now.Add(-2 * time.Hour).Format(time.RFC3339), Durability: "working"},
		},
		{
			ID: "project-alpha-old", Scope: "project:alpha", Type: "fact",
			Title: "Old Alpha plan", Source: "manual",
			CreatedAt: now.Add(-30 * 24 * time.Hour).Format(time.RFC3339),
			Text:      "The old Alpha plan used the standalone binary.",
		},
		{
			ID: "gmail_thread/newsletter", Scope: "global", Type: "email",
			Title: "Your active projects changed this week", Source: "newsletter",
			Provider: "gmail", ProviderID: "newsletter", LastSynced: now.Add(-time.Minute).Format(time.RFC3339),
			CreatedAt: now.Add(-time.Minute).Format(time.RFC3339),
			Text:      "From: Product Newsletter <newsletter@marketing.example>\n\nWhat materially changed across active projects recently? Unsubscribe here.",
			Meta: map[string]any{
				"from": []string{"newsletter@marketing.example"},
				"to":   []string{"self@example.com"},
			},
		},
		{
			ID: "gmail_thread/open-review", Scope: "project:alpha", Type: "email",
			Title: "Review notes request", Source: "open-review", Provider: "gmail", ProviderID: "open-review",
			CreatedAt: now.Add(-90 * time.Minute).Format(time.RFC3339), LastSynced: now.Add(-90 * time.Minute).Format(time.RFC3339),
			Text: "From: Sam <sam@example.org>\n\nCan you send the cedar review notes?",
			Meta: map[string]any{
				"from": []string{"sam@example.org"}, "to": []string{"self@example.com"},
				"messages": []commitmentMessageEvidence{{
					MessageRef: "gmail_thread/open-review#msg-1", Sender: "sam@example.org",
					To: []string{"self@example.com"}, At: now.Add(-90 * time.Minute).Format(time.RFC3339), BlockRefs: []string{"body"},
				}},
			},
		},
		{
			ID: "gmail_thread/closed-review", Scope: "project:alpha", Type: "email",
			Title: "Closed budget request", Source: "closed-review", Provider: "gmail", ProviderID: "closed-review",
			CreatedAt: now.Add(-3 * time.Hour).Format(time.RFC3339), LastSynced: now.Add(-time.Hour).Format(time.RFC3339),
			Text: "From: Lee <lee@example.org>\n\nCan you send the basalt budget?\n\n---\n\nFrom: Self <self@example.com>\n\nI sent the basalt budget.",
			Meta: map[string]any{
				"from": []string{"lee@example.org", "self@example.com"}, "to": []string{"self@example.com", "lee@example.org"},
				"messages": []commitmentMessageEvidence{
					{MessageRef: "gmail_thread/closed-review#msg-1", Sender: "lee@example.org", To: []string{"self@example.com"}, At: now.Add(-3 * time.Hour).Format(time.RFC3339), BlockRefs: []string{"body"}},
					{MessageRef: "gmail_thread/closed-review#msg-2", Sender: "self@example.com", To: []string{"lee@example.org"}, At: now.Add(-time.Hour).Format(time.RFC3339), BlockRefs: []string{"body"}},
				},
			},
		},
		{
			ID: "gmail_thread/open-beta", Scope: "project:beta", Type: "email",
			Title: "Beta release request", Source: "open-beta", Provider: "gmail", ProviderID: "open-beta",
			CreatedAt: now.Add(-20 * time.Minute).Format(time.RFC3339), LastSynced: now.Add(-20 * time.Minute).Format(time.RFC3339),
			Text: "From: Kim <kim@example.org>\n\nCan you send the quartz release notes?",
			Meta: map[string]any{
				"from": []string{"kim@example.org"}, "to": []string{"self@example.com"},
				"messages": []commitmentMessageEvidence{{
					MessageRef: "gmail_thread/open-beta#msg-1", Sender: "kim@example.org",
					To: []string{"self@example.com"}, At: now.Add(-20 * time.Minute).Format(time.RFC3339), BlockRefs: []string{"body"},
				}},
			},
		},
	}
	for _, memory := range memories {
		if err := writeMemory(cfg, memory); err != nil {
			t.Fatalf("write %s: %v", memory.ID, err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	current, err := mcpContextMemory(context.Background(), cfg, map[string]any{
		"query": "What materially changed across my active projects recently?",
	})
	if err != nil {
		t.Fatal(err)
	}
	currentText := current.(map[string]any)["context"].(string)
	projectAt := strings.Index(currentText, "Alpha launch path changed")
	newsletterAt := strings.Index(currentText, "Your active projects changed this week")
	if projectAt < 0 || newsletterAt < 0 || projectAt > newsletterAt {
		t.Fatalf("current-state context did not put recent project evidence ahead of newsletter noise:\n%s", currentText)
	}

	loops, err := mcpContextMemory(context.Background(), cfg, map[string]any{
		"query": "What am I waiting on, what do I owe, and what has closed?",
	})
	if err != nil {
		t.Fatal(err)
	}
	loopText := loops.(map[string]any)["context"].(string)
	var cedarLine string
	for _, line := range strings.Split(loopText, "\n") {
		if strings.Contains(line, "cedar review notes") {
			cedarLine = line
			break
		}
	}
	if !strings.Contains(cedarLine, "[open;") || !strings.Contains(cedarLine, "owed_by_self]") {
		t.Fatalf("open-loop context missed the typed open commitment:\n%s", loopText)
	}
	if strings.Contains(loopText, "basalt budget") || strings.Contains(loopText, "closed-review") {
		t.Fatalf("open-loop context surfaced a closed commitment:\n%s", loopText)
	}
	contracted, err := mcpContextMemory(context.Background(), cfg, map[string]any{
		"query": "What's still open?",
	})
	if err != nil {
		t.Fatal(err)
	}
	if contractedText := contracted.(map[string]any)["context"].(string); !strings.Contains(contractedText, "cedar review notes") {
		t.Fatalf("contraction created a false qualifier:\n%s", contractedText)
	}

	alphaLoops, err := mcpContextMemory(context.Background(), cfg, map[string]any{
		"query": "What do I owe on Alpha?",
	})
	if err != nil {
		t.Fatal(err)
	}
	alphaText := alphaLoops.(map[string]any)["context"].(string)
	if !strings.Contains(alphaText, "cedar review notes") || strings.Contains(alphaText, "quartz release notes") {
		t.Fatalf("qualified open-loop question ignored Alpha:\n%s", alphaText)
	}

	alphaState, err := mcpContextMemory(context.Background(), cfg, map[string]any{
		"query": "What materially changed across Alpha?",
	})
	if err != nil {
		t.Fatal(err)
	}
	alphaStateText := alphaState.(map[string]any)["context"].(string)
	if !strings.Contains(alphaStateText, "Alpha launch path changed") || strings.Contains(alphaStateText, "Beta release request") {
		t.Fatalf("qualified current-state question ignored Alpha:\n%s", alphaStateText)
	}
}

func TestContextIntentDoesNotHijackOrdinaryQueries(t *testing.T) {
	for _, query := range []string{
		"open source search",
		"closed captions",
		"recently read newsletter",
		"project Alpha",
		"Find the email saying what do I owe",
	} {
		if got := contextIntentOf(query); got != contextIntentGeneric {
			t.Fatalf("contextIntentOf(%q) = %q, want generic", query, got)
		}
	}
}
