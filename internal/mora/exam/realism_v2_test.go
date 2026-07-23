package exam

import (
	"strings"
	"testing"
)

// fixedDraw drives the property generator deterministically so these tests can
// build one concrete ledger and mutate it per case.
func fixedDraw(label string, min, max int) int { return min }

// v2Ledger upgrades a generated v1 ledger to schema v2 and gives it the three
// shapes v2 requires: a composite artifact, a wrapped body, and an attributed
// quote.
func v2Ledger() Ledger {
	l := GenerateLedger(fixedDraw)
	l.Version = SchemaV2
	for i, a := range l.Artifacts {
		if a.ID != "a/thread" {
			continue
		}
		l.Artifacts[i].Messages[0].Wrap = 72
		l.Artifacts[i].Messages[0].Body = append(l.Artifacts[i].Messages[0].Body,
			Block{ID: "b2", Kind: "footer", Text: "Unsubscribe from these updates at any time."})
		last := len(a.Messages) - 1
		for j, b := range a.Messages[last].Body {
			if b.Kind == "quoted_reply" {
				l.Artifacts[i].Messages[last].Body[j].Attr = "On 14 Jul 2026, Person 2 <other2@example.org> wrote:"
			}
		}
	}
	l.NonObligations = append(l.NonObligations, NonObligation{
		ID: "n/thread-footer", Class: "footer", Why: "List boilerplate under a live thread.",
		Span: Span{ArtifactID: "a/thread", MessageID: "m1", BlockID: "b2", Quote: "Unsubscribe from these updates"},
	})
	return l
}

func TestSchemaV2Accepts(t *testing.T) {
	if err := Validate(v2Ledger()); err != nil {
		t.Fatalf("valid v2 ledger rejected: %v", err)
	}
}

