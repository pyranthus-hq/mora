package corrections

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/sessions"
)

var (
	zeroTime time.Time
	t0       = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
)

func hour(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }

func bash(session, id, command string, at time.Time) sessions.Call {
	return sessions.Call{Harness: sessions.HarnessClaude, SessionID: session, ToolUseID: id, MessageID: "m-" + id,
		TS: at, FileFirst: at, Command: command}
}

func codexCall(session, id, command string, at time.Time) sessions.Call {
	return sessions.Call{Harness: sessions.HarnessCodex, SessionID: session, ToolUseID: id, TS: at, FileFirst: at, Command: command}
}

func rejected(session, id string, at time.Time) sessions.Denial {
	return sessions.Denial{SessionID: session, ToolUseID: id, MessageID: "m-" + id, TS: at, FileFirst: at,
		Kind: "user-rejected", Tool: "Bash"}
}

func ptr(f float64) *float64 { return &f }

// fixture is what Scan reads from the shared synthetic home.
type fixture struct {
	calls   []sessions.Call
	denials []sessions.Denial
	st      sessions.Stats
}

func (f *fixture) Call(c sessions.Call)     { f.calls = append(f.calls, c) }
func (f *fixture) Denial(d sessions.Denial) { f.denials = append(f.denials, d) }

func scanFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	for from, to := range map[string]string{"claude": ".claude", "codex": ".codex"} {
		src := filepath.Join("..", "sessions", "testdata", "home", from)
		if err := os.CopyFS(filepath.Join(home, to), os.DirFS(src)); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{}
	st, err := sessions.Scan(context.Background(), sessions.Options{Home: home, Getenv: func(string) string { return "" }}, f)
	if err != nil {
		t.Fatal(err)
	}
	f.st = st
	return f
}

func checkFixtureReport(t *testing.T, r Report) {
	t.Helper()
	if want := (Window{First: "2026-10-01", Last: "2026-10-04"}); r.Window != want {
		t.Errorf("Window = %+v", r.Window)
	}
	harnesses := []Harness{
		{"claude", 5, 1, 18, "typed"},
		{"codex", 1, 0, 2, "no_rejection_signal"},
	}
	if !reflect.DeepEqual(r.Harnesses, harnesses) {
		t.Errorf("Harnesses = %+v", r.Harnesses)
	}
	if want := (Counts{BashRejected: 4, BashInterrupted: 1, OtherRejected: 2, OtherInterrupted: 1, UserActions: 7}); r.Corrections != want {
		t.Errorf("Corrections = %+v", r.Corrections)
	}
	repeat := Repeat{Scored: 5, Eligible: 4, SameCommand: 2, SameShape: 3, Rate: ptr(0.5), ShapeRate: ptr(0.75),
		BaselinePairs: 9, BaselineRepeated: 4, BaselineRate: ptr(0.4444)}
	if !reflect.DeepEqual(r.Repeat, repeat) {
		got, _ := json.Marshal(r.Repeat)
		t.Errorf("Repeat = %s", got)
	}
	top := []Pattern{{"git push --force", 1, 1}, {"npm publish", 1, 1}, {"rm -rf", 0, 1}}
	if !reflect.DeepEqual(r.Top, top) {
		t.Errorf("Top = %+v", r.Top)
	}
	if want := (NotCounted{BlockedByRule: 1, BlockedByAutoMode: 1, CancelledSiblings: 1, InterruptedInSubagent: 1}); r.NotCounted != want {
		t.Errorf("NotCounted = %+v", r.NotCounted)
	}
	if r.Read.CopiedCalls != 4 || r.Read.FilesRead != 7 || r.Read.EmptyFiles != 1 || r.Read.UnknownRecords != 1 ||
		r.Read.MalformedLines != 1 || r.Read.PartialLastLines != 1 || r.Read.CodexCommandsUnreadable != 1 {
		t.Errorf("Read = %+v", r.Read)
	}
}

