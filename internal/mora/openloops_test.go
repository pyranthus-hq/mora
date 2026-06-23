package mora

import (
	"context"
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
	ctx := context.Background()

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
}

// TestOpenLoopsMissingLedgerIsEmpty proves a vault that never created live-tasks.md
// yields an empty map, not an error (the common cold-start case).
func TestOpenLoopsMissingLedgerIsEmpty(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
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
	ctx := context.Background()

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
	ctx := context.Background()

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
		!strings.Contains(res.SynthesisPrompt, "Send Neil Patel the pilot SOW") {
		t.Fatalf("synthesis prompt missing the open-loops block:\n%s", res.SynthesisPrompt)
	}
}

// TestThinkOpenLoopsAbsentWhenPersonNotNamed proves the block is purely additive:
// a query that names nobody carries no open_loops (and the prompt is unchanged).
func TestThinkOpenLoopsAbsentWhenPersonNotNamed(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
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

// TestMeetingPrepOpenLoops proves meeting_prep additively surfaces each attendee's
// open loops, joined on the attendee's canonical entity id, done tasks excluded.
func TestMeetingPrepOpenLoops(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

	if err := writeMemory(cfg, personMemNamed("e1", "gmail", "riya@a.com", "Riya Karode", now.Add(-48*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, eventMemFull("evt", "Acme sync", now.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{"riya@a.com": "Riya Karode"}, "riya@a.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	writeLiveTasks(t, cfg,
		"| Send Riya Karode the revised SOW | work | you | P1 | active | — | this week | 2026-06-12 |",
		"| Close out Riya Karode onboarding | work | you | P2 | done | — | — | 2026-06-05 |",
	)

	res, err := buildMeetingPrep(ctx, cfg, now, "", nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.OpenLoops) != 1 || res.OpenLoops[0].Person != "Riya Karode" {
		t.Fatalf("expected one Open-Loops block for the attendee, got %+v", res.OpenLoops)
	}
	if l := res.OpenLoops[0].Loops; len(l) != 1 || l[0].Task != "Send Riya Karode the revised SOW" {
		t.Fatalf("expected only the OPEN attendee task, got %+v", res.OpenLoops[0].Loops)
	}
	if !strings.Contains(res.SynthesisPrompt, "OPEN LOOPS") {
		t.Fatalf("meeting prompt missing open-loops block:\n%s", res.SynthesisPrompt)
	}
}
