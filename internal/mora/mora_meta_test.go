package mora

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// adversarialMeta exercises the values the spec calls out as parser-breakers for
// the old strings.Cut(":")+Trim('"') frontmatter loop: commas, quotes, colons,
// unicode, and a nested participant list (handle↔name pairs). Numbers are kept as
// strings because json.Unmarshal decodes bare numbers to float64 (round-trip would
// not DeepEqual an int); real Meta carries counts as strings already.
func adversarialMeta() map[string]any {
	return map[string]any{
		"from":        []any{"a@x.com", "b@y.com"},
		"occurred_at": "2026-06-04T12:00:00Z",
		"subject":     `Re: lunch, "today": 1pm — café`,
		"participants": []any{
			map[string]any{"handle": "+15551234567", "name": "Neil, Patel"},
			map[string]any{"handle": "neil@x.com", "name": `O'Brien "Q"`},
		},
		"message_count": "12",
	}
}

func TestMemoryMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.md")
	want := adversarialMeta()
	in := Memory{
		ID:        "imessage_chat/abc",
		Scope:     "personal",
		Type:      "imessage",
		Title:     "chat",
		CreatedAt: "2026-06-04T12:00:00Z",
		Text:      "hello",
		Meta:      want,
	}
	body, err := renderMemory(in)
	if err != nil {
		t.Fatalf("renderMemory: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := parseMemory(path)
	if err != nil {
		t.Fatalf("parseMemory: %v", err)
	}
	if !reflect.DeepEqual(got.Meta, want) {
		t.Fatalf("Meta round-trip mismatch:\n got=%#v\nwant=%#v", got.Meta, want)
	}
	// All other fields must still parse correctly (meta must not corrupt the loop).
	if got.ID != in.ID || got.Scope != in.Scope || got.Type != in.Type || got.Title != in.Title {
		t.Fatalf("non-meta fields corrupted: %#v", got)
	}
}

func TestMemoryMetaSingleLine(t *testing.T) {
	body, err := renderMemory(Memory{ID: "x/1", Title: "t", Meta: adversarialMeta()})
	if err != nil {
		t.Fatal(err)
	}
	// The frontmatter is everything before the closing "\n---\n".
	s := string(body)
	fm := s
	if i := strings.Index(s[4:], "\n---\n"); i >= 0 {
		fm = s[4 : 4+i]
	}
	var metaLines int
	for _, line := range strings.Split(fm, "\n") {
		if strings.HasPrefix(line, "meta:") {
			metaLines++
		}
	}
	if metaLines != 1 {
		t.Fatalf("want exactly one meta: line, got %d in:\n%s", metaLines, fm)
	}
}

func TestMemoryMetaOmittedWhenEmpty(t *testing.T) {
	body, err := renderMemory(Memory{ID: "x/1", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "meta:") {
		t.Fatalf("empty Meta must not emit a meta: line:\n%s", body)
	}
}

// TestMemoryMetaPreservesLargeNumber proves a numeric Meta value (e.g. a 19-digit
// id) survives render→parse without precision loss — float64 would mangle it.
func TestMemoryMetaPreservesLargeNumber(t *testing.T) {
	const big = "1234567890123456789"
	dir := t.TempDir()
	path := filepath.Join(dir, "m.md")
	in := Memory{ID: "x/1", Title: "t", Meta: map[string]any{"thread_id": json.Number(big)}}
	body, err := renderMemory(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := parseMemory(path)
	if err != nil {
		t.Fatal(err)
	}
	if v := fmt.Sprint(got.Meta["thread_id"]); v != big {
		t.Fatalf("numeric meta precision lost: got %q want %q", v, big)
	}
}

// TestMemoryMetaDeterministic proves render is byte-stable across calls (sorted
// keys, single line) so re-renders never churn the file or the graph.
func TestMemoryMetaDeterministic(t *testing.T) {
	m := Memory{ID: "x/1", Title: "t", Meta: adversarialMeta()}
	a, _ := renderMemory(m)
	b, _ := renderMemory(m)
	if string(a) != string(b) {
		t.Fatalf("render not deterministic:\n%s\n---\n%s", a, b)
	}
}