func TestScoreFixture(t *testing.T) {
	f := scanFixture(t)
	c := NewCorpus(zeroTime)
	for _, call := range f.calls {
		c.Call(call)
	}
	for _, d := range f.denials {
		c.Denial(d)
	}
	r := c.Report(f.st)
	checkFixtureReport(t, r)

	// A second Report must not change anything, and Scan order must not matter.
	if again := c.Report(f.st); !reflect.DeepEqual(r, again) {
		t.Errorf("Report changed on a second call")
	}
	feed := make([]func(*Corpus), 0, len(f.calls)+len(f.denials))
	for _, call := range f.calls {
		feed = append(feed, func(c *Corpus) { c.Call(call) })
	}
	for _, d := range f.denials {
		feed = append(feed, func(c *Corpus) { c.Denial(d) })
	}
	for round := range 20 {
		rng := rand.New(rand.NewPCG(uint64(round), 7))
		rng.Shuffle(len(feed), func(i, j int) { feed[i], feed[j] = feed[j], feed[i] })
		c := NewCorpus(zeroTime)
		for _, add := range feed {
			add(c)
		}
		shuffled := c.Report(f.st)
		checkFixtureReport(t, shuffled)
		if !reflect.DeepEqual(r, shuffled) {
			t.Fatalf("round %d differs", round)
		}
	}
}

func report(calls []sessions.Call, denials []sessions.Denial) Report {
	c := NewCorpus(zeroTime)
	for _, call := range calls {
		c.Call(call)
	}
	for _, d := range denials {
		c.Denial(d)
	}
	return c.Report(sessions.Stats{})
}

func TestLinkedSessionsAreNotLater(t *testing.T) {
	calls := []sessions.Call{
		bash("a", "t1", "git push --force", hour(0)),
		bash("b", "t1", "git push --force", hour(0)),
		bash("b", "t2", "git push --force", hour(2)),
		// c shares an id with b only, and is still the same conversation line.
		bash("b", "t3", "git status", hour(3)),
		bash("c", "t3", "git status", hour(3)),
		bash("c", "t4", "git push --force", hour(4)),
	}
	r := report(calls, []sessions.Denial{rejected("a", "t1", hour(1))})
	if r.Repeat.Scored != 1 || r.Repeat.Eligible != 0 || r.Repeat.SameCommand != 0 {
		t.Errorf("Repeat = %+v", r.Repeat)
	}
	if r.Read.CopiedCalls != 2 {
		t.Errorf("CopiedCalls = %d", r.Read.CopiedCalls)
	}
}

// An unrelated later session makes the correction eligible. The linked session
// that ran the command again must still not count as a repeat.
func TestLinkedRunIsNotARepeatWhenEligible(t *testing.T) {
	calls := []sessions.Call{
		bash("a", "t1", "git push --force", hour(0)),
		bash("b", "t1", "git push --force", hour(0)),
		bash("b", "t2", "git push --force", hour(2)),
		bash("c", "t3", "git status", hour(3)),
	}
	r := report(calls, []sessions.Denial{rejected("a", "t1", hour(1))})
	if r.Repeat.Eligible != 1 || r.Repeat.SameCommand != 0 || r.Repeat.SameShape != 0 {
		t.Errorf("Repeat = %+v", r.Repeat)
	}
}

func TestCopyKeepsLowestOrigin(t *testing.T) {
	a, b := bash("a", "t1", "git status", hour(0)), bash("b", "t1", "git status", hour(0))
	b.FileFirst = hour(-1)
	for _, order := range [][]sessions.Call{{a, b}, {b, a}} {
		c := NewCorpus(zeroTime)
		c.Call(order[0])
		c.Call(order[1])
		held := c.calls[newCallID(sessions.HarnessClaude, "t1")]
		if got := c.refs[held.session].id; got != "b" {
			t.Errorf("kept session %q, want b", got)
		}
	}
}

func TestCopiedDenialCountsOnce(t *testing.T) {
	calls := []sessions.Call{bash("a", "t1", "git push --force", hour(0)), bash("b", "t1", "git push --force", hour(0))}
	denials := []sessions.Denial{rejected("a", "t1", hour(1)), rejected("b", "t1", hour(1))}
	r := report(calls, denials)
	if r.Corrections.BashRejected != 1 || r.Corrections.UserActions != 1 || r.Repeat.Scored != 1 {
		t.Errorf("Corrections = %+v, Repeat = %+v", r.Corrections, r.Repeat)
	}
	if r.Read.CopiedCalls != 1 {
		t.Errorf("CopiedCalls = %d: copied denials are not copied calls", r.Read.CopiedCalls)
	}
}

