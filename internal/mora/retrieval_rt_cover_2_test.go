package mora

// AREA=rt test-coverage worker (part 2): the pure-logic + entity-resolution
// branches in salience.go, filters.go, classify.go, gazetteer.go that the
// existing suites leave open. Every test is TestRt_*, every helper rt-prefixed.

import (
	"context"
	"database/sql"
	saliencepkg "github.com/pyranthus-hq/mora/internal/salience"
	"math"
	"reflect"
	"strings"
	"testing"
)

// ---- salience.go: sat negative-input clamp ----

// TestRt_SatNegativeInputClamps pins the sat() lower guard: a negative x makes
// log1p(x) negative, so the normalized value goes below 0 and must clamp to 0
// (never a spurious negative saturation).
func TestRt_SatNegativeInputClamps(t *testing.T) {
	if got := saliencepkg.Saturate(-0.5, 12); got != 0 {
		t.Fatalf("sat(-0.5,12) = %v, want 0 (negative input clamps to 0)", got)
	}
	// A value just below zero also clamps (boundary).
	if got := saliencepkg.Saturate(-1e-9, 250); got != 0 {
		t.Fatalf("sat(-1e-9,250) = %v, want 0", got)
	}
	// Sanity: a small positive input is strictly positive (the clamp is one-sided).
	if got := saliencepkg.Saturate(0.5, 12); !(got > 0) {
		t.Fatalf("sat(0.5,12) = %v, want > 0", got)
	}
}

// ---- classify.go: isShortcode on a short non-digit handle ----

// TestRt_ClassifyShortNonDigitHandle pins the isShortcode digit-scan reject: a
// short (<=6 char) handle that contains a non-digit is NOT a shortcode, so it
// falls through to the person default. (Existing shortcode tests only exercise
// non-digit handles that are ALSO too long, which reject on the length guard.)
func TestRt_ClassifyShortNonDigitHandle(t *testing.T) {
	cases := []struct{ identity, want string }{
		{"ab1", "person"},   // len 3, non-digit 'a' ⇒ not a shortcode
		{"12a45", "person"}, // len 5, non-digit 'a' mid-run
		{"x", "person"},     // len 1 alpha
		{"12-34", "person"}, // len 5, '-' is not a digit
	}
	for _, c := range cases {
		if got := classifyIdentity(c.identity, ""); got != c.want {
			t.Errorf("classifyIdentity(%q) = %q, want %q", c.identity, got, c.want)
		}
	}
}

// ---- gazetteer.go: normalizeGazName remaining eligibility branches ----

