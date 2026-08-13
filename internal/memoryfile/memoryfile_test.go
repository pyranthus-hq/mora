package memoryfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func coreBMemFullMemory() Document {
	return Document{
		ID:    "mem_full_1",
		Scope: "project:wink",
		Type:  "decision",
		Title: "ns: value #tag [x]", // forces QuoteYAML on ":", "#", "[", "]"
		Tags:  []string{"t1", "t2"},
		// Windows-shaped source path forces quoting and contains escape sequences.
		// It must survive render/parse byte-for-byte for attachment identity hashes.
		Source:      `C:\Users\Adit\Documents\source#1.pdf`,
		CreatedAt:   "2026-06-30T10:00:00Z",
		Provider:    "gmail",
		Account:     "work",
		ProviderID:  "gmail_thread:abc#1", // forces QuoteYAML
		ContentHash: "deadbeef",
		LastSynced:  "2026-06-30T09:00:00Z",
		Truncated:   true,
		DeletedAt:   "2026-06-30T11:00:00Z",
		Text:        "body line one\nbody line two",
		Meta:        map[string]any{"from": "a@b.com", "n": json.Number("123456789012345678")},
	}
}

func coreBMemWriteFile(t *testing.T, body []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "m.md")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatalf("write temp memory: %v", err)
	}
	return p
}

