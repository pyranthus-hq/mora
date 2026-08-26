package mora

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestHumanizeIndexBusy: a raw SQLITE_BUSY becomes an actionable retry message;
// other errors pass through unchanged; nil stays nil.
func TestHumanizeIndexBusy(t *testing.T) {
	if humanizeIndexBusy(nil) != nil {
		t.Error("nil must stay nil")
	}
	got := humanizeIndexBusy(errors.New("database is locked (5) (SQLITE_BUSY)"))
	if got == nil || !strings.Contains(got.Error(), "retry in a few seconds") {
		t.Errorf("busy error not humanized: %v", got)
	}
	if got := humanizeIndexBusy(errors.New("no such table: entities")); got.Error() != "no such table: entities" {
		t.Errorf("non-busy error must pass through unchanged: %v", got)
	}
}

// TestWindowDigestSurfacesInProgressCalendarEvent (P1-F mirror): the brief's
// calendar section must not drop an event that started within the grace window —
// the meeting you just walked into. Pins the live digest.go upcoming-filter fix.
func TestWindowDigestSurfacesInProgressCalendarEvent(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	started := now.Add(-15 * time.Minute).Format(time.RFC3339) // in progress, within 30m grace
	m := Memory{
		ID: "inprog-evt", Type: "event", Title: "In progress sync", Scope: "global",
		Provider: "calendar", ProviderID: "calendar_event/inprog", Source: "calendar_event/inprog",
		CreatedAt: started, Meta: map[string]any{"occurred_at": started},
	}
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
	cfg = ungatedDigestConfig(cfg)
	d, err := buildDigest(cfg, now, briefOpts{sinceHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ti := range surfacedTitles(d) {
		if ti == "In progress sync" {
			found = true
		}
	}
	if !found {
		t.Fatalf("in-progress calendar event was dropped from the brief; surfaced=%v", surfacedTitles(d))
	}
}

// TestFilteredBriefBypassesCache (§3): a filtered brief must never read the
// persisted (unfiltered) cache; it generates fresh.
func TestFilteredBriefBypassesCache(t *testing.T) {
	cfg := resolveCfg(t)
	seedBriefFile(t, cfg, "2026-06-08", "SENTINEL-CACHED-BRIEF")
	body, generated, err := resolveBrief(cfg, resolveFixedNow, briefOpts{entityIDSet: map[string]bool{"person:riya@a.com": true}})
	if err != nil {
		t.Fatal(err)
	}
	if !generated {
		t.Error("filtered brief must be generated, not served from the unfiltered cache")
	}
	if strings.Contains(body, "SENTINEL-CACHED-BRIEF") {
		t.Error("filtered brief leaked the cached (unfiltered) file")
	}
}

// TestUnfilteredBriefStillReadsCache (§3 regression guard): the global brief still
// reads the fresh persisted cache.
func TestUnfilteredBriefStillReadsCache(t *testing.T) {
	cfg := resolveCfg(t)
	seedBriefFile(t, cfg, "2026-06-08", "SENTINEL-CACHED-BRIEF")
	body, generated, err := resolveBrief(cfg, resolveFixedNow, briefOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if generated {
		t.Error("unfiltered fresh brief must be read from cache, not generated")
	}
	if !strings.Contains(body, "SENTINEL-CACHED-BRIEF") {
		t.Error("unfiltered brief should be the cached file verbatim")
	}
}

// TestResolveBriefThreadsFiltersToGenerate (§3): the generate path threads the full
// filter set into BOTH buildDigest calls (was dropping everything but perSourceCap).
func TestResolveBriefThreadsFiltersToGenerate(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	for _, m := range []Memory{
		personMem("riya-call", "gmail", "riya@a.com", now.Add(-2*time.Hour)),
		personMem("bob-call", "gmail", "bob@z.com", now.Add(-2*time.Hour)),
	} {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	body, generated, err := resolveBrief(cfg, now, briefOpts{entityIDSet: map[string]bool{"person:riya@a.com": true}})
	if err != nil {
		t.Fatal(err)
	}
	if !generated {
		t.Error("filtered brief must generate")
	}
	if !strings.Contains(body, "riya-call") {
		t.Errorf("filtered brief missing riya's item:\n%s", body)
	}
	if strings.Contains(body, "bob-call") {
		t.Errorf("filtered brief leaked bob's item (filters not threaded):\n%s", body)
	}
}

// TestFilteredDeltaIsPreviewOnly pins §5: every filter dimension (not just source)
// refuses --advance, and the guard is hoisted above the lock so an entity/scope/
// since-days advance can't slip past. Unfiltered advance is still allowed.
func TestFilteredDeltaIsPreviewOnly(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	for _, opts := range []briefOpts{
		{advance: true, entityIDSet: map[string]bool{"person:riya@a.com": true}},
		{advance: true, scope: "project:acme"},
		{advance: true, sinceDays: 7},
		{advance: true, source: "gmail"},
	} {
		if _, _, err := advanceBrief(cfg, now, opts, 1<<20, false); err == nil || !strings.Contains(err.Error(), "preview-only") {
			t.Errorf("%+v: err=%v, want a preview-only error", opts, err)
		}
	}
	// Unfiltered advance must NOT be rejected as preview-only.
	if _, _, err := advanceBrief(cfg, now, briefOpts{advance: true}, 1<<20, false); err != nil && strings.Contains(err.Error(), "preview-only") {
		t.Errorf("unfiltered advance wrongly rejected: %v", err)
	}
}

func personMem(id, provider, from string, created time.Time) Memory {
	return Memory{
		ID: id, Scope: "global", Type: "email", Title: id,
		Text:     "From: " + from + "\n\nI will send " + id + " today.",
		Provider: provider, ProviderID: provider + "_thread/" + id, Source: provider + "_thread/" + id,
		CreatedAt: created.Format(time.RFC3339),
		Meta: map[string]any{
			"from":        []string{from},
			"to":          []string{"x@y.com"},
			"occurred_at": created.Format(time.RFC3339),
		},
	}
}

func surfacedTitles(d Digest) []string {
	var out []string
	for _, s := range d.Sections {
		for _, it := range s.Items {
			out = append(out, it.Title)
		}
	}
	sort.Strings(out)
	return out
}

// TestBuildDigestEntityFilter pins the filter wiring inside buildDigest (window
// path): only memories referencing the filtered entity surface; the salience map
// stays whole-vault (computed before the filter).
func TestBuildDigestEntityFilter(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	for _, m := range []Memory{
		personMem("riya-1", "gmail", "riya@a.com", now.Add(-2*time.Hour)),
		personMem("bob-1", "gmail", "bob@z.com", now.Add(-2*time.Hour)),
		personMem("riya-2", "gmail", "riya@a.com", now.Add(-3*time.Hour)),
	} {
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	d, err := buildDigest(cfg, now, briefOpts{sinceHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	if got := surfacedTitles(d); !reflect.DeepEqual(got, []string{"bob-1", "riya-1", "riya-2"}) {
		t.Fatalf("unfiltered surfaced %v, want all three", got)
	}

	d, err = buildDigest(cfg, now, briefOpts{sinceHours: 24, entityIDSet: map[string]bool{"person:riya@a.com": true}})
	if err != nil {
		t.Fatal(err)
	}
	if got := surfacedTitles(d); !reflect.DeepEqual(got, []string{"riya-1", "riya-2"}) {
		t.Fatalf("entity-filtered surfaced %v, want [riya-1 riya-2]", got)
	}
}

func TestBriefOptsFiltered(t *testing.T) {
	if (briefOpts{}).filtered() {
		t.Error("empty opts must not be filtered")
	}
	for _, o := range []briefOpts{
		{source: "gmail"},
		{entityIDSet: map[string]bool{"person:x@y.com": true}},
		{scope: "project:acme"},
		{sinceDays: 7},
	} {
		if !o.filtered() {
			t.Errorf("%+v should be filtered", o)
		}
	}
	// P1-D: a negative sinceDays must NOT register as filtered (it is clamped/inert).
	if (briefOpts{sinceDays: -7}).filtered() {
		t.Error("negative sinceDays must not register as filtered")
	}
	// P1-E: sinceHours is pulse-only and is NOT part of the brief's filtered() set.
	if (briefOpts{sinceHours: 24}).filtered() {
		t.Error("sinceHours must not register as filtered (pulse-only)")
	}
}

// TestResolveEntityID_ReturnsAliasIDSet is the P1-A integration pin: a merged
// person resolved by display name returns the FULL alias-id set (every gmail
// variant), so a downstream membership filter catches memories under any address.
func TestResolveEntityID_ReturnsAliasIDSet(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)
	for i, addr := range []string{"alex.owner@gmail.com", "alexowner@gmail.com", "alex.owner+promos@gmail.com"} {
		if err := writeMemory(cfg, senderEmail(string(rune('a'+i)), addr, "Alex Owner", "x@y.com")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	canon, set, ok, _, err := resolveEntityID(ctx, cfg, "Alex Owner")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("merged 'Alex Owner' should resolve")
	}
	for _, want := range []string{
		"person:alex.owner@gmail.com", "person:alexowner@gmail.com", "person:alex.owner+promos@gmail.com",
	} {
		if !set[want] {
			t.Errorf("idSet %v missing merged-away alias %q", set, want)
		}
	}
	if !set[canon] {
		t.Errorf("idSet must contain the canonical id %q", canon)
	}
}

// TestAliasIDSet pins the set builder: canonical id ∪ personID(alias) for every
// ADDRESS/HANDLE alias (contains '@' or starts with '+'); plain display-name
// aliases are excluded ("person:Alex Owner" is a dead key personRefs never emits).

// TestAliasIDSetPhoneHandleAndEmptyAliases: '+' phone handles count; display names
// are dropped; nil aliases yield just the canonical id.

// TestMemoryMentionsEntity pins the P1-A fix: membership is tested against the
// resolved alias-id SET (canonical id ∪ every address/handle alias id), NOT a
// scalar canonical id. personRefs emits RAW pre-merge ids, so a memory that
// references the person under a merged-away address must still match.

// TestMemoryMentionsEntityEmptySetMatchesNothing: the function itself returns
// false for an empty set; the caller gates "no entity filter" on len(set)==0.
