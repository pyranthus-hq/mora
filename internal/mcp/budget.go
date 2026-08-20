package mcp

const (
	CharsPerToken         = 4
	DefaultContextTokens  = 6000
	MaxContextTokens      = 20000
	LargeContextMaxTokens = 50000
	EnvelopeDivisor       = 3
)

func ContextDefaultTokens(profile string) int {
	switch profile {
	case "small":
		return DefaultContextTokens / 2
	case "large":
		return DefaultContextTokens * 2
	default:
		return DefaultContextTokens
	}
}
func ContextMaxTokens(profile string) int {
	if profile == "large" {
		return LargeContextMaxTokens
	}
	return MaxContextTokens
}
func DigestSnippetChars(profile string, defaultChars int) int {
	switch profile {
	case "small":
		return 120
	case "large":
		return 400
	default:
		return defaultChars
	}
}
func ResolveContextBudgetTokens(profile string, maxTokens int) (tokens, charBudget int) {
	if maxTokens <= 0 {
		maxTokens = ContextDefaultTokens(profile)
	}
	if ceiling := ContextMaxTokens(profile); maxTokens > ceiling {
		maxTokens = ceiling
	}
	return maxTokens, maxTokens * CharsPerToken
}
func ResolveContextBudget(profile string, maxTokens int) int {
	_, chars := ResolveContextBudgetTokens(profile, maxTokens)
	return chars
}
func EstimateTokensUsed(bytes int) int {
	if bytes <= 0 {
		return 0
	}
	return (bytes + CharsPerToken - 1) / CharsPerToken
}
func EnvelopeBudgetChars(profile string, maxTokens int) int {
	return ResolveContextBudget(profile, maxTokens) / EnvelopeDivisor
}
