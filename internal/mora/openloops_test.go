package mora

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestOpenLoopsByPersonJoinIsSound proves the task↔person JOIN uses the multi-token
// person gazetteer: a distinctive FULL name attaches, a bare first name does NOT
// (zero mis-association), "Samsung" never attaches to a "Sam …" person, and DONE
// tasks are excluded. Owner is deliberately not the join key.
func TestOpenLoopsByPersonJoinIsSound(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)

	// Create the person entity "Sam Rivera" (sender ⇒ trusted display name).
	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/s1", Scope: "personal", Type: "email", Title: "hi",
		CreatedAt: "2026-06-01T00:00:00Z", Text: "note",
		Meta: map[string]any{
			"from":  []string{"sam@example.com"},
			"to":    []string{"adit@x.com"},
			"names": map[string]string{"sam@example.com": "Sam Rivera"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	writeLiveTasks(t, cfg,
		"| Send Sam Rivera the contract | work | you | P1 | active | — | wk | 2026-06-10 |", // full name → match
		"| Ping Sam about lunch | personal | you | P3 | queued | — | — | 2026-06-09 |",      // bare first name → NO match
		"| Order Samsung monitor | ops | you | P3 | queued | — | — | 2026-06-09 |",          // Samsung → NO match
		"| Pay Sam Rivera the retainer | work | you | P2 | done | — | — | 2026-06-05 |",     // full name but DONE → excluded
	)

	db := openRO(t, cfg)
	defer db.Close()
	byPerson, err := openLoopsByPerson(ctx, cfg, db)
	if err != nil {
		t.Fatalf("openLoopsByPerson: %v", err)
	}
	if len(byPerson) != 1 {
		t.Fatalf("expected exactly one person keyed (Sam Rivera), got %d: %+v", len(byPerson), byPerson)
	}
	var loops []OpenLoop
	for _, v := range byPerson {
		loops = v
	}
	if len(loops) != 1 || loops[0].Task != "Send Sam Rivera the contract" {
		t.Fatalf("JOIN unsound: want only the full-name OPEN task, got %+v", loops)
	}
	if loops[0].Lane != openLoopLaneTaskLedger ||
		loops[0].Direction != commitOwedBySelf ||
		loops[0].Lifecycle != commitOpen {
		t.Fatalf("task-ledger lane is not typed and labelled: %+v", loops[0])
	}
}

// TestOpenLoopsMissingLedgerIsEmpty proves a vault that never created live-tasks.md
// yields an empty map, not an error (the common cold-start case).
func TestOpenLoopsMissingLedgerIsEmpty(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)
	// A fresh vault has no live-tasks.md ledger at all (the cold-start case).
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	db := openRO(t, cfg)
	defer db.Close()
	byPerson, err := openLoopsByPerson(ctx, cfg, db)
	if err != nil {
		t.Fatalf("missing ledger should not error: %v", err)
	}
	if len(byPerson) != 0 {
		t.Fatalf("missing ledger should yield empty, got %+v", byPerson)
	}
}

