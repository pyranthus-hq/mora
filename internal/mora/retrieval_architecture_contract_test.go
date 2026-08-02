package mora

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetrievalArchitectureDescribesMergedSegmentSurface(t *testing.T) {
	t.Parallel()

	docs := map[string]struct {
		required  []string
		forbidden []string
	}{
		"00-overview.md": {
			required: []string{
				"static keyword surface (parent FTS + bounded Gmail message-segment FTS)",
				"fuses its four arms",
			},
			forbidden: []string{
				"routes to **FTS-only under the static-hash embedder**",
				"How hybrid fuses its three arms",
			},
		},
		"01-data-model-and-storage.md": {
			required: []string{
				"static keyword surface (parent FTS plus bounded Gmail message-segment FTS)",
			},
			forbidden: []string{
				"Under the default static-hash embedder search is FTS-only",
			},
		},
		"02-retrieval-search.md": {
			required: []string{
				"at most `pool` distinct parent memories",
				"final-result evidence completion",
			},
			forbidden: []string{
				"The segment query retains its full ordered parent list",
			},
		},
		"06-mcp-server.md": {
			required: []string{
				"else the static keyword surface (parent FTS plus bounded Gmail message-segment FTS)",
				"static keyword path (`searchMemories`",
			},
			forbidden: []string{
				"Hybrid only when a semantic embedder is active, else FTS-only",
				"the FTS-only path (`searchMemories`",
			},
		},
		"07-synthesis-think-digest.md": {
			required: []string{
				"under static-hash the vector arm is omitted",
			},
			forbidden: []string{
				"under the static-hash embedder the vector arm is empty and harmless",
				"the vector arm is empty and harmless under static-hash",
				"hybrid's vector arm is harmless under static-hash here",
			},
		},
		"09-eval-and-testing.md": {
			required: []string{
				"`rFTS >= 0 || rSegment >= 0`",
				"`rFTS >= 0 || rVec >= 0 || rGraph >= 0 || rSegment >= 0`",
				"Hybrid uses FTS∨Vec∨Graph∨Segment",
			},
			forbidden: []string{
				"For the FTS surface, `foundByAnyArm` is `rFTS >= 0` only.",
				"Hybrid uses FTS∨Vec∨Graph (`eval_test.go:237-238`).",
			},
		},
		"15-concurrency-contract.md": {
			required: []string{
				"static keyword surface (parent FTS plus bounded Gmail message-segment FTS)",
			},
			forbidden: []string{
				"where `defaultSearch` is FTS-only",
			},
		},
	}

	for name, contract := range docs {
		name, contract := name, contract
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join("..", "..", "docs", "architecture", name)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			text := string(body)
			for _, want := range contract.required {
				if !strings.Contains(text, want) {
					t.Errorf("missing merged retrieval contract %q", want)
				}
			}
			for _, stale := range contract.forbidden {
				if strings.Contains(text, stale) {
					t.Errorf("stale pre-segment retrieval claim remains: %q", stale)
				}
			}
		})
	}
}
