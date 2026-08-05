package mora

import (
	"context"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// --- test helpers -----------------------------------------------------------

// digestSeed writes a memory with a controlled provider/source + age so the
// delta windowing and grouping logic is deterministic. As of Phase 12 the digest
// groups by sourceInstanceKey == m.Provider, so the seed sets Provider (the
// instance key) AND a per-item Source (ProviderID), plus a content_hash so the
// delta classifier has something to diff.
func digestSeed(t *testing.T, cfg Config, provider, title string, ago time.Duration, now time.Time) Memory {
	t.Helper()
	return digestSeedHash(t, cfg, provider, title, ago, now, "h-"+title)
}

// digestSeedUnindexed writes one digest fixture without rebuilding the derived
// index. Bulk-fixture tests use this helper and rebuild once after all writes;
// rebuilding after every insert turns an otherwise linear setup into O(n²).
func digestSeedUnindexed(t *testing.T, cfg Config, provider, title string, ago time.Duration, now time.Time) Memory {
	t.Helper()
	return digestSeedHashUnindexed(t, cfg, provider, title, ago, now, "h-"+title)
}

// digestSeedHash is digestSeed with an explicit content_hash, so a test can write
// the "same" memory twice with a CHANGED hash (the updated case) — the delta is
// hash-driven, never created_at (which writeMappedMemory preserves on a change).
func digestSeedHash(t *testing.T, cfg Config, provider, title string, ago time.Duration, now time.Time, hash string) Memory {
	t.Helper()
	m := digestSeedHashUnindexed(t, cfg, provider, title, ago, now, hash)
	rebuildDigestIndex(t, cfg)
	return m
}

func digestSeedHashUnindexed(t *testing.T, cfg Config, provider, title string, ago time.Duration, now time.Time, hash string) Memory {
	t.Helper()
	m := Memory{
		ID:          "id-" + title,
		Scope:       "global",
		Type:        "email",
		Title:       title,
		Text:        "From: alice@example.com\n\nI will send " + title + " today.",
		Source:      provider + "_thread/" + title, // per-item ProviderID, NOT the instance key
		Provider:    provider,
		ProviderID:  provider + "_thread/" + title,
		ContentHash: hash,
		CreatedAt:   now.Add(-ago).Format(time.RFC3339),
		Meta: map[string]any{
			"from":        []string{"alice@example.com"},
			"occurred_at": now.Add(-ago).Format(time.RFC3339),
		},
	}
	if provider == "imessage" {
		m.Type = "imessage"
		m.Text = "Digest Person: I will send " + title + " today."
		m.Meta = map[string]any{
			"participants": []map[string]string{{"handle": "+15550101999", "name": "Digest Person"}},
			"occurred_at":  now.Add(-ago).Format(time.RFC3339),
		}
	}
	if err := writeMemory(cfg, m); err != nil {
		t.Fatalf("writeMemory: %v", err)
	}
	return m
}

func rebuildDigestIndex(t *testing.T, cfg Config) {
	t.Helper()
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuildIndex: %v", err)
	}
}

// ungatedDigestConfig is the explicit seam for tests of pure digest mechanics
// (ordering, recurrence collapse, and watermark accounting). Product-facing tests
// keep the real DataDir and therefore exercise commitment eligibility.
func ungatedDigestConfig(cfg Config) Config {
	cfg.DataDir = ""
	return cfg
}

// digestSections indexes a digest's sections by instance key for assertions.
func digestSections(d Digest) map[string]DigestSection {
	out := map[string]DigestSection{}
	for _, s := range d.Sections {
		out[s.Source] = s
	}
	return out
}

// enableSources writes a sources.json enabling exactly the given connector types
// (Name == Type, the single-account reality) so ingestingConnectors enumerates
// them for the three-state classifier.
func enableSources(t *testing.T, cfg Config, types ...string) {
	t.Helper()
	var srcs []Source
	for _, ty := range types {
		s := Source{Name: ty, Type: ty, Scope: "personal", Enabled: ptr(true), CreatedAt: time.Now().Format(time.RFC3339)}
		if ty == "calendar" {
			s.Calendar = "primary"
		}
		srcs = append(srcs, s)
	}
	if err := saveSources(cfg, srcs); err != nil {
		t.Fatalf("saveSources: %v", err)
	}
}

