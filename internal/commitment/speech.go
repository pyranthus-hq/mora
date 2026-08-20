package commitment

import (
	"regexp"
	"strings"
)

// SpeechContext contains stable caller-established parties for one normalized utterance.
type SpeechContext struct {
	Author, Addressee, Self, Counterparty Atom
	ReportedActor                         *Atom
}

var firstPersonCommitmentPhrases = []string{"i'll ", "i'd ", "i will ", "i owe ", "i need to ", "i should ", "i promised ", "let me ", "i can send", "i can share", "i can introduce", "i'll follow up", "i will follow up", "i'll get back", "i will get back"}
var directRequestPhrases = []string{"can you ", "could you ", "would you ", "please send", "please share", "please review", "please confirm", "please sign", "please introduce", "please add", "need your approval", "needs your approval", "need your sign-off", "waiting for your", "get back to me", "when can you", "do you mind"}
var manualPromiseRE = regexp.MustCompile(`(?i)\bi told\s+((?:[\p{L}\p{N}_.’'\-]+\s+){0,3}[\p{L}\p{N}_.’'\-]+)\s+i(?:['’]d|\s+would)\s+`)

// hypotheticalRE matches a question ABOUT a hypothetical action ("what
// would you do if...", "how would you use this") rather than a request FOR one. The
// interrogative "what/how" ahead of the modal is the tell: a real request opens
// directly with the modal ("Would you send...", "Could you confirm...").
var hypotheticalRE = regexp.MustCompile(`(?i)\b(what|how)\s+(would|will|could)\s+(you|we|i|they)\b`)

// conditionalFeatureRE matches product-research framing: a hypothetical
// feature introduced by "if we/you/I added/built/had/shipped..." This is market
// research about a feature that does not exist, not a task handed to anyone.
var conditionalFeatureRE = regexp.MustCompile(`(?i)\bif\s+(we|you|i)\s+(added|add|build|built|had|have|made|make|create|created|shipped|ship|launched|launch|introduced|introduce|offered|offer)\b`)

// isHypothetical reports whether text poses a hypothetical or
// research-style question about a possible future rather than requesting or
// promising a concrete action. "What would you do if I asked you to send the
// deck?" and "If we added this, what would you use it for?" must never open a
// commitment merely because they contain a request/promise phrase as a substring.
func isHypothetical(lower string) bool {
	if hypotheticalRE.MatchString(lower) || conditionalFeatureRE.MatchString(lower) {
		return true
	}
	return speechContainsAny(lower, []string{
		"hypothetically", "purely hypothetical", "just curious what", "just curious how",
		"out of curiosity", "as a thought experiment",
	})
}

// isQuotedExcerpt reports whether text is substantially a quoted excerpt
// (opens with a quotation mark that closes over most of the sentence) rather than
// the speaker's own words. Pasting a copied passage into a conversation ("'We need
// your approval before Friday' — from the vendor's note") must not be attributed as
// the paster's own request or promise merely because it appears in their turn.
func isQuotedExcerpt(text string) bool {
	t := strings.TrimSpace(text)
	openers := map[string]string{"\"": "\"", "“": "”"}
	for open, close := range openers {
		if !strings.HasPrefix(t, open) {
			continue
		}
		rest := t[len(open):]
		idx := strings.LastIndex(rest, close)
		if idx < 0 {
			continue
		}
		quoted := strings.TrimSpace(rest[:idx])
		if len(strings.Fields(quoted)) >= 4 && float64(len(quoted)) >= 0.6*float64(len(t)) {
			return true
		}
	}
	return false
}

var (
	mailGreetingRE = regexp.MustCompile(`(?i)^(hi|hello|dear)\b.*[,!]$`)
	mailSignoffRE  = regexp.MustCompile(`(?i)^(best|best regards|regards|thanks|thank you|sincerely|warmly)[,!]?$`)
)

// PastedCorrespondence recognizes a whole email-like artifact pasted
// into a chat. The chat envelope proves who pasted the text, not who authored the
// request inside it, so fail closed unless later provenance identifies that author.
// Requiring multiple structural signals avoids suppressing ordinary short messages.
func PastedCorrespondence(text string) bool {
	if !strings.Contains(text, "\n") {
		return false
	}
	var lines []string
	headerCount := 0
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		lines = append(lines, line)
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "from:") || strings.HasPrefix(lower, "to:") ||
			strings.HasPrefix(lower, "cc:") || strings.HasPrefix(lower, "subject:") {
			headerCount++
		}
		if strings.Contains(lower, "forwarded message") {
			return true
		}
	}
	if headerCount >= 2 {
		return true
	}
	if len(lines) < 4 || len(strings.Fields(text)) < 10 || !mailGreetingRE.MatchString(lines[0]) {
		return false
	}
	start := len(lines) - 3
	if start < 1 {
		start = 1
	}
	for _, line := range lines[start:] {
		if mailSignoffRE.MatchString(line) {
			return true
		}
	}
	return false
}