// TestOpenLoopsPerPersonCapped proves one person's open loops are bounded (MCP
// budget discipline): beyond openLoopsPerPersonCap they roll into an honest More
// count rather than blowing the synthesis envelope.
func TestOpenLoopsPerPersonCapped(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)

	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/n1", Scope: "personal", Type: "email", Title: "hi",
		CreatedAt: "2026-06-01T00:00:00Z", Text: "note",
		Meta: map[string]any{
			"from":  []string{"neil@example.com"},
			"to":    []string{"adit@x.com"},
			"names": map[string]string{"neil@example.com": "Neil Patel"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	rows := make([]string, 0, openLoopsPerPersonCap+5)
	for i := 0; i < openLoopsPerPersonCap+5; i++ {
		rows = append(rows, "| Neil Patel task "+strconv.Itoa(i)+" | work | you | P2 | active | — | wk | 2026-06-10 |")
	}
	writeLiveTasks(t, cfg, rows...)

	res, err := buildThink(ctx, cfg, "what is Neil Patel working on", "", 5, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.OpenLoops) != 1 {
		t.Fatalf("expected one person block, got %+v", res.OpenLoops)
	}
	pl := res.OpenLoops[0]
	if len(pl.Loops) != openLoopsPerPersonCap || pl.More != 5 {
		t.Fatalf("expected %d capped loops + More=5, got %d loops + More=%d", openLoopsPerPersonCap, len(pl.Loops), pl.More)
	}
	if !strings.Contains(res.SynthesisPrompt, "and 5 more open with Neil Patel") {
		t.Fatalf("synthesis prompt missing honest More line:\n%s", res.SynthesisPrompt)
	}
}

// TestThinkOpenLoops proves `think` additively surfaces a person's open loops when
// the query NAMES that person, excludes done tasks, and renders the block in the
// synthesis prompt — without disturbing the evidence/gaps.
func TestThinkOpenLoops(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)

	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/t1", Scope: "personal", Type: "email", Title: "Pilot kickoff",
		CreatedAt: "2026-06-01T00:00:00Z", Text: "kicking off the pilot program",
		Meta: map[string]any{
			"from":  []string{"neil@example.com"},
			"to":    []string{"adit@x.com"},
			"names": map[string]string{"neil@example.com": "Neil Patel"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	writeLiveTasks(t, cfg,
		"| Send Neil Patel the pilot SOW | work | you | P1 | active | — | this week | 2026-06-10 |",
		"| Pay Neil Patel the invoice | work | you | P2 | done | — | — | 2026-06-05 |",
	)

	res, err := buildThink(ctx, cfg, "what is Neil Patel working on", "", 5, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.OpenLoops) != 1 || res.OpenLoops[0].Person != "Neil Patel" {
		t.Fatalf("expected one Open-Loops block for Neil Patel, got %+v", res.OpenLoops)
	}
	loops := res.OpenLoops[0].Loops
	if len(loops) != 1 || loops[0].Task != "Send Neil Patel the pilot SOW" || loops[0].Status == "done" {
		t.Fatalf("expected only the OPEN Neil task, got %+v", loops)
	}
	if !strings.Contains(res.SynthesisPrompt, "OPEN LOOPS") ||
		!strings.Contains(res.SynthesisPrompt, "Send Neil Patel the pilot SOW") ||
		!strings.Contains(res.SynthesisPrompt, "[open; owed_by_self; task-ledger]") {
		t.Fatalf("synthesis prompt missing the open-loops block:\n%s", res.SynthesisPrompt)
	}
}

// TestOpenLoopLanesNeverContradict proves #155's reconciliation contract. An
// evidence-derived commitment owns direction and lifecycle when an exact,
// unambiguous task-ledger row describes the same obligation; the ledger survives
// only as supporting provenance, never as a second contradictory state.
func TestOpenLoopLanesNeverContradict(t *testing.T) {
	ledger := []OpenLoop{{
		Task: "Send Neil Patel the pilot SOW", Status: "active",
		Direction: commitOwedBySelf, Lifecycle: commitOpen, Lane: openLoopLaneTaskLedger,
	}}
	evidence := []OpenLoop{{
		Task: "Send Neil Patel the pilot SOW", CommitmentID: "commit:v1:authoritative",
		Direction: commitOwedByCounterparty, Lifecycle: commitOpen, Lane: openLoopLaneEvidence,
	}}

	got := reconcileOpenLoopLanes(ledger, evidence)
	if len(got) != 1 {
		t.Fatalf("same obligation emitted %d states, want one authoritative row: %+v", len(got), got)
	}
	if got[0].Lane != openLoopLaneEvidence ||
		got[0].Direction != commitOwedByCounterparty ||
		got[0].Lifecycle != commitOpen ||
		len(got[0].SupportingLanes) != 1 ||
		got[0].SupportingLanes[0] != openLoopLaneTaskLedger {
		t.Fatalf("evidence lane did not stay authoritative with ledger provenance: %+v", got[0])
	}

	// A closing evidence transition also wins: the stale active ledger row must
	// not resurrect the obligation on an open-loops surface.
	evidence[0].Lifecycle = commitClosed
	if got := reconcileOpenLoopLanes(ledger, evidence); len(got) != 0 {
		t.Fatalf("closed evidence contradicted by stale open ledger row: %+v", got)
	}
}

func TestThinkOpenLoopsEvidenceIsAuthoritative(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	cfg.SelfEmails = []string{"self@example.com"}
	ctx := testCtx(t)

	// The inbound memory establishes Neil's trusted graph name. The outbound
	// memory then contributes the immutable, evidence-derived obligation.
	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/neil-name", Scope: "personal", Type: "email", Title: "hello",
		Provider: "gmail", ProviderID: "neil-name",
		CreatedAt: "2026-06-01T00:00:00Z", Text: "Hello.",
		Meta: map[string]any{
			"from":  []string{"neil@example.com"},
			"to":    []string{"self@example.com"},
			"names": map[string]string{"neil@example.com": "Neil Patel"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	task := "Could you send the pilot SOW, Neil Patel?"
	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/pilot-sow", Scope: "personal", Type: "email", Title: "Pilot",
		Provider: "gmail", ProviderID: "pilot-sow",
		CreatedAt: "2026-06-02T00:00:00Z", Text: task,
		Meta: map[string]any{
			"from": []string{"self@example.com"},
			"to":   []string{"neil@example.com"},
			"names": map[string]string{
				"self@example.com": "Self",
				"neil@example.com": "Neil Patel",
			},
			"messages": []commitmentMessageEvidence{{
				MessageRef: "gmail_thread/pilot-sow#msg-1",
				Sender:     "self@example.com",
				To:         []string{"neil@example.com"},
				At:         "2026-06-02T00:00:00Z",
				BlockRefs:  []string{"body"},
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	snapshot, err := readCommitmentSnapshot(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Commitments) != 1 {
		t.Fatalf("fixture produced %d commitments, want one: %+v", len(snapshot.Commitments), snapshot.Commitments)
	}
	writeLiveTasks(t, cfg,
		"| "+task+" | work | you | P1 | active | — | this week | 2026-06-02 |",
	)

	res, err := buildThink(ctx, cfg, "What is open with Neil Patel?", "", 5, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.OpenLoops) != 1 || len(res.OpenLoops[0].Loops) != 1 {
		t.Fatalf("want one reconciled obligation, got %+v", res.OpenLoops)
	}
	loop := res.OpenLoops[0].Loops[0]
	if loop.Lane != openLoopLaneEvidence ||
		loop.Direction != commitOwedByCounterparty ||
		loop.Lifecycle != commitOpen ||
		loop.CommitmentID == "" ||
		len(loop.SupportingLanes) != 1 ||
		loop.SupportingLanes[0] != openLoopLaneTaskLedger {
		t.Fatalf("think did not preserve evidence authority and ledger provenance: %+v", loop)
	}
	if !strings.Contains(res.SynthesisPrompt, "[open; owed_by_counterparty; evidence+task-ledger]") {
		t.Fatalf("prompt omitted reconciled lane labels:\n%s", res.SynthesisPrompt)
	}
}

// TestThinkOpenLoopsAbsentWhenPersonNotNamed proves the block is purely additive:
// a query that names nobody carries no open_loops (and the prompt is unchanged).
func TestThinkOpenLoopsAbsentWhenPersonNotNamed(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)
	run(t, "write", "--scope", "global", "--type", "note", "--title", "Roadmap", "--text", "the pilot roadmap plan")
	writeLiveTasks(t, cfg,
		"| Send Neil Patel the pilot SOW | work | you | P1 | active | — | this week | 2026-06-10 |",
	)
	res, err := buildThink(ctx, cfg, "what is the pilot roadmap", "", 5, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.OpenLoops) != 0 {
		t.Fatalf("no person named ⇒ no open loops, got %+v", res.OpenLoops)
	}
	if strings.Contains(res.SynthesisPrompt, "OPEN LOOPS") {
		t.Fatalf("open-loops block must be absent when no person is named:\n%s", res.SynthesisPrompt)
	}
}