func TestCopiedDenialLinksSessions(t *testing.T) {
	calls := []sessions.Call{
		bash("a", "t1", "git push --force", hour(0)),
		bash("b", "t2", "git push --force", hour(3)),
	}
	denials := []sessions.Denial{rejected("a", "t1", hour(1)), rejected("b", "t1", hour(1))}
	if r := report(calls, denials); r.Repeat.Eligible != 0 {
		t.Errorf("Repeat = %+v", r.Repeat)
	}
}

func TestSameSessionIsNotLater(t *testing.T) {
	calls := []sessions.Call{
		bash("a", "t1", "git push --force", hour(0)),
		bash("a", "t2", "git push --force", hour(2)),
	}
	r := report(calls, []sessions.Denial{rejected("a", "t1", hour(1))})
	if r.Repeat.Eligible != 0 || r.Repeat.SameCommand != 0 {
		t.Errorf("Repeat = %+v", r.Repeat)
	}
}

func TestParallelSessionIsNotLater(t *testing.T) {
	calls := []sessions.Call{
		bash("a", "t1", "git push --force", hour(1)),
		bash("b", "t2", "git status", hour(0)),
		bash("b", "t3", "git push --force", hour(3)),
	}
	r := report(calls, []sessions.Denial{rejected("a", "t1", hour(2))})
	if r.Repeat.Eligible != 0 || r.Repeat.SameCommand != 0 {
		t.Errorf("Repeat = %+v", r.Repeat)
	}
}

func TestCodexCanRepeatClaude(t *testing.T) {
	calls := []sessions.Call{
		bash("a", "t1", "npm publish", hour(0)),
		codexCall("x", "c1", "npm publish", hour(2)),
	}
	r := report(calls, []sessions.Denial{rejected("a", "t1", hour(1))})
	if r.Repeat.Eligible != 1 || r.Repeat.SameCommand != 1 || r.Repeat.SameShape != 1 {
		t.Errorf("Repeat = %+v", r.Repeat)
	}
}

func TestLaterCallCountsInAnySegment(t *testing.T) {
	calls := []sessions.Call{
		bash("a", "t1", "git push --force", hour(0)),
		bash("b", "t2", "cd x && git push --force && ls", hour(2)),
	}
	r := report(calls, []sessions.Denial{rejected("a", "t1", hour(1))})
	if r.Repeat.SameCommand != 1 {
		t.Errorf("Repeat = %+v", r.Repeat)
	}
}

func TestThreeRejectionsScoreOnce(t *testing.T) {
	calls := []sessions.Call{
		bash("a", "t1", "npm publish", hour(0)),
		bash("a", "t2", "npm publish", hour(1)),
		bash("a", "t3", "npm publish", hour(2)),
	}
	denials := []sessions.Denial{rejected("a", "t1", hour(0)), rejected("a", "t2", hour(1)), rejected("a", "t3", hour(2))}
	r := report(calls, denials)
	if r.Corrections.BashRejected != 3 || r.Repeat.Scored != 1 {
		t.Errorf("Corrections = %+v, Repeat = %+v", r.Corrections, r.Repeat)
	}
}

func TestOtherToolIsCountedNotScored(t *testing.T) {
	d := rejected("a", "t1", hour(1))
	d.Tool = "Edit"
	stop := d
	stop.ToolUseID, stop.Kind = "t2", "interrupted"
	r := report(nil, []sessions.Denial{d, stop})
	if r.Corrections.OtherRejected != 1 || r.Corrections.OtherInterrupted != 1 || r.Repeat.Scored != 0 {
		t.Errorf("Corrections = %+v, Repeat = %+v", r.Corrections, r.Repeat)
	}
}

func TestUserActionsCountMessages(t *testing.T) {
	one, two := rejected("a", "t1", hour(1)), rejected("a", "t2", hour(1))
	one.Tool, two.Tool = "Edit", "Edit"
	two.MessageID = one.MessageID
	if r := report(nil, []sessions.Denial{one, two}); r.Corrections.UserActions != 1 {
		t.Errorf("UserActions = %d", r.Corrections.UserActions)
	}
}

