package mora

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBriefSectionElisionContract(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Now()

	enableSources(t, cfg, "imessage", "gmail")
	seedSyncStatus(t, cfg, "imessage", now.Add(-1*time.Hour))
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))

	for i := 0; i < 10; i++ {
		digestSeed(t, cfg, "imessage", fmt.Sprintf("Text %d", i), time.Duration(i+1)*time.Minute, now)
	}

	tests := []struct {
		name                 string
		maxTokens            int
		wantItemsNonNull     bool
		wantTruncated        bool
		wantElidedByBudget   int
		wantEmptyExplanation string
	}{
		{
			name:                 "tight_budget_elides_all_items",
			maxTokens:            100,
			wantItemsNonNull:     true,
			wantTruncated:        true,
			wantElidedByBudget:   10,
			wantEmptyExplanation: "all items elided by token budget",
		},
		{
			name:                 "normal_budget_surfaces_items",
			maxTokens:            6000,
			wantItemsNonNull:     true,
			wantTruncated:        false,
			wantElidedByBudget:   0,
			wantEmptyExplanation: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := callMCPTool(context.Background(), "brief", map[string]any{
				"max_tokens": float64(tt.maxTokens),
			})
			if err != nil {
				t.Fatalf("callMCPTool brief: %v", err)
			}
			sc, ok := res.(map[string]any)
			if !ok {
				t.Fatalf("expected map result, got %T", res)
			}

			sections, ok := sc["sections"].([]DigestSection)
			if !ok {
				t.Fatalf("expected []DigestSection, got %T", sc["sections"])
			}

			var imsgSec *DigestSection
			for i := range sections {
				if sections[i].Source == "imessage" {
					imsgSec = &sections[i]
					break
				}
			}
			if imsgSec == nil {
				t.Fatalf("imessage section missing")
			}

			if tt.wantItemsNonNull && imsgSec.Items == nil {
				t.Errorf("imessage section Items is nil, want non-nil slice []")
			}

			b, err := json.Marshal(imsgSec)
			if err != nil {
				t.Fatalf("marshal section: %v", err)
			}
			jsonStr := string(b)
			if imsgSec.Items != nil && len(imsgSec.Items) == 0 {
				if !strings.Contains(jsonStr, `"items":[]`) {
					t.Errorf("marshaled JSON = %s, want containing \"items\":[]", jsonStr)
				}
			}

			if imsgSec.Truncated != tt.wantTruncated {
				t.Errorf("Truncated = %v, want %v", imsgSec.Truncated, tt.wantTruncated)
			}

			if imsgSec.ElidedByBudget != tt.wantElidedByBudget {
				t.Errorf("ElidedByBudget = %d, want %d", imsgSec.ElidedByBudget, tt.wantElidedByBudget)
			}

			emptyExp, _ := sc["empty_explanation"].(string)
			if emptyExp != tt.wantEmptyExplanation {
				t.Errorf("empty_explanation = %q, want %q", emptyExp, tt.wantEmptyExplanation)
			}
		})
	}
}

