package memory

import (
	"reflect"
	"strings"
	"testing"
)

// TestContentHashIncludesMeta proves a change in structured Meta (and nothing
// else) changes the ContentHash, so a re-ingest rewrites the file with the new
// identity data instead of being skipped by the content-hash guard.
func TestContentHashIncludesMeta(t *testing.T) {
	base := Item{Kind: "gmail_thread", ProviderID: "t1", Title: "Re: OAuth", Body: "body"}
	withMeta := base
	withMeta.Meta = map[string]any{"from": []any{"a@x.com"}}

	h0 := MapItem(base, "personal", 0).ContentHash
	h1 := MapItem(withMeta, "personal", 0).ContentHash
	if h0 == h1 {
		t.Fatalf("ContentHash must change when Meta changes: both %s", h0)
	}

	// Same Meta -> same hash (deterministic, key order independent).
	a := withMeta
	a.Meta = map[string]any{"from": []any{"a@x.com"}, "b": "1"}
	b := withMeta
	b.Meta = map[string]any{"b": "1", "from": []any{"a@x.com"}}
	if MapItem(a, "personal", 0).ContentHash != MapItem(b, "personal", 0).ContentHash {
		t.Fatal("ContentHash must be independent of Meta key insertion order")
	}
}

// TestCanonicalMetaSorted proves the canonical encoding is single-line, sorted,
// and "" for empty — the contract render + hash both rely on.
func TestCanonicalMetaSorted(t *testing.T) {
	if s, _ := CanonicalMeta(nil); s != "" {
		t.Fatalf("nil meta must canonicalize to empty, got %q", s)
	}
	if s, _ := CanonicalMeta(map[string]any{}); s != "" {
		t.Fatalf("empty meta must canonicalize to empty, got %q", s)
	}
	s, err := CanonicalMeta(map[string]any{"b": "2", "a": "1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s, "\n") {
		t.Fatalf("canonical meta must be single-line: %q", s)
	}
	if s != `{"a":"1","b":"2"}` {
		t.Fatalf("canonical meta not sorted: %q", s)
	}
}

// TestMapItemCopiesMeta proves Meta is defensively copied (mutating the source
// map after mapping must not change the mapped memory).
func TestMapItemCopiesMeta(t *testing.T) {
	src := map[string]any{"k": "v"}
	it := Item{Kind: "gmail_thread", ProviderID: "t1", Title: "t", Body: "b", Meta: src}
	m := MapItem(it, "personal", 0)
	src["k"] = "mutated"
	if !reflect.DeepEqual(m.Meta, map[string]any{"k": "v"}) {
		t.Fatalf("Meta not defensively copied: %#v", m.Meta)
	}
}
