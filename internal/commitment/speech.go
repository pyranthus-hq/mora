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

// ClassifySpeech decides whether an utterance creates an obligation and who owns it.
func ClassifySpeech(text string, speech SpeechContext) (owner Atom, direction Direction, ok bool) {
	text = oneLine(text)
	lower := strings.ToLower(text)
	if text == "" || !atomPresent(speech.Self) || !atomPresent(speech.Counterparty) {
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
	return speechContainsAny(lower, []string{"send", "share", "review", "confirm", "sign", "bring", "upload", "deliver", "call", "follow up", "get back", "organize", "archive", "initial", "choose", "return", "introduce", "leave", "export", "provide", "finish", "prepare", "add", "post", "text", "count", "hold", "reserve", "log"})
}

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
