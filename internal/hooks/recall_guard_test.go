package hooks

import (
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestFilterRecallQueryDropsGenericVerbs(t *testing.T) {
	got := FilterRecallQuery("Should we use pnpm or npm, and is the dev server port 4321?")
	for _, banned := range []string{"use", "Should"} {
		for _, w := range strings.Fields(got) {
			if strings.EqualFold(strings.Trim(w, ",?"), banned) {
				t.Fatalf("%q survived in %q", banned, got)
			}
		}
	}
	if !strings.Contains(got, "pnpm") || !strings.Contains(got, "4321?") {
		t.Fatalf("content words were lost: %q", got)
	}
	if len(RecallTokens("please use run set add")) != 0 {
		t.Fatal("an all-generic prompt must have no content tokens")
	}
}

func TestRecallOverlapOK(t *testing.T) {
	const prompt = "Should we use pnpm or npm, and is the dev server port 4321?"
	postgres := memory.Memory{Title: "Use Postgres 16", Text: "We use Postgres 16 for local dev."}
	vitest := memory.Memory{Title: "Use Vitest", Text: "Run unit tests with vitest, not jest."}
	pnpm := memory.Memory{Title: "Package manager is pnpm", Text: "Use pnpm, not npm."}
	port := memory.Memory{Title: "Dev port", Text: "Local port is 4321."}
	rareOnly := func(words ...string) func(string) bool {
		return func(w string) bool {
			for _, x := range words {
				if x == w {
					return true
				}
			}
			return false
		}
	}
	cases := []struct {
		name   string
		m      memory.Memory
		prompt string
		rare   func(string) bool
		want   bool
	}{
		{"one shared everyday word is not enough", postgres, prompt, nil, false},
		{"no shared word", vitest, prompt, nil, false},
		{"two shared words", pnpm, prompt, nil, true},
		{"one word that is in the title", port, prompt, nil, true},
		{"one body-only word that is rare in the index", memory.Memory{Title: "Package manager", Text: "Use pnpm, not npm."}, "Can I just use npm here?", rareOnly("npm"), true},
		{"the same word when it is not rare", memory.Memory{Title: "Package manager", Text: "Use pnpm, not npm."}, "Can I just use npm here?", rareOnly(), false},
		{"long generic word is not enough", memory.Memory{Title: "Use Postgres 16", Text: "This project's database is Postgres."}, "Something is broken in the project, can you look", rareOnly(), false},
		{"long generic word, another note", memory.Memory{Title: "Use Vitest", Text: "Test every function with vitest."}, "explain what this function returns", rareOnly(), false},
		{"title word of a vitest prompt", vitest, "Should we set up vitest for this repo?", nil, true},
		{"title word of a pnpm-or-yarn prompt", pnpm, "Is it pnpm or yarn in this repo?", nil, true},
		{"two short tokens, one body hit", memory.Memory{Text: "dev environment notes"}, "dev port please", nil, false},
		{"one token prompt, one hit", memory.Memory{Text: "the dev box"}, "what about dev?", nil, true},
		{"plural folds", memory.Memory{Text: "tentacle design and sync"}, "tentacles sync design", nil, true},
		{"all generic prompt", pnpm, "please use run set add", nil, false},
		{"acronym IT survives", memory.Memory{Title: "IT policy", Text: "Laptops are rotated yearly."}, "How does IT handle backups here", nil, true},
		{"lower case it is a stopword", memory.Memory{Title: "IT policy", Text: "Laptops are rotated yearly."}, "how does it handle backups here", nil, false},
	}
	for _, c := range cases {
		if got := RecallOverlapOK(c.m, c.prompt, c.rare); got != c.want {
			t.Errorf("%s: RecallOverlapOK = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSteppedInMarkdown(t *testing.T) {
	if SteppedInMarkdown(0) != "" || !strings.Contains(SteppedInMarkdown(1), "1 lesson") || !strings.Contains(SteppedInMarkdown(3), "3 lessons") {
		t.Fatal("SteppedInMarkdown wording")
	}
}
