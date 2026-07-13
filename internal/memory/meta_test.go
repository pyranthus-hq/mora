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

// TestCanonicalMeta proves the canonical encoding is single-line, sorted,
// and "" for empty — the contract render + hash both rely on, and tests error handling.
func TestCanonicalMeta(t *testing.T) {
	tests := []struct {
		name    string
		meta    map[string]any
		want    string
		wantErr bool
	}{
		{
			name:    "nil meta",
			meta:    nil,
			want:    "",
			wantErr: false,
		},
		{
			name:    "empty meta",
			meta:    map[string]any{},
			want:    "",
			wantErr: false,
		},
		{
			name:    "sorted keys",
			meta:    map[string]any{"b": "2", "a": "1"},
			want:    `{"a":"1","b":"2"}`,
			wantErr: false,
		},
		{
			name: "unmarshalable type",
			meta: map[string]any{
				"unmarshalable": func() {},
			},
			want:    "",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CanonicalMeta(tt.meta)
			if (err != nil) != tt.wantErr {
				t.Errorf("CanonicalMeta() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("CanonicalMeta() = %v, want %v", got, tt.want)
			}
			if !tt.wantErr && strings.Contains(got, "\n") {
				t.Errorf("canonical meta must be single-line: %q", got)
			}
		})
	}
}
