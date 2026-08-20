package meeting

import (
	"github.com/pyranthus-hq/mora/internal/evidencetext"
	"time"
)

func OneLine(s string) string { return evidencetext.OneLine(s) }
func HistoricalText(asOf time.Time, date, attendee, raw string) string {
	return evidencetext.HistoricalText(asOf, date, attendee, raw)
}
func HistoricalPrefix(asOf time.Time, date, attendee string) string {
	return evidencetext.HistoricalPrefix(asOf, date, attendee)
}
func RelativeAge(asOf, fact time.Time) string  { return evidencetext.RelativeAge(asOf, fact) }
func EvidenceSegments(text string) []string    { return evidencetext.EvidenceSegments(text) }
func IsQuotedReplyLine(line string) bool       { return evidencetext.IsQuotedReplyLine(line) }
func SenderAuthoredBody(text string) string    { return evidencetext.SenderAuthoredBody(text) }
func StripSpeakerPrefix(segment string) string { return evidencetext.StripSpeakerPrefix(segment) }
func IsForwardedSubject(title string) bool     { return evidencetext.IsForwardedSubject(title) }
func IsLeadInFragment(text string) bool        { return evidencetext.IsLeadInFragment(text) }
func StripURLs(text string) string             { return evidencetext.StripURLs(text) }
func UnwrapHardWraps(text string) string       { return evidencetext.UnwrapHardWraps(text) }
func ContinuesSentence(line, next string) bool { return evidencetext.ContinuesSentence(line, next) }
func ContainsPhrase(text, phrase string) bool  { return evidencetext.ContainsPhrase(text, phrase) }
func ContainsAnyPhrase(text string, phrases []string) bool {
	return evidencetext.ContainsAnyPhrase(text, phrases)
}
func ContainsPersonalTrivia(text string) bool { return evidencetext.ContainsPersonalTrivia(text) }
func StripNoiseTokens(text string) string     { return evidencetext.StripNoiseTokens(text) }
func TokenIsNoise(tok string) bool            { return evidencetext.TokenIsNoise(tok) }
func PersonalTriviaOnly(text string, materialPhrases []string) bool {
	return evidencetext.PersonalTriviaOnly(text, materialPhrases)
}
