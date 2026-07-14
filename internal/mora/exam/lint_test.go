package exam

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestExamNoRealIdentities(t *testing.T) {
	if _, err := Load(filepath.Join("testdata", "real-domain-in-body.json")); !hasNamedError(err, LintRealIdentity) {
		t.Fatalf("Load error = %v, want %s", err, LintRealIdentity)
	}
	for _, text := range []string{
		"contact dana@company.dev",
		"call +14155550123",
		"call (415) 555-0123",
		"call 415-555-0123",
		"call 4155550123",
		"visit https://company.dev/review",
		"call +155501001371",
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
	if err := LintCorpus(map[string][]byte{"safe.md": []byte("https://example.net/review 5550100137")}); err != nil {
		t.Fatalf("LintCorpus rejected synthetic URL/handle: %v", err)
	}
	if err := LintCorpus(map[string][]byte{"unsafe.md": []byte("owner@company.dev")}); err == nil {
		t.Fatal("LintCorpus accepted a real domain")
	}
}

func TestLoadRejectsUnknownLedgerFields(t *testing.T) {
	tests := []struct {
		name, fixture, want string
	}{
		{name: "raw identity lint", fixture: "real-domain-in-unknown-field.json", want: "ERR_REAL_IDENTITY_LEDGER [real_identity_ledger]:"},
		{name: "strict schema", fixture: "unknown-field.json", want: `unknown field "reviewer_note"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(filepath.Join("testdata", tt.fixture))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error = %v, want %q", err, tt.want)
			}
		})
	}
}
