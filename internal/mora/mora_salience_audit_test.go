package mora

import (
	"bytes"
	"context"
	"database/sql"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// ---- Phase 14-06 (SC#4): the REAL index-rebuild byte-identical salience audit ----
//
// TestBuildGraphSalienceDeterministic (mora_graph_salience_test.go) proves buildGraph
// is byte-identical when called TWICE IN MEMORY. That is necessary but NOT sufficient
// for SC#4: it never touches the salience_micros column round-trip (write via
// writeGraph's INSERT, read back via SELECT) nor the read-path People ranking. A
// non-determinism in the DDL/INSERT/SELECT — or a NULL-vs-0 scan drift, or an
// order-by-salience instability on equal scores — would slip past the in-memory check.
//
// This audit runs the FULL pipeline `rebuildIndex` (file → parseMemory → buildGraph →
// writeGraph → entities table) TWICE over the SAME seeded multi-source vault and
// asserts:
//
//	(a) the ORDERED person rows of the read-path People overview (Salience desc, then
//	    the deterministic Count→Name→evidence tie-break renderPersonSection uses) — the
//	    (id, salience_micros) tuples in overview order — are byte-identical across the
//	    two rebuilds (same ids, same order, same frozen scores);
//	(b) each person's salience_micros read straight from the entities table is identical
//	    across the two rebuilds;
//	(c) a service sender's salience_micros is exactly 0, it is ABSENT from the People
//	    overview (Kind=="service" → excluded by renderGraphOverview), yet it is PRESENT
//	    in the entities table and returned by graphListEntities (searchable, not surfaced).
//
// Determinism is structural, not luck: the seed uses FIXED timestamps and message
// counts, and the kernel's recency is vault-relative (no time.Now), so the expected
// order and the frozen micros are stable. No wall clock enters any expectation.

// personOverviewRow is one ordered People-overview row: the canonical person id and its
// frozen salience_micros, in the order renderGraphOverview's renderPersonSection emits.
type personOverviewRow struct {
	id     string
	micros int64
}

// allEntitySalience reads (id, kind, salience_micros) for every entities row, straight
// from the persisted table (the column round-trip the in-memory test cannot exercise).
func allEntitySalience(t *testing.T, cfg Config) map[string]struct {
	kind   string
	micros int64
} {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, kind, salience_micros FROM entities`)
	if err != nil {
		t.Fatalf("read entities table: %v", err)
	}
	defer rows.Close()
	out := map[string]struct {
		kind   string
		micros int64
	}{}
	for rows.Next() {
		var id, kind string
		var sal sql.NullInt64
		if err := rows.Scan(&id, &kind, &sal); err != nil {
			t.Fatal(err)
		}
		out[id] = struct {
			kind   string
			micros int64
		}{kind: kind, micros: sal.Int64}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// orderedPeople returns the People-overview rows in the EXACT read-path order: Salience
// desc, then the deterministic tie-break renderPersonSection uses (Count desc → Name →
// evidence-id join). It is built from graphListEntities (the same read surface the CLI
// overview consumes), filtered to person-kind, mirroring renderGraphOverview's
// service-exclusion + renderPersonSection's sort so the ordered tuple matches what a
// human sees. The entities table is queried for the canonical id of each row (Entity
// carries the display name, not the id) so the assertion keys on the stable id.
func orderedPeople(t *testing.T, cfg Config) []personOverviewRow {
	t.Helper()
	ctx := testCtx(t)
	ents, err := graphListEntities(ctx, cfg)
	if err != nil {
		t.Fatalf("graphListEntities: %v", err)
	}

	// Map display_name -> id from the entities table so we can key on the stable id.
	// Person display names are unique in this fixture by construction.
	nameToID := map[string]string{}
	db, err := sql.Open("sqlite", dbPath(cfg)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT id, display_name FROM entities WHERE id LIKE 'person:%'`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			db.Close()
			t.Fatal(err)
		}
		nameToID[name] = id
	}
	rows.Close()
	db.Close()

	var people []Entity
	for _, e := range ents {
		// renderGraphOverview excludes service-kind from the People overview.
		if e.Kind == "person" {
			people = append(people, e)
		}
	}
	// Mirror renderPersonSection's deterministic sort: Salience desc → Count desc →
	// Name → evidence-id join.
	sort.SliceStable(people, func(i, j int) bool {
		if people[i].Salience != people[j].Salience {
			return people[i].Salience > people[j].Salience
		}
		if people[i].Count != people[j].Count {
			return people[i].Count > people[j].Count
		}
		if people[i].Name != people[j].Name {
			return people[i].Name < people[j].Name
		}
		return strings.Join(people[i].MemoryIDs, ",") < strings.Join(people[j].MemoryIDs, ",")
	})

	out := make([]personOverviewRow, 0, len(people))
	for _, e := range people {
		id, ok := nameToID[e.Name]
		if !ok {
			t.Fatalf("person display_name %q has no entities-table id", e.Name)
		}
		out = append(out, personOverviewRow{id: id, micros: e.Salience})
	}
	return out
}