func TestNoRateWithoutData(t *testing.T) {
	r := report([]sessions.Call{bash("a", "t1", "git push --force", hour(0))}, []sessions.Denial{rejected("a", "t1", hour(1))})
	if r.Repeat.Scored != 1 || r.Repeat.Eligible != 0 {
		t.Fatalf("Repeat = %+v", r.Repeat)
	}
	if r.Repeat.Rate != nil || r.Repeat.ShapeRate != nil || r.Repeat.BaselineRate != nil {
		t.Errorf("rates must be nil: %+v", r.Repeat)
	}
	empty := NewCorpus(zeroTime).Report(sessions.Stats{})
	if empty.Top == nil || empty.Harnesses == nil {
		t.Errorf("slices must not be nil: %+v", empty)
	}
	b, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Repeat map[string]any `json:"repeat"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"rate", "shape_rate", "baseline_rate"} {
		if v, ok := doc.Repeat[name]; !ok || v != nil {
			t.Errorf("%s = %v, want JSON null", name, v)
		}
	}
}

func TestRateRoundsToFourPlaces(t *testing.T) {
	if got := *rate(1, 3); got != 0.3333 {
		t.Errorf("rate = %v", got)
	}
}

func TestCallWithNoSimpleCommandIsNotScored(t *testing.T) {
	r := report([]sessions.Call{bash("a", "t1", "# only a comment", hour(0))}, []sessions.Denial{rejected("a", "t1", hour(1))})
	if r.Corrections.BashRejected != 1 || r.Repeat.Scored != 0 {
		t.Errorf("Corrections = %+v, Repeat = %+v", r.Corrections, r.Repeat)
	}
}

func TestTopRows(t *testing.T) {
	var calls []sessions.Call
	var denials []sessions.Denial
	for i := range 12 {
		// Letters, because the Key turns digits into 0.
		id := "t" + string(rune('g'+i))
		calls = append(calls, bash("a", id, "rm -rf one"+id, hour(0)), bash("b", id+"-later", "rm -rf two"+id, hour(3)))
		denials = append(denials, rejected("a", id, hour(1)))
	}
	// Twelve rejections of one shape are one row, with the counts added up.
	r := report(calls, denials)
	if want := []Pattern{{"rm -rf", 0, 12}}; !reflect.DeepEqual(r.Top, want) {
		t.Errorf("Top = %+v", r.Top)
	}

	shapes := []string{"git status", "git log", "git diff", "git show", "git fetch", "git pull", "git tag", "git stash", "git rm", "git mv", "git init", "git clone"}
	calls, denials = nil, nil
	for i, shape := range shapes {
		id := fmt.Sprintf("t%d", i)
		calls = append(calls, bash("a", id, shape, hour(0)), bash("b", id+"-later", shape, hour(3)))
		denials = append(denials, rejected("a", id, hour(1)))
	}
	calls = append(calls, bash("a", "o1", "mytool x", hour(0)), bash("b", "o2", "mytool x", hour(3)), bash("a", "n1", "docker ps", hour(0)))
	denials = append(denials, rejected("a", "o1", hour(1)), rejected("a", "n1", hour(1)))
	r = report(calls, denials)
	if len(r.Top) != 10 {
		t.Fatalf("Top has %d rows", len(r.Top))
	}
	if r.Top[0].Shape != "git clone" || r.Top[9].Shape != "git stash" {
		t.Errorf("Top order: %+v", r.Top)
	}
	calls = []sessions.Call{bash("a", "o1", "mytool x", hour(0)), bash("b", "o2", "mytool x", hour(3))}
	r = report(calls, []sessions.Denial{rejected("a", "o1", hour(1))})
	if want := []Pattern{{OtherShape, 1, 0}}; !reflect.DeepEqual(r.Top, want) {
		t.Errorf("Top = %+v", r.Top)
	}
}

func TestBaselineExcludesCorrectedKeys(t *testing.T) {
	calls := []sessions.Call{
		bash("a", "t1", "git push --force", hour(0)),
		bash("a", "t2", "git status", hour(0)),
		bash("b", "t3", "git push --force", hour(3)),
		bash("b", "t4", "git status", hour(3)),
	}
	r := report(calls, []sessions.Denial{rejected("a", "t1", hour(1))})
	if r.Repeat.BaselinePairs != 1 || r.Repeat.BaselineRepeated != 1 {
		t.Errorf("Repeat = %+v", r.Repeat)
	}
}

func TestSinceDropsOldRecords(t *testing.T) {
	c := NewCorpus(hour(2))
	c.Call(bash("a", "t1", "git status", hour(0)))
	c.Denial(rejected("a", "t1", hour(1)))
	c.Call(bash("b", "t2", "npm publish", hour(3)))
	c.Denial(rejected("b", "t2", hour(4)))
	r := c.Report(sessions.Stats{})
	if r.Corrections.BashRejected != 1 || r.Harnesses[0].Calls != 1 || r.Repeat.Scored != 1 {
		t.Errorf("Report = %+v", r)
	}
	if want := (Window{Since: "2026-10-01", First: "2026-10-01", Last: "2026-10-01"}); r.Window != want {
		t.Errorf("Window = %+v", r.Window)
	}
}

// A fork's shared calls can sit before since. It is still the same conversation
// line, so its later run must not look like a repeat.
func TestSinceKeepsLinksFromOldCalls(t *testing.T) {
	c := NewCorpus(hour(2))
	c.Call(bash("a", "t1", "git status", hour(0)))
	c.Call(bash("b", "t1", "git status", hour(0)))
	c.Call(bash("a", "t2", "npm publish", hour(3)))
	c.Denial(rejected("a", "t2", hour(4)))
	c.Call(bash("b", "t3", "npm publish", hour(5)))
	r := c.Report(sessions.Stats{})
	if r.Repeat.Eligible != 0 {
		t.Errorf("Repeat = %+v", r.Repeat)
	}
	if r.Read.CopiedCalls != 0 {
		t.Errorf("CopiedCalls = %d, want 0 for calls outside the window", r.Read.CopiedCalls)
	}
}

// A session already running at the correction is not later, even when --since
// cuts off the calls that show it was running.
func TestParallelSessionCutBySinceIsNotLater(t *testing.T) {
	c := NewCorpus(hour(2))
	c.Call(bash("p", "t0", "ls", hour(0)))
	c.Call(bash("z", "t1", "git push --force origin main", hour(3)))
	c.Denial(rejected("z", "t1", hour(3)))
	c.Call(bash("p", "t2", "git push --force origin main", hour(4)))
	r := c.Report(sessions.Stats{})
	if r.Repeat.Eligible != 0 || r.Repeat.SameCommand != 0 || r.Repeat.Rate != nil || len(r.Top) != 0 {
		t.Errorf("Repeat = %+v, Top = %+v", r.Repeat, r.Top)
	}
}

// A session that starts after since still counts as later.
func TestSinceKeepsSessionsThatStartInside(t *testing.T) {
	c := NewCorpus(hour(2))
	c.Call(bash("a", "t1", "npm publish", hour(3)))
	c.Denial(rejected("a", "t1", hour(3)))
	c.Call(bash("b", "t2", "npm publish", hour(4)))
	if r := c.Report(sessions.Stats{}); r.Repeat.Eligible != 1 || r.Repeat.SameCommand != 1 {
		t.Errorf("Repeat = %+v", r.Repeat)
	}
}

// A command rejected before since was still rejected, so it is not baseline.
func TestBaselineExcludesCorrectionsBeforeSince(t *testing.T) {
	c := NewCorpus(hour(2))
	c.Call(bash("a", "t1", "make release", hour(0)))
	c.Denial(rejected("a", "t1", hour(1)))
	c.Call(bash("u", "t2", "make release", hour(3)))
	c.Call(bash("v", "t3", "make release", hour(4)))
	r := c.Report(sessions.Stats{})
	if r.Repeat.BaselinePairs != 0 || r.Repeat.BaselineRate != nil || r.Corrections.BashRejected != 0 || r.Repeat.Scored != 0 {
		t.Errorf("Report = %+v", r)
	}
}

// A rejection just inside the window is scored, though its call came just before.
func TestDenialInsideSinceScoresAnOldCall(t *testing.T) {
	c := NewCorpus(hour(2))
	c.Call(bash("a", "t1", "make deploy", hour(1)))
	c.Denial(rejected("a", "t1", hour(3)))
	c.Call(bash("b", "t2", "make deploy", hour(4)))
	c.Call(bash("d", "t3", "make deploy", hour(5)))
	r := c.Report(sessions.Stats{})
	if r.Corrections.BashRejected != 1 || r.Repeat.Scored != 1 || r.Repeat.Eligible != 1 || r.Repeat.SameCommand != 1 {
		t.Errorf("Report = %+v", r)
	}
	if r.Repeat.BaselinePairs != 0 {
		t.Errorf("a rejected command reached the baseline: %+v", r.Repeat)
	}
}

// The count must not depend on which copy arrives first, even when since falls
// between two copies that carry different timestamps.
func TestCopiedCallsIgnoreOrder(t *testing.T) {
	a, b := bash("a", "t1", "git status", hour(0)), bash("b", "t1", "git status", hour(4))
	c, d := bash("a", "t2", "git status", hour(3)), bash("b", "t2", "git status", hour(4))
	for _, order := range [][]sessions.Call{{a, b, c, d}, {b, a, d, c}, {b, c, a, d}} {
		corpus := NewCorpus(hour(2))
		for _, call := range order {
			corpus.Call(call)
		}
		if got := corpus.Report(sessions.Stats{}).Read.CopiedCalls; got != 1 {
			t.Errorf("CopiedCalls = %d, want 1 for the pair whose kept copy is inside the window", got)
		}
	}
}

func event(session, kind, id string, at time.Time) sessions.Denial {
	return sessions.Denial{SessionID: session, ToolUseID: id, TS: at, FileFirst: at, Kind: kind}
}

func TestCopiedEventsCountOnce(t *testing.T) {
	c := NewCorpus(zeroTime)
	for _, s := range []string{"a", "fork-1", "fork-2"} {
		c.Denial(event(s, sessions.KindBetweenCalls, "", hour(0)))
		c.Denial(event(s, sessions.KindCodexTurnInterrupt, "turn-1", hour(1)))
		c.Denial(event(s, sessions.KindCodexPatchDeclined, "call-1", hour(1)))
		u := event(s, sessions.KindUntypedRejection, "t1", hour(1))
		u.Tool = "Bash"
		c.Denial(u)
	}
	// Another interrupt at another time is another event.
	c.Denial(event("a", sessions.KindBetweenCalls, "", hour(2)))
	r := c.Report(sessions.Stats{})
	if r.NotCounted.InterruptedBetweenCalls != 2 || r.NotCounted.UntypedRejections != 1 {
		t.Errorf("NotCounted = %+v", r.NotCounted)
	}
	if r.Read.InterruptedBetweenCalls != 2 || r.Read.UntypedRejections != 1 || r.Read.CodexTurnInterrupts != 1 || r.Read.CodexPatchesDeclined != 1 {
		t.Errorf("Read = %+v", r.Read)
	}
	if r.Corrections != (Counts{}) || r.Repeat.Scored != 0 {
		t.Errorf("an event became a correction: %+v", r)
	}
}

func TestSinceDropsOldEvents(t *testing.T) {
	c := NewCorpus(hour(2))
	for _, kind := range []string{sessions.KindBetweenCalls, sessions.KindCodexTurnInterrupt, sessions.KindCodexPatchDeclined, sessions.KindUntypedRejection} {
		c.Denial(event("a", kind, kind, hour(1)))
	}
	r := c.Report(sessions.Stats{})
	if r.NotCounted != (NotCounted{}) || r.Read.Stats != (sessions.Stats{}) {
		t.Errorf("NotCounted = %+v, Read = %+v", r.NotCounted, r.Read)
	}
}

func writeTranscript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func scanInto(t *testing.T, home string, c *Corpus) sessions.Stats {
	t.Helper()
	st, err := sessions.Scan(t.Context(), sessions.Options{Home: home, Getenv: func(string) string { return "" }}, c)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// Port of test_copied_bash_calls_keep_original_when_fork_path_sorts_first.
func TestCopiedBashCallsKeepOriginalWhenForkPathSortsFirst(t *testing.T) {
	home := t.TempDir()
	later := "2026-01-01T00:01:00Z"
	claudeCall := func(session string) string {
		return `{"type":"assistant","timestamp":"` + later + `","sessionId":"` + session +
			`","message":{"id":"m","content":[{"type":"tool_use","id":"shared-claude","name":"Bash","input":{"command":"echo fake"}}]}}` + "\n"
	}
	project := filepath.Join(home, ".claude", "projects", "-fake-work")
	// The first line gives each file its first timestamp, with no type.
	writeTranscript(t, filepath.Join(project, "a-fork.jsonl"), `{"timestamp":"2026-01-02T00:00:00Z"}`+"\n"+claudeCall("fork-session"))
	writeTranscript(t, filepath.Join(project, "z-original.jsonl"), `{"timestamp":"2026-01-01T00:00:00Z"}`+"\n"+claudeCall("original-session"))
	meta := func(id, ts string) string {
		return `{"timestamp":"` + ts + `","type":"session_meta","payload":{"id":"` + id + `","timestamp":"` + ts + `"}}` + "\n"
	}
	shell := `{"timestamp":"` + later + `","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"shared-codex","arguments":"{\"cmd\":\"echo fake\"}"}}` + "\n"
	rollouts := filepath.Join(home, ".codex", "sessions")
	writeTranscript(t, filepath.Join(rollouts, "rollout-a-fork.jsonl"), meta("fork-thread", "2026-01-02T00:00:00Z")+shell)
	writeTranscript(t, filepath.Join(rollouts, "rollout-z-original.jsonl"), meta("original-thread", "2026-01-01T00:00:00Z")+shell)
	c := NewCorpus(zeroTime)
	r := c.Report(scanInto(t, home, c))
	if r.Read.CopiedCalls != 2 {
		t.Errorf("CopiedCalls = %d", r.Read.CopiedCalls)
	}
	for harness, want := range map[string][2]string{
		sessions.HarnessClaude: {"shared-claude", "original-session"},
		sessions.HarnessCodex:  {"shared-codex", "original-thread"},
	} {
		held := c.calls[newCallID(harness, want[0])]
		if got := c.refs[held.session].id; got != want[1] {
			t.Errorf("%s kept session %q, want %q", harness, got, want[1])
		}
	}
}