func TestEmptyBriefExplanations(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Now()

	t.Run("no_changes_steady_state", func(t *testing.T) {
		enableSources(t, cfg, "gmail")
		seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
		m := Memory{ID: "manual_01", Scope: "global", Type: "note", Title: "Note 01", Text: "Note body", Source: "manual", CreatedAt: now.Add(-48 * time.Hour).Format(time.RFC3339)}
		if err := writeMemory(cfg, m); err != nil {
			t.Fatalf("writeMemory: %v", err)
		}

		if err := saveBriefSnapshot(cfg, briefSnapshot{
			Key:   "gmail",
			Items: map[string]string{},
		}, now); err != nil {
			t.Fatalf("saveBriefSnapshot: %v", err)
		}

		res, err := callMCPTool(context.Background(), "brief", map[string]any{})
		if err != nil {
			t.Fatalf("callMCPTool brief: %v", err)
		}
		sc := res.(map[string]any)
		if got := sc["empty_explanation"]; got != "no changes since last brief" {
			t.Errorf("empty_explanation = %v, want %q", got, "no changes since last brief")
		}
	})

	t.Run("empty_vault", func(t *testing.T) {
		withTempHome(t)
		run(t, "init")
		cfgEmpty := mustConfig(t)

		d, err := buildDigest(cfgEmpty, now, briefOpts{})
		if err != nil {
			t.Fatalf("buildDigest: %v", err)
		}
		if d.EmptyExplanation != "no memory items found in vault" {
			t.Errorf("EmptyExplanation = %q, want %q", d.EmptyExplanation, "no memory items found in vault")
		}
	})

	t.Run("stale_or_unavailable", func(t *testing.T) {
		withTempHome(t)
		run(t, "init")
		cfg2 := mustConfig(t)
		enableSources(t, cfg2, "gmail")
		seedSyncStatus(t, cfg2, "gmail", now.Add(-72*time.Hour)) // stale > 48h

		d, err := buildDigest(cfg2, now, briefOpts{})
		if err != nil {
			t.Fatalf("buildDigest: %v", err)
		}
		if d.EmptyExplanation != "all source connectors are stale or unavailable" {
			t.Errorf("EmptyExplanation = %q, want %q", d.EmptyExplanation, "all source connectors are stale or unavailable")
		}
	})

	t.Run("filtered_no_matches", func(t *testing.T) {
		withTempHome(t)
		run(t, "init")
		cfg3 := mustConfig(t)
		enableSources(t, cfg3, "gmail")
		seedSyncStatus(t, cfg3, "gmail", now.Add(-1*time.Hour))
		digestSeed(t, cfg3, "gmail", "Email subject", 1*time.Hour, now)

		d, err := buildDigest(cfg3, now, briefOpts{scope: "nonexistent_scope"})
		if err != nil {
			t.Fatalf("buildDigest: %v", err)
		}
		if d.EmptyExplanation != "no memory items match the active filters" {
			t.Errorf("EmptyExplanation = %q, want %q", d.EmptyExplanation, "no memory items match the active filters")
		}
	})
}

func TestMCPBriefEmptyColdStartBaselineRoundTrip(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 7, 30, 16, 0, 0, 0, time.UTC)

	oldClock := briefClock
	briefClock = func() time.Time { return now }
	t.Cleanup(func() { briefClock = oldClock })

	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-time.Hour))
	digestSeed(t, cfg, "gmail", "Outside cold-start window", 8*24*time.Hour, now)

	payload := mcpRoundTripPayload(t, "brief", `{}`)
	const want = "no memory items found in initial 7-day baseline window"
	got, ok := payload["empty_explanation"].(string)
	if !ok || got != want {
		t.Fatalf("empty_explanation = %v, want %q; payload=%v", got, want, payload)
	}
	if strings.Contains(got, "since last brief") {
		t.Fatalf("cold-start explanation falsely implies a prior brief: %q", got)
	}
}

func TestMCPBriefFilteredEmptyExplanationSurvivesFallback(t *testing.T) {
	fixedNow := time.Date(2026, 7, 30, 16, 0, 0, 0, time.UTC)

	tests := []struct {
		name                 string
		args                 string
		setup                func(t *testing.T, cfg Config)
		wantEmptyExplanation string
	}{
		{
			name: "genuine_filter_miss",
			args: `{"scope":"missing"}`,
			setup: func(t *testing.T, cfg Config) {
				digestSeed(t, cfg, "gmail", "Wrong scope", 48*time.Hour, fixedNow)
			},
			wantEmptyExplanation: "no memory items match the active filters",
		},
		{
			name: "matching_cold_start_row_outside_courtesy_window",
			args: `{"scope":"global"}`,
			setup: func(t *testing.T, cfg Config) {
				digestSeed(t, cfg, "gmail", "Old matching row", 8*24*time.Hour, fixedNow)
			},
			wantEmptyExplanation: "no memory items found in initial 7-day baseline window",
		},
		{
			name: "matching_row_unchanged_since_prior_brief",
			args: `{"scope":"global"}`,
			setup: func(t *testing.T, cfg Config) {
				m := digestSeed(t, cfg, "gmail", "Unchanged matching row", 48*time.Hour, fixedNow)
				if err := saveBriefSnapshot(cfg, briefSnapshot{
					Key:   "gmail",
					Items: map[string]string{m.ID: m.ContentHash},
				}, fixedNow.Add(-time.Hour)); err != nil {
					t.Fatalf("saveBriefSnapshot: %v", err)
				}
			},
			wantEmptyExplanation: "no changes since last brief",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			cfg := mustConfig(t)

			oldClock := briefClock
			briefClock = func() time.Time { return fixedNow }
			t.Cleanup(func() { briefClock = oldClock })

			enableSources(t, cfg, "gmail")
			seedSyncStatus(t, cfg, "gmail", fixedNow.Add(-time.Hour))
			tt.setup(t, cfg)

			payload := mcpRoundTripPayload(t, "brief", tt.args)
			if got := payload["empty_explanation"]; got != tt.wantEmptyExplanation {
				t.Errorf("empty_explanation = %v, want %q; payload=%v", got, tt.wantEmptyExplanation, payload)
			}
			if got := payload["since_hours"]; got != float64(briefFallbackWindowHours) {
				t.Errorf("since_hours = %v, want %d to prove the empty delta reached the fallback", got, briefFallbackWindowHours)
			}
		})
	}
}

