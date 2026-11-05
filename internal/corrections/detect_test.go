package corrections

import (
	"testing"

	"github.com/pyranthus-hq/mora/internal/sessions"
)

// The two text rows of section 3.7 never become a Denial. Their counts come
// from Stats, so TestStatsRowsReachNotCounted covers them.
func TestClassify(t *testing.T) {
	cases := []struct {
		name       string
		d          sessions.Denial
		kind, drop string
	}{
		{"rejected", sessions.Denial{Kind: "user-rejected"}, KindRejected, ""},
		{"rejected in a subagent counts", sessions.Denial{Kind: "user-rejected", Subagent: true}, KindRejected, ""},
		{"rejected ignores origin", sessions.Denial{Kind: "user-rejected", NextOrigin: "peer", Shutdown: true}, KindRejected, ""},
		{"interrupted", sessions.Denial{Kind: "interrupted"}, KindInterrupted, ""},
		{"interrupted, next prompt human", sessions.Denial{Kind: "interrupted", NextOrigin: "human"}, KindInterrupted, ""},
		{"interrupted in a subagent", sessions.Denial{Kind: "interrupted", Subagent: true}, "", InterruptedInSubagent},
		{"interrupted by shutdown", sessions.Denial{Kind: "interrupted", Shutdown: true}, "", InterruptedNotHuman},
		{"interrupted, next prompt peer", sessions.Denial{Kind: "interrupted", NextOrigin: "peer"}, "", InterruptedNotHuman},
		{"interrupted, next prompt task", sessions.Denial{Kind: "interrupted", NextOrigin: "task_notification"}, "", InterruptedNotHuman},
		{"cancelled", sessions.Denial{Kind: "cancelled"}, "", CancelledSiblings},
		{"rule", sessions.Denial{Kind: "permission-rule"}, "", BlockedByRule},
		{"auto mode blocked", sessions.Denial{Kind: "automode-blocked"}, "", BlockedByAutoMode},
		{"auto mode unavailable", sessions.Denial{Kind: "automode-unavailable"}, "", BlockedByAutoMode},
		{"auto mode parse error", sessions.Denial{Kind: "automode-parsing-error"}, "", BlockedByAutoMode},
		{"no typed field", sessions.Denial{Kind: sessions.KindUntypedRejection}, "", UntypedRejections},
		{"between tool calls", sessions.Denial{Kind: sessions.KindBetweenCalls}, "", InterruptedBetweenCalls},
		{"unknown", sessions.Denial{Kind: "future-kind"}, "", UnknownDenialKinds},
		{"empty", sessions.Denial{}, "", UnknownDenialKinds},
	}
	for _, c := range cases {
		kind, drop := Classify(c.d)
		if kind != c.kind || drop != c.drop {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, kind, drop, c.kind, c.drop)
		}
	}
}

// The per-line counts in Stats include copies and old lines, so the report must
// not use them.
func TestRawStatsDoNotReachNotCounted(t *testing.T) {
	r := NewCorpus(zeroTime).Report(sessions.Stats{InterruptedBetweenCalls: 2, UntypedRejections: 3, CodexTurnInterrupts: 4, CodexPatchesDeclined: 5})
	if r.NotCounted != (NotCounted{}) || r.Read.Stats != (sessions.Stats{}) {
		t.Errorf("NotCounted = %+v, Read = %+v", r.NotCounted, r.Read)
	}
}
