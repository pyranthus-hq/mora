package mora

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"strings"
)

// openLoopsPerPersonCap bounds how many open loops a single person contributes to
// the additive block, keeping the MCP synthesis envelope bounded the way every
// other surface is (the snippet/limit discipline in mora_mcp_budget_test.go).
// Loops beyond the cap are reported as a More count, never silently dropped.
const openLoopsPerPersonCap = 8

// C1 "Open-Loops": two provenance-labelled, non-merging lanes joined into think:
// the live-tasks.md task ledger and the materialized message-evidence inventory.
// Meeting preparation has its own cited commitment surface; this block is only
// the additive "what's still open with Sam" context for think.

type OpenLoopLane string

const (
	openLoopLaneTaskLedger OpenLoopLane = "task-ledger"
	openLoopLaneEvidence   OpenLoopLane = "evidence"
)

// OpenLoop is the shared typed shape emitted by both engines. Status preserves
// the task ledger's raw workflow label; Lifecycle and Direction are the common
// obligation contract. When exact provenance proves that both lanes describe
// one obligation, the evidence row is authoritative and task-ledger appears
// only in SupportingLanes.
type OpenLoop struct {
	Task            string         `json:"task"`
	Status          string         `json:"status,omitempty"`
	Pri             string         `json:"pri,omitempty"`
	Horizon         string         `json:"horizon,omitempty"`
	Direction       Direction      `json:"direction"`
	Lifecycle       string         `json:"lifecycle"`
	Lane            OpenLoopLane   `json:"lane"`
	SupportingLanes []OpenLoopLane `json:"supporting_lanes,omitempty"`
	CommitmentID    string         `json:"commitment_id,omitempty"`
}

// PersonOpenLoops groups one person's open loops for the additive block. More is
// the count of further open loops beyond openLoopsPerPersonCap (0 when none) — an
// honest "and N more" rather than a silent truncation.
type PersonOpenLoops struct {
	Person string     `json:"person"`
	Loops  []OpenLoop `json:"loops"`
	More   int        `json:"more,omitempty"`
}

// capPersonLoops bounds one person's loop list to openLoopsPerPersonCap, returning
// the kept prefix (ledger order) and the number dropped.
func capPersonLoops(loops []OpenLoop) ([]OpenLoop, int) {
	if len(loops) <= openLoopsPerPersonCap {
		return loops, 0
	}
	return loops[:openLoopsPerPersonCap], len(loops) - openLoopsPerPersonCap
}

// closedTaskStatuses are the ledger statuses that mean a task is finished and so is
// NOT an open loop. A tight DENY-list (rather than an allow-list) keeps an unknown
// status surfacing as open — failing toward visibility, never silent suppression.
var closedTaskStatuses = map[string]bool{
	"done": true, "closed": true, "cancelled": true, "canceled": true,
	"dropped": true, "abandoned": true, "wontfix": true,
}

func taskIsOpen(status string) bool {
	return !closedTaskStatuses[strings.ToLower(strings.TrimSpace(status))]
}

func taskLedgerDirection(owner string, cfg Config) Direction {
	owner = strings.ToLower(strings.TrimSpace(owner))
	switch owner {
	case "":
		return commitDirectionUnknown
	case "you", "me", "self", "user":
		return commitOwedBySelf
	}
	for address := range selfEmails(cfg) {
		address = strings.ToLower(strings.TrimSpace(address))
		if owner == address || owner == strings.SplitN(address, "@", 2)[0] {
			return commitOwedBySelf
		}
	}
	return commitOwedByCounterparty
}

// openLoopsByPerson groups every OPEN task to the canonical person ids it
// references. The JOIN key is a MULTI-TOKEN person-gazetteer match over the task's
// Task text + Blocker — the same distinctive-full-name matcher think/graph use, so
// a single-token first name never matches (zero mis-association: "Sam" never
// matches "Samsung", and a task is never wrongly attributed to a shared first
// name). The Owner column is deliberately NOT the join key: Owner is who is
// RESPONSIBLE (usually you), not who the loop is WITH — joining on it would flip
// "what you owe Sam" into "what Sam owes". A missing live-tasks.md ledger yields an
// empty map, not an error.
//
// Ambiguity inherits the gazetteer's codebase-wide contract: two distinct people
// sharing a full name collapse to the smallest-id entity (loadPersonGazetteer) —
// the same resolution think's thin-coverage and graph expansion already use — so a
// shared full name resolves consistently, never to an unrelated third party.
func openLoopsByPerson(ctx context.Context, cfg Config, db *sql.DB) (map[string][]OpenLoop, error) {
	tasks, err := listTasks(cfg)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string][]OpenLoop{}, nil
		}
		return nil, err
	}
	gaz, _, err := loadPersonGazetteer(ctx, db)
	if err != nil {
		return nil, err
	}
	out := map[string][]OpenLoop{}
	for _, lt := range tasks {
		if !taskIsOpen(lt.Status) {
			continue
		}
		ids := map[string]bool{}
		for _, id := range gazetteerScan(gaz, lt.Task) {
			ids[id] = true
		}
		for _, id := range gazetteerScan(gaz, lt.Blocker) {
			ids[id] = true
		}
		if len(ids) == 0 {
			continue
		}
		loop := OpenLoop{
			Task: lt.Task, Status: lt.Status, Pri: lt.Pri, Horizon: lt.Horizon,
			Direction: taskLedgerDirection(lt.Owner, cfg),
			Lifecycle: commitOpen,
			Lane:      openLoopLaneTaskLedger,
		}
		for id := range ids {
			out[id] = append(out[id], loop)
		}
	}
	return out, nil
}