func isRetrospectiveAnalysis(lower string) bool {
	if speechContainsAny(lower, directRequestPhrases) ||
		speechContainsAny(lower, []string{"i'll ", "i will ", "let me ", "i promised "}) {
		return false
	}
	return speechContainsAny(lower, []string{
		"problems we have had", "problems we've had", "issues we have had", "issues we've had",
		"what went wrong", "root cause", "the problem was", "the issue was",
		"had trouble with", "was not working", "wasn't working", "did not work", "didn't work",
	})
}

// ClassifySpeech decides whether an utterance creates an obligation and who owns it.
func ClassifySpeech(text string, speech SpeechContext) (owner Atom, direction Direction, ok bool) {
	text = oneLine(text)
	lower := strings.ToLower(text)
	if text == "" || !atomPresent(speech.Self) || !atomPresent(speech.Counterparty) {
		return Atom{}, "", false
	}
	if isHypothetical(lower) || isQuotedExcerpt(text) || isRetrospectiveAnalysis(lower) {
		return Atom{}, "", false
	}
	if speech.ReportedActor != nil && atomPresent(*speech.ReportedActor) {
		owner = *speech.ReportedActor
	} else if DirectRequest(lower) || speechContainsAny(lower, []string{"please bring", "needs your ", "still needs your "}) {
		if !atomPresent(speech.Addressee) {
			return Atom{}, "", false
		}
		owner = speech.Addressee
	} else if FirstPersonCommitment(lower) {
		if !atomPresent(speech.Author) || nonActionableAcknowledgement(lower) {
			return Atom{}, "", false
		}
		owner = speech.Author
	} else {
		return Atom{}, "", false
	}
	switch {
	case atomEqual(owner, speech.Self):
		return owner, OwedBySelf, true
	case atomEqual(owner, speech.Counterparty):
		return owner, OwedByCounterparty, true
	default:
		return Atom{}, "", false
	}
}

// FirstPersonCommitment reports whether text contains an actionable explicit promise.
func FirstPersonCommitment(text string) bool {
	lower := strings.ToLower(text)
	if !speechContainsAny(lower, firstPersonCommitmentPhrases) {
		return false
	}
	if speechContainsAny(lower, []string{"i'd already ", "i'd previously ", "i'd just "}) {
		return false
	}
	// "I should/I'll/I'd add that ..." is the discourse sense of "add" (introducing
	// a remark, as in analysis or meeting notes) not the action sense ("I'll add the
	// invoice"). Only the complementizer "that" tells them apart.
	if discourseAddRE.MatchString(lower) {
		return false
	}
	return speechContainsAny(lower, []string{"send", "share", "review", "confirm", "sign", "bring", "upload", "deliver", "call", "follow up", "get back", "organize", "archive", "initial", "choose", "return", "introduce", "leave", "export", "provide", "finish", "prepare", "add", "post", "text", "count", "hold", "reserve", "log"})
}

var discourseAddRE = regexp.MustCompile(`(?i)\bi(?:'ll|'d| will| should| can) add that\b`)

// DirectRequest reports whether text contains an explicit request.
func DirectRequest(text string) bool {
	return speechContainsAny(strings.ToLower(text), directRequestPhrases)
}

// ManualPromise recognizes locally authored "I told X I'd ..." promises.
func ManualPromise(text string) bool {
	lower := strings.ToLower(oneLine(text))
	if !manualPromiseRE.MatchString(lower) || speechContainsAny(lower, []string{" i would have ", " i would already ", " i would previously "}) {
		return false
	}
	return FirstPersonCommitment(strings.Replace(lower, " i would ", " i will ", 1))
}

// ManualPromiseCounterpartyLabel returns the explicitly named counterparty.
func ManualPromiseCounterpartyLabel(text string) string {
	match := manualPromiseRE.FindStringSubmatch(oneLine(text))
	if len(match) != 2 {
		return ""
	}
	return strings.Join(strings.Fields(match[1]), " ")
}

func nonActionableAcknowledgement(lower string) bool {
	trimmed := strings.TrimSpace(lower)
	if !strings.HasPrefix(trimmed, "thanks") && !strings.HasPrefix(trimmed, "thank you") && !strings.HasPrefix(trimmed, "yep") && !strings.HasPrefix(trimmed, "okay") {
		return false
	}
	return strings.Contains(trimmed, " meet you ") || strings.Contains(trimmed, " see you ")
}
func atomPresent(a Atom) bool {
	return strings.TrimSpace(a.Kind) != "" && strings.TrimSpace(a.Value) != ""
}
func speechContainsAny(text string, phrases []string) bool {
	for _, phrase := range phrases {
		if containsPhrase(text, phrase) {
			return true
		}
	}
	return false
}

func containsPhrase(text, phrase string) bool {
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
		before := i == 0 || !wordByte(text[i-1]) || !wordByte(phrase[0])
		after := end == len(text) || !wordByte(text[end]) || !wordByte(phrase[len(phrase)-1])
		if before && after {
			return true
		}
		start = i + 1
	}
	return false
}
func wordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '_'
}
