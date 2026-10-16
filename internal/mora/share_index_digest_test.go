package mora

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestShareIndexDigestIsDeterministicAcrossGenerations: two builds of one
// corpus into two different generations produce byte-identical indexes. The
// index once stored each row's absolute, run-private generation path, which
// made every build's digest unique.
func TestShareIndexDigestIsDeterministicAcrossGenerations(t *testing.T) {
	cfg := packetHConfig(t)
	var entries []shareBlobEntry
	for i := 0; i < 20; i++ {
		m := fixtureMemory(fmt.Sprintf("mem_20260716_0000%02d_aaaaaaaa", i), fmt.Sprintf("Title %d", i), fmt.Sprintf("body %d alpha", i))
		body, err := renderMemory(m)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, shareBlobEntry{mem: m, body: body})
	}
	c1, i1, err := buildShareGenerationFromEntries(context.Background(), cfg, "acme", "gen-run_one", entries)
	if err != nil {
		t.Fatal(err)
	}
	c2, i2, err := buildShareGenerationFromEntries(context.Background(), cfg, "acme", "gen-run_two", entries)
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Fatalf("corpus digests differ for one corpus: %s vs %s", c1, c2)
	}
	if i1 != i2 {
		t.Fatalf("index digests differ for one corpus: %s vs %s", i1, i2)
	}
}

// TestSharedSearchPathNamesTheServedGeneration: the index stores a relative
// path, so search must rebuild the absolute one from the generation it served.
func TestSharedSearchPathNamesTheServedGeneration(t *testing.T) {
	m := fixtureMemory("mem_20260601_000000_aaaaaaaa", "Zanzibar", "zanzibar harbor notes")
	cfg, _, _ := subscribeGitShare(t, []Memory{m})
	head := mustHead(t, cfg, "neil")
	lists, err := searchSharedCorpora(context.Background(), cfg, "zanzibar", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 1 || len(lists[0]) != 1 {
		t.Fatalf("search returned %v; want one shared hit", lists)
	}
	got := lists[0][0].Path
	want := filepath.Join(shareGenCorpusDir(cfg, "neil", head.Gen), m.ID+".md")
	if got != want {
		t.Fatalf("shared hit path = %q; want the served generation's corpus file %q", got, want)
	}
	body, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("shared hit path does not open: %v", err)
	}
	if !bytes.Contains(body, []byte("zanzibar harbor notes")) {
		t.Fatalf("shared hit path holds the wrong memory: %q", body)
	}
}
