package rules

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestRenderTemplateFile(t *testing.T) {
	want := "---\n" +
		"id: compound-cd\n" +
		"title: One command per Bash call\n" +
		"created_at: 2026-11-03T23:00:00Z\n" +
		"source: template:compound-cd\n" +
		`spec: {"version":1,"tool":"Bash","match":"^\\s*cd\\s+[^;&|\\n]*?\\s*(&&|;)"}` + "\n" +
		"---\n" +
		"Use one command per Bash call. Set the working folder through the tool instead of chaining cd.\n"
	if got := string(Render(compoundCD(t))); got != want {
		t.Fatalf("Render:\n%s\nwant:\n%s", got, want)
	}
}

func TestParse(t *testing.T) {
	r := compoundCD(t)
	got, err := Parse(Render(r))
	if err != nil || got != r {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	// CRLF from a Windows checkout, unknown keys and extra blank lines are fine.
	crlf := strings.ReplaceAll(string(Render(r)), "\n", "\r\n")
	crlf = strings.Replace(crlf, "title:", "owner: adit\r\n\r\ntitle:", 1)
	if got, err := Parse([]byte(crlf)); err != nil || got != r {
		t.Fatalf("CRLF: %+v, %v", got, err)
	}
	// A file with no body parses with an empty message, which Validate rejects.
	noBody := "---\nid: x\nspec: {\"version\":1,\"tool\":\"Bash\",\"match\":\"x\"}\n---"
	if got, err := Parse([]byte(noBody)); err != nil || got.Message != "" || Validate(got) == nil {
		t.Fatalf("no body: %+v, %v", got, err)
	}
	bad := map[string]string{
		"no frontmatter":   "id: x\n",
		"no closing line":  "---\nid: x\nspec: {}\n",
		"not key value":    "---\nid x\nspec: {}\n---\nm\n",
		"duplicate key":    "---\nid: x\nid: y\nspec: {}\n---\nm\n",
		"no spec":          "---\nid: x\n---\nm\n",
		"spec not json":    "---\nid: x\nspec: {nope\n---\nm\n",
		"unknown spec key": "---\nid: x\nspec: {\"version\":1,\"tool\":\"Bash\",\"match\":\"x\",\"unles\":\"y\"}\n---\nm\n",
		"spec trailing":    "---\nid: x\nspec: {\"version\":1} {}\n---\nm\n",
	}
	for name, body := range bad {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%s: Parse accepted %q", name, body)
		}
	}
}

func TestReadRule(t *testing.T) {
	vault := t.TempDir()
	r := compoundCD(t)
	writeRule(t, vault, r)
	got, err := ReadRule(vault, r.ID)
	if err != nil || got != r {
		t.Fatalf("ReadRule: %+v, %v", got, err)
	}
	if _, err := ReadRule(vault, "missing"); !os.IsNotExist(err) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := ReadRule(vault, "../etc"); err == nil {
		t.Fatal("ReadRule accepted a path id")
	}
	if err := os.WriteFile(RulePath(vault, "big"), []byte(strings.Repeat("x", maxRuleFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRule(vault, "big"); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("big file: %v", err)
	}
}

func TestTemplate(t *testing.T) {
	r := compoundCD(t)
	if err := Validate(r); err != nil {
		t.Fatal(err)
	}
	// Byte-identical to Steward's hook-rules.json and fixed-signatures-v3.json.
	if r.Spec.Match != `^\s*cd\s+[^;&|\n]*?\s*(&&|;)` {
		t.Fatalf("match = %q", r.Spec.Match)
	}
	if r.Message != "Use one command per Bash call. Set the working folder through the tool instead of chaining cd." {
		t.Fatalf("message = %q", r.Message)
	}
	if _, ok := Template("nope", time.Time{}); ok {
		t.Fatal("unknown template accepted")
	}
	for _, name := range TemplateNames {
		if _, ok := Template(name, time.Time{}); !ok {
			t.Fatalf("listed template %q missing", name)
		}
	}
}