func TestSchemaV2FieldGating(t *testing.T) {
	cases := []struct {
		name   string
		rule   string
		mutate func(*Ledger)
	}{
		{"v1 rejects attr", RuleChannelGrain, func(l *Ledger) {
			v1 := GenerateLedger(fixedDraw)
			v1.Artifacts[1].Messages[0].Body[0].Attr = "On 14 Jul 2026, Person 2 wrote:"
			v1.Artifacts[1].Messages[0].Body[0].Kind = "quoted_reply"
			*l = v1
		}},
		{"v1 rejects wrap", RuleChannelGrain, func(l *Ledger) {
			v1 := GenerateLedger(fixedDraw)
			v1.Artifacts[1].Messages[0].Wrap = 72
			*l = v1
		}},
		{"attr on authored block", RuleChannelGrain, func(l *Ledger) {
			for i, a := range l.Artifacts {
				if a.ID == "a/thread" {
					l.Artifacts[i].Messages[0].Body[0].Attr = "On 14 Jul 2026, Person 2 wrote:"
				}
			}
		}},
		{"wrap on non-gmail", RuleChannelGrain, func(l *Ledger) {
			for i, a := range l.Artifacts {
				if a.Channel == "imessage" {
					l.Artifacts[i].Messages[0].Wrap = 72
				}
			}
		}},
		{"wrap out of bounds", RuleChannelGrain, func(l *Ledger) {
			for i, a := range l.Artifacts {
				if a.ID == "a/thread" {
					l.Artifacts[i].Messages[0].Wrap = 20
				}
			}
		}},
		{"authored text faking quote syntax", RuleChannelGrain, func(l *Ledger) {
			for i, a := range l.Artifacts {
				if a.ID == "a/thread" {
					l.Artifacts[i].Messages[0].Body[0].Text += "\n> a hand-faked quote line"
				}
			}
		}},
		{"two defects on one block", RuleOneDefectArtifact, func(l *Ledger) {
			l.NonObligations = append(l.NonObligations, NonObligation{
				ID: "n/dup-span", Class: "marketing", Why: "Second label on the same block.",
				Span: Span{ArtifactID: "a/thread", MessageID: "m1", BlockID: "b2", Quote: "Unsubscribe from these updates"},
			})
		}},
		{"defect on a commitment's opening block", RuleOneDefectArtifact, func(l *Ledger) {
			l.NonObligations = append(l.NonObligations, NonObligation{
				ID: "n/on-commitment", Class: "trivia", Why: "Label on the very block that opens a commitment.",
				Span: Span{ArtifactID: "a/thread", MessageID: "m1", BlockID: "b1", Quote: "Message body 1 of the thread."},
			})
		}},
		{"missing realism shapes", RuleReplyChainQuotes, func(l *Ledger) {
			upgraded := GenerateLedger(fixedDraw)
			upgraded.Version = SchemaV2
			*l = upgraded
		}},
		{"blank attr", RuleChannelGrain, func(l *Ledger) {
			for i, a := range l.Artifacts {
				if a.ID != "a/thread" {
					continue
				}
				last := len(a.Messages) - 1
				for j, b := range a.Messages[last].Body {
					if b.Kind == "quoted_reply" {
						l.Artifacts[i].Messages[last].Body[j].Attr = "   "
					}
				}
			}
		}},
		{"multi-line attr smuggling unquoted text", RuleChannelGrain, func(l *Ledger) {
			for i, a := range l.Artifacts {
				if a.ID != "a/thread" {
					continue
				}
				last := len(a.Messages) - 1
				for j, b := range a.Messages[last].Body {
					if b.Kind == "quoted_reply" {
						l.Artifacts[i].Messages[last].Body[j].Attr = "On 14 Jul 2026, Person 2 wrote:\nnot actually quoted"
					}
				}
			}
		}},
		{"composite only via a closed commitment", RuleReplyChainQuotes, func(l *Ledger) {
			l.NonObligations = l.NonObligations[:len(l.NonObligations)-1]
			l.NonObligations = append(l.NonObligations, NonObligation{
				ID: "n/chat-tail", Class: "self_spoken", Why: "Self acknowledgement, not an ask.",
				Span: Span{ArtifactID: "a/chat", MessageID: "m2", BlockID: "b1", Quote: "Thanks, noted."},
			})
		}},
		{"wrap on a message with no authored text", RuleReplyChainQuotes, func(l *Ledger) {
			for i, a := range l.Artifacts {
				if a.ID == "a/thread" {
					l.Artifacts[i].Messages[0].Body[0].Kind = "notification"
				}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := v2Ledger()
			tc.mutate(&l)
			err := Validate(l)
			if err == nil || !strings.Contains(err.Error(), "["+tc.rule+"]") {
				t.Fatalf("want %s violation, got %v", tc.rule, err)
			}
		})
	}
}

func TestSchemaV2QuotedReplyMayCarryQuoteSyntax(t *testing.T) {
	l := v2Ledger()
	for i, a := range l.Artifacts {
		if a.ID != "a/thread" {
			continue
		}
		last := len(a.Messages) - 1
		for j, b := range a.Messages[last].Body {
			if b.Kind == "quoted_reply" {
				l.Artifacts[i].Messages[last].Body[j].Text += "\n> already-prefixed residue a client left behind"
			}
		}
	}
	if err := Validate(l); err != nil {
		t.Fatalf("quoted_reply block with quote syntax must be legal under v2: %v", err)
	}
}

func TestRenderMessageBodyV2Kinds(t *testing.T) {
	msg := Message{Body: []Block{
		{ID: "b1", Kind: "authored", Text: "Plain ask."},
		{ID: "b2", Kind: "quoted_reply", Attr: "On 14 Jul 2026, Sam wrote:", Text: "First line.\nSecond line."},
		{ID: "b3", Kind: "forwarded", Text: "The forwarded ask."},
		{ID: "b4", Kind: "signature", Text: "Sam Rivera"},
	}}
	got := renderMessageBody(msg, SchemaV2)
	want := "Plain ask.\n\n" +
		"On 14 Jul 2026, Sam wrote:\n> First line.\n> Second line.\n\n" +
		"---------- Forwarded message ---------\nThe forwarded ask.\n\n" +
		"-- \nSam Rivera"
	if got != want {
		t.Fatalf("v2 body =\n%q\nwant\n%q", got, want)
	}
	if flat := renderMessageBody(msg, SchemaV1); flat != renderBlocks(msg.Body) {
		t.Fatalf("v1 rendering must stay the frozen flat join, got %q", flat)
	}
}

func TestRenderMessageBodyV2Wrap(t *testing.T) {
	long := "This sentence is deliberately much longer than forty columns so the greedy wrapper has to break it."
	msg := Message{Wrap: 40, Body: []Block{
		{ID: "b1", Kind: "authored", Text: long},
		{ID: "b2", Kind: "quoted_reply", Text: long},
	}}
	got := renderMessageBody(msg, SchemaV2)
	parts := strings.Split(got, "\n\n")
	if len(parts) != 2 {
		t.Fatalf("expected two rendered blocks, got %d", len(parts))
	}
	for _, line := range strings.Split(parts[0], "\n") {
		if len(line) > 40 {
			t.Fatalf("authored line exceeds wrap width: %q", line)
		}
	}
	if strings.Count(parts[0], "\n") == 0 {
		t.Fatal("authored block was not wrapped")
	}
	if !strings.Contains(parts[1], "> "+long) {
		t.Fatal("quoted block must not be wrapped — only authored text carries the client's wrap")
	}
}

func TestWrapTextLeavesFittingLinesVerbatim(t *testing.T) {
	odd := "spacing  kept   intact\tand tabs too"
	if got := wrapText(odd, 72); got != odd {
		t.Fatalf("a line that already fits must pass through verbatim, got %q", got)
	}
}

func TestRendererVersionForSchemas(t *testing.T) {
	if RendererVersionFor(SchemaV1) != RendererVersion {
		t.Fatal("schema v1 must keep the frozen renderer name")
	}
	if RendererVersionFor(SchemaV2) == RendererVersion {
		t.Fatal("schema v2 must not reuse the frozen v1 renderer name")
	}
}

func TestLintDateFingerprint(t *testing.T) {
	l := v2Ledger()
	if err := LintDateFingerprint(l); err != nil {
		t.Fatalf("interleaved dates flagged: %v", err)
	}
	// Push every negative artifact earlier than every positive one.
	positives := map[string]bool{}
	for _, c := range l.Commitments {
		if c.State == "open" && c.DuplicateOf == "" {
			positives[c.OpenedBy.ArtifactID] = true
		}
	}
	for i, a := range l.Artifacts {
		if !positives[a.ID] {
			l.Artifacts[i].OccurredAt = "2026-06-01T10:00:00Z"
		} else {
			l.Artifacts[i].OccurredAt = "2026-07-10T10:00:00Z"
		}
	}
	if err := LintDateFingerprint(l); err == nil || !strings.Contains(err.Error(), LintDateLeak) {
		t.Fatalf("disjoint class date ranges must be rejected, got %v", err)
	}
}

func TestLintTitleFingerprint(t *testing.T) {
	l := v2Ledger()
	if err := LintTitleFingerprint(l); err != nil {
		t.Fatalf("clean subjects flagged: %v", err)
	}
	positives := map[string]bool{}
	for _, c := range l.Commitments {
		if c.State == "open" && c.DuplicateOf == "" {
			positives[c.OpenedBy.ArtifactID] = true
		}
	}
	for i, a := range l.Artifacts {
		if positives[a.ID] {
			l.Artifacts[i].Subject = a.Subject + " pending"
		}
	}
	if err := LintTitleFingerprint(l); err == nil || !strings.Contains(err.Error(), LintTitleLeak) {
		t.Fatalf("a subject token marking every positive must be rejected, got %v", err)
	}
}
