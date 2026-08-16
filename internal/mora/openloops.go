package mora

import (
	"context"
	"database/sql"
	openloopspkg "github.com/pyranthus-hq/mora/internal/openloops"
	"os"
	"strings"
)

const openLoopsPerPersonCap = openloopspkg.PerPersonCap

type OpenLoopLane = openloopspkg.Lane

const (
	openLoopLaneTaskLedger = openloopspkg.LaneTaskLedger
	openLoopLaneEvidence   = openloopspkg.LaneEvidence
)

type OpenLoop = openloopspkg.Loop
type PersonOpenLoops = openloopspkg.Person

func capPersonLoops(loops []OpenLoop) ([]OpenLoop, int) { return openloopspkg.Cap(loops) }
func taskIsOpen(status string) bool                     { return openloopspkg.TaskIsOpen(status) }
func taskLedgerDirection(owner string, cfg Config) Direction {
	self := selfEmails(cfg)
	addresses := make([]string, 0, len(self))
	for address := range self {
		addresses = append(addresses, address)
	}
	return openloopspkg.LedgerDirection(owner, addresses)
}
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

func reconcileOpenLoopLanes(ledger, evidence []OpenLoop) []OpenLoop {
	return openloopspkg.Reconcile(ledger, evidence)
}
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
func entityDisplayName(ctx context.Context, db *sql.DB, pid string) string {
	var display string
	if err := db.QueryRowContext(ctx, `SELECT display_name FROM entities WHERE id = ?`, pid).Scan(&display); err == nil && display != "" {
		return display
	}
	return strings.TrimPrefix(pid, "person:")
}
func renderOpenLoops(b *strings.Builder, loops []PersonOpenLoops) {
	b.WriteString(openloopspkg.Render(loops))
}
