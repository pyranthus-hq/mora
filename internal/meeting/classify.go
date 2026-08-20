package meeting

import (
	identitypkg "github.com/pyranthus-hq/mora/internal/identity"
	"strings"
	"time"

	commitmentpkg "github.com/pyranthus-hq/mora/internal/commitment"
	graphpkg "github.com/pyranthus-hq/mora/internal/graph"
	"github.com/pyranthus-hq/mora/internal/memory"
)

const (
	OpenLoops     = "open_loops"
	Unresolved    = "unresolved_threads"
	Staleness     = "staleness_guards"
	SharedContext = "material_shared_context"
)

var meetingNotificationSubjects = []string{"invitation:", "updated invitation:", "declined:", "accepted:", "tentative:", "canceled:", "cancelled:", "rescheduled:", "reminder:", "notification:"}
var meetingNotificationBodyMarkers = []string{"has declined this invitation", "has accepted this invitation", "you have been invited to the following event", "view all guest info", "join with google meet", "microsoft teams meeting", "join zoom meeting", "join by phone", "meeting id:", "passcode:", "rsvp to this event"}
var thirdPartyAssignmentPrefixes = []string{"action item for", "action items for", "todo for", "to do for"}
var unresolvedThreadPhrases = []string{"still waiting", "not decided", "haven't decided", "have not decided", "unresolved", "open question", "tbd", "pending", "blocked on", "need to decide", "decide whether", "circle back", "follow up", "follow-up", "next steps", "awaiting"}
var stalenessGuardPhrases = []string{"moved to ", "moving to ", "relocated to ", "new role", "new title", "new job", "new company", "now at ", "no longer at", "no longer with", "formerly at", "formerly with", "changed roles", "changed companies", "changed jobs"}
var materialContextPhrases = []string{"decision", "proposal", "contract", "pilot", "launch", "roadmap", "budget", "pricing", "fundraising", "funding", "hiring", "partnership", "introduction", "intro", "document", "deck", "review", "approval", "deadline", "next step", "project:", "milestone"}
var nonObligationQuestionPhrases = []string{"questions about your order", "any questions", "how did we do", "how are we doing", "rate your", "your feedback", "give feedback", "leave a review", "take our survey", "was this helpful", "view in browser", "unsubscribe", "manage your subscription"}
var interrogativeOpeners = []string{"who ", "what ", "when ", "where ", "why ", "how ", "which ", "whose ", "is there", "are there", "is it", "do we", "can you", "could you", "would you", "will you", "do you", "did you", "are you", "have you", "should we", "should i", "any chance", "when can", "let me know if"}

// ClassifierInput contains the caller-owned facts needed by ClassifyEvidence.
type ClassifierInput struct {
	Memory         memory.Memory
	SignalText     string
	Self           map[string]bool
	OccurredAt, At time.Time
	ServiceOnly    bool
}

// IsMeetingNotification reports whether a memory is generated event plumbing.
func IsMeetingNotification(m memory.Memory) bool {
	title := strings.ToLower(strings.TrimSpace(m.Title))
	for _, prefix := range meetingNotificationSubjects {
		if strings.HasPrefix(title, prefix) {
			return true
		}
	}
	return ContainsAnyPhrase(strings.ToLower(m.Text), meetingNotificationBodyMarkers)
}

// SelfNameTokens derives possible user-name tokens from known self addresses.
func SelfNameTokens(self map[string]bool) map[string]bool { return identitypkg.SelfNameTokens(self) }

// AssignedToThirdParty detects an explicit assignment to someone other than self.
func AssignedToThirdParty(text string, selfNames map[string]bool) bool {
	lower := strings.ToLower(text)
	for _, prefix := range thirdPartyAssignmentPrefixes {
		idx := strings.Index(lower, prefix)
		if idx < 0 {
			continue
		}
		rest := strings.TrimLeft(lower[idx+len(prefix):], " \t*:")
		assignee := strings.FieldsFunc(rest, func(r rune) bool { return r < 'a' || r > 'z' })
		if len(assignee) == 0 {
			continue
		}
		if !selfNames[assignee[0]] {
			return true
		}
	}
	return false
}

// IsIMessage reports whether provider identity identifies an iMessage memory.
func IsIMessage(m memory.Memory) bool {
	return strings.EqualFold(m.Provider, "imessage") || strings.Contains(strings.ToLower(m.ProviderID), "imessage")
}

// IsGmail reports whether provider identity identifies a Gmail memory.
func IsGmail(m memory.Memory) bool {
	return strings.EqualFold(m.Provider, "gmail") || strings.Contains(strings.ToLower(m.ProviderID), "gmail")
}

// IsTwoPartyExchange rejects mail addressed to someone outside the meeting roster.
func IsTwoPartyExchange(m memory.Memory, self map[string]bool, attendees ...string) bool {
	inRoom := map[string]bool{}
	for addr := range self {
		inRoom[graphpkg.MailboxKey(addr)] = true
	}
	for _, a := range attendees {
		if key := graphpkg.MailboxKey(a); key != "" {
			inRoom[key] = true
		}
	}
	for _, field := range []string{"to", "cc", "bcc"} {
		for _, raw := range metaStrings(m.Meta[field]) {
			key := graphpkg.MailboxKey(strings.ToLower(strings.TrimSpace(raw)))
			if key != "" && !inRoom[key] {
				return false
			}
		}
	}
	return true
}

