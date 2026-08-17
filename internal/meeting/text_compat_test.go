package meeting

import (
	"github.com/pyranthus-hq/mora/internal/memory"
	"testing"
	"time"
)

func TestEvidenceTextCompatibilitySurface(t *testing.T) {
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if OneLine(" a  b ") != "a b" || HistoricalText(now, "2026-01-01T00:00:00Z", "Sam", "done") == "" || HistoricalPrefix(now, "bad", "") == "unexpected" {
		t.Fatal("historical adapter")
	}
	_ = RelativeAge(now, now)
	_ = EvidenceSegments("one. two")
	_ = IsQuotedReplyLine("On Tue, A wrote:")
	_ = SenderAuthoredBody("yes\n--\nsig")
	_ = StripSpeakerPrefix("Sam: hello")
	_ = IsForwardedSubject("Fwd: x")
	_ = IsLeadInFragment("FYI:")
	_ = StripURLs("see https://example.com/x")
	_ = UnwrapHardWraps("one\ntwo")
	_ = ContinuesSentence("one", "two")
	_ = ContainsPhrase("one two", "two")
	_ = ContainsAnyPhrase("one two", []string{"two"})
	_ = ContainsPersonalTrivia("birthday")
	_ = StripNoiseTokens("hello https://x.com/a")
	_ = TokenIsNoise("https://x.com/a")
	_ = PersonalTriviaOnly("birthday", []string{"deadline"})
	_ = SelectNextEvent([]memory.Memory{{Type: "email"}, {Type: "event", Meta: map[string]any{"occurred_at": "bad"}}}, now, nil, nil, 0, 1)
}
