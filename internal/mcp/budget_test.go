package mcp

import "testing"

func TestContextProfiles(t *testing.T) {
	cases := []struct {
		profile           string
		def, max, snippet int
	}{{"small", 3000, 20000, 120}, {"", 6000, 20000, 200}, {"unknown", 6000, 20000, 200}, {"large", 12000, 50000, 400}}
	for _, tc := range cases {
		if got := ContextDefaultTokens(tc.profile); got != tc.def {
			t.Errorf("%q default=%d", tc.profile, got)
		}
		if got := ContextMaxTokens(tc.profile); got != tc.max {
			t.Errorf("%q max=%d", tc.profile, got)
		}
		if got := DigestSnippetChars(tc.profile, 200); got != tc.snippet {
			t.Errorf("%q snippet=%d", tc.profile, got)
		}
	}
}
func TestResolveContextBudgetDefaultsAndClampsBeforeMultiply(t *testing.T) {
	cases := []struct {
		profile                string
		request, tokens, chars int
	}{{"", 0, 6000, 24000}, {"", -1, 6000, 24000}, {"small", 0, 3000, 12000}, {"large", 0, 12000, 48000}, {"", 42, 42, 168}, {"", 999999999, 20000, 80000}, {"large", 999999999, 50000, 200000}}
	for _, tc := range cases {
		tokens, chars := ResolveContextBudgetTokens(tc.profile, tc.request)
		if tokens != tc.tokens || chars != tc.chars {
			t.Errorf("(%q,%d)=(%d,%d)", tc.profile, tc.request, tokens, chars)
		}
		if got := ResolveContextBudget(tc.profile, tc.request); got != tc.chars {
			t.Errorf("chars=%d", got)
		}
	}
}
func TestEstimateTokensUsedRoundsUp(t *testing.T) {
	for bytes, want := range map[int]int{-1: 0, 0: 0, 1: 1, 4: 1, 5: 2, 8: 2, 9: 3} {
		if got := EstimateTokensUsed(bytes); got != want {
			t.Errorf("%d=%d", bytes, got)
		}
	}
}
func TestEnvelopeBudgetChars(t *testing.T) {
	if got := EnvelopeBudgetChars("", 0); got != 8000 {
		t.Fatalf("default=%d", got)
	}
	if got := EnvelopeBudgetChars("large", 50000); got != 66666 {
		t.Fatalf("large=%d", got)
	}
	if EnvelopeBudgetChars("", 20000) <= EnvelopeBudgetChars("", 6000) {
		t.Fatal("budget knob does not scale")
	}
}
