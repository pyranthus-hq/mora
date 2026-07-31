package mora

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestMatchSnippetEmptyQueryFallsBackToHeadClip(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		limit int
	}{
		{
			name:  "ascii",
			text:  "opening context " + strings.Repeat("middle detail ", 20) + "buried tail",
			limit: 48,
		},
		{
			name:  "multiline unicode",
			text:  "Résumé opening\n\n" + strings.Repeat("café notes\t", 20) + "buried tail",
			limit: 52,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchSnippet(tt.text, "", tt.limit)
			want := snippet(tt.text, tt.limit)
			if got != want {
				t.Fatalf("matchSnippet empty-query fallback = %q, want head clip %q", got, want)
			}
		})
	}
}

func TestMCPListMemoryReturnsBudgetedPreviews(t *testing.T) {
	tests := []struct {
		name     string
		rows     int
		wantDrop bool
	}{
		{name: "rows fit aggregate budget", rows: 2, wantDrop: false},
		{name: "aggregate budget drops rows", rows: 60, wantDrop: true},
	}

	type previewWant struct {
		text      string
		truncated bool
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			cfg := mustConfig(t)
			createdAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
			wantByID := make(map[string]previewWant, tt.rows)

			for i := 0; i < tt.rows; i++ {
				id := fmt.Sprintf("list-preview-%02d", i)
				body := "short preview body"
				if i > 0 {
					body = fmt.Sprintf("HEAD-%02d ", i) +
						strings.Repeat("preview filler ", 40) +
						fmt.Sprintf("TAIL-%02d", i)
				}
				flat := strings.Join(strings.Fields(body), " ")
				truncated := utf8.RuneCountInString(flat) > searchSnippetLen
				preview := flat
				if truncated {
					preview = snippet(body, searchSnippetLen)
				}
				wantByID[id] = previewWant{text: preview, truncated: truncated}

				if err := writeMemory(cfg, Memory{
					ID:        id,
					Scope:     "global",
					Type:      "insight",
					Title:     fmt.Sprintf("List preview %02d", i),
					Source:    "test",
					CreatedAt: createdAt.Add(-time.Duration(i) * time.Minute).Format(time.RFC3339),
					Text:      body,
					Meta:      map[string]any{"row": i},
				}); err != nil {
					t.Fatalf("seed row %d: %v", i, err)
				}
			}

			raw, err := mcpListMemory(context.Background(), cfg, map[string]any{"limit": float64(tt.rows)})
			if err != nil {
				t.Fatalf("mcpListMemory: %v", err)
			}
			out, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("mcpListMemory returned %T, want map[string]any", raw)
			}
			memories, ok := out["memories"].([]Memory)
			if !ok {
				t.Fatalf("mcpListMemory memories = %T, want []Memory", out["memories"])
			}
			if len(memories) == 0 {
				t.Fatal("mcpListMemory returned no previews")
			}

			for _, got := range memories {
				want, exists := wantByID[got.ID]
				if !exists {
					t.Fatalf("unexpected preview id %q", got.ID)
				}
				if got.Text != want.text {
					t.Errorf("%s text = %q, want head preview %q", got.ID, got.Text, want.text)
				}
				if got.Truncated != want.truncated {
					t.Errorf("%s Truncated = %v, want %v", got.ID, got.Truncated, want.truncated)
				}
				if got.Meta != nil {
					t.Errorf("%s Meta = %#v, want nil", got.ID, got.Meta)
				}
				if want.truncated && strings.Contains(got.Text, "TAIL-") {
					t.Errorf("%s empty-query preview reached the buried tail: %q", got.ID, got.Text)
				}
			}

			wantDropped := tt.rows - len(memories)
			dropped, hasDropped := out["memories_truncated"].(int)
			if tt.wantDrop {
				if wantDropped <= 0 {
					t.Fatalf("fixture did not exceed aggregate budget: rows=%d kept=%d", tt.rows, len(memories))
				}
				if !hasDropped || dropped != wantDropped {
					t.Fatalf("memories_truncated = %v, want %d", out["memories_truncated"], wantDropped)
				}
			} else {
				if wantDropped != 0 {
					t.Fatalf("rows should fit aggregate budget: rows=%d kept=%d", tt.rows, len(memories))
				}
				if hasDropped {
					t.Fatalf("memories_truncated must be absent when no rows drop, got %d", dropped)
				}
			}
		})
	}
}
