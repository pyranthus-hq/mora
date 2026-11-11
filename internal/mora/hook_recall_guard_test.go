package mora

import (
	"fmt"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// seedRecallVault builds a vault shaped like a real one: a few rules plus
// enough ordinary notes that common words ("project", "function", "dev") are
// common in the index and a word like "npm" is not.
func seedRecallVault(t *testing.T) Config {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	notes := []Memory{
		{Title: "Use Postgres 16", Text: "We use Postgres 16 for local dev. This project's database is Postgres."},
		{Title: "Use Vitest", Text: "Run unit tests for every function with vitest, not jest."},
		{Title: "Package manager is pnpm", Text: "Use pnpm, not npm."},
		// Unreviewed agent notes never ride recall, even when they match.
		{Title: "Tentative port", Text: "The repository dev server uses npm and port 4321.", Tags: []string{memory.TentativeTag}},
	}
	for i := 0; i < 24; i++ {
		text := fmt.Sprintf("Plain note %d about weekly planning.", i)
		if i%2 == 0 {
			text = fmt.Sprintf("Note %d about the project, one function and the dev environment.", i)
		}
		notes = append(notes, Memory{Title: fmt.Sprintf("Filler %d", i), Text: text})
	}
	for _, m := range notes {
		m.ID, m.Scope, m.Type, m.Source, m.CreatedAt = newID(), "global", "decision", "test", "2026-06-01T00:00:00Z"
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// The first blind user asked a pnpm/port question and recall injected the
// unrelated "Use Postgres 16" and "Use Vitest" rules, because the search layer
// ORs every term and "use" (and then "dev") matched.
func TestHookRecallDoesNotInjectUnrelatedRulesOnGenericWords(t *testing.T) {
	seedRecallVault(t)
	got := runHookRecall(t, "Should we use pnpm or npm, and is the dev server port 4321?")
	for _, bad := range []string{"Postgres", "Vitest", "Filler", "Tentative"} {
		if strings.Contains(got, bad) {
			t.Fatalf("unrelated %s note was injected:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "Package manager is pnpm") {
		t.Fatalf("the related pnpm note must still be recalled:\n%s", got)
	}
}

func TestHookRecallGenericVerbAloneInjectsNothing(t *testing.T) {
	seedRecallVault(t)
	if got := runHookRecall(t, "please use it and run it"); got != "" {
		t.Fatalf("a prompt of generic verbs must inject nothing, got:\n%s", got)
	}
}

// Prompts a person really types. The first three have a confirmed note that
// answers them and must recall it; the last two only share a generic word with
// a rule and must inject nothing.
func TestHookRecallRealPrompts(t *testing.T) {
	seedRecallVault(t)
	cases := []struct {
		prompt string
		want   string // substring that must appear; "" means nothing unrelated may be injected
	}{
		{"Can I just use npm here?", "Package manager is pnpm"},
		{"Should we set up vitest for this repo?", "Use Vitest"},
		{"Is it pnpm or yarn in this repo?", "Package manager is pnpm"},
		{"Something is broken in the project, can you take a look", ""},
		{"explain what this function returns", ""},
	}
	for _, c := range cases {
		got := runHookRecall(t, c.prompt)
		if c.want == "" {
			for _, bad := range []string{"Postgres", "Vitest", "Filler"} {
				if strings.Contains(got, bad) {
					t.Errorf("%q recalled unrelated %s:\n%s", c.prompt, bad, got)
				}
			}
		} else if !strings.Contains(got, c.want) {
			t.Errorf("%q must recall %q, got:\n%s", c.prompt, c.want, got)
		}
	}
}

// An all-caps acronym is a real word, not the stopword "it".
func TestHookRecallKeepsAcronymTokens(t *testing.T) {
	cfg := seedRecallVault(t)
	if err := writeMemory(cfg, Memory{ID: newID(), Scope: "global", Type: "decision", Source: "test", CreatedAt: "2026-06-01T00:00:00Z",
		Title: "IT policy", Text: "Laptops are rotated yearly."}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
		t.Fatal(err)
	}
	if got := runHookRecall(t, "What does IT say about laptop rotation?"); !strings.Contains(got, "IT policy") {
		t.Fatalf("IT must count as a content token:\n%s", got)
	}
}

// hookRecallSearch must not depend on the search layer's term policy: even a
// result set that contains a zero-overlap row is filtered here.
func TestHookRecallSearchDropsZeroOverlapRows(t *testing.T) {
	cfg := seedRecallVault(t)
	mems, err := hookRecallSearch(testCtx(t), cfg, "Should we use pnpm or npm, and is the dev server port 4321?", "", 12)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range mems {
		if strings.Contains(m.Title, "Postgres") || strings.Contains(m.Title, "Vitest") || strings.Contains(m.Title, "Filler") {
			t.Fatalf("zero-overlap row survived hookRecallSearch: %s", m.Title)
		}
	}
}

// A small vault says nothing about rarity, and the lookup never errors.
func TestRecallRareFuncSmallCorpusAnswersNotRare(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := writeMemory(cfg, Memory{ID: newID(), Scope: "global", Type: "note", Source: "test", CreatedAt: "2026-06-01T00:00:00Z", Title: "A", Text: "npm"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
		t.Fatal(err)
	}
	rare, closeRare := recallRareFunc(testCtx(t), cfg)
	defer closeRare()
	if rare("npm") {
		t.Fatal("a one-note vault cannot make a word rare")
	}
}