func TestMCPBriefDigestEmptyExplanationRoundTrip(t *testing.T) {
	fixedNow := time.Date(2026, 7, 30, 16, 0, 0, 0, time.UTC)

	type setupFunc func(t *testing.T, cfg Config, now time.Time)
	seedCurrentSnapshot := func(t *testing.T, cfg Config, key string, m Memory, now time.Time) {
		t.Helper()
		if err := saveBriefSnapshot(cfg, briefSnapshot{
			Key:   key,
			Items: map[string]string{m.ID: m.ContentHash},
		}, now.Add(-time.Hour)); err != nil {
			t.Fatalf("saveBriefSnapshot(%s): %v", key, err)
		}
	}
	setupWindowMiss := func(t *testing.T, cfg Config, now time.Time) {
		enableSources(t, cfg, "gmail")
		seedSyncStatus(t, cfg, "gmail", now.Add(-time.Hour))
		digestSeed(t, cfg, "gmail", "Older than window", 2*time.Hour, now)
	}
	setupWindowFilterMiss := func(t *testing.T, cfg Config, now time.Time) {
		enableSources(t, cfg, "gmail")
		seedSyncStatus(t, cfg, "gmail", now.Add(-time.Hour))
		digestSeed(t, cfg, "gmail", "In window wrong scope", 30*time.Minute, now)
	}
	setupSteadyState := func(t *testing.T, cfg Config, now time.Time) {
		enableSources(t, cfg, "gmail")
		seedSyncStatus(t, cfg, "gmail", now.Add(-time.Hour))
		m := digestSeed(t, cfg, "gmail", "Already briefed", 48*time.Hour, now)
		seedCurrentSnapshot(t, cfg, "gmail", m, now)
	}
	setupHealthyFilterMiss := func(t *testing.T, cfg Config, now time.Time) {
		enableSources(t, cfg, "gmail")
		seedSyncStatus(t, cfg, "gmail", now.Add(-time.Hour))
		digestSeed(t, cfg, "gmail", "Healthy filtered item", 30*time.Minute, now)
	}
	setupSourceFilterWithUnrelatedStale := func(t *testing.T, cfg Config, now time.Time) {
		enableSources(t, cfg, "gmail", "imessage")
		seedSyncStatus(t, cfg, "gmail", now.Add(-time.Hour))
		seedSyncStatus(t, cfg, "imessage", now.Add(-72*time.Hour))
		digestSeed(t, cfg, "gmail", "Fresh selected source", 30*time.Minute, now)
	}
	setupAllUncertainFilter := func(t *testing.T, cfg Config, now time.Time) {
		enableSources(t, cfg, "gmail", "imessage")
		seedSyncStatus(t, cfg, "gmail", now.Add(-72*time.Hour))
		// imessage deliberately has no status: unavailable/never synced.
		digestSeed(t, cfg, "gmail", "Stale filtered item", 30*time.Minute, now)
	}
	setupMixedState := func(t *testing.T, cfg Config, now time.Time) {
		enableSources(t, cfg, "gmail", "imessage")
		seedSyncStatus(t, cfg, "gmail", now.Add(-time.Hour))
		seedSyncStatus(t, cfg, "imessage", now.Add(-72*time.Hour))
		m := digestSeed(t, cfg, "gmail", "Already seen mixed", 48*time.Hour, now)
		seedCurrentSnapshot(t, cfg, "gmail", m, now)
		if err := saveBriefSnapshot(cfg, briefSnapshot{
			Key:   "imessage",
			Items: map[string]string{},
		}, now.Add(-time.Hour)); err != nil {
			t.Fatalf("saveBriefSnapshot(imessage): %v", err)
		}
	}
	setupBudgetElision := func(t *testing.T, cfg Config, now time.Time) {
		enableSources(t, cfg, "imessage")
		seedSyncStatus(t, cfg, "imessage", now.Add(-time.Hour))
		digestSeed(t, cfg, "imessage", "Budgeted text", time.Minute, now)
	}

	tests := []struct {
		name                 string
		tool                 string
		args                 string
		setup                setupFunc
		wantEmptyExplanation string
		wantPrompt           bool
		wantElidedSource     string
		wantElidedByBudget   int
	}{
		{
			name:                 "digest_plain_explicit_window",
			tool:                 "digest",
			args:                 `{"since_hours":1}`,
			setup:                setupWindowMiss,
			wantEmptyExplanation: "no memory items found in requested time window",
		},
		{
			name:                 "digest_plain_matching_source_outside_window",
			tool:                 "digest",
			args:                 `{"since_hours":1,"source":"gmail"}`,
			setup:                setupWindowMiss,
			wantEmptyExplanation: "no memory items found in requested time window",
		},
		{
			name:                 "digest_envelope_window_and_filter",
			tool:                 "digest",
			args:                 `{"since_hours":1,"scope":"missing","envelope":true}`,
			setup:                setupWindowFilterMiss,
			wantEmptyExplanation: "no memory items match the active filters in the requested time window",
			wantPrompt:           true,
		},
		{
			name:                 "digest_plain_mixed_fresh_and_stale",
			tool:                 "digest",
			args:                 `{}`,
			setup:                setupMixedState,
			wantEmptyExplanation: "some source connectors are stale or unavailable",
		},
		{
			name:                 "digest_envelope_true_steady_state",
			tool:                 "digest",
			args:                 `{"envelope":true}`,
			setup:                setupSteadyState,
			wantEmptyExplanation: "no changes since last brief",
			wantPrompt:           true,
		},
		{
			name:                 "brief_plain_true_steady_state",
			tool:                 "brief",
			args:                 `{}`,
			setup:                setupSteadyState,
			wantEmptyExplanation: "no changes since last brief",
		},
		{
			name:                 "brief_plain_healthy_filter_miss",
			tool:                 "brief",
			args:                 `{"scope":"missing"}`,
			setup:                setupHealthyFilterMiss,
			wantEmptyExplanation: "no memory items match the active filters",
		},
		{
			name:                 "digest_plain_source_filter_ignores_unrelated_stale_source",
			tool:                 "digest",
			args:                 `{"source":"gmail","scope":"missing"}`,
			setup:                setupSourceFilterWithUnrelatedStale,
			wantEmptyExplanation: "no memory items match the active filters",
		},
		{
			name:                 "brief_envelope_filter_with_stale_and_unavailable_sources",
			tool:                 "brief",
			args:                 `{"scope":"missing","envelope":true}`,
			setup:                setupAllUncertainFilter,
			wantEmptyExplanation: "all source connectors are stale or unavailable",
			wantPrompt:           true,
		},
		{
			name:                 "digest_plain_budget_elision",
			tool:                 "digest",
			args:                 `{"max_tokens":100}`,
			setup:                setupBudgetElision,
			wantEmptyExplanation: "all items elided by token budget",
			wantElidedSource:     "imessage",
			wantElidedByBudget:   1,
		},
		{
			name:                 "digest_envelope_budget_elision",
			tool:                 "digest",
			args:                 `{"max_tokens":100,"envelope":true}`,
			setup:                setupBudgetElision,
			wantEmptyExplanation: "all items elided by token budget",
			wantPrompt:           true,
			wantElidedSource:     "imessage",
			wantElidedByBudget:   1,
		},
		{
			name:                 "brief_plain_budget_elision",
			tool:                 "brief",
			args:                 `{"max_tokens":100}`,
			setup:                setupBudgetElision,
			wantEmptyExplanation: "all items elided by token budget",
			wantElidedSource:     "imessage",
			wantElidedByBudget:   1,
		},
		{
			name:                 "brief_envelope_budget_elision",
			tool:                 "brief",
			args:                 `{"max_tokens":100,"envelope":true}`,
			setup:                setupBudgetElision,
			wantEmptyExplanation: "all items elided by token budget",
			wantPrompt:           true,
			wantElidedSource:     "imessage",
			wantElidedByBudget:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			cfg := mustConfig(t)

			oldClock := briefClock
			briefClock = func() time.Time { return fixedNow }
			t.Cleanup(func() { briefClock = oldClock })

			tt.setup(t, cfg, fixedNow)
			payload := mcpRoundTripPayload(t, tt.tool, tt.args)

			if got := payload["empty_explanation"]; got != tt.wantEmptyExplanation {
				t.Errorf("empty_explanation = %v, want %q; payload=%v", got, tt.wantEmptyExplanation, payload)
			}
			_, hasPrompt := payload["synthesis_prompt"]
			if hasPrompt != tt.wantPrompt {
				t.Errorf("synthesis_prompt present = %v, want %v", hasPrompt, tt.wantPrompt)
			}
			if tt.wantElidedSource != "" {
				section := decodedSectionBySource(t, payload, tt.wantElidedSource)
				items, ok := section["items"].([]any)
				if !ok {
					t.Fatalf("%s items = %T, want decoded [] (not null): %v", tt.wantElidedSource, section["items"], section)
				}
				if len(items) != 0 {
					t.Errorf("%s items len = %d, want 0", tt.wantElidedSource, len(items))
				}
				elided, ok := section["elided_by_budget"].(float64)
				if !ok {
					t.Fatalf("%s elided_by_budget = %T, want number: %v", tt.wantElidedSource, section["elided_by_budget"], section)
				}
				if got := int(elided); got != tt.wantElidedByBudget {
					t.Errorf("%s elided_by_budget = %d, want %d", tt.wantElidedSource, got, tt.wantElidedByBudget)
				}
				more, ok := section["more_count"].(float64)
				if !ok {
					t.Fatalf("%s more_count = %T, want number: %v", tt.wantElidedSource, section["more_count"], section)
				}
				if got := int(more); got != tt.wantElidedByBudget {
					t.Errorf("%s more_count = %d, want %d", tt.wantElidedSource, got, tt.wantElidedByBudget)
				}
				if got, _ := section["truncated"].(bool); !got {
					t.Errorf("%s truncated = %v, want true", tt.wantElidedSource, section["truncated"])
				}
			}
		})
	}
}

