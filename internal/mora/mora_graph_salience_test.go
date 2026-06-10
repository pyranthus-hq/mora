package mora

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
)

// ---- Phase 14-03 Task 1: buildGraph salience pass (frozen, byte-identical) ----

// findEntity returns the emitted graphEntity with the given id, or fails the test.
func findGraphEntity(t *testing.T, ents []graphEntity, id string) graphEntity {
	t.Helper()
	for _, e := range ents {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("entity %q not emitted (have %d entities)", id, len(ents))
	return graphEntity{}
}

// TestBuildGraphSaliencePersonPositiveServiceZero pins the core gate: a real person
// emits a positive int64 Salience and a service-kind entity emits exactly 0. The
// service is a no-reply address that classifyIdentity flags service from the address
// alone, so the kernel already scores it 0 — the entity carries that through.
func TestBuildGraphSaliencePersonPositiveServiceZero(t *testing.T) {
	const occ = "2026-06-01T00:00:00Z"
	person := "friend@example.com"
	service := "no-reply@billing.example.com"
	mems := []Memory{
		emailMemory("em_person", person, "me@example.com", occ, 9),
		emailMemory("em_service", service, "me@example.com", occ, 9),
	}
	ents, _, _ := buildGraph(mems)

	p := findGraphEntity(t, ents, personID(person))
	if p.Kind != "person" {
		t.Fatalf("expected %q to classify person, got %q", person, p.Kind)
	}
	if p.Salience <= 0 {
		t.Fatalf("person Salience=%d, want > 0", p.Salience)
	}

	s := findGraphEntity(t, ents, personID(service))
	if s.Kind != "service" {
		t.Fatalf("expected %q to classify service, got %q", service, s.Kind)
	}
	if s.Salience != 0 {
		t.Fatalf("service Salience=%d, want exactly 0", s.Salience)
	}
}

// TestBuildGraphSalienceMatchesKernel asserts the graph does NOT re-implement the
// salience math: each emitted person's Salience equals the 14-01 kernel's micros for
// the same canonical id (single-identity people, so canon is identity here).
func TestBuildGraphSalienceMatchesKernel(t *testing.T) {
	const occ = "2026-06-01T00:00:00Z"
	a := "alice@example.com"
	b := "+15550001111"
	mems := []Memory{
		emailMemory("em_a", a, "me@example.com", occ, 12),
		imsgMemory("im_b", b, "Bob Roberts", occ, 400),
	}
	ents, _, _ := buildGraph(mems)
	kernel := aggregatePersonSalience(mems)

	for _, id := range []string{personID(a), personID(b)} {
		e := findGraphEntity(t, ents, id)
		if e.Salience != kernel[id] {
			t.Fatalf("graph Salience for %q = %d, kernel = %d — graph must reuse the kernel, not re-implement it", id, e.Salience, kernel[id])
		}
		if e.Salience <= 0 {
			t.Fatalf("expected positive Salience for %q, got %d", id, e.Salience)
		}
	}
}

// TestBuildGraphSalienceMergedPersonNoDoubleCount asserts the pass runs AFTER A3
// merge: two Gmail mailboxes of one human (dot/+tag variants → same mailboxKey) merge
// to one canonical entity that carries a SINGLE salience value, max-folded across the
// pre-merge ids (never summed/double-counted). The merged human appears once.
func TestBuildGraphSalienceMergedPersonNoDoubleCount(t *testing.T) {
	const occ = "2026-06-01T00:00:00Z"
	// dotted + tagged Gmail variants of the same mailbox → A3 RULE 1 merge.
	v1 := "sam.smith@gmail.com"
	v2 := "samsmith+news@gmail.com"
	mems := []Memory{
		emailMemory("em_v1", v1, "me@example.com", occ, 10),
		emailMemory("em_v2", v2, "me@example.com", occ, 10),
	}
	ents, _, _ := buildGraph(mems)

	// Exactly one person entity carries BOTH mailbox aliases — the two variants merged
	// to a single canonical human (the recipient "me" is a separate, expected person).
	var merged []graphEntity
	for _, e := range ents {
		if e.Kind != "person" {
			continue
		}
		hasV1, hasV2 := false, false
		for _, a := range e.Aliases {
			if a == v1 {
				hasV1 = true
			}
			if a == v2 {
				hasV2 = true
			}
		}
		if hasV1 || hasV2 {
			merged = append(merged, e)
		}
	}
	if len(merged) != 1 {
		t.Fatalf("expected the two mailbox variants to merge to 1 person entity, got %d: %+v", len(merged), merged)
	}
	if merged[0].Salience <= 0 {
		t.Fatalf("merged person Salience=%d, want > 0", merged[0].Salience)
	}
	// The canonical id's salience must be max-folded through canon, NOT summed across
	// the two mailboxes — assert it stays near a single saturated mailbox's ceiling
	// (well under 2× — a sum would roughly double it).
	single := aggregatePersonSalience([]Memory{emailMemory("em_one", v1, "me@example.com", occ, 10)})
	if merged[0].Salience > single[personID(v1)]*2 {
		t.Fatalf("merged Salience=%d looks summed (single=%d) — must be max-folded, not double-counted", merged[0].Salience, single[personID(v1)])
	}
}

// TestBuildGraphSalienceDeterministic is the byte-identical invariant WITH salience:
// two buildGraph passes over the same mems produce reflect.DeepEqual entities INCLUDING
// the Salience field, and the entity emission order (by id) is unchanged.
func TestBuildGraphSalienceDeterministic(t *testing.T) {
	const occ = "2026-06-01T00:00:00Z"
	mems := []Memory{
		emailMemory("em1", "alice@example.com", "me@example.com", occ, 5),
		imsgMemory("im1", "+15550002222", "Carol", "2026-05-20T00:00:00Z", 800),
		eventMemory("ev1", "dave@example.com", "me@example.com", "2026-04-01T00:00:00Z"),
		emailMemory("em2", "no-reply@notify.example.com", "me@example.com", occ, 3),
	}
	e1, _, _ := buildGraph(mems)
	e2, _, _ := buildGraph(mems)
	if !reflect.DeepEqual(e1, e2) {
		t.Fatalf("entities (incl. Salience) nondeterministic across rebuilds:\n%+v\n%+v", e1, e2)
	}

	// At least one person carries a positive frozen score (the pass actually ran).
	sawPositive := false
	for _, e := range e1 {
		if e.Kind == "person" && e.Salience > 0 {
			sawPositive = true
		}
		if e.Kind == "service" && e.Salience != 0 {
			t.Fatalf("service entity %q has Salience=%d, want 0", e.ID, e.Salience)
		}
	}
	if !sawPositive {
		t.Fatal("no person entity carried a positive Salience — the pass did not run")
	}

	// The emission order (by id) is unchanged by the pass: ids are sorted ascending.
	for i := 1; i < len(e1); i++ {
		if e1[i-1].ID > e1[i].ID {
			t.Fatalf("entity order not sorted by id at %d: %q > %q", i, e1[i-1].ID, e1[i].ID)
		}
	}
}

// TestBuildGraphSalienceNoWallClock asserts the score is vault-relative, not wall-clock:
// the SAME memories with every instant shifted EARLIER by a fixed offset (same deltas,
// older vault) yield IDENTICAL salience, because recency decays against the vault max,
// not time.Now(). If the pass consulted the wall clock, the older vault would score lower.
func TestBuildGraphSalienceNoWallClock(t *testing.T) {
	build := func(occ, occ2 string) map[string]int64 {
		mems := []Memory{
			emailMemory("em1", "alice@example.com", "me@example.com", occ, 8),
			imsgMemory("im1", "+15550002222", "Carol", occ2, 600),
		}
		ents, _, _ := buildGraph(mems)
		out := map[string]int64{}
		for _, e := range ents {
			if e.Kind == "person" {
				out[e.ID] = e.Salience
			}
		}
		return out
	}
	// Recent vault vs a vault 5 years older — same internal deltas (30 days apart).
	recent := build("2026-06-01T00:00:00Z", "2026-05-02T00:00:00Z")
	old := build("2021-06-01T00:00:00Z", "2021-05-02T00:00:00Z")
	if !reflect.DeepEqual(recent, old) {
		t.Fatalf("salience depends on wall-clock — recent vault %v != old vault %v (must be vault-relative)", recent, old)
	}
}

// ---- Phase 14-03 Task 2: persist salience_micros (rebuild round-trip) ----

// readSalience reads the entities table's salience_micros keyed by id. A missing
// column/table yields an empty map (clean RED rather than an error).
func readSalience(t *testing.T, cfg Config) map[string]int64 {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, salience_micros FROM entities`)
	if err != nil {
		return map[string]int64{}
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var sal sql.NullInt64
		if err := rows.Scan(&id, &sal); err != nil {
			t.Fatal(err)
		}
		out[id] = sal.Int64
	}
	return out
}

// TestRebuildPersistsSalienceMicros round-trips the frozen score through the entities
// table: a real person row carries salience_micros > 0, a service row carries 0, and a
// SECOND rebuild writes byte-identical values (the column is additive-by-rebuild).
func TestRebuildPersistsSalienceMicros(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	mustWrite := func(m Memory) {
		t.Helper()
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	const occ = "2026-06-01T00:00:00Z"
	person := "friend@example.com"
	service := "no-reply@billing.example.com"
	mustWrite(emailMemory("em_person", person, "me@example.com", occ, 9))
	mustWrite(emailMemory("em_service", service, "me@example.com", occ, 9))

	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	sal := readSalience(t, cfg)
	if sal[personID(person)] <= 0 {
		t.Fatalf("person salience_micros=%d, want > 0 (have %d entity rows)", sal[personID(person)], len(sal))
	}
	if sal[personID(service)] != 0 {
		t.Fatalf("service salience_micros=%d, want 0", sal[personID(service)])
	}
	// A structural entity row carries 0 (unaffected).
	if v, ok := sal["scope:personal"]; ok && v != 0 {
		t.Fatalf("structural entity salience_micros=%d, want 0", v)
	}

	// Second rebuild → byte-identical salience_micros (additive-by-rebuild, no drift).
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	sal2 := readSalience(t, cfg)
	if !reflect.DeepEqual(sal, sal2) {
		t.Fatalf("salience_micros drifted across rebuilds:\n%v\n%v", sal, sal2)
	}
}

// TestRebuildSalienceColumnIsAdditive simulates an EXISTING pre-column entities table:
// it drops the salience_micros column-bearing schema by creating the OLD entities table
// shape, then a rebuild must succeed (the duplicate-column-tolerant ALTER adds the
// column) and populate salience_micros — no manual migration, no aborted transaction.
func TestRebuildSalienceColumnIsAdditive(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	// Replace any existing entities table with the OLD schema (no salience_micros
	// column), mimicking a vault indexed before this change.
	db, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS entities`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE entities (id TEXT PRIMARY KEY, kind TEXT, display_name TEXT, aliases TEXT, mention_count INTEGER, first_seen TEXT, last_seen TEXT)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	// Sanity: the pre-state genuinely lacks the column (so the rebuild's ALTER is what
	// adds it, not a leftover new-schema table).
	if _, err := db.Exec(`SELECT salience_micros FROM entities LIMIT 0`); err == nil {
		db.Close()
		t.Fatal("test setup invalid: entities already has salience_micros before rebuild")
	}
	db.Close()

	if err := writeMemory(cfg, emailMemory("em_person", "friend@example.com", "me@example.com", "2026-06-01T00:00:00Z", 9)); err != nil {
		t.Fatal(err)
	}
	// The rebuild must add the column via the tolerant ALTER and populate it.
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("rebuild on a pre-column DB failed (additive ALTER missing/aborting): %v", err)
	}
	sal := readSalience(t, cfg)
	if sal[personID("friend@example.com")] <= 0 {
		t.Fatalf("upgraded DB did not populate salience_micros: %v", sal)
	}
}
