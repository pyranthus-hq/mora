package openloops

import (
	"github.com/pyranthus-hq/mora/internal/commitment"
	"strings"
	"testing"
)

func TestCap(t *testing.T) {
	eight := make([]Loop, PerPersonCap)
	if got, more := Cap(eight); len(got) != 8 || more != 0 {
		t.Fatalf("boundary=%d/%d", len(got), more)
	}
	nine := append(eight, Loop{})
	if got, more := Cap(nine); len(got) != 8 || more != 1 {
		t.Fatalf("over=%d/%d", len(got), more)
	}
}
func TestTaskIsOpen(t *testing.T) {
	for _, s := range []string{"done", " CLOSED ", "Cancelled", "canceled", "dropped", "abandoned", "WONTFIX"} {
		if TaskIsOpen(s) {
			t.Fatalf("%q open", s)
		}
	}
	for _, s := range []string{"", "active", "blocked", "future"} {
		if !TaskIsOpen(s) {
			t.Fatalf("%q closed", s)
		}
	}
}
func TestLedgerDirection(t *testing.T) {
	self := []string{"Adit@Example.com"}
	cases := map[string]commitment.Direction{"": commitment.DirectionUnknown, "you": commitment.OwedBySelf, "ME": commitment.OwedBySelf, "self": commitment.OwedBySelf, "user": commitment.OwedBySelf, "adit@example.com": commitment.OwedBySelf, "adit": commitment.OwedBySelf, "sam": commitment.OwedByCounterparty}
	for owner, want := range cases {
		if got := LedgerDirection(owner, self); got != want {
			t.Fatalf("%q=%q want %q", owner, got, want)
		}
	}
}
func TestIdentity(t *testing.T) {
	if got := Identity("  Send   THE Plan "); got != "send the plan" {
		t.Fatalf("got %q", got)
	}
}
func TestReconcileAuthoritativeAndAmbiguous(t *testing.T) {
	ledger := []Loop{{Task: " Send plan ", Lifecycle: commitment.Open, Lane: LaneTaskLedger, Direction: commitment.OwedBySelf}, {Task: "different", Lifecycle: commitment.Open, Lane: LaneTaskLedger}}
	evidence := []Loop{{Task: "send PLAN", Lifecycle: commitment.Open, Lane: LaneEvidence, Direction: commitment.OwedByCounterparty}, {Task: "closed", Lifecycle: commitment.Closed, Lane: LaneEvidence}}
	got := Reconcile(ledger, evidence)
	if len(got) != 2 || got[0].Lane != LaneEvidence || len(got[0].SupportingLanes) != 1 || got[1].Task != "different" {
		t.Fatalf("got=%+v", got)
	}
	amb := Reconcile([]Loop{{Task: "same", Lifecycle: commitment.Open, Lane: LaneTaskLedger}}, []Loop{{Task: "same", Lifecycle: commitment.Open, Lane: LaneEvidence}, {Task: "same", Lifecycle: commitment.Open, Lane: LaneEvidence}})
	if len(amb) != 3 {
		t.Fatalf("ambiguous=%+v", amb)
	}
}
func TestRender(t *testing.T) {
	if Render(nil) != "" {
		t.Fatal("empty render")
	}
	got := Render([]Person{{Person: "Sam", Loops: []Loop{{Task: "send plan", Pri: "p1", Lifecycle: commitment.Open, Direction: commitment.OwedBySelf, Lane: LaneEvidence, SupportingLanes: []Lane{LaneTaskLedger}}}, More: 2}})
	for _, want := range []string{"\nOPEN LOOPS", "Sam — send plan [open; owed_by_self; evidence+task-ledger] (p1)", "…and 2 more open with Sam"} {
		if !strings.Contains(got, want) {
			t.Fatalf("render %q missing %q", got, want)
		}
	}
}