// One marker and one untyped rejection, copied into two forks, through Scan.
func TestForkedEventsCountOnceFromFiles(t *testing.T) {
	home := t.TempDir()
	lines := func(session string) string {
		id := `"sessionId":"` + session + `"`
		return `{"type":"assistant","timestamp":"2026-10-01T10:00:00.000Z",` + id + `,"message":{"id":"m1","content":[{"type":"tool_use","id":"u1","name":"Bash","input":{"command":"ls"}}]}}` + "\n" +
			`{"type":"user","timestamp":"2026-10-01T10:00:01.000Z",` + id + `,"message":{"content":[{"type":"tool_result","content":"The user doesn't want to proceed with this tool use.","is_error":true,"tool_use_id":"u1"}]}}` + "\n" +
			`{"type":"user","timestamp":"2026-10-01T10:00:02.000Z",` + id + `,"message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}` + "\n"
	}
	for _, s := range []string{"one", "fork-a", "fork-b"} {
		writeTranscript(t, filepath.Join(home, ".claude", "projects", "-fake-work", s+".jsonl"), lines(s))
	}
	c := NewCorpus(zeroTime)
	r := c.Report(scanInto(t, home, c))
	if r.NotCounted.InterruptedBetweenCalls != 1 || r.NotCounted.UntypedRejections != 1 || r.Read.CopiedCalls != 2 {
		t.Errorf("NotCounted = %+v, CopiedCalls = %d", r.NotCounted, r.Read.CopiedCalls)
	}
	since := NewCorpus(time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC))
	r = since.Report(scanInto(t, home, since))
	if r.NotCounted != (NotCounted{}) {
		t.Errorf("with since, NotCounted = %+v", r.NotCounted)
	}
}