// seedSyncStatus writes a healthy SyncStatus for a connector type so it reads as
// recently-synced (delta / no-changes) rather than "unavailable — never synced".
func seedSyncStatus(t *testing.T, cfg Config, ctype string, lastSuccess time.Time) {
	t.Helper()
	seedSyncStatusFull(t, cfg, ctype, &memory.SyncStatus{
		Source:        ctype,
		LastSynced:    lastSuccess.UTC().Format(time.RFC3339),
		LastAttemptAt: lastSuccess.UTC().Format(time.RFC3339),
		LastSuccessAt: lastSuccess.UTC().Format(time.RFC3339),
		ItemCount:     1,
	})
}

func seedSyncStatusFull(t *testing.T, cfg Config, ctype string, st *memory.SyncStatus) {
	t.Helper()
	path := syncStatusPathFor(cfg, Source{Name: ctype, Type: ctype})
	if path == "" {
		t.Fatalf("no sync path for %q", ctype)
	}
	if err := memory.SaveStatus(path, st); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}
}

// deltaPreview runs buildDigest in DELTA preview mode (advance=false), the common
// case under test.
func deltaPreview(t *testing.T, cfg Config, now time.Time) Digest {
	t.Helper()
	d, err := buildDigest(cfg, now, briefOpts{})
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	return d
}

// deltaCommit runs buildDigest in DELTA commit mode (advance=true) — it advances
// the watermark under the lock.
func deltaCommit(t *testing.T, cfg Config, now time.Time) Digest {
	t.Helper()
	// The committing surface is advanceBrief (issue #62 defect 1); buildDigest is now
	// the pure preview build. A huge budget + no artifact commits over everything,
	// matching the pre-#62 cap-only commit for the small fixtures these helpers seed.
	d, _, err := advanceBrief(cfg, now, briefOpts{advance: true}, 1<<20, false)
	if err != nil {
		t.Fatalf("advanceBrief(advance): %v", err)
	}
	return d
}

// --- Task 1: typed Delta seam + delta-aware buildDigest + deleted filter + rank/label ---

// TestBuildDeltaWindowsAndGroups (legacy, ported): the PLAIN-WINDOW path windows
// by true-in-world activity and groups by the instance key, firing the human labels.
func TestBuildDeltaWindowsAndGroups(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	digestSeed(t, cfg, "gmail", "Recent email", 2*time.Hour, now)
	digestSeed(t, cfg, "imessage", "Recent text", 1*time.Hour, now)
	digestSeed(t, cfg, "gmail", "Old email", 48*time.Hour, now) // outside 24h

	d, err := buildDigest(cfg, now, briefOpts{sinceHours: 24, perSourceCap: 10})
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	secs := digestSections(d)
	if got := len(secs["gmail"].Items); got != 1 {
		t.Fatalf("gmail should have 1 in-window item, got %d", got)
	}
	if secs["gmail"].Items[0].Title != "Recent email" {
		t.Fatalf("gmail item should be the recent one, got %q", secs["gmail"].Items[0].Title)
	}
	if got := len(secs["imessage"].Items); got != 1 {
		t.Fatalf("imessage should have 1 in-window item, got %d", got)
	}
}

// TestBuildDigestPerSourceCapWindow (legacy, ported): the plain-window cap keeps
// the most recent N.
func TestBuildDigestPerSourceCapWindow(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)

	digestSeed(t, cfg, "gmail", "Email A (oldest)", 5*time.Hour, now)
	digestSeed(t, cfg, "gmail", "Email B", 3*time.Hour, now)
	digestSeed(t, cfg, "gmail", "Email C (newest)", 1*time.Hour, now)

	d, err := buildDigest(cfg, now, briefOpts{sinceHours: 24, perSourceCap: 2})
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	secs := digestSections(d)
	items := secs["gmail"].Items
	if len(items) != 2 {
		t.Fatalf("cap=2 should keep 2 gmail items, got %d", len(items))
	}
	if items[0].Title != "Email C (newest)" || items[1].Title != "Email B" {
		t.Fatalf("cap should keep the 2 most-recent in recency order, got %q, %q", items[0].Title, items[1].Title)
	}
}

