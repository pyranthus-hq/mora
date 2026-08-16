package meeting

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var quotedBlockMarkers = []string{"---------- forwarded message", "-----original message-----", "begin forwarded message:", "________________________________", "this email and any files transmitted", "confidential and intended solely", "sent from my iphone", "unsubscribe"}
var quotedReplyLine = regexp.MustCompile(`(?i)^\s*on .{0,120}\bwrote:\s*$`)
var speakerPrefix = regexp.MustCompile(`^[\p{L}][\p{L}\p{N} .'’\-]{0,30}:\s+`)
var urlPattern = regexp.MustCompile(`(?i)(?:https?://|www\.)\S+|[a-z0-9][a-z0-9.\-]*\.[a-z]{2,}/\S*`)
var personalTriviaPhrases = []string{"kid's name", "kids' names", "son's name", "daughter's name", "birthday", "favorite food", "favourite food", "favorite drink", "favourite drink", "hobby", "vacation", "spouse", "wife", "husband"}
var urlNoiseMarkers = []string{"http://", "https://", "://", "www.", ".com/", ".org/", ".net/", "/search", "/maps", "/url?", "%0a", "%20", "%2f", "utm_"}

func OneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
func HistoricalText(asOf time.Time, date, attendee, raw string) string {
	return HistoricalPrefix(asOf, date, attendee) + "“" + OneLine(raw) + "”"
}
func HistoricalPrefix(asOf time.Time, date, attendee string) string {
	fact, err := time.Parse(time.RFC3339, date)
	if err != nil {
		return ""
	}
	age := RelativeAge(asOf, fact)
	attendee = strings.TrimSpace(attendee)
	if attendee == "" {
		return age + " — "
	}
	return fmt.Sprintf("%s · %s — ", age, attendee)
}
func RelativeAge(asOf, fact time.Time) string {
	age := asOf.Sub(fact)
	if age < 0 {
		age = 0
	}
	days := int(age.Hours()/24 + 0.5)
	switch {
	case days < 1:
		return "Earlier that day"
	case days == 1:
		return "~1 day ago"
	case days < 60:
		return fmt.Sprintf("~%d days ago", days)
	case days < 730:
		months := int(float64(days)/30.4375 + 0.5)
		return fmt.Sprintf("~%d months ago", months)
	default:
		years := int(float64(days)/365.25 + 0.5)
		return fmt.Sprintf("~%d years ago", years)
	}
}
func EvidenceSegments(text string) []string {
	text = UnwrapHardWraps(StripURLs(text))
	var segments []string
	var current strings.Builder
	flush := func() {
		if segment := OneLine(current.String()); segment != "" {
			segments = append(segments, segment)
		}
		current.Reset()
	}
	for _, r := range text {
		current.WriteRune(r)
		switch r {
		case '\n', '.', '!', '?', ';':
			flush()
		}
	}
	flush()
	return segments
}
func isSignatureDelimiter(line string) bool {
	t := strings.TrimRight(line, " \t")
	return t == "--" || t == "-"
}
func IsQuotedReplyLine(line string) bool { return quotedReplyLine.MatchString(line) }
func SenderAuthoredBody(text string) string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		lower := strings.ToLower(strings.TrimSpace(line))
		if quotedReplyLine.MatchString(line) || isSignatureDelimiter(line) {
			break
		}
		stop := false
		for _, marker := range quotedBlockMarkers {
			if strings.Contains(lower, marker) {
				stop = true
				break
			}
		}
		if stop {
			break
		}
		if strings.HasPrefix(strings.TrimSpace(line), ">") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
func StripSpeakerPrefix(segment string) string {
	return strings.TrimSpace(speakerPrefix.ReplaceAllString(segment, ""))
}
func IsForwardedSubject(title string) bool {
	lower := strings.ToLower(strings.TrimSpace(title))
	return strings.HasPrefix(lower, "fwd:") || strings.HasPrefix(lower, "fw:")
}
func IsLeadInFragment(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || strings.HasSuffix(t, ":") {
		return true
	}
	return len(strings.Fields(t)) < 3
}
func StripURLs(text string) string { return urlPattern.ReplaceAllString(text, " ") }
func UnwrapHardWraps(text string) string {
	lines := strings.Split(text, "\n")
	var out strings.Builder
	for i, line := range lines {
		out.WriteString(line)
		if i == len(lines)-1 {
			break
		}
		trimmed := strings.TrimRight(line, " \t")
		next := strings.TrimLeft(lines[i+1], " \t")
		if ContinuesSentence(trimmed, next) {
			out.WriteByte(' ')
		} else {
			out.WriteByte('\n')
		}
	}
	return out.String()
}
func ContinuesSentence(line, next string) bool {
	if line == "" || next == "" {
		return false
	}
	switch line[len(line)-1] {
	case '.', '!', '?', ';', ':', '|', '-', '*':
		return false
	}
	r := rune(next[0])
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}
func ContainsPhrase(text, phrase string) bool {
	if phrase == "" {
		return false
	}
	for start := 0; start < len(text); {
		i := strings.Index(text[start:], phrase)
		if i < 0 {
			return false
		}
		i += start
		end := i + len(phrase)
		okBefore := i == 0 || !isWordByte(text[i-1]) || !isWordByte(phrase[0])
		okAfter := end == len(text) || !isWordByte(text[end]) || !isWordByte(phrase[len(phrase)-1])
		if okBefore && okAfter {
			return true
		}
		start = i + 1
	}
	return false
}
func isWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}
func ContainsAnyPhrase(text string, phrases []string) bool {
	for _, phrase := range phrases {
		if ContainsPhrase(text, phrase) {
			return true
		}
	}
	return false
}
func ContainsPersonalTrivia(text string) bool {
	return ContainsAnyPhrase(strings.ToLower(text), personalTriviaPhrases)
}
func StripNoiseTokens(text string) string {
	kept := make([]string, 0, 8)
	for _, tok := range strings.Fields(text) {
		if !TokenIsNoise(tok) {
			kept = append(kept, tok)
		}
	}
	return strings.Join(kept, " ")
}
func TokenIsNoise(tok string) bool {
	lower := strings.ToLower(tok)
	for _, marker := range urlNoiseMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	slashes := 0
	for i := 1; i+1 < len(tok); i++ {
		a, b := tok[i-1], tok[i+1]
		if tok[i] == '/' && isLetter(a) && isLetter(b) {
			slashes++
		}
	}
	if slashes >= 2 {
		return true
	}
	letters := 0
	for _, r := range tok {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			letters++
		}
	}
	total := len([]rune(tok))
	return total >= 8 && letters*2 < total
}
func isLetter(b byte) bool { return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') }
func PersonalTriviaOnly(text string, materialPhrases []string) bool {
	lower := strings.ToLower(text)
	return ContainsPersonalTrivia(lower) && !ContainsAnyPhrase(lower, materialPhrases)
}
