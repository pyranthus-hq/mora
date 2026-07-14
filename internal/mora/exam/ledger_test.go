package exam

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func hasNamedError(err error, name string) bool {
	if err == nil {
		return false
	}
	want := "ERR_" + strings.ToUpper(name) + " [" + name + "]:"
	return strings.HasPrefix(err.Error(), want)
}

func cloneLedger(t *testing.T, in Ledger) Ledger {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Ledger
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func validTestLedger() Ledger {
	meetingID := "calendar_event/exam-test"
	return Ledger{
		Version: 1,
		AsOf:    "2026-07-14T12:00:00Z",
		Self:    Identity{ID: "p/self", Display: "Alex Morgan", Emails: []string{"alex@example.com"}, Handles: []string{"+15550100101"}},
		People: []Identity{
			{ID: "p/sam", Display: "Sam Rivera", Emails: []string{"sam@example.org"}, Handles: []string{"+15550100102"}},
			{ID: "p/dana", Emails: []string{"dana@example.net"}, Handles: []string{"+15550100137"}},
		},
		Artifacts: []Artifact{
			{
				ID: "a/gmail-thread", MemoryID: "gmail_thread/exam-thread", Channel: "gmail", Subject: "Review packet", OccurredAt: "2026-07-10T10:00:00Z",
				Messages: []Message{
					{ID: "m1", From: "p/sam", To: []string{"p/self"}, At: "2026-07-10T09:00:00Z", Body: []Block{{ID: "b1", Kind: "authored", Text: "Can you send the review packet before Friday?"}, {ID: "b2", Kind: "quoted_reply", Text: "Earlier request: archive the old packet."}, {ID: "b3", Kind: "forwarded", Text: "-----Original Message-----\nA forwarded status note."}, {ID: "b4", Kind: "footer", Text: "This message is for the intended recipient."}}},
					{ID: "m2", From: "p/self", To: []string{"p/sam"}, At: "2026-07-10T10:00:00Z", Body: []Block{{ID: "b1", Kind: "authored", Text: "I will send it before Friday."}}},
				},
			},
			{
				ID: "a/imessage-chat", MemoryID: "imessage_chat/exam-chat", Channel: "imessage", Subject: "+15550100137", OccurredAt: "2026-07-11T10:00:00Z", Participants: []string{"p/dana"},
				Messages: []Message{
					{ID: "m1", From: "p/dana", At: "2026-07-11T09:00:00Z", Body: []Block{{ID: "b1", Kind: "authored", Text: "I will confirm the room tomorrow."}}},
					{ID: "m2", From: "p/self", At: "2026-07-11T10:00:00Z", Body: []Block{{ID: "b1", Kind: "authored", Text: "Thanks, that works."}}},
				},
			},
			{
				ID: "a/calendar", MemoryID: meetingID, Channel: "calendar", Subject: "Exam review", OccurredAt: "2026-07-14T11:30:00Z",
				Messages: []Message{{ID: "m1", From: "p/sam", To: []string{"p/self", "p/sam", "p/dana"}, At: "2026-07-14T11:00:00Z", Body: []Block{{ID: "b1", Kind: "notification", Text: "Review the packet together."}}}},
			},
			{
				ID: "a/note", MemoryID: "exam-note-closure", Channel: "notes", Subject: "Packet delivered", OccurredAt: "2026-07-12T10:00:00Z",
				Messages: []Message{{ID: "m1", From: "p/self", At: "2026-07-12T10:00:00Z", Body: []Block{{ID: "b1", Kind: "authored", Text: "The review packet was delivered to Sam."}}}},
			},
		},
		Commitments: []Commitment{
			{ID: "c/self-open", Owner: "p/self", Counterparty: "p/sam", Direction: "owed_by_self", Summary: "Send packet", OpenedBy: Span{ArtifactID: "a/gmail-thread", MessageID: "m1", BlockID: "b1", Quote: "Can you send the review packet before Friday?"}, DueAt: "2026-07-11T17:00:00Z", DueKind: "relative", State: "open", ExpectedIn: []string{"meeting:" + meetingID}},
			{ID: "c/other-open", Owner: "p/dana", Counterparty: "p/self", Direction: "owed_by_counterparty", Summary: "Confirm room", OpenedBy: Span{ArtifactID: "a/imessage-chat", MessageID: "m1", BlockID: "b1", Quote: "I will confirm the room tomorrow."}, DueKind: "relative", State: "open"},
			{ID: "c/closed", Owner: "p/self", Counterparty: "p/sam", Direction: "owed_by_self", Summary: "Deliver packet", OpenedBy: Span{ArtifactID: "a/gmail-thread", MessageID: "m2", BlockID: "b1", Quote: "I will send it before Friday."}, DueKind: "none", State: "closed", Transitions: []Transition{{To: "closed", At: "2026-07-12T10:00:00Z", Evidence: Span{ArtifactID: "a/note", MessageID: "m1", BlockID: "b1", Quote: "The review packet was delivered to Sam."}}}},
		},
	}
}

type validatorMutation struct {
	rule   string
	mutate func(*Ledger)
}

func validatorMutations() []validatorMutation {
	return []validatorMutation{
		{RuleIdentity, func(l *Ledger) { l.Commitments[0].Owner = "p/missing" }},
		{RuleTimestamp, func(l *Ledger) { l.Artifacts[0].Messages[0].At = "not-a-time" }},
		{RuleTransition, func(l *Ledger) { l.Commitments[2].State = "open" }},
		{RuleDirection, func(l *Ledger) { l.Commitments[0].Direction = "owed_by_counterparty" }},
		{RuleClosure, func(l *Ledger) {
			l.Commitments[2].Transitions[0].Evidence = Span{ArtifactID: "a/gmail-thread", MessageID: "m2", BlockID: "b1", Quote: "I will send it before Friday."}
		}},
		{RuleEvidenceSpan, func(l *Ledger) { l.Commitments[0].OpenedBy.Quote = "words not in the block" }},
		{RuleReplyChainQuotes, func(l *Ledger) { l.Artifacts[0].Messages[0].Body[1].Kind = "authored" }},
		{RuleSelfAttendee, func(l *Ledger) { l.Artifacts[2].Messages[0].To = []string{"p/sam", "p/dana"} }},
		{RuleOneDefectArtifact, func(l *Ledger) {
			l.NonObligations = []NonObligation{{ID: "n/masked", Span: l.Commitments[0].OpenedBy, Class: "trivia", Why: "collides with a commitment"}}
		}},
		{RuleClassBalance, func(l *Ledger) { l.Commitments[0].Owner, l.Commitments[0].Direction = "p/dana", "owed_by_counterparty" }},
		{RulePersonaHygiene, func(l *Ledger) { l.People[0].Emails[0] = "sam@company.dev" }},
		{RuleChannelGrain, func(l *Ledger) { l.Artifacts[0].Messages[0].Body[3].Text = "> removed by connector" }},
	}
}

func TestLedgerValidatorRejects(t *testing.T) {
	seen := map[string]bool{}
	for _, tt := range validatorMutations() {
		t.Run(tt.rule, func(t *testing.T) {
			l := cloneLedger(t, validTestLedger())
			tt.mutate(&l)
			err := Validate(l)
			if !hasNamedError(err, tt.rule) {
				t.Fatalf("Validate error = %v, want named rule %q", err, tt.rule)
			}
			seen[tt.rule] = true
		})
	}
	for _, rule := range RequiredValidatorRules {
		if !seen[rule] {
			t.Errorf("manifest rule %q has no rejecting row", rule)
		}
	}
}

func TestValidateZeroMessageCalendarReturnsChannelGrain(t *testing.T) {
	l := cloneLedger(t, validTestLedger())
	l.Artifacts[2].Messages = nil

	err := Validate(l)
	if !hasNamedError(err, RuleChannelGrain) {
		t.Fatalf("Validate error = %v, want named rule %q", err, RuleChannelGrain)
	}
}

func TestManifestCompleteness(t *testing.T) {
	if len(RequiredValidatorRules) != 12 {
		t.Fatalf("validator manifest has %d rules, want 12", len(RequiredValidatorRules))
	}
	want := map[string]bool{}
	for _, name := range RequiredValidatorRules {
		want[name] = true
	}
	implemented := implementedValidatorRules(t)
	for name := range implemented {
		if !want[name] {
			t.Errorf("validator emits unmanifested rule %q", name)
		}
	}
	for name := range want {
		if !implemented[name] {
			t.Errorf("manifested validator rule %q is not emitted by validate.go", name)
		}
	}
	// This is intentionally a second, manifest-owned drive of every broken row.
	// Neutering a validator branch therefore makes both the behavioral table and
	// the completeness meta-test fail by the rule's public name.
	for _, tt := range validatorMutations() {
		l := cloneLedger(t, validTestLedger())
		tt.mutate(&l)
		if err := Validate(l); !hasNamedError(err, tt.rule) {
			t.Errorf("manifested rule %q has no live rejecting implementation: %v", tt.rule, err)
		}
	}
	if len(RequiredLints) != 2 {
		t.Fatalf("lint manifest has %d entries, want 2", len(RequiredLints))
	}
}

func implementedValidatorRules(t *testing.T) map[string]bool {
	t.Helper()
	constants := map[string]string{}
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				values, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range values.Names {
					if !strings.HasPrefix(name.Name, "Rule") || i >= len(values.Values) {
						continue
					}
					literal, ok := values.Values[i].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal(err)
					}
					constants[name.Name] = value
				}
			}
		}
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "validate.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	implemented := map[string]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		fun, ok := call.Fun.(*ast.Ident)
		if !ok || fun.Name != "ruleError" {
			return true
		}
		name, ok := call.Args[0].(*ast.Ident)
		if !ok {
			t.Errorf("ruleError first argument must be a named rule constant at %s", fset.Position(call.Pos()))
			return true
		}
		value, ok := constants[name.Name]
		if !ok {
			if strings.HasPrefix(name.Name, "Rule") {
				t.Errorf("ruleError uses unknown rule constant %s", name.Name)
			}
			return true
		}
		implemented[value] = true
		return true
	})
	return implemented
}