// TestBuildDigestSortsByInstantAcrossTimezones (legacy, ported): recency order is
// by instant, not raw RFC3339 string.
func TestBuildDigestSortsByInstantAcrossTimezones(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 5, 16, 0, 0, 0, time.UTC)
	mk := func(title, created string) {
		if err := writeMemory(cfg, Memory{ID: "id-" + title, Scope: "global", Type: "note", Title: title, Text: title, Provider: "gmail", Source: "gmail_thread/" + title, ProviderID: "gmail_thread/" + title, ContentHash: "h-" + title, CreatedAt: created}); err != nil {
			t.Fatalf("writeMemory: %v", err)
		}
	}
	mk("Older", "2026-06-05T15:00:00Z")
	mk("Newer", "2026-06-05T10:30:00-05:00")
	cfg = ungatedDigestConfig(cfg)

	d, err := buildDigest(cfg, now, briefOpts{sinceHours: 24, perSourceCap: 10})
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	items := digestSections(d)["gmail"].Items
	if len(items) != 2 || items[0].Title != "Newer" {
		var got []string
		for _, it := range items {
			got = append(got, it.Title)
		}
		t.Fatalf("expected instant-sorted order [Newer, Older], got %v", got)
	}
}

// TestRenderDigestRespectsBudget (legacy, ported).
func TestRenderDigestRespectsBudget(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 20; i++ {
		digestSeed(t, cfg, "gmail", "Email number "+string(rune('a'+i)), time.Duration(i)*time.Minute, now)
	}
	d, err := buildDigest(cfg, now, briefOpts{sinceHours: 24, perSourceCap: 50})
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	small := renderDigest(d, 200)
	large := renderDigest(d, 4000)
	if len(small) == 0 {
		t.Fatalf("render produced empty output")
	}
	if len(small) > 400 {
		t.Fatalf("budget=200 should truncate to ~200 chars, got %d", len(small))
	}
	if len(small) >= len(large) {
		t.Fatalf("small budget (%d) should render less than large (%d)", len(small), len(large))
	}
}

// TestDeltaSurfacesOnlyNewNotWindow (SC#1): after a commit baselines an item,
// an unchanged in-window memory is NOT surfaced on the next delta run.
func TestDeltaSurfacesOnlyNewNotWindow(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))

	digestSeed(t, cfg, "gmail", "Existing email", 1*time.Hour, now)

	// First run (cold start): commit baselines all hashes; the 7d window displays it.
	deltaCommit(t, cfg, now)

	// Second run, no new memories: the in-window item must NOT re-surface (it is
	// unchanged vs the watermark). State must read "no changes since last brief".
	d2 := deltaPreview(t, cfg, now.Add(1*time.Hour))
	sec := digestSections(d2)["gmail"]
	if len(sec.Items) != 0 {
		t.Fatalf("unchanged in-window item must NOT surface in delta, got %d items", len(sec.Items))
	}
	if sec.State != stateNoChanges {
		t.Fatalf("steady-state empty delta must be %q, got %q", stateNoChanges, sec.State)
	}
}

// TestDeltaNewVsUpdatedChange (D-01, M-5): a brand-new memory renders Change=new;
// a previously-seen memory whose content_hash changed renders Change=updated.
func TestDeltaNewVsUpdatedChange(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))

	digestSeedHash(t, cfg, "gmail", "Thread", 1*time.Hour, now, "hash-v1")
	deltaCommit(t, cfg, now) // baseline Thread@hash-v1

	// Same stableID, NEW hash (the grown/edited case — created_at would be
	// preserved by writeMappedMemory, so only the hash moves).
	digestSeedHash(t, cfg, "gmail", "Thread", 1*time.Hour, now, "hash-v2")
	// And a genuinely new memory.
	digestSeedHash(t, cfg, "gmail", "Fresh", 30*time.Minute, now, "hash-fresh")

	d := deltaPreview(t, cfg, now.Add(1*time.Hour))
	byTitle := map[string]string{}
	for _, it := range digestSections(d)["gmail"].Items {
		byTitle[it.Title] = it.Change
	}
	if byTitle["Thread"] != "updated" {
		t.Fatalf("changed-hash memory must be Change=updated, got %q", byTitle["Thread"])
	}
	if byTitle["Fresh"] != "new" {
		t.Fatalf("brand-new memory must be Change=new, got %q", byTitle["Fresh"])
	}
}

