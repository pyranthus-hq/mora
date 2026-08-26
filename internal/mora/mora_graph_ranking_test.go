package mora

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// ---- Phase 14-04 Task 1: read salience_micros into Entity.Salience ----

// findListEntity returns the graphListEntities Entity with the given display name,
// or fails the test.
func findListEntity(t *testing.T, ents []Entity, name string) Entity {
	t.Helper()
	for _, e := range ents {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("entity %q not in list (have %d entities)", name, len(ents))
	return Entity{}
}

// hasListEntity reports whether the list carries an entity with the given name.
func hasListEntity(ents []Entity, name string) bool {
	for _, e := range ents {
		if e.Name == name {
			return true
		}
	}
	return false
}

// TestGraphSalienceReadPopulatesEntity proves the read path SELECTs salience_micros
// into Entity.Salience: a real person carries a positive frozen score, a service
// carries 0 yet is STILL in the list (searchable), and a structural entity is 0.
func TestGraphSalienceReadPopulatesEntity(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)

	mustWrite := func(m Memory) {
		t.Helper()
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	const occ = "2026-06-01T00:00:00Z"
	// A Scope-bearing memory materializes a structural "scope" entity (Salience must
	// stay 0 — only persons carry a frozen score).
	pm := emailMemory("em_person", "friend@example.com", "me@example.com", occ, 9)
	pm.Scope = "personal"
	mustWrite(pm)
	mustWrite(emailMemory("em_service", "no-reply@billing.example.com", "me@example.com", occ, 9))

	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	ents, err := graphListEntities(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}

	person := findListEntity(t, ents, "friend@example.com")
	if person.Salience <= 0 {
		t.Fatalf("person Salience=%d, want > 0 (read path must SELECT salience_micros)", person.Salience)
	}

	// The service must remain in the list (searchable) even though it scores 0.
	if !hasListEntity(ents, "no-reply@billing.example.com") {
		t.Fatal("service dropped from graphListEntities — services must stay searchable")
	}
	service := findListEntity(t, ents, "no-reply@billing.example.com")
	if service.Salience != 0 {
		t.Fatalf("service Salience=%d, want exactly 0", service.Salience)
	}

	// A structural entity (scope) carries no salience (NULL → 0).
	scope := findListEntity(t, ents, "personal")
	if scope.Kind != "scope" {
		t.Fatalf("expected a structural scope entity, got kind=%q", scope.Kind)
	}
	if scope.Salience != 0 {
		t.Fatalf("structural entity Salience=%d, want 0", scope.Salience)
	}
}

// ---- Phase 14-04 Task 2: rank People by salience + exclude services ----

// peopleNamesInOrder extracts the names printed in the "People" block of a
// renderGraphOverview dump, in print order, for ranking/exclusion assertions.
func peopleNamesInOrder(out string) []string {
	lines := strings.Split(out, "\n")
	var names []string
	inPeople := false
	for _, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "People (") {
			inPeople = true
			continue
		}
		if inPeople {
			// A blank line or the next section header ends the People block.
			if trimmed == "" || (!strings.HasPrefix(ln, "  ") && trimmed != "") {
				break
			}
			// Row form: "  <name padded> <bar> <num>" — the name is the leading field.
			fields := strings.Fields(trimmed)
			if len(fields) == 0 {
				continue
			}
			// The name may contain spaces but the trailing bar+number do not; recover the
			// name by trimming the bar/number tail. Simplest robust check: the raw row
			// starts with two spaces then the (possibly truncated) name.
			names = append(names, strings.TrimSpace(graphRowName(ln)))
		}
	}
	return names
}

// graphRowName extracts the name column from a People overview row "  <name…> <bar> <n>".
// renderGraphOverview pads the name to a fixed width, so the name is the first
// whitespace-delimited token group before the bar (a run of █) or the number.
func graphRowName(row string) string {
	r := strings.TrimPrefix(row, "  ")
	// Cut at the first bar cell or the trailing number; names are left-padded to a
	// fixed width with spaces, so split on the run of >=1 space that precedes the bar.
	if i := strings.IndexRune(r, '█'); i >= 0 {
		return strings.TrimSpace(r[:i])
	}
	// No bar (zero salience) — strip the trailing number column.
	fields := strings.Fields(r)
	if len(fields) >= 1 {
		// Drop the last field (the count/salience number).
		return strings.TrimSpace(strings.Join(fields[:len(fields)-1], " "))
	}
	return strings.TrimSpace(r)
}

// TestGraphPeopleRankingBySalience is the bills-over-friends fix: a high-salience
// person with a LOWER raw Count ranks ABOVE a low-salience person with a HIGHER
// Count in the People overview. Services are excluded from People entirely.
func TestGraphPeopleRankingBySalience(t *testing.T) {
	// friend: low Count (1 memory) but high Salience (recent, high msg count).
	// bills:  high Count (6 memories) but low Salience (old, saturated email volume).
	// vendor service: high Count but kind=service → excluded from People.
	ents := []Entity{
		{Name: "Best Friend", Kind: "person", Count: 1, Salience: 825000, MemoryIDs: []string{"im1"}},
		{Name: "bills@vendor.com", Kind: "person", Count: 6, Salience: 290675, MemoryIDs: []string{"e1", "e2", "e3", "e4", "e5", "e6"}},
		{Name: "no-reply@statements.com", Kind: "service", Count: 9, Salience: 0, MemoryIDs: []string{"s1"}},
	}
	var buf bytes.Buffer
	renderGraphOverview(&buf, ents, 12)
	out := buf.String()

	names := peopleNamesInOrder(out)
	if len(names) != 2 {
		t.Fatalf("People block has %d rows, want 2 (services excluded): %q\n%s", len(names), names, out)
	}
	// The high-salience friend (lower Count) ranks FIRST, above the high-Count bills.
	if names[0] != "Best Friend" {
		t.Fatalf("People ranked by Count not Salience: first=%q, want %q\nfull:\n%s", names[0], "Best Friend", out)
	}
	if names[1] != "bills@vendor.com" {
		t.Fatalf("second person = %q, want %q\n%s", names[1], "bills@vendor.com", out)
	}

	// The service is absent from the People overview.
	if strings.Contains(out, "no-reply@statements.com") {
		t.Fatalf("service leaked into the People overview:\n%s", out)
	}
}

