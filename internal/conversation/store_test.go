package conversation

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestOriginalSearchSurvivesLossyTreeAndRepeatImport(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	meta := Session{ID: "codex/test", Provider: "codex", Scope: "project:work", Source: "/synthetic/test.jsonl"}
	messages := []Message{{Kind: "user", Text: "Choose blue, because red fails."}, {Kind: "echo", Text: strings.Repeat("noise ", 2000) + "uniqueplatypus exact constraint" + strings.Repeat("noise ", 2000)}, {Kind: "user", Text: "Correction: choose green instead of blue."}}
	if err := s.Import(meta, messages); err != nil {
		t.Fatal(err)
	}
	if err := s.Import(meta, messages); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search("uniqueplatypus", "project:work", 5)
	if err != nil || len(hits) != 1 || hits[0].Index != 1 || !strings.Contains(hits[0].Excerpt, "uniqueplatypus") {
		t.Fatalf("hits=%+v err=%v", hits, err)
	}
	hits, err = s.Search("uniqueplatypus", "project:other", 5)
	if err != nil || len(hits) != 0 {
		t.Fatalf("scope leak %+v %v", hits, err)
	}
	z, err := s.Zoom(meta.ID, 1, 1)
	if err != nil || z.Message == nil || z.Message.Text != messages[1].Text {
		t.Fatalf("original lost: %v", err)
	}
	messages[0].Text = "rewritten history"
	if err := s.Import(meta, messages); err == nil {
		t.Fatal("accepted rewritten prefix")
	}
	z, err = s.Zoom(meta.ID, 0, 1)
	if err != nil || z.Message.Text != "Choose blue, because red fails." {
		t.Fatal("history overwritten")
	}
}

func TestSmallViewStillCoversNonPowerOfTwoHistory(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	meta := Session{ID: "codex/odd", Provider: "codex", Scope: "project:work"}
	var messages []Message
	for i := 0; i < 127; i++ {
		messages = append(messages, Message{Kind: "user", Text: strings.Repeat("long original ", 80)})
	}
	if err := s.Import(meta, messages); err != nil {
		t.Fatal(err)
	}
	v, err := s.View(meta.ID, 2048)
	if err != nil || len(v.Text) > 2048 {
		t.Fatalf("view %d %v", len(v.Text), err)
	}
	next := 0
	for _, n := range v.Nodes {
		if n.Start != next {
			t.Fatal("hole")
		}
		next += n.Count
	}
	if next != 127 {
		t.Fatalf("coverage %d", next)
	}
}

func TestConcurrentImportsKeepOriginalsAndRepairIndex(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	meta := Session{ID: "codex/concurrent", Provider: "codex", Scope: "project:work"}
	messages := []Message{{Kind: "user", Text: "repairkeyword"}}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.Import(meta, messages) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(s.Dir, "index.db")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Search("repairkeyword", "", 5); err == nil {
		t.Fatal("missing index not reported")
	}
	if err := s.Reindex(); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search("repairkeyword", "", 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("repair %+v %v", hits, err)
	}
	// The cache is disposable; originals remain sufficient for navigation.
	if err := os.WriteFile(s.path(meta.ID)+".tree", []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	v, err := s.View(meta.ID, 2048)
	if err != nil || !strings.Contains(v.Text, "repairkeyword") {
		t.Fatalf("cache recovery %q %v", v.Text, err)
	}
}

func TestTreeBudgetCoverageAndExactZoom(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	meta := Session{ID: "claude/tree", Provider: "claude", Scope: "project:work"}
	var messages []Message
	for i := 0; i < 128; i++ {
		messages = append(messages, Message{Kind: "user", Text: strings.Repeat("日本語 correction ", 100)})
	}
	if err := s.Import(meta, messages); err != nil {
		t.Fatal(err)
	}
	v, err := s.View(meta.ID, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Text) > 2048 || !utf8.ValidString(v.Text) {
		t.Fatalf("bad view bytes %d", len(v.Text))
	}
	next := 0
	for _, n := range v.Nodes {
		if n.Start != next || n.Count < 1 {
			t.Fatalf("hole %+v", n)
		}
		next += n.Count
	}
	if next != len(messages) {
		t.Fatalf("coverage %d", next)
	}
	for _, r := range [][2]int{{-1, 1}, {0, 3}, {1, 2}, {128, 1}, {0, 256}} {
		if _, err := s.Zoom(meta.ID, r[0], r[1]); err == nil {
			t.Fatalf("accepted %v", r)
		}
	}
	z, err := s.Zoom(meta.ID, 0, 128)
	if err != nil || len(z.Nodes) != 2 || z.Nodes[0].Count != 64 {
		t.Fatalf("zoom %+v %v", z, err)
	}
}

func TestSessionIdentityCannotChangeScope(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	meta := Session{ID: "codex/test", Provider: "codex", Scope: "project:a"}
	if err := s.Import(meta, []Message{{Kind: "user", Text: "private"}}); err != nil {
		t.Fatal(err)
	}
	meta.Scope = "project:b"
	if err := s.Import(meta, []Message{{Kind: "user", Text: "private"}}); err == nil {
		t.Fatal("scope mutation accepted")
	}
}

func TestReindexDropsDeletedOriginals(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	for _, id := range []string{"a", "b"} {
		if err := s.Import(Session{ID: id, Provider: "codex", Scope: "global", Source: id}, []Message{{Kind: "user", Text: "secret" + id}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(s.path("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Search("secreta", "", 8); err == nil {
		t.Fatal("orphan index should fail closed")
	}
	if err := s.Reindex(); err != nil {
		t.Fatal(err)
	}
	hits, err := s.Search("secreta", "", 8)
	if err != nil || len(hits) != 0 {
		t.Fatalf("deleted original returned: %v %v", hits, err)
	}
	hits, err = s.Search("secretb", "", 8)
	if err != nil || len(hits) != 1 {
		t.Fatalf("surviving original lost: %v %v", hits, err)
	}
}

func TestRepeatImportComparesRawJSONValues(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	meta := Session{ID: "codex/formatted", Provider: "codex", Scope: "global", Source: "synthetic"}
	messages := []Message{{Kind: "user", Text: "keep <this> exact", Raw: []byte(`{ "type": "message", "text": "<this>", "n": 9007199254740993 }`)}}
	if err := s.Import(meta, messages); err != nil {
		t.Fatal(err)
	}
	if err := s.Import(meta, messages); err != nil {
		t.Fatalf("JSON serialization mistaken for rewrite: %v", err)
	}
	messages[0].Raw = []byte(`{"type":"message","text":"<this>","n":9007199254740992}`)
	if err := s.Import(meta, messages); err == nil {
		t.Fatal("real payload mutation accepted")
	}
}