// evidenceLoopsByPerson adapts the authoritative, whole-snapshot commitment
// inventory into the shared OpenLoop shape. It includes terminal commitments so
// reconciliation can suppress a stale open ledger row; only open canonical rows
// are ultimately rendered.
func evidenceLoopsByPerson(ctx context.Context, cfg Config) (map[string][]OpenLoop, error) {
	snapshot, err := readCommitmentSnapshot(ctx, cfg)
	if err != nil {
		return nil, err
	}
	out := map[string][]OpenLoop{}
	for _, commitment := range snapshot.Commitments {
		if commitment.DuplicateOf != "" {
			continue
		}
		pid, _, ok, ambiguous, err := resolveEntityID(ctx, cfg, commitment.Counterparty.Value)
		if err != nil {
			return nil, err
		}
		// Precision first: an absent or ambiguous graph identity cannot safely be
		// attached to a person's open-loop block.
		if !ok || len(ambiguous) != 0 {
			continue
		}
		out[pid] = append(out[pid], OpenLoop{
			Task:         commitment.Summary,
			Direction:    commitment.Direction,
			Lifecycle:    commitment.State,
			Lane:         openLoopLaneEvidence,
			CommitmentID: commitment.ID,
		})
	}
	return out, nil
}

func openLoopIdentity(task string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(task)), " "))
}

// reconcileOpenLoopLanes keeps the engines separate unless an exact normalized
// summary gives one unambiguous evidence match. In that one case evidence owns
// state and direction, and the task ledger is retained only as provenance.
func reconcileOpenLoopLanes(ledger, evidence []OpenLoop) []OpenLoop {
	evidenceMatches := map[string][]int{}
	for i := range evidence {
		key := openLoopIdentity(evidence[i].Task)
		if key != "" {
			evidenceMatches[key] = append(evidenceMatches[key], i)
		}
	}
	supported := map[int]bool{}
	ledgerMatched := make([]bool, len(ledger))
	for i := range ledger {
		matches := evidenceMatches[openLoopIdentity(ledger[i].Task)]
		if len(matches) != 1 {
			continue
		}
		supported[matches[0]] = true
		ledgerMatched[i] = true
	}

	out := make([]OpenLoop, 0, len(ledger)+len(evidence))
	for i, loop := range evidence {
		if supported[i] {
			loop.SupportingLanes = []OpenLoopLane{openLoopLaneTaskLedger}
		}
		if loop.Lifecycle == commitOpen {
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

// openLoopsForQuery returns the open-loop blocks for the people NAMED IN THE QUERY
// — the additive "what's still open with them" surface for `think`. Deterministic:
// people in gazetteer-scan (sorted) order, their loops in ledger order.
func openLoopsForQuery(ctx context.Context, cfg Config, query string) ([]PersonOpenLoops, error) {
	db, err := ensureIndexDB(ctx, cfg)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	byPerson, err := openLoopsByPerson(ctx, cfg, db)
	if err != nil {
		return nil, err
	}
	evidenceByPerson, err := evidenceLoopsByPerson(ctx, cfg)
	if err != nil {
		return nil, err
	}
	gaz, _, err := loadPersonGazetteer(ctx, db)
	if err != nil {
		return nil, err
	}
	var out []PersonOpenLoops
	for _, pid := range gazetteerScan(gaz, query) {
		loops := reconcileOpenLoopLanes(byPerson[pid], evidenceByPerson[pid])
		if len(loops) == 0 {
			continue
		}
		capped, more := capPersonLoops(loops)
		out = append(out, PersonOpenLoops{Person: entityDisplayName(ctx, db, pid), Loops: capped, More: more})
	}
	return out, nil
}

// entityDisplayName resolves a canonical person id to its display name, falling
// back to the bare identity when the row is gone.
func entityDisplayName(ctx context.Context, db *sql.DB, pid string) string {
	var display string
	if err := db.QueryRowContext(ctx, `SELECT display_name FROM entities WHERE id = ?`, pid).Scan(&display); err == nil && display != "" {
		return display
	}
	return strings.TrimPrefix(pid, "person:")
}

// renderOpenLoops appends the additive OPEN LOOPS block to a synthesis prompt
// builder. Every line names its lane and typed state/direction so task-ledger
// support can never masquerade as evidence-derived authority.
func renderOpenLoops(b *strings.Builder, loops []PersonOpenLoops) {
	if len(loops) == 0 {
		return
	}
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
}
