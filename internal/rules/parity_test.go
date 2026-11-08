package rules

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The parity cases are every hit and miss string from venture
// lanes/steward/tests/test_signatures_v3.py, with the result Python's re gives
// on contracts/fixed-signatures-v3.json. prototype-signatures-re2.json is the
// RE2 port; it is test data only and never shipped as a template.

type parityCase struct {
	Sig       string  `json:"sig"`
	Command   string  `json:"command"`
	Agent     *string `json:"agent"`
	Want      bool    `json:"want"`
	Source    string  `json:"source"`
	KnownDiff string  `json:"known_diff,omitempty"`
}

func loadPrototype(t *testing.T) map[string]Spec {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "prototype-signatures-re2.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Signatures []struct {
			ID   string `json:"id"`
			Spec Spec   `json:"spec"`
		} `json:"signatures"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	out := map[string]Spec{}
	for _, s := range doc.Signatures {
		out[s.ID] = s.Spec
	}
	return out
}

func TestPrototypeParity(t *testing.T) {
	sigs := loadPrototype(t)
	matchers := map[string]*Matcher{}
	for id, s := range sigs {
		matchers[id] = mustMatcher(t, id, s)
	}
	data, err := os.ReadFile(filepath.Join("testdata", "parity_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []parityCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 50 {
		t.Fatalf("only %d parity cases; the Python suite has 54", len(cases))
	}
	for _, c := range cases {
		m := matchers[c.Sig]
		if m == nil {
			t.Fatalf("case names unknown signature %q", c.Sig)
		}
		a := Action{Command: c.Command}
		if c.Agent != nil {
			a.AgentID = *c.Agent
		}
		got := m.Hit(a)
		switch {
		case c.KnownDiff != "" && got == c.Want:
			t.Errorf("%s %q is marked known_diff (%s) but now agrees with Python", c.Sig, c.Command, c.KnownDiff)
		case c.KnownDiff == "" && got != c.Want:
			t.Errorf("%s %q (%s): Go %v, Python %v", c.Sig, c.Command, c.Source, got, c.Want)
		}
	}
}

// The raw v3 contract uses Python lookarounds, so loading it as-is must fail
// with the errors that tell the author how to port it.
func TestRawV3ContractRejected(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "fixed-signatures-v3.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Signatures []struct {
			ID      string `json:"id"`
			Tool    string `json:"tool"`
			Match   string `json:"match"`
			Exclude string `json:"exclude"`
			Context string `json:"context"`
		} `json:"signatures"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	want := map[string]error{
		"compound-cd":        nil,
		"nested-agent-cli":   nil,
		"memory-cli-no-json": errLookahead,
		"memory-data-folder": errLookbehind,
	}
	for _, s := range doc.Signatures {
		if s.ID == "compound-cd" && s.Match != compoundCDMatch {
			t.Errorf("compound-cd template match drifted from the v3 contract: %q", s.Match)
		}
		_, err := NewMatcher(s.ID, Spec{Version: 1, Tool: s.Tool, Match: s.Match, Exclude: s.Exclude, Context: s.Context})
		if !errors.Is(err, want[s.ID]) {
			t.Errorf("%s: err = %v, want %v", s.ID, err, want[s.ID])
		}
	}
	// The data-folder pattern also ends with a lookahead; check it on its own.
	if err := pythonOnly(`(?:^|x)/vault/mora(?=$|[/\s])`); err != errLookahead {
		t.Errorf("trailing lookahead: %v", err)
	}
}
