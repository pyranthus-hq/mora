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

// C1 "Open-Loops": the task ledger (live-tasks.md) joined into think / meeting_prep
// as an ADDITIVE, labels-only block — "what's still open with Sam". It never
// reorders, filters, or removes any existing evidence/attendee output; it only
// appends a person-keyed list of unfinished tasks the agent can weave in.

// OpenLoop is one unfinished task tied to a person.
type OpenLoop struct {
	Task    string `json:"task"`
	Status  string `json:"status"`
	Pri     string `json:"pri,omitempty"`
	Horizon string `json:"horizon,omitempty"`
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
		loop := OpenLoop{Task: lt.Task, Status: lt.Status, Pri: lt.Pri, Horizon: lt.Horizon}
		for id := range ids {
			out[id] = append(out[id], loop)
		}
	}
	return out, nil
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
	if err != nil || len(byPerson) == 0 {
		return nil, err
	}
	gaz, _, err := loadPersonGazetteer(ctx, db)
	if err != nil {
		return nil, err
	}
	var out []PersonOpenLoops
	for _, pid := range gazetteerScan(gaz, query) {
		loops := byPerson[pid]
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
// builder. Labels only (person — task [status]); never invents or reorders.
func renderOpenLoops(b *strings.Builder, loops []PersonOpenLoops) {
	if len(loops) == 0 {
		return
	}
	b.WriteString("\nOPEN LOOPS (unfinished tasks involving people named above — weave in any still relevant; do NOT invent status or new tasks):\n")
	for _, pl := range loops {
		for _, l := range pl.Loops {
			b.WriteString("- " + pl.Person + " — " + l.Task + " [" + l.Status + "]")
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
