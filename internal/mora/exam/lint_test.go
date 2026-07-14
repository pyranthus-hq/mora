package exam

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestExamNoRealIdentities(t *testing.T) {
	fixture, err := Load(filepath.Join("testdata", "real-domain-in-body.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Lint(fixture); err == nil || !strings.Contains(err.Error(), LintRealIdentity) {
		t.Fatalf("Lint error = %v, want %s", err, LintRealIdentity)
	}
	for _, text := range []string{
		"contact dana@company.dev",
		"call +14155550123",
		"call (415) 555-0123",
		"call 415-555-0123",
		"legacy company name",
	} {
		l := Ledger{Version: 1, Artifacts: []Artifact{{Subject: text}}}
		if strings.Contains(text, "legacy") {
			l.Artifacts[0].Subject = "north" + "wind handoff"
		}
		if err := Lint(l); err == nil {
			t.Errorf("Lint accepted %q", text)
		}
	}
	if err := LintCorpus(map[string][]byte{"safe.md": []byte("dana@example.net +15550100137")}); err != nil {
		t.Fatalf("LintCorpus rejected synthetic identifiers: %v", err)
	}
	if err := LintCorpus(map[string][]byte{"unsafe.md": []byte("owner@company.dev")}); err == nil {
		t.Fatal("LintCorpus accepted a real domain")
	}
}
