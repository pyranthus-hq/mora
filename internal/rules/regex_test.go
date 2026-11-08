package rules

import "testing"

func TestPythonOnly(t *testing.T) {
	cases := []struct {
		pattern string
		want    error
	}{
		{`a(?=b)`, errLookahead},
		{`a(?!b)`, errLookahead},
		{`(?<=a)b`, errLookbehind},
		{`(?<!a)b`, errLookbehind},
		{`(?>a+)b`, errAtomic},
		{`a*+`, errPossessive},
		{`a++`, errPossessive},
		{`a?+`, errPossessive},
		{`a{2,3}+`, errPossessive},
		{`(a)\1`, errBackref},
		{`(?P<x>a)(?P=x)`, errBackref},
		{`a\Z`, errBigZ},
		{`[\Z]`, errBigZ},
		// Not Python-only: these must pass the scan.
		{`[(?=]x`, nil},
		{`\(?=x`, nil},
		{`[]?+]`, nil},
		{`[^]a]+`, nil},
		{`[[:alpha:]]+x`, nil},
		{`a*?b`, nil},
		{`a{2}b+`, nil},
		{`(?i)cd`, nil},
		{`(?:a|b)+`, nil},
		{`(?P<name>a)`, nil},
		{`\\1`, nil},
		{`\z`, nil},
		{compoundCDMatch, nil},
	}
	for _, tc := range cases {
		if got := pythonOnly(tc.pattern); got != tc.want {
			t.Errorf("pythonOnly(%q) = %v, want %v", tc.pattern, got, tc.want)
		}
	}
}

func TestNormalizeSpace(t *testing.T) {
	in := "cd /tmp\v&& make　x\x1cy\u0085z"
	want := "cd /tmp && make x y z"
	if got := normalizeSpace(in); got != want {
		t.Fatalf("normalizeSpace = %q, want %q", got, want)
	}
	plain := "cd /tmp\t&& ls\n"
	if got := normalizeSpace(plain); got != plain {
		t.Fatalf("ASCII whitespace changed: %q", got)
	}
}