// renderedOverview renders the ACTUAL production `mora graph` overview (renderGraphOverview
// over graphListEntities) for the current persisted index. Using the production render path
// — not a test-local re-sort — means the byte-identical assertion below covers the real CLI
// output (the renderPersonSection salience sort + the service-exclusion render filter)
// through the persisted salience_micros column, not just the test's mirror of it.
func renderedOverview(t *testing.T, cfg Config) string {
	t.Helper()
	ents, err := graphListEntities(context.Background(), cfg)
	if err != nil {
		t.Fatalf("graphListEntities: %v", err)
	}
	var buf bytes.Buffer
	renderGraphOverview(&buf, ents, 20)
	return buf.String()
}

// TestSalienceRebuildAuditByteIdentical is the SC#4 end-to-end proof.
func TestSalienceRebuildAuditByteIdentical(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)

	// A deterministic multi-source vault. FIXED instants + message counts; the most
	// recent instant (2026-06-01) is the vault recency anchor.
	//
	//   - friend@example.com: a multi-channel REAL person (email + imessage), recent,
	//     high volume → the top-salience human.
	//   - +15551230000 (Texter): a single-channel recent iMessage human, high volume.
	//   - olduser@example.com: a low-volume, OLD email person → low salience (recency
	//     decay), but > 0.
	//   - dave@example.com: a calendar attendee (event), one occurrence.
	//   - no-reply@billing.example.com: a SERVICE sender (classifyIdentity → service
	//     from the address alone) → salience_micros 0, excluded from People, searchable.
	person := "friend@example.com"
	texter := "+15551230000"
	old := "olduser@example.com"
	organizer := "dave@example.com"
	service := "no-reply@billing.example.com"

	const recent = "2026-06-01T00:00:00Z"
	const recentIM = "2026-05-28T00:00:00Z"
	const oldTS = "2024-01-01T00:00:00Z"
	const eventTS = "2026-05-15T00:00:00Z"

	mustWrite := func(m Memory) {
		t.Helper()
		if err := writeMemory(cfg, m); err != nil {
			t.Fatalf("writeMemory %s: %v", m.ID, err)
		}
	}
	// friend across two channels (email + imessage) → multi-channel real person.
	mustWrite(emailMemory("em_friend", person, "me@example.com", recent, 11))
	mustWrite(imsgMemory("im_friend", "+15559990000", "Best Friend", recentIM, 300))
	// texter: single high-volume iMessage relationship.
	mustWrite(imsgMemory("im_texter", texter, "Texter", recentIM, 500))
	// old low-volume email person.
	mustWrite(emailMemory("em_old", old, "me@example.com", oldTS, 1))
	// calendar event with an organizer person.
	mustWrite(eventMemory("ev_dave", organizer, "me@example.com", eventTS))
	// service sender.
	mustWrite(emailMemory("em_service", service, "me@example.com", recent, 9))

	// First full rebuild (file → parse → buildGraph → writeGraph → entities table).
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("rebuild #1: %v", err)
	}
	order1 := orderedPeople(t, cfg)
	table1 := allEntitySalience(t, cfg)
	render1 := renderedOverview(t, cfg)

	// Second full rebuild over the SAME on-disk vault.
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("rebuild #2: %v", err)
	}
	order2 := orderedPeople(t, cfg)
	table2 := allEntitySalience(t, cfg)
	render2 := renderedOverview(t, cfg)

	// (a) The ordered People-overview rows (id + salience_micros, in overview order) are
	// byte-identical across the two rebuilds.
	if !reflect.DeepEqual(order1, order2) {
		t.Fatalf("People overview order/salience drifted across rebuilds:\n#1 %+v\n#2 %+v", order1, order2)
	}

	// (a') The ACTUAL production `mora graph` overview render (the real renderPersonSection
	// salience sort + service-exclusion filter, fed by the persisted column) is byte-for-byte
	// identical across the two rebuilds — closing any gap between the test's mirror sort and
	// the user-facing output.
	if render1 != render2 {
		t.Fatalf("rendered `mora graph` overview drifted across rebuilds:\n--- rebuild #1 ---\n%s\n--- rebuild #2 ---\n%s", render1, render2)
	}

	// (b) Every persisted entity's salience_micros is identical across the two rebuilds.
	if !reflect.DeepEqual(table1, table2) {
		t.Fatalf("entities-table salience_micros drifted across rebuilds:\n#1 %+v\n#2 %+v", table1, table2)
	}

	// Sanity: the pass actually ran and produced a meaningful ranking, not all-zeros.
	if len(order1) < 3 {
		t.Fatalf("expected at least 3 person rows in the People overview, got %d: %+v", len(order1), order1)
	}
	sawPositive := false
	for _, r := range order1 {
		if r.micros > 0 {
			sawPositive = true
		}
	}
	if !sawPositive {
		t.Fatal("no person row carried a positive salience_micros — the rebuild salience pass did not run")
	}

	// The People overview is sorted by salience descending (the read-path ranking).
	for i := 1; i < len(order1); i++ {
		if order1[i-1].micros < order1[i].micros {
			t.Fatalf("People overview not salience-descending at %d: %d < %d", i, order1[i-1].micros, order1[i].micros)
		}
	}

	// A recent high-volume human outranks the OLD low-volume one (recency + volume win).
	rank := map[string]int{}
	for i, r := range order1 {
		rank[r.id] = i
	}
	if rank[personID(person)] >= rank[personID(old)] {
		t.Fatalf("recent multi-channel friend (rank %d) should outrank the old low-volume person (rank %d)",
			rank[personID(person)], rank[personID(old)])
	}

	// (c) The service sender: salience_micros exactly 0, EXCLUDED from the People
	// overview, yet PRESENT in the entities table AND returned by graphListEntities.
	svcID := personID(service)
	svc, ok := table1[svcID]
	if !ok {
		t.Fatalf("service entity %q absent from the entities table — it must stay searchable", svcID)
	}
	if svc.kind != "service" {
		t.Fatalf("service entity %q kind=%q, want \"service\"", svcID, svc.kind)
	}
	if svc.micros != 0 {
		t.Fatalf("service entity %q salience_micros=%d, want exactly 0", svcID, svc.micros)
	}
	for _, r := range order1 {
		if r.id == svcID {
			t.Fatalf("service %q must be EXCLUDED from the People overview, but it is present", svcID)
		}
	}
	// Present-and-searchable: graphListEntities returns it (with Kind=="service").
	ents, err := graphListEntities(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	foundService := false
	for _, e := range ents {
		if e.Kind == "service" {
			foundService = true
		}
	}
	if !foundService {
		t.Fatalf("service entity not returned by graphListEntities — it must remain searchable via list_entities/get_entity")
	}
}
