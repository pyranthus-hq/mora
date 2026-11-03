package rules

import (
	"strings"
	"testing"
	"time"
)

func compoundCD(t *testing.T) Rule {
	t.Helper()
	r, ok := Template("compound-cd", time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("compound-cd template missing")
	}
	return r
}

func TestValidate(t *testing.T) {
	base := compoundCD(t)
	if err := Validate(base); err != nil {
		t.Fatalf("template rule should validate: %v", err)
	}
	cases := []struct {
		name string
		edit func(*Rule)
		want string
	}{
		{"bad id", func(r *Rule) { r.ID = "Bad_ID" }, "invalid rule id"},
		{"empty id", func(r *Rule) { r.ID = "" }, "invalid rule id"},
		{"long id", func(r *Rule) { r.ID = strings.Repeat("a", 49) }, "invalid rule id"},
		{"201-rune message", func(r *Rule) { r.Message = strings.Repeat("é", 201) }, "message is 201 characters; the limit is 200"},
		{"newline in message", func(r *Rule) { r.Message = "one\ntwo" }, "message must be a single line"},
		{"empty message", func(r *Rule) { r.Message = "" }, "message is required"},
		{"untrimmed message", func(r *Rule) { r.Message = " x" }, "must not start or end with whitespace"},
		{"control char", func(r *Rule) { r.Message = "a\x07b" }, "control characters"},
		{"tool Edit", func(r *Rule) { r.Spec.Tool = "Edit" }, `spec tool must be Bash, got "Edit"`},
		{"version 2", func(r *Rule) { r.Spec.Version = 2 }, "spec version must be 1"},
		{"no match", func(r *Rule) { r.Spec.Match = "" }, "spec match is required"},
		{"bad context", func(r *Rule) { r.Spec.Context = "main" }, "spec context must be empty"},
		{"lookahead", func(r *Rule) { r.Spec.Match = "a(?!x)" }, "match: lookahead is not supported by Go RE2. Put the forbidden text in `unless`"},
		{"lookbehind", func(r *Rule) { r.Spec.Match = "(?<!x)a" }, "match: lookbehind is not supported. Use a consumed class"},
		{"backreference", func(r *Rule) { r.Spec.Match = `(a)\1` }, `match: backreferences such as \1`},
		{"big Z", func(r *Rule) { r.Spec.Match = `a\Z` }, `match: \Z is not supported by Go RE2. Use \z`},
		{"bad exclude", func(r *Rule) { r.Spec.Exclude = "(?=x)" }, "exclude: lookahead"},
		{"bad unless", func(r *Rule) { r.Spec.Unless = "(" }, "unless: error parsing regexp"},
		{"multi-line title", func(r *Rule) { r.Title = "a\nb" }, "title must be a single trimmed line"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.edit(&r)
			err := Validate(r)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

func TestValidateMode(t *testing.T) {
	for _, m := range []string{ModeOff, ModeShadow, ModeWarn} {
		if err := ValidateMode(m); err != nil {
			t.Errorf("ValidateMode(%q) = %v", m, err)
		}
	}
	if err := ValidateMode("block"); err == nil || !strings.Contains(err.Error(), "mode block is not supported yet") {
		t.Errorf("ValidateMode(block) = %v", err)
	}
	if err := ValidateMode("loud"); err == nil || !strings.Contains(err.Error(), "mode must be off, shadow or warn") {
		t.Errorf("ValidateMode(loud) = %v", err)
	}
}

func TestHashStableAcrossRenderParse(t *testing.T) {
	r := compoundCD(t)
	r.Spec.Exclude = `"[^"]*"`
	r.Spec.Unless = `--json\b`
	h := Hash(r)
	if !strings.HasPrefix(h, "sha256:") || len(h) != len("sha256:")+64 {
		t.Fatalf("Hash = %q", h)
	}
	got, err := Parse(Render(r))
	if err != nil {
		t.Fatal(err)
	}
	if Hash(got) != h {
		t.Fatalf("hash changed across Render/Parse: %s vs %s", Hash(got), h)
	}
	again, err := Parse(Render(got))
	if err != nil || Hash(again) != h {
		t.Fatalf("second round trip: %v, %s", err, Hash(again))
	}
	if ShortHash(h) != h[7:19] {
		t.Fatalf("ShortHash = %q", ShortHash(h))
	}
}

func TestHashChangesWithEveryHashedField(t *testing.T) {
	base := compoundCD(t)
	h := Hash(base)
	edits := map[string]func(*Rule){
		"id":      func(r *Rule) { r.ID = "other" },
		"version": func(r *Rule) { r.Spec.Version = 2 },
		"tool":    func(r *Rule) { r.Spec.Tool = "Edit" },
		"match":   func(r *Rule) { r.Spec.Match += "x" },
		"exclude": func(r *Rule) { r.Spec.Exclude = "x" },
		"unless":  func(r *Rule) { r.Spec.Unless = "x" },
		"context": func(r *Rule) { r.Spec.Context = ContextSubagent },
		"message": func(r *Rule) { r.Message += "." },
	}
	for name, edit := range edits {
		r := base
		edit(&r)
		if Hash(r) == h {
			t.Errorf("changing %s kept the hash", name)
		}
	}
	// Descriptive fields are not hashed, so editing them keeps an approval.
	r := base
	r.Title, r.CreatedAt, r.Source = "New title", "2027-01-01T00:00:00Z", "manual"
	if Hash(r) != h {
		t.Error("title, created_at or source changed the hash")
	}
}
