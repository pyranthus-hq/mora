package search

import (
	"fmt"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func noAnnotate(results, _ []memory.Memory) []memory.Memory { return results }
func mem(id string) memory.Memory {
	return memory.Memory{ID: id, Scope: "project:x", Title: id, Source: "test", CreatedAt: "2026-08-13T00:00:00Z"}
}
func TestClusterProviderAnchorAndBackfill(t *testing.T) {
	a, b, c := mem("a"), mem("b"), mem("c")
	a.Provider, b.Provider = "gmail", "gmail"
	a.ProviderID, b.ProviderID = "thread/1", "thread/1"
	got := ClusterAndTruncate([]string{"a", "b", "c"}, []memory.Memory{a, b, c}, 2, noAnnotate)
	if len(got) != 2 || got[0].ID != "a" || len(got[0].Corroborating) != 1 || got[0].Corroborating[0].ID != "b" || got[1].ID != "c" {
		t.Fatalf("got=%+v", got)
	}
}

func TestClusterRollingDigestCopiesByContentHash(t *testing.T) {
	first, repeated := mem("digest-window-1"), mem("digest-window-2")
	first.Provider, first.ProviderID = "filesystem", "rollup/2026-08-23"
	repeated.Provider, repeated.ProviderID = "filesystem", "rollup/2026-08-24"
	first.ContentHash, repeated.ContentHash = "sha256:canonical-observation", "sha256:canonical-observation"

	got := ClusterAndTruncate([]string{first.ID, repeated.ID}, []memory.Memory{first, repeated}, 2, noAnnotate)
	if len(got) != 1 || got[0].ID != first.ID || len(got[0].Corroborating) != 1 || got[0].Corroborating[0].ID != repeated.ID {
		t.Fatalf("rolling digest was not collapsed with lineage: %+v", got)
	}
}

func TestClusterPersonTimeStrictWindowAndNoFallback(t *testing.T) {
	at := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	makeAt := func(id string, d time.Duration, explicit bool) memory.Memory {
		m := mem(id)
		m.Meta = map[string]any{"from": "same@example.com"}
		if explicit {
			m.Meta["occurred_at"] = at.Add(d).Format(time.RFC3339)
		}
		return m
	}
	a := makeAt("a", 0, true)
	inside := makeAt("inside", 23*time.Hour, true)
	boundary := makeAt("boundary", 24*time.Hour, true)
	missing := makeAt("missing", 0, false)
	got := ClusterAndTruncate([]string{"a", "inside", "boundary", "missing"}, []memory.Memory{a, inside, boundary, missing}, 4, noAnnotate)
	if len(got) != 3 || len(got[0].Corroborating) != 1 || got[0].Corroborating[0].ID != "inside" {
		t.Fatalf("got=%+v", got)
	}
}
func TestClusterRefusesOverFiveMembers(t *testing.T) {
	var rows []memory.Memory
	var ids []string
	for i := 0; i < 6; i++ {
		m := mem(fmt.Sprintf("m%d", i))
		m.Provider = "gmail"
		m.ProviderID = "same"
		rows = append(rows, m)
		ids = append(ids, m.ID)
	}
	got := ClusterAndTruncate(ids, rows, 6, noAnnotate)
	if len(got) != 6 {
		t.Fatalf("refused cluster len=%d", len(got))
	}
	for _, m := range got {
		if len(m.Corroborating) > 0 {
			t.Fatalf("refused member clustered: %+v", m)
		}
	}
}
func TestClusterNoTransitivePersonTimeChain(t *testing.T) {
	at := time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC)
	mk := func(id, who string, h int) memory.Memory {
		m := mem(id)
		m.Meta = map[string]any{"from": who, "to": "bridge@example.com", "occurred_at": at.Add(time.Duration(h) * time.Hour).Format(time.RFC3339)}
		return m
	}
	a := mk("a", "a@example.com", 0)
	b := mk("b", "a@example.com", 20)
	c := mk("c", "c@example.com", 40)
	got := ClusterAndTruncate([]string{"a", "b", "c"}, []memory.Memory{a, b, c}, 3, noAnnotate)
	if len(got) != 2 || len(got[0].Corroborating) != 1 {
		t.Fatalf("star topology=%+v", got)
	}
}
func TestClusterPreservesNonNilEmptyAndCallsAnnotator(t *testing.T) {
	called := false
	annotate := func(r, p []memory.Memory) []memory.Memory { called = true; return r }
	got := ClusterAndTruncate([]string{"gone"}, []memory.Memory{}, 1, annotate)
	if got == nil || !called {
		t.Fatalf("got=%v called=%v", got, called)
	}
	if ClusterAndTruncate(nil, nil, 0, annotate) != nil {
		t.Fatal("nonpositive limit must return nil")
	}
}