// TestDeltaDeletedFilteredFromDeltaAndColdStart (M-4): a cancelled calendar event
// (deleted_at set, NEW content_hash) never renders as a live [updated] item, and
// never appears in the cold-start 7d window.
func TestDeltaDeletedFilteredFromDeltaAndColdStart(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "calendar")
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	seedSyncStatus(t, cfg, "calendar", now.Add(-1*time.Hour))

	// A live upcoming event + a CANCELLED one (deleted_at, new hash), both upcoming.
	digestSeedHash(t, cfg, "calendar", "Live meeting", -2*24*time.Hour, now, "h-live") // +2 days
	cancelled := Memory{
		ID: "id-Cancelled", Scope: "global", Type: "note", Title: "Cancelled meeting",
		Text: "cancelled", Provider: "calendar", Source: "calendar_event/Cancelled",
		ProviderID: "calendar_event/Cancelled", ContentHash: "h-cancelled-v2",
		CreatedAt: now.Add(-3 * 24 * time.Hour).Format(time.RFC3339), // +3 days upcoming
		DeletedAt: now.Format(time.RFC3339),
	}
	if err := writeMemory(cfg, cancelled); err != nil {
		t.Fatalf("writeMemory: %v", err)
	}

	// Cold start: the cancelled event must NOT appear in the 7d display window.
	d := deltaPreview(t, cfg, now)
	for _, it := range digestSections(d)["calendar"].Items {
		if it.Title == "Cancelled meeting" {
			t.Fatalf("cancelled event must NOT appear in the cold-start 7d window")
		}
	}
}

// TestDeltaGroupsByInstanceKeyFiresLabels (M-1/M-5/M-6): items group by the
// instance key (Provider), so the previously-DEAD Emails/Texts/Calendar labels
// now fire, and sections are ranked Calendar→Texts→Emails off the descriptor.
func TestDeltaGroupsByInstanceKeyFiresLabels(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail", "imessage", "calendar")
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	seedSyncStatus(t, cfg, "imessage", now.Add(-1*time.Hour))
	seedSyncStatus(t, cfg, "calendar", now.Add(-1*time.Hour))

	digestSeed(t, cfg, "gmail", "An email", 1*time.Hour, now)
	digestSeed(t, cfg, "imessage", "A text", 1*time.Hour, now)
	digestSeedHash(t, cfg, "calendar", "An event", -1*24*time.Hour, now, "h-ev") // +1 day upcoming

	d := deltaPreview(t, cfg, now)

	// Data-driven rank order: calendar(0) < imessage(1) < gmail(2).
	var order []string
	for _, s := range d.Sections {
		order = append(order, s.Source)
	}
	want := []string{"calendar", "imessage", "gmail"}
	if len(order) != 3 || order[0] != want[0] || order[1] != want[1] || order[2] != want[2] {
		t.Fatalf("section rank order = %v, want %v", order, want)
	}
	// The human labels (previously dead under m.Source grouping) fire.
	if digestSourceLabel("gmail") != "Emails" || digestSourceLabel("imessage") != "Texts" || digestSourceLabel("calendar") != "Calendar" {
		t.Fatalf("data-driven labels wrong")
	}
}

// TestDeltaByteStableAcrossRepeatedRuns: two consecutive preview builds over the
// same vault+snapshot produce identical rendered output (sorted sections, UTC
// Generated). Determinism invariant.
func TestDeltaByteStableAcrossRepeatedRuns(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail", "imessage")
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	seedSyncStatus(t, cfg, "imessage", now.Add(-1*time.Hour))
	for i := 0; i < 5; i++ {
		digestSeed(t, cfg, "gmail", "G"+string(rune('a'+i)), time.Duration(i)*time.Minute, now)
		digestSeed(t, cfg, "imessage", "I"+string(rune('a'+i)), time.Duration(i)*time.Minute, now)
	}
	a := renderDigest(deltaPreview(t, cfg, now), 8000)
	b := renderDigest(deltaPreview(t, cfg, now), 8000)
	if a != b {
		t.Fatalf("delta render must be byte-stable across repeated runs:\nA:\n%s\nB:\n%s", a, b)
	}
}

