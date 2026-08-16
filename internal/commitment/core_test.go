package commitment

import "testing"

type commitDue = Due

const (
	commitDueNone         = DueNone
	commitDueRelative     = DueRelative
	commitDueExplicitDate = DueExplicitDate
)

func classifyCommitmentDue(text, at string) Due { return ClassifyDue(text, at) }
func commitmentID(messageRef, blockRef string, slot int) string {
	return ID(messageRef, blockRef, slot)
}
func TestCommitmentDueClassification(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		occurredAt string
		want       commitDue
	}{
		{
			name:       "explicit calendar date",
			text:       "Can you send the signed outline by July 14?",
			occurredAt: "2026-07-12T17:00:00Z",
			want:       commitDue{Kind: commitDueExplicitDate, At: "2026-07-14"},
		},
		{
			name:       "explicit calendar date never infers a clock",
			text:       "Can you send the signed outline by July 14 at 3:30 pm?",
			occurredAt: "2026-07-12T17:00:00Z",
			want:       commitDue{Kind: commitDueExplicitDate, At: "2026-07-14"},
		},
		{
			name:       "relative deadline",
			text:       "I will confirm the sample count tomorrow.",
			occurredAt: "2026-07-12T17:00:00Z",
			want:       commitDue{Kind: commitDueRelative},
		},
		{
			name:       "anchored relative deadline",
			text:       "Please send the route cards before the review.",
			occurredAt: "2026-07-12T17:00:00Z",
			want:       commitDue{Kind: commitDueRelative},
		},
		{
			name:       "relative condition",
			text:       "I will share the access code when I reach the desk.",
			occurredAt: "2026-07-12T17:00:00Z",
			want:       commitDue{Kind: commitDueRelative},
		},
		{
			name:       "named session anchor",
			text:       "For the north gallery handoff session, please bring the lighting plan.",
			occurredAt: "2026-07-12T17:00:00Z",
			want:       commitDue{Kind: commitDueRelative},
		},
		{
			name:       "urgency does not invent a deadline",
			text:       "Please send the receipt urgently.",
			occurredAt: "2026-07-12T17:00:00Z",
			want:       commitDue{Kind: commitDueNone},
		},
		{
			name:       "purpose without an event anchor does not invent a deadline",
			text:       "Please bring the lighting plan for the north gallery.",
			occurredAt: "2026-07-12T17:00:00Z",
			want:       commitDue{Kind: commitDueNone},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyCommitmentDue(tt.text, tt.occurredAt); got != tt.want {
				t.Fatalf("due = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestCommitmentIDEvidenceOnly(t *testing.T) {
	const want = "commit:v1:10b7c665ae18290d686f4947d1afcf69240905e84a21df6eba0c5d36be2409c8"
	if got := commitmentID("memory#message", "block", 0); got != want {
		t.Fatalf("commitment id = %q, want %q", got, want)
	}
	if got := commitmentID("", "block", 0); got != "" {
		t.Fatalf("missing evidence minted id %q", got)
	}
}

func TestDueAndIdentityEdges(t *testing.T) {
	for _, text := range []string{"by 2026-02-29", "by February 30", "by 2026-13-01"} {
		if got := ClassifyDue(text, "2026-01-01T00:00:00Z"); got.Kind != DueNone {
			t.Fatalf("%q due=%+v", text, got)
		}
	}
	if got := ClassifyDue("by 2028-02-29", "2026-01-01T00:00:00Z"); got.At != "2028-02-29" {
		t.Fatalf("leap due=%+v", got)
	}
	if DueValue(Due{Kind: DueExplicitDate, At: "2026-01-02"}) != "2026-01-02" || DueValue(Due{Kind: DueRelative}) != DueRelative {
		t.Fatal("due value")
	}
	if ID("m", "b", -1) != "" || ID("m", "", 0) != "" {
		t.Fatal("invalid id minted")
	}
	if ID("ab", "c", 0) == ID("a", "bc", 0) {
		t.Fatal("length framing collided")
	}
}
