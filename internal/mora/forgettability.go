package mora

import (
	retentionpkg "github.com/pyranthus-hq/mora/internal/retention"
	"time"
)

type forgettabilityOptions = retentionpkg.Options
type forgettabilityCandidate = retentionpkg.Candidate
type forgettabilityResult = retentionpkg.Result
type forgettabilityRanking struct {
	All      []forgettabilityResult
	Selected []forgettabilityResult
	Gaps     MeetingGaps
}

func (r forgettabilityRanking) ByID(id string) forgettabilityResult {
	for _, item := range r.All {
		if item.StableID == id {
			return item
		}
	}
	return forgettabilityResult{}
}
func rankForgettability(now time.Time, eventTitle string, attendeeNames []string, candidates []forgettabilityCandidate, opts forgettabilityOptions) forgettabilityRanking {
	r := retentionpkg.Rank(now, eventTitle, attendeeNames, candidates, opts, forgettabilityPolicy())
	return forgettabilityRanking{All: r.All, Selected: r.Selected, Gaps: MeetingGaps{ThinAttendees: r.ThinAttendees}}
}

func forgettabilityPolicy() retentionpkg.Policy {
	return retentionpkg.Policy{ThinCoverageK: thinkThinK, EvidenceCap: meetingPrepEvidenceCap, Tokenize: tokenizeWords, IsStopword: func(s string) bool { return ftsStopwords[s] }}
}
func forgettabilityDistinctiveTokens(s string, names []string) map[string]bool {
	return retentionpkg.DistinctiveTokens(s, names, forgettabilityPolicy())
}
func intersectionSize(a, b map[string]bool) int { return retentionpkg.IntersectionSize(a, b) }
func parseForgettabilityTime(occurredAt, createdAt string) (time.Time, bool) {
	return retentionpkg.ParseTime(occurredAt, createdAt)
}