// --- Task 2: three-state + silent-data-loss guard + commit-time advance ---

// TestThreeStateNoChangesVsUnavailable (SC#3, D-03): a healthy synced source with
// no new items reads "no changes"; an enabled+ingesting source that NEVER synced
// (no status) reads "unavailable" — never conflating empty delta with broken.
func TestThreeStateNoChangesVsUnavailable(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail", "imessage")
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	// gmail synced recently with a baseline; imessage NEVER synced (no status).
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "An email", 1*time.Hour, now)
	deltaCommit(t, cfg, now) // baseline gmail

	d := deltaPreview(t, cfg, now.Add(1*time.Hour))
	secs := digestSections(d)
	if secs["gmail"].State != stateNoChanges {
		t.Fatalf("synced+empty gmail must be %q, got %q", stateNoChanges, secs["gmail"].State)
	}
	if secs["imessage"].State != stateUnavailable {
		t.Fatalf("never-synced imessage must be %q, got %q", stateUnavailable, secs["imessage"].State)
	}
}

// TestThreeStateEnumerationZeroMemoryUnavailable (M-2/SC#3): an enabled+ingesting
// connector with ZERO memories (all-deleted/never-synced) still emits a section
// and surfaces "unavailable" — its absence from the memory grouping must NOT hide
// it. A non-ingesting / disabled connector is NEVER in the set.
func TestThreeStateEnumerationZeroMemoryUnavailable(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail") // only gmail enabled+ingesting; imessage NOT enabled
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	// gmail enabled but never synced AND zero memories.
	d := deltaPreview(t, cfg, now)
	secs := digestSections(d)
	if _, ok := secs["gmail"]; !ok {
		t.Fatalf("enabled+ingesting gmail must emit a section even with zero memories")
	}
	if secs["gmail"].State != stateUnavailable {
		t.Fatalf("zero-memory never-synced gmail must be %q, got %q", stateUnavailable, secs["gmail"].State)
	}
	if _, ok := secs["imessage"]; ok {
		t.Fatalf("non-enabled imessage must NOT be enumerated (never 'unavailable')")
	}
}

// TestThreeStateStale (D-03): a source whose last clean sync is >48h old reads
// "stale" rather than "no changes" or "unavailable".
func TestThreeStateStale(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	seedSyncStatus(t, cfg, "gmail", now.Add(-72*time.Hour)) // 3 days ago > 48h
	digestSeed(t, cfg, "gmail", "Old email", 100*time.Hour, now)
	deltaCommit(t, cfg, now)

	d := deltaPreview(t, cfg, now)
	if got := digestSections(d)["gmail"].State; got != stateStale {
		t.Fatalf("source synced 72h ago must be %q, got %q", stateStale, got)
	}
}

// TestThreeStateUnavailableOnError (D-03): a recorded sync error (LastError /
// ErrorCount>0) reads "unavailable".
func TestThreeStateUnavailableOnError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	seedSyncStatusFull(t, cfg, "gmail", &memory.SyncStatus{
		Source: "gmail", LastSynced: now.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
		LastAttemptAt: now.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
		LastSuccessAt: now.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
		ErrorCount:    2, LastError: "quota exceeded",
	})
	d := deltaPreview(t, cfg, now)
	if got := digestSections(d)["gmail"].State; got != stateUnavailable {
		t.Fatalf("source with a sync error must be %q, got %q", stateUnavailable, got)
	}
}

// TestThreeStateRecoveredSourceHealthy (M-3): a source that errored then recovered
// (ErrorCount reset by Plan 01) reads healthy — NOT "unavailable" forever.
func TestThreeStateRecoveredSourceHealthy(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	// Clean recovery: ErrorCount=0, LastError="", a fresh LastSuccessAt (M-3 reset).
	seedSyncStatusFull(t, cfg, "gmail", &memory.SyncStatus{
		Source: "gmail", LastSynced: now.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
		LastAttemptAt: now.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
		LastSuccessAt: now.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
		ErrorCount:    0, LastError: "",
	})
	digestSeed(t, cfg, "gmail", "An email", 1*time.Hour, now)
	deltaCommit(t, cfg, now)
	d := deltaPreview(t, cfg, now.Add(1*time.Hour))
	if got := digestSections(d)["gmail"].State; got == stateUnavailable {
		t.Fatalf("a recovered source must NOT read %q; got %q", stateUnavailable, got)
	}
}