func TestCommittedLedgerIsValid(t *testing.T) {
	l, err := Load(filepath.Join("..", "eval", "obligations-v1", "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(l); err != nil {
		t.Fatal(err)
	}
	if err := Lint(l); err != nil {
		t.Fatal(err)
	}
	var expectedFailures int
	for _, c := range l.Commitments {
		if c.ExpectedFailure != "" {
			expectedFailures++
		}
	}
	if expectedFailures < 3 {
		t.Fatalf("known expected failures = %d, want at least 3", expectedFailures)
	}
	if _, err := os.Stat(filepath.Join("..", "eval", "obligations-v1", "OBLIGATIONS.md")); err != nil {
		t.Fatal(err)
	}
}

func TestCommittedLedgerCoverage(t *testing.T) {
	l, err := Load(filepath.Join("..", "eval", "obligations-v1", "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	channels := map[string]bool{}
	blockKinds := map[string]bool{}
	hasReply, hasSelfTurn := false, false
	artifactChannel := map[string]string{}
	for _, a := range l.Artifacts {
		channels[a.Channel] = true
		artifactChannel[a.ID] = a.Channel
		hasReply = hasReply || a.Channel == "gmail" && len(a.Messages) > 1
		for _, m := range a.Messages {
			hasSelfTurn = hasSelfTurn || a.Channel == "imessage" && m.From == l.Self.ID
			for _, b := range m.Body {
				blockKinds[b.Kind] = true
			}
		}
	}
	for _, channel := range []string{"gmail", "imessage", "calendar", "notes"} {
		if !channels[channel] {
			t.Errorf("ledger lacks channel %s", channel)
		}
	}
	for _, kind := range []string{"quoted_reply", "forwarded", "footer"} {
		if !blockKinds[kind] {
			t.Errorf("ledger lacks block kind %s", kind)
		}
	}
	if !hasReply || !hasSelfTurn {
		t.Errorf("reply_chain=%v imessage_self_turn=%v", hasReply, hasSelfTurn)
	}
	hasSubject, hasCrossClosure, hasDuplicate, hasThirdParty := false, false, false, false
	hasExplicit, hasRelative, hasDaily, hasFlywheel := false, false, false, false
	for _, c := range l.Commitments {
		hasSubject = hasSubject || c.OpenedBy.MessageID == ""
		hasDuplicate = hasDuplicate || c.DuplicateOf != ""
		hasThirdParty = hasThirdParty || (c.Owner != l.Self.ID && c.Counterparty != l.Self.ID)
		hasExplicit = hasExplicit || c.DueKind == "explicit_date"
		hasRelative = hasRelative || c.DueKind == "relative"
		hasDaily = hasDaily || contains(c.ExpectedIn, "daily")
		hasFlywheel = hasFlywheel || c.ID == "c/flywheel" && c.RequiresMerge == "+15550100137|dana@example.net" && artifactChannel[c.OpenedBy.ArtifactID] == "imessage"
		for _, tr := range c.Transitions {
			hasCrossClosure = hasCrossClosure || artifactChannel[c.OpenedBy.ArtifactID] != artifactChannel[tr.Evidence.ArtifactID]
		}
	}
	if !hasSubject || !hasCrossClosure || !hasDuplicate || !hasThirdParty || !hasExplicit || !hasRelative || !hasDaily || !hasFlywheel {
		t.Fatalf("coverage subject=%v cross_closure=%v duplicate=%v third_party=%v explicit=%v relative=%v daily=%v flywheel=%v", hasSubject, hasCrossClosure, hasDuplicate, hasThirdParty, hasExplicit, hasRelative, hasDaily, hasFlywheel)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestCommittedBrokenFixtures(t *testing.T) {
	base, err := Load(filepath.Join("..", "eval", "obligations-v1", "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("masked-artifact", func(t *testing.T) {
		var fixture struct {
			NonObligation NonObligation `json:"non_obligation"`
		}
		b, err := os.ReadFile(filepath.Join("testdata", "masked-artifact.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &fixture); err != nil {
			t.Fatal(err)
		}
		l := cloneLedger(t, base)
		l.NonObligations = append(l.NonObligations, fixture.NonObligation)
		if err := Validate(l); !hasNamedError(err, RuleOneDefectArtifact) {
			t.Fatalf("Validate error = %v, want %s", err, RuleOneDefectArtifact)
		}
	})
	t.Run("skewed-classes", func(t *testing.T) {
		var fixture struct {
			Owner     string `json:"owner"`
			Direction string `json:"direction"`
		}
		b, err := os.ReadFile(filepath.Join("testdata", "skewed-classes.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &fixture); err != nil {
			t.Fatal(err)
		}
		l := cloneLedger(t, base)
		for i := range l.Commitments {
			if l.Commitments[i].State == "open" && l.Commitments[i].DuplicateOf == "" {
				l.Commitments[i].Owner = fixture.Owner
				l.Commitments[i].Direction = fixture.Direction
				if l.Commitments[i].Counterparty == fixture.Owner {
					l.Commitments[i].Counterparty = "p/sam"
				}
			}
		}
		if err := Validate(l); !hasNamedError(err, RuleClassBalance) {
			t.Fatalf("Validate error = %v, want %s", err, RuleClassBalance)
		}
	})
}
