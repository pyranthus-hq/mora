package urgency

import (
	"github.com/pyranthus-hq/mora/internal/memory"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestUrgentSnippetAnchorsOnDeadlinePhrase(t *testing.T) {
	body := "From: alice@acme.com\n\nHi — hope you're well. " + strings.Repeat("filler ", 40) +
		"Please sign the MSA by end of day today. " + strings.Repeat("tail ", 40) + "Thanks, Alice"
	s := urgentSnippet(body, 120, "by end of day")
	if !strings.Contains(strings.ToLower(s), "by end of day") {
		t.Fatalf("deadline-anchored snippet must include the deadline phrase; got %q", s)
	}
	if strings.Contains(s, "Thanks, Alice") {
		t.Fatalf("anchored snippet must not degrade to the sign-off tail; got %q", s)
	}
}

func TestUrgentSnippetFallsBackToHead(t *testing.T) {
	body := "The actual ask is right here at the top. " + strings.Repeat("later ", 100)
	s := urgentSnippet(body, 60, "")
	if !strings.HasPrefix(strings.TrimSpace(s), "The actual ask") {
		t.Fatalf("with no phrase, the snippet must lead with the head; got %q", s)
	}
}

func TestUrgentSnippetStripsFromPrefix(t *testing.T) {
	body := "From: alice@acme.com\n\nThe deadline is tomorrow morning."
	s := urgentSnippet(body, 200, "")
	if strings.HasPrefix(s, "From:") {
		t.Fatalf("snippet must strip the leading From: envelope line; got %q", s)
	}
}

func TestUrgentSnippetStripsDisplayNameFrom(t *testing.T) {
	body := "From: Jane Smith <jane@client.com>\n\nThe deadline is tomorrow, please review."
	s := urgentSnippet(body, 200, "")
	if strings.Contains(s, "jane@client.com") || strings.Contains(s, "Jane Smith") {
		t.Fatalf("snippet must strip the WHOLE From line (display name + address); got %q", s)
	}
	if !strings.HasPrefix(strings.TrimSpace(s), "The deadline") {
		t.Fatalf("snippet must lead with the body; got %q", s)
	}
}

func TestMatchDeadlinePhraseIgnoresNegation(t *testing.T) {
	if got := matchDeadlinePhrase("Heads up", "this is not urgent, no rush at all"); got != "" {
		t.Fatalf("negated urgency must not match; got %q", got)
	}
	if got := matchDeadlinePhrase("Re", "please sign by end of day today"); got == "" {
		t.Fatalf("a genuine deadline phrase must still match")
	}
	// Word boundary: "urgent" inside "insurgents" must not match.
	if got := matchDeadlinePhrase("News", "the insurgents advanced overnight"); got != "" {
		t.Fatalf("a phrase embedded in a larger word must not match; got %q", got)
	}
}

func TestExportedUrgencySurface(t *testing.T) {
	m := memory.Memory{Meta: map[string]any{"labels": []any{"UNREAD", "IMPORTANT", "STARRED", 3}}}
	u, i, s := Labels(m)
	if !u || !i || !s || Score(m, "deadline") != 8 {
		t.Fatalf("labels=(%v,%v,%v) score=%d", u, i, s, Score(m, "deadline"))
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if !Within(now.Add(-time.Hour), now) || Within(time.Time{}, now) {
		t.Fatal("recency changed")
	}
	if MatchDeadline("Urgent approval", "") != "urgent" {
		t.Fatal("match changed")
	}
	if Snippet("body", 0, "") != "body" || StripFromLine("From: X\n\nbody") != "body" || !IsWordByte('a') || IsWordByte('-') {
		t.Fatal("text helpers changed")
	}
}

func TestUrgencyHelperEdges(t *testing.T) {
	cases := []struct {
		in   any
		want []string
	}{{[]string{"", "A"}, []string{"A"}}, {[]any{"B", "", 3}, []string{"B"}}, {"C", []string{"C"}}, {"", nil}, {3, nil}}
	for _, tc := range cases {
		if got := metaStrings(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("metaStrings(%v)=%v", tc.in, got)
		}
	}
	if stripFromLine("plain") != "plain" || stripFromLine("From: sender") != "" {
		t.Fatal("strip edge changed")
	}
	if runeIndexFold([]rune("Élan URGENT"), "") != 0 || runeIndexFold([]rune("Élan URGENT"), "urgent") != 5 || runeIndexFold([]rune("body"), "missing") != -1 {
		t.Fatal("rune search changed")
	}
	long := strings.Repeat("x", 210)
	if got := urgentSnippet(long, 0, ""); len([]rune(got)) != 201 || !strings.HasSuffix(got, "…") {
		t.Fatalf("default clip len=%d", len([]rune(got)))
	}
}