func TestGraphPeopleBarsAreMonotonicWithDisplayedCount(t *testing.T) {
	ents := []Entity{
		{Name: "Higher count", Kind: "person", Count: 42, Salience: 100000},
		{Name: "Higher rank", Kind: "person", Count: 25, Salience: 900000},
		{Name: "Tie A", Kind: "person", Count: 25, Salience: 800000},
		{Name: "Zero", Kind: "person", Count: 0, Salience: 700000},
	}
	var buf bytes.Buffer
	renderGraphOverview(&buf, ents, 12)
	rows := strings.Split(buf.String(), "\n")
	lengths := map[string]int{}
	for _, row := range rows {
		for _, name := range []string{"Higher count", "Higher rank", "Tie A", "Zero"} {
			if strings.Contains(row, name) {
				lengths[name] = strings.Count(row, "█")
			}
		}
	}
	if lengths["Higher count"] <= lengths["Higher rank"] {
		t.Fatalf("displayed 42 must have a longer bar than 25: %v\n%s", lengths, buf.String())
	}
	if lengths["Higher rank"] != lengths["Tie A"] {
		t.Fatalf("equal displayed counts must have equal bars: %v\n%s", lengths, buf.String())
	}
	if lengths["Zero"] != 0 {
		t.Fatalf("zero displayed count must have no bar: %v\n%s", lengths, buf.String())
	}
}

// TestGraphOverviewDeterministicAndClean proves two renders are byte-identical and
// no ANSI escape leaks into the human overview (byte-clean invariant), including a
// salience tie (equal micros) resolved by the existing Count→name tie-break.
func TestGraphOverviewDeterministicAndClean(t *testing.T) {
	ents := []Entity{
		// Two people with EQUAL salience → deterministic Count desc, then name tie-break.
		{Name: "Zara", Kind: "person", Count: 3, Salience: 500000, MemoryIDs: []string{"z1"}},
		{Name: "Alex", Kind: "person", Count: 5, Salience: 500000, MemoryIDs: []string{"a1"}},
		{Name: "Service Co", Kind: "service", Count: 99, Salience: 0, MemoryIDs: []string{"sv"}},
		{Name: "golang", Kind: "tag", Count: 7, MemoryIDs: []string{"t1"}},
	}
	var b1, b2 bytes.Buffer
	renderGraphOverview(&b1, ents, 12)
	renderGraphOverview(&b2, ents, 12)
	if b1.String() != b2.String() {
		t.Fatalf("renderGraphOverview not byte-identical across two renders:\n--- a ---\n%s\n--- b ---\n%s", b1.String(), b2.String())
	}
	if strings.ContainsRune(b1.String(), 0x1b) {
		t.Fatalf("ANSI escape leaked into the human overview (byte-clean invariant):\n%q", b1.String())
	}

	// Equal salience → Count desc tie-break: Alex (Count 5) before Zara (Count 3).
	names := peopleNamesInOrder(b1.String())
	if len(names) < 2 || names[0] != "Alex" || names[1] != "Zara" {
		t.Fatalf("equal-salience tie-break not deterministic (want Alex, Zara by Count desc): %q\n%s", names, b1.String())
	}

	// Non-person section (Tags) is still present and ranks by Count (unchanged).
	if !strings.Contains(b1.String(), "golang") {
		t.Fatalf("non-person Tags section missing — only People should change:\n%s", b1.String())
	}
}

// TestGraphServiceStillSearchable proves the service exclusion is render-time only:
// a service is absent from the People overview but graphListEntities still returns
// it AND graphGetEntity resolves it by name (searchable, not surfaced).
func TestGraphServiceStillSearchable(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)

	mustWrite := func(m Memory) {
		t.Helper()
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	const occ = "2026-06-01T00:00:00Z"
	mustWrite(emailMemory("em_person", "pal@example.com", "me@example.com", occ, 9))
	mustWrite(emailMemory("em_service", "no-reply@billing.example.com", "me@example.com", occ, 9))
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	ents, err := graphListEntities(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Render the overview and confirm the service is excluded from People.
	var buf bytes.Buffer
	renderGraphOverview(&buf, ents, 12)
	if strings.Contains(buf.String(), "no-reply@billing.example.com") {
		t.Fatalf("service appeared in the People overview:\n%s", buf.String())
	}

	// The service is still in the list (search-facing) and resolvable via get_entity.
	if !hasListEntity(ents, "no-reply@billing.example.com") {
		t.Fatal("service dropped from the searchable entity list")
	}
	data, err := graphGetEntity(ctx, cfg, "no-reply@billing.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if found, _ := data["found"].(bool); !found {
		t.Fatalf("get_entity could not resolve the service (must stay searchable): %v", data)
	}
}

var _ = fmt.Sprintf // keep fmt imported for future row-format assertions