// TestRt_NormalizeGazNameEligibility closes the two open normalizeGazName branches:
// a multi-token name with a letter-free token is rejected (hasLetter guard), and a
// multi-token name with no substantive (>=3-rune) token is rejected (hasLong guard).
func TestRt_NormalizeGazNameEligibility(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"123 456", "", false},               // both tokens letter-free ⇒ hasLetter guard
		{"ab cd", "", false},                 // two 2-rune tokens, none >=3 ⇒ hasLong guard
		{"42nd street", "42nd street", true}, // digit-bearing but letter-present, substantive ⇒ eligible
	}
	for _, c := range cases {
		got, ok := normalizeGazName(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("normalizeGazName(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// ---- filters.go: aliasIDSet empty-alias skip ----

// TestRt_AliasIDSetSkipsEmptyAlias pins that a blank/whitespace-only alias is
// skipped (the a=="" continue) rather than producing a "person:" ghost id, while
// real address/handle aliases still fold in.
func TestRt_AliasIDSetSkipsEmptyAlias(t *testing.T) {
	got := aliasIDSet("person:riya@a.com", []string{"", "   ", "riya@b.com"})
	want := map[string]bool{
		"person:riya@a.com": true,
		"person:riya@b.com": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("aliasIDSet = %v, want %v (blank aliases skipped)", got, want)
	}
}

// ---- filters.go: resolveEntityID / resolveEntityFilter edge + error branches ----

// rtNeilIndex builds a minimal indexed vault with one known person (Neil Patel),
// returning the config for entity-resolution tests.
func rtNeilIndex(t *testing.T) Config {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := writeMemory(cfg, senderEmail("t1", "neil@example.com", "Neil Patel", "x@y.com")); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestRt_ResolveEntityIDEmptyName pins the empty/whitespace short-circuit: it
// returns ok=false with no DB touch (no error, no ambiguity).
func TestRt_ResolveEntityIDEmptyName(t *testing.T) {
	cfg := rtNeilIndex(t)
	for _, name := range []string{"", "   "} {
		canon, set, ok, amb, err := resolveEntityID(context.Background(), cfg, name)
		if err != nil || ok || canon != "" || set != nil || amb != nil {
			t.Fatalf("resolveEntityID(%q) = (%q,%v,%v,%v,%v), want the empty short-circuit", name, canon, set, ok, amb, err)
		}
	}
}

// TestRt_ResolveEntityIDQueryError pins the entities-query error path: a cancelled
// context (on an already-built index, so ensureIndexDB opens without rebuilding)
// makes db.QueryContext fail, and resolveEntityID surfaces that error.
func TestRt_ResolveEntityIDQueryError(t *testing.T) {
	cfg := rtNeilIndex(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled: the entities QueryContext must fail
	if _, _, _, _, err := resolveEntityID(ctx, cfg, "Neil Patel"); err == nil {
		t.Fatal("cancelled context must surface a query error from resolveEntityID")
	}
}

// TestRt_ResolveEntityFilterWrapsError pins resolveEntityFilter's error seam: when
// resolveEntityID errors, the error is returned (via humanizeIndexBusy, which
// passes a non-busy error through unchanged) rather than a silently-empty set.
func TestRt_ResolveEntityFilterWrapsError(t *testing.T) {
	cfg := rtNeilIndex(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	set, err := resolveEntityFilter(ctx, cfg, "Neil Patel")
	if err == nil {
		t.Fatal("resolveEntityFilter must surface the underlying resolve error")
	}
	if set != nil {
		t.Fatalf("errored resolveEntityFilter must return a nil set, got %v", set)
	}
}

// TestRt_ResolveEntityIDAmbiguousEmptyDisplay pins the id-fallback formatting in
// the ambiguous branch: a candidate whose display_name is empty is labeled by its
// bare id (person: prefix stripped) instead of a blank name. Two entities share
// the alias "Riya"; one has its display_name cleared directly in the index, so the
// ambiguity list must fall back to the id for that candidate.
func TestRt_ResolveEntityIDAmbiguousEmptyDisplay(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)
	for _, m := range []Memory{
		senderEmail("r1", "riya.k@alpha.com", "Riya", "x@y.com"),
		senderEmail("r2", "riya.s@beta.com", "Riya", "x@y.com"),
	} {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	// Clear one candidate's display_name (keeping the "Riya" alias so it still
	// matches) so the ambiguous formatter must fall back to its bare id.
	rw, err := sql.Open("sqlite", dbPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rw.ExecContext(ctx,
		`UPDATE entities SET display_name='', aliases='["Riya","riya.s@beta.com"]' WHERE id='person:riya.s@beta.com'`); err != nil {
		rw.Close()
		t.Fatal(err)
	}
	rw.Close()

	_, _, ok, amb, err := resolveEntityID(ctx, cfg, "Riya")
	if err != nil {
		t.Fatal(err)
	}
	if ok || len(amb) < 2 {
		t.Fatalf("expected ambiguity with >=2 candidates, got ok=%v amb=%v", ok, amb)
	}
	found := false
	for _, a := range amb {
		if strings.Contains(a, "riya.s@beta.com <riya.s@beta.com>") {
			found = true
		}
	}
	if !found {
		t.Fatalf("empty-display candidate must be labeled by its bare id; amb=%v", amb)
	}
}

// TestRt_ResolveEntityFilterSuccess is the positive companion: a resolvable name
// returns its alias-id set with no error (the success branch of resolveEntityFilter).
func TestRt_ResolveEntityFilterSuccess(t *testing.T) {
	cfg := rtNeilIndex(t)
	set, err := resolveEntityFilter(context.Background(), cfg, "Neil Patel")
	if err != nil {
		t.Fatalf("resolveEntityFilter: %v", err)
	}
	if !set["person:neil@example.com"] {
		t.Fatalf("resolved set %v must contain the canonical id", set)
	}
}

// ---- salience.go: recencyDecay determinism sanity (documents the >1 clamp is dead) ----

// TestRt_RecencyDecayNeverExceedsOne is a property guard backing the report note
// that recencyDecay's `decay > 1` clamp is unreachable: for any lastSeen strictly
// before vaultMax the decay is in (recencyFloor, 1), never above 1. (The == case is
// handled by the earlier deltaDays<=0 → 1 short-circuit.)
func TestRt_RecencyDecayNeverExceedsOne(t *testing.T) {
	vaultMax := "2026-06-01T00:00:00Z"
	for _, ls := range []string{
		"2026-05-31T23:00:00Z", // 1h before
		"2026-05-01T00:00:00Z", // ~31d
		"2025-12-03T00:00:00Z", // one half-life
	} {
		got := saliencepkg.RecencyDecay(ls, vaultMax)
		if got > 1 || got <= 0 || math.IsNaN(got) {
			t.Fatalf("recencyDecay(%q) = %v, want in (0,1]", ls, got)
		}
	}
}