// TestPreviewNeverWritesWatermark (SC#4): a preview (advance=false) writes NO
// snapshot — re-running preview keeps surfacing the same cold-start items.
func TestPreviewNeverWritesWatermark(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "An email", 1*time.Hour, now)

	deltaPreview(t, cfg, now) // preview — must write nothing
	snap := loadBriefSnapshot(cfg, "gmail")
	if len(snap.Items) != 0 {
		t.Fatalf("preview must NOT write the watermark, found %d items", len(snap.Items))
	}
}

// TestCommitAdvancesAndIsIdempotent (SC#4): commit baselines, and a SECOND commit
// reports "no changes" for every instance (idempotent).
func TestCommitAdvancesAndIsIdempotent(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "An email", 1*time.Hour, now)

	deltaCommit(t, cfg, now) // cold-start baseline
	snap := loadBriefSnapshot(cfg, "gmail")
	if len(snap.Items) != 1 {
		t.Fatalf("commit must baseline the 1 memory, got %d", len(snap.Items))
	}

	d2 := deltaCommit(t, cfg, now.Add(1*time.Hour)) // second commit, no changes
	if got := digestSections(d2)["gmail"].State; got != stateNoChanges {
		t.Fatalf("idempotent second commit must read %q, got %q", stateNoChanges, got)
	}
	if len(digestSections(d2)["gmail"].Items) != 0 {
		t.Fatalf("second commit must surface no items")
	}
}

// TestSilentDataLossGuardResurfaces (guard): when new+updated exceeds the cap, a
// "+N more" is set and the unshown items re-surface on the NEXT run (commit
// advances only over SHOWN items).
func TestSilentDataLossGuardResurfaces(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))

	// Baseline empty so the next run is true steady-state delta (not cold start).
	deltaCommit(t, cfg, now)

	// 12 brand-new memories, cap 8 → 8 shown, 4 truncated.
	for i := 0; i < 12; i++ {
		title := "Email" + string(rune('a'+i))
		digestSeedHash(t, cfg, "gmail", title, time.Duration(i)*time.Minute, now, "h-"+title)
	}
	d, _, err := advanceBrief(cfg, now.Add(1*time.Hour), briefOpts{advance: true, perSourceCap: 8}, 1<<20, false)
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	sec := digestSections(d)["gmail"]
	if len(sec.Items) != 8 || sec.MoreCount != 4 || !sec.Truncated {
		t.Fatalf("over-cap delta must show 8 + MoreCount=4 + Truncated, got items=%d more=%d trunc=%v", len(sec.Items), sec.MoreCount, sec.Truncated)
	}

	// Next run: the 4 unshown must re-surface (not silently marked-read).
	d2, err := buildDigest(cfg, now.Add(2*time.Hour), briefOpts{perSourceCap: 8})
	if err != nil {
		t.Fatalf("buildDigest 2: %v", err)
	}
	sec2 := digestSections(d2)["gmail"]
	if len(sec2.Items) != 4 {
		t.Fatalf("the 4 unshown items must re-surface next run, got %d", len(sec2.Items))
	}
	for _, it := range sec2.Items {
		if it.Change != "new" {
			t.Fatalf("re-surfaced unshown item must still be Change=new, got %q", it.Change)
		}
	}
}