// mcpRoundTripPayload drives the real stdio JSON-RPC server and verifies the
// CallToolResult's text and structuredContent mirrors decode to the same object.
func mcpRoundTripPayload(t *testing.T, tool, args string) map[string]any {
	t.Helper()
	res := mcpResult(t, budgetCall(tool, args))
	isError, ok := res["isError"].(bool)
	if !ok || isError {
		t.Fatalf("%s isError = %T %v, want false: %v", tool, res["isError"], res["isError"], res)
	}
	structured, ok := res["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("%s structuredContent = %T, want object: %v", tool, res["structuredContent"], res)
	}
	content, ok := res["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("%s content = %T %v, want non-empty content array", tool, res["content"], res["content"])
	}
	first, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("%s content[0] = %T, want object", tool, content[0])
	}
	text, ok := first["text"].(string)
	if !ok {
		t.Fatalf("%s content[0].text = %T, want string", tool, first["text"])
	}
	var textPayload map[string]any
	if err := json.Unmarshal([]byte(text), &textPayload); err != nil {
		t.Fatalf("%s content text is not JSON: %v\n%s", tool, err, text)
	}
	if !reflect.DeepEqual(structured, textPayload) {
		t.Fatalf("%s structuredContent and text payload differ\nstructured=%v\ntext=%v", tool, structured, textPayload)
	}
	return structured
}

func decodedSectionBySource(t *testing.T, payload map[string]any, source string) map[string]any {
	t.Helper()
	sections, ok := payload["sections"].([]any)
	if !ok {
		t.Fatalf("sections = %T, want decoded array: %v", payload["sections"], payload)
	}
	for _, raw := range sections {
		section, ok := raw.(map[string]any)
		if ok && section["source"] == source {
			return section
		}
	}
	t.Fatalf("section %q missing: %v", source, sections)
	return nil
}