func TestCoreB_MemRenderParseRoundtrip(t *testing.T) {
	m := coreBMemFullMemory()
	body, err := Render(m)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// The meta line must be one canonical JSON line (sorted keys, no raw newline).
	if !strings.Contains(string(body), "\nmeta: {") {
		t.Fatalf("expected a canonical meta line, got:\n%s", body)
	}
	got, err := Parse(coreBMemWriteFile(t, body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.ID != m.ID || got.Scope != m.Scope || got.Type != m.Type {
		t.Fatalf("id/scope/type mismatch: %+v", got)
	}
	if got.Title != m.Title {
		t.Fatalf("title did not round-trip: want %q got %q", m.Title, got.Title)
	}
	if got.Source != m.Source {
		t.Fatalf("source did not round-trip: want %q got %q", m.Source, got.Source)
	}
	if got.ProviderID != m.ProviderID {
		t.Fatalf("provider_id did not round-trip: want %q got %q", m.ProviderID, got.ProviderID)
	}
	if strings.Join(got.Tags, ",") != "t1,t2" {
		t.Fatalf("tags did not round-trip: %v", got.Tags)
	}
	if got.CreatedAt != m.CreatedAt || got.Provider != m.Provider || got.Account != m.Account {
		t.Fatalf("created_at/provider/account mismatch: %+v", got)
	}
	if got.ContentHash != m.ContentHash || got.LastSynced != m.LastSynced {
		t.Fatalf("content_hash/last_synced mismatch: %+v", got)
	}
	if !got.Truncated {
		t.Fatalf("truncated did not round-trip: %+v", got)
	}
	if got.DeletedAt != m.DeletedAt {
		t.Fatalf("deleted_at did not round-trip: %+v", got)
	}
	if got.Text != m.Text {
		t.Fatalf("text did not round-trip: want %q got %q", m.Text, got.Text)
	}
	if got.Meta["from"] != "a@b.com" {
		t.Fatalf("meta.from lost: %+v", got.Meta)
	}
	// UseNumber must keep the 19-digit id exact (no float64 precision loss).
	if s := fmt.Sprintf("%v", got.Meta["n"]); s != "123456789012345678" {
		t.Fatalf("meta.n precision lost: %q", s)
	}
}

func TestCoreB_MemRenderMemoryMinimalOmitsOptionalLines(t *testing.T) {
	m := Document{ID: "mem_min", Scope: "global", Type: "insight", Title: "Plain", Source: "manual", CreatedAt: "2026-06-30T00:00:00Z", Text: "hello"}
	body, err := Render(m)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	s := string(body)
	for _, mustHave := range []string{"id: mem_min", "scope: global", "type: insight", "title: Plain", "source: manual", "created_at: 2026-06-30T00:00:00Z"} {
		if !strings.Contains(s, mustHave) {
			t.Fatalf("expected %q in:\n%s", mustHave, s)
		}
	}
	for _, absent := range []string{"provider:", "account:", "content_hash:", "last_synced:", "truncated:", "deleted_at:", "\nmeta:"} {
		if strings.Contains(s, absent) {
			t.Fatalf("did not expect %q in minimal render:\n%s", absent, s)
		}
	}
	// A plain (no special char) title must NOT be quoted.
	if strings.Contains(s, `title: "Plain"`) {
		t.Fatalf("plain title should not be quoted:\n%s", s)
	}
}

func TestCoreB_MemRenderMemoryMetaMarshalError(t *testing.T) {
	m := Document{ID: "mem_bad", Scope: "global", Title: "T", Text: "b", Meta: map[string]any{"c": make(chan int)}}
	_, err := Render(m)
	if err == nil {
		t.Fatalf("expected Render to fail on unmarshalable meta")
	}
	if !strings.Contains(err.Error(), "unsupported type") {
		t.Fatalf("expected json unsupported-type error, got: %v", err)
	}
}

func TestCoreB_MemParseMemoryErrors(t *testing.T) {
	// Nonexistent path -> read error (not a frontmatter error).
	if _, err := Parse(filepath.Join(t.TempDir(), "nope.md")); err == nil {
		t.Fatalf("expected read error for missing file")
	} else if strings.Contains(err.Error(), "frontmatter") {
		t.Fatalf("expected os read error, got frontmatter error: %v", err)
	}

	cases := []struct{ body, wantErr string }{
		{"no frontmatter here\n", "missing frontmatter"},
		{"---\nid: x\n", "invalid frontmatter"},        // opens but never closes
		{"---\ntitle: X\n---\n\nbody\n", "missing id"}, // closes, but no id
		{"---\nid: x\nsource: \"unterminated\n---\n\nbody\n", "source frontmatter value is corrupt: invalid syntax"},
	}
	for _, c := range cases {
		_, err := Parse(coreBMemWriteFile(t, []byte(c.body)))
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Fatalf("body %q: want error %q, got %v", c.body, c.wantErr, err)
		}
	}
}

func TestCoreB_MemQuotedScalarDecodingIsExact(t *testing.T) {
	// Mutation tripwire: replacing strconv.Unquote with quote trimming leaves
	// doubled backslashes and the escaped quote in the parsed identity fields.
	body := "---\n" +
		"id: mem_windows\n" +
		"title: \"A \\\"quoted\\\" title\"\n" +
		"source: \"C:\\\\Users\\\\Adit\\\\legacy.pdf\"\n" +
		"provider_id: \"imessage:legacy\\\\attachment\"\n" +
		"---\n\nbody\n"
	m, err := Parse(coreBMemWriteFile(t, []byte(body)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.Title != `A "quoted" title` {
		t.Fatalf("quoted title decoded as %q", m.Title)
	}
	if m.Source != `C:\Users\Adit\legacy.pdf` {
		t.Fatalf("Windows source decoded as %q", m.Source)
	}
	if m.ProviderID != `imessage:legacy\attachment` {
		t.Fatalf("provider identity decoded as %q", m.ProviderID)
	}
}

func TestCoreB_MemLegacyLeadingQuoteTitleRemainsReadable(t *testing.T) {
	// Before QuoteYAML treated quote characters as requiring encoding, a title
	// beginning with `"` and containing none of :#[] was written raw. The legacy
	// parser trimmed that quote. Preserve that readable result rather than making
	// a cosmetic title ambiguity invalidate the entire memory.
	body := "---\n" +
		"id: mem_legacy_quote\n" +
		"title: \"Quoted title\n" +
		"source: manual\n" +
		"---\n\nbody\n"
	m, err := Parse(coreBMemWriteFile(t, []byte(body)))
	if err != nil {
		t.Fatalf("legacy leading-quote title became unreadable: %v", err)
	}
	if m.Title != "Quoted title" {
		t.Fatalf("legacy title = %q, want old-parser value %q", m.Title, "Quoted title")
	}
}

func TestCoreB_MemScalarCodecRoundTrips(t *testing.T) {
	// The renderer and parser are a codec pair, not independent string helpers.
	// Every value here must survive encode/decode exactly; deleting either the
	// quote trigger or strconv.Unquote makes at least one row fail.
	values := []string{
		"plain",
		"",
		" leading",
		"trailing ",
		`C:\Users\Adit\relative\source.md`,
		`a "quoted" value`,
		"tab\tand\nnewline",
		"Unicode café",
	}
	for _, want := range values {
		encoded := QuoteYAML(want)
		got, err := ParseFrontmatterScalar("source", encoded)
		if err != nil {
			t.Fatalf("decode %q from %q: %v", want, encoded, err)
		}
		if got != want {
			t.Fatalf("scalar codec: %q -> %q -> %q", want, encoded, got)
		}
	}
}

func TestCoreB_MemParseMemoryNoColonLineIgnored(t *testing.T) {
	body := "---\nid: mem_nc\nscope: global\njustacomment\nfuture_field: \"syntax older Mora does not know\ntitle: Hello\n---\n\nbody\n"
	m, err := Parse(coreBMemWriteFile(t, []byte(body)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if m.ID != "mem_nc" || m.Title != "Hello" || m.Scope != "global" {
		t.Fatalf("colon-less line should be skipped, got %+v", m)
	}
}

func TestCoreB_MemParseMemoryMetaEdges(t *testing.T) {
	// Corrupt meta: warn to stderr, ignore, but keep the rest of the memory.
	corrupt := "---\nid: mem_cm\nscope: global\ntitle: T\nmeta: {not valid json\n---\n\nbody\n"
	m, err := Parse(coreBMemWriteFile(t, []byte(corrupt)))
	if err != nil {
		t.Fatalf("Parse(corrupt meta): %v", err)
	}
	if m.ID != "mem_cm" || m.Meta != nil {
		t.Fatalf("corrupt meta should be dropped, other fields kept: %+v", m)
	}
	// Empty object -> len(meta)==0 -> Meta stays nil.
	empty := "---\nid: mem_em\nscope: global\ntitle: T\nmeta: {}\n---\n\nbody\n"
	m2, err := Parse(coreBMemWriteFile(t, []byte(empty)))
	if err != nil {
		t.Fatalf("Parse(empty meta): %v", err)
	}
	if m2.Meta != nil {
		t.Fatalf("empty meta object should leave Meta nil, got %+v", m2.Meta)
	}
}
