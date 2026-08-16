// Package openloops owns deterministic obligation-lane reconciliation and rendering.
package openloops

import (
	"github.com/pyranthus-hq/mora/internal/commitment"
	"strconv"
	"strings"
)

const PerPersonCap = 8

type Lane string

const (
	LaneTaskLedger Lane = "task-ledger"
	LaneEvidence   Lane = "evidence"
)

type Loop struct {
	Task            string               `json:"task"`
	Status          string               `json:"status,omitempty"`
	Pri             string               `json:"pri,omitempty"`
	Horizon         string               `json:"horizon,omitempty"`
	Direction       commitment.Direction `json:"direction"`
	Lifecycle       string               `json:"lifecycle"`
	Lane            Lane                 `json:"lane"`
	SupportingLanes []Lane               `json:"supporting_lanes,omitempty"`
	CommitmentID    string               `json:"commitment_id,omitempty"`
}
type Person struct {
	Person string `json:"person"`
	Loops  []Loop `json:"loops"`
	More   int    `json:"more,omitempty"`
}

func Cap(loops []Loop) ([]Loop, int) {
	if len(loops) <= PerPersonCap {
		return loops, 0
	}
	return loops[:PerPersonCap], len(loops) - PerPersonCap
}

var closedStatuses = map[string]bool{"done": true, "closed": true, "cancelled": true, "canceled": true, "dropped": true, "abandoned": true, "wontfix": true}

func TaskIsOpen(status string) bool {
	return !closedStatuses[strings.ToLower(strings.TrimSpace(status))]
}
func LedgerDirection(owner string, selfEmails []string) commitment.Direction {
	owner = strings.ToLower(strings.TrimSpace(owner))
	switch owner {
	case "":
		return commitment.DirectionUnknown
	case "you", "me", "self", "user":
		return commitment.OwedBySelf
	}
	for _, address := range selfEmails {
		address = strings.ToLower(strings.TrimSpace(address))
		if owner == address || owner == strings.SplitN(address, "@", 2)[0] {
			return commitment.OwedBySelf
		}
	}
	return commitment.OwedByCounterparty
}
func Identity(task string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(task)), " "))
}
func Reconcile(ledger, evidence []Loop) []Loop {
	evidenceMatches := map[string][]int{}
	for i := range evidence {
		key := Identity(evidence[i].Task)
		if key != "" {
			evidenceMatches[key] = append(evidenceMatches[key], i)
		}
	}
	supported := map[int]bool{}
	ledgerMatched := make([]bool, len(ledger))
	for i := range ledger {
		matches := evidenceMatches[Identity(ledger[i].Task)]
		if len(matches) != 1 {
			continue
		}
		supported[matches[0]] = true
		ledgerMatched[i] = true
	}
	out := make([]Loop, 0, len(ledger)+len(evidence))
	for i, loop := range evidence {
		if supported[i] {
			loop.SupportingLanes = []Lane{LaneTaskLedger}
		}
		if loop.Lifecycle == commitment.Open {
			out = append(out, loop)
		}
	}
	for i, loop := range ledger {
		if !ledgerMatched[i] {
			out = append(out, loop)
		}
	}
	return out
}
func Render(loops []Person) string {
	if len(loops) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nOPEN LOOPS (unfinished tasks involving people named above — weave in any still relevant; do NOT invent status or new tasks):\n")
	for _, pl := range loops {
		for _, l := range pl.Loops {
			b.WriteString("- " + pl.Person + " — " + l.Task + " [" + l.Lifecycle + "; " + string(l.Direction) + "; " + string(l.Lane))
			for _, supporting := range l.SupportingLanes {
				b.WriteString("+" + string(supporting))
			}
			b.WriteString("]")
			if l.Pri != "" {
				b.WriteString(" (" + l.Pri + ")")
			}
			b.WriteString("\n")
		}
		if pl.More > 0 {
			b.WriteString("- …and " + strconv.Itoa(pl.More) + " more open with " + pl.Person + "\n")
		}
	}
	return b.String()
}