// FirstPersonCommitment reports whether text contains an explicit user promise.
func FirstPersonCommitment(text string) bool {
	return commitmentpkg.FirstPersonCommitment(text)
}

// DirectRequest reports whether text contains an explicit request.
func DirectRequest(text string) bool {
	return commitmentpkg.DirectRequest(text)
}

// UserAuthoredTask reports whether the memory is a locally authored task.
func UserAuthoredTask(m memory.Memory) bool {
	if !strings.EqualFold(m.Type, "task") {
		return false
	}
	return m.Provider == "" || m.Source == "manual" || m.Source == "mcp"
}

// LastConversationLine returns the final authored speaker/body pair.
func LastConversationLine(text string) (speaker, body string) {
	lines := strings.Split(text, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "*") {
			continue
		}
		label, content, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(content) == "" {
			continue
		}
		label = strings.ToLower(strings.TrimSpace(label))
		if label == "me" {
			return "me", strings.TrimSpace(content)
		}
		return "other", strings.TrimSpace(content)
	}
	return "", ""
}
func lowerStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.ToLower(strings.TrimSpace(s)))
	}
	return out
}

// ActionableQuestion excludes trivia and transactional question-shaped noise.
func ActionableQuestion(text string) bool {
	return strings.Contains(text, "?") && !PersonalTriviaOnly(text, materialContextPhrases) && !ContainsAnyPhrase(strings.ToLower(text), nonObligationQuestionPhrases)
}

// GmailActionableAsk applies the strict email-specific question gate.
func GmailActionableAsk(text string) bool {
	if !ActionableQuestion(text) {
		return false
	}
	lower := strings.ToLower(text)
	return ContainsAnyPhrase(lower, interrogativeOpeners) || DirectRequest(lower)
}

// UserOwnedOpenLoop applies provider-specific direction and ownership policy.
func UserOwnedOpenLoop(m memory.Memory, signal string, self map[string]bool) bool {
	if UserAuthoredTask(m) {
		return true
	}
	if IsIMessage(m) {
		speaker, body := LastConversationLine(m.Text)
		if body == "" || PersonalTriviaOnly(body, materialContextPhrases) {
			return false
		}
		if speaker == "me" {
			return FirstPersonCommitment(body) || strings.Contains(body, "?")
		}
		return DirectRequest(body) || strings.Contains(body, "?")
	}
	if IsGmail(m) {
		if len(self) == 0 {
			return false
		}
		senders := lowerStrings(metaStrings(m.Meta["from"]))
		recipients := append(lowerStrings(metaStrings(m.Meta["to"])), lowerStrings(metaStrings(m.Meta["cc"]))...)
		allSelf := len(senders) > 0
		anySelf := false
		for _, sender := range senders {
			if self[sender] {
				anySelf = true
			} else {
				allSelf = false
			}
		}
		toSelf := false
		for _, recipient := range recipients {
			if self[recipient] {
				toSelf = true
				break
			}
		}
		switch {
		case allSelf:
			return FirstPersonCommitment(signal) || GmailActionableAsk(signal)
		case !anySelf && toSelf:
			return DirectRequest(signal) || GmailActionableAsk(signal)
		default:
			return false
		}
	}
	if m.Provider == "" && (m.Source == "manual" || m.Source == "mcp") {
		return FirstPersonCommitment(signal)
	}
	return false
}

// EndsInActionableQuestion applies the appropriate provider question gate.
func EndsInActionableQuestion(m memory.Memory, signal string) bool {
	if IsIMessage(m) {
		_, question := LastConversationLine(m.Text)
		return ActionableQuestion(question)
	}
	if IsGmail(m) {
		return GmailActionableAsk(signal)
	}
	return ActionableQuestion(signal)
}

// PersonalTriviaOnlyMeeting applies the meeting material-context vocabulary.
func PersonalTriviaOnlyMeeting(text string) bool {
	return PersonalTriviaOnly(text, materialContextPhrases)
}

// MaterialSharedContext reports whether dated text carries material context.
func MaterialSharedContext(signal string, occurred, at time.Time) bool {
	if PersonalTriviaOnly(signal, materialContextPhrases) || !ContainsAnyPhrase(signal, materialContextPhrases) {
		return false
	}
	return occurred.IsZero() || !occurred.After(at)
}

// ClassifyEvidence deterministically assigns at most one meeting-brief section.
func ClassifyEvidence(in ClassifierInput) string {
	m := in.Memory
	if IsMeetingNotification(m) || AssignedToThirdParty(in.SignalText, SelfNameTokens(in.Self)) || in.ServiceOnly {
		return ""
	}
	if UserOwnedOpenLoop(m, in.SignalText, in.Self) {
		return OpenLoops
	}
	if ContainsAnyPhrase(in.SignalText, unresolvedThreadPhrases) || EndsInActionableQuestion(m, in.SignalText) {
		return Unresolved
	}
	if ContainsAnyPhrase(in.SignalText, stalenessGuardPhrases) {
		return Staleness
	}
	if MaterialSharedContext(in.SignalText, in.OccurredAt, in.At) {
		return SharedContext
	}
	return ""
}