// TestSilentDataLossUnshownUpdatedKeepsOldHash (guard, the subtle case): an
// UPDATED item (in the snapshot with an OLD hash) that is truncated past the cap
// must KEEP its OLD hash in the committed snapshot so it re-surfaces as "updated"
// next run — never have its hash silently advanced.
func TestSilentDataLossUnshownUpdatedKeepsOldHash(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))

	// Baseline an OLD item with an OLD hash, dated so it sorts LAST (truncated).
	digestSeedHash(t, cfg, "gmail", "OldThread", 100*time.Hour, now, "old-hash")
	deltaCommit(t, cfg, now)

	// Bump the old item's hash (updated) + add cap-many newer items so the updated
	// one is truncated past the cap.
	digestSeedHash(t, cfg, "gmail", "OldThread", 100*time.Hour, now, "new-hash")
	for i := 0; i < 8; i++ {
		title := "New" + string(rune('a'+i))
		digestSeedHash(t, cfg, "gmail", title, time.Duration(i)*time.Minute, now, "h-"+title)
	}
	d, _, err := advanceBrief(cfg, now.Add(1*time.Hour), briefOpts{advance: true, perSourceCap: 8}, 1<<20, false)
	if err != nil {
		t.Fatalf("buildDigest: %v", err)
	}
	// OldThread must be truncated (not shown).
	for _, it := range digestSections(d)["gmail"].Items {
		if it.Title == "OldThread" {
			t.Fatalf("OldThread should be truncated past the cap")
		}
	}
	// The committed snapshot must keep OldThread's OLD hash.
	snap := loadBriefSnapshot(cfg, "gmail")
	if got := snap.Items["id-OldThread"]; got != "old-hash" {
		t.Fatalf("unshown updated item must keep OLD hash in snapshot, got %q", got)
	}

	// Next run: it re-surfaces as updated.
	d2, err := buildDigest(cfg, now.Add(2*time.Hour), briefOpts{perSourceCap: 8})
	if err != nil {
		t.Fatalf("buildDigest 2: %v", err)
	}
	found := false
	for _, it := range digestSections(d2)["gmail"].Items {
		if it.Title == "OldThread" {
			found = true
			if it.Change != "updated" {
				t.Fatalf("re-surfaced item must be Change=updated, got %q", it.Change)
			}
		}
	}
	if !found {
		t.Fatalf("the truncated updated item must re-surface next run")
	}
}

// TestM4CancelledEventDroppedFromSnapshotAndRecreationIsNew (M-4 commit): a
// previously-baselined event that is later cancelled (deleted_at) has its id
// DROPPED from the committed snapshot, so a later same-id recreation re-surfaces
// as "new".
func TestM4CancelledEventDroppedFromSnapshotAndRecreationIsNew(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "calendar")
	now := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	seedSyncStatus(t, cfg, "calendar", now.Add(-1*time.Hour))

	// Baseline an upcoming event.
	ev := Memory{
		ID: "id-Standup", Scope: "global", Type: "note", Title: "Standup",
		Text: "standup", Provider: "calendar", Source: "calendar_event/Standup",
		ProviderID: "calendar_event/Standup", ContentHash: "h-standup-v1",
		CreatedAt: now.Add(-1 * 24 * time.Hour).Format(time.RFC3339), // +1 day upcoming
	}
	if err := writeMemory(cfg, ev); err != nil {
		t.Fatalf("writeMemory: %v", err)
	}
	cfg = ungatedDigestConfig(cfg)
	deltaCommit(t, cfg, now)
	if _, ok := loadBriefSnapshot(cfg, "calendar").Items["id-Standup"]; !ok {
		t.Fatalf("event must be baselined before cancellation")
	}

	// Cancel it (deleted_at set, NEW hash) and commit again.
	ev.DeletedAt = now.Format(time.RFC3339)
	ev.ContentHash = "h-standup-cancelled"
	if err := writeMemory(cfg, ev); err != nil {
		t.Fatalf("rewrite cancelled: %v", err)
	}
	deltaCommit(t, cfg, now.Add(1*time.Hour))
	if _, ok := loadBriefSnapshot(cfg, "calendar").Items["id-Standup"]; ok {
		t.Fatalf("cancelled event id must be DROPPED from the committed snapshot")
	}

	// Recreate the event (deleted_at cleared) → it must re-surface as "new".
	ev.DeletedAt = ""
	ev.ContentHash = "h-standup-v2"
	if err := writeMemory(cfg, ev); err != nil {
		t.Fatalf("recreate: %v", err)
	}
	d := deltaPreview(t, cfg, now.Add(2*time.Hour))
	found := false
	for _, it := range digestSections(d)["calendar"].Items {
		if it.Title == "Standup" {
			found = true
			if it.Change != "new" {
				t.Fatalf("recreated event must re-surface as new, got %q", it.Change)
			}
		}
	}
	if !found {
		t.Fatalf("recreated event must re-surface in the delta")
	}
}
