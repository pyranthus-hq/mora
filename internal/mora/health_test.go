package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/pyranthus-hq/mora/internal/google"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// TestSourceHealthStates (HEALTH-01/-03): a table walk across the four states —
// never/failed/stale/fresh — using real enabled sources + on-disk SyncStatus
// fixtures, proving sourceHealthAll dispatches thresholds on Source.Type (24h
// for the Google family, 48h for imessage) and that failed wins over stale even
// when both conditions would otherwise apply.
func TestSourceHealthStates(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail", "calendar", "imessage", "applecalendar")
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	// gmail: fresh — synced 1h ago, well inside the 24h Google threshold.
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	// calendar: never synced — no status file at all.
	// imessage: stale — synced 50h ago, no error, but past the 48h local threshold.
	seedSyncStatus(t, cfg, "imessage", now.Add(-50*time.Hour))
	// applecalendar: failed — a recorded error wins even though age alone (2h) would read fresh.
	seedSyncStatusFull(t, cfg, "applecalendar", &memory.SyncStatus{
		Source:        "applecalendar",
		LastAttemptAt: now.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
		LastSuccessAt: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339),
		LastError:     "database or disk is full (13)",
	})

	got := sourceHealthAll(cfg, now)
	if len(got) != 4 {
		t.Fatalf("sourceHealthAll returned %d entries, want 4: %+v", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Key > got[i].Key {
			t.Fatalf("sourceHealthAll must be sorted by Key, got %+v", got)
		}
	}
	byKey := map[string]sourceHealth{}
	for _, h := range got {
		byKey[h.Key] = h
	}

	if h := byKey["gmail"]; h.State != healthFresh {
		t.Fatalf("gmail (synced 1h ago, 24h threshold) state = %q, want %q: %+v", h.State, healthFresh, h)
	}
	if h := byKey["calendar"]; h.State != healthNever {
		t.Fatalf("never-synced calendar state = %q, want %q: %+v", h.State, healthNever, h)
	}
	if h := byKey["imessage"]; h.State != healthStale {
		t.Fatalf("imessage (synced 50h ago, 48h threshold) state = %q, want %q: %+v", h.State, healthStale, h)
	}
	if h := byKey["applecalendar"]; h.State != healthFailed || h.LastError == "" {
		t.Fatalf("applecalendar with a recorded error = %+v, want state %q with a non-empty LastError", h, healthFailed)
	}
}

func TestIsolationLastKnownGoodStaleProvenance(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail", "calendar")
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	success := now.Add(-2 * time.Hour).Format(time.RFC3339)
	attempt := now.Add(-time.Hour).Format(time.RFC3339)
	seedSyncStatusFull(t, cfg, "gmail", &memory.SyncStatus{
		Source: "gmail", LastSynced: success, LastSuccessAt: success, LastAttemptAt: attempt,
		LastError: "typed failure", ErrorCode: errCodeConnectorUnavailable, ErrorCount: 1, ItemCount: 3,
	})
	seedSyncStatusFull(t, cfg, "calendar", &memory.SyncStatus{
		Source: "calendar", LastSynced: success, LastSuccessAt: success, LastAttemptAt: attempt,
		LastError: "typed failure", ErrorCode: errCodeConnectorUnavailable, ErrorCount: 1,
	})
	states := buildSourceStates(cfg, Digest{Sections: []DigestSection{
		{Source: "gmail", State: stateStale}, {Source: "calendar", State: stateStale},
	}})
	if len(states) != 2 || states[0].Instance != "calendar" || states[1].Instance != "gmail" {
		t.Fatalf("states = %+v", states)
	}
	state := states[1]
	if state.State != "stale" || state.LastSuccessAt != success || state.LastAttemptAt != attempt || state.ErrorCode != errCodeConnectorUnavailable || !state.Errored {
		t.Fatalf("stale provenance = %+v", state)
	}
	if state.LastSynced != success {
		t.Fatalf("last-known-good alias changed: %+v", state)
	}
}

func TestIsolationRecoveryPreservesFailureHistory(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	handle, err := beginOperation(cfg, operationKindIngest, "ingesting", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := finishOperation(cfg, handle, operationFailed, "failed", operationCounts{Errors: 1}, "ingest_failed", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	failedPath := filepath.Join(operationRoot(cfg), string(operationKindIngest), handle.RunID+".json")
	clean := now.Add(-10 * time.Minute).Format(time.RFC3339)
	seedSyncStatusFull(t, cfg, "gmail", &memory.SyncStatus{
		Source: "gmail", LastSynced: clean, LastSuccessAt: clean, LastAttemptAt: clean, ItemCount: 4,
	})
	health := sourceHealthFor(cfg, Source{Name: "gmail", Type: "gmail"}, "gmail", now)
	if health.State != healthFresh || health.ErrorCode != "" {
		t.Fatalf("recovered health = %+v", health)
	}
	body, err := os.ReadFile(failedPath)
	if err != nil {
		t.Fatalf("failed history removed: %v", err)
	}
	var record operationRecord
	if err := json.Unmarshal(body, &record); err != nil || record.RunID != handle.RunID || record.State != operationFailed {
		t.Fatalf("failed history = %+v, err %v", record, err)
	}
}

// TestSourceHealthAllNonNilWhenEmpty (▸CX JSON contract discipline): with no
// enabled sources, sourceHealthAll must return a non-nil empty slice, not nil —
// the doctor JSON report's `sources` field must marshal as `[]`, never `null`.
func TestSourceHealthAllNonNilWhenEmpty(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	got := sourceHealthAll(cfg, now)
	if got == nil {
		t.Fatal("sourceHealthAll returned nil, want a non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("sourceHealthAll with no enabled sources = %+v, want empty", got)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(b) != "[]" {
		t.Fatalf("json.Marshal(sourceHealthAll(...)) = %s, want []", b)
	}
}

// TestDoctorStrictExitsOnStaleSource (HEALTH-01/-03): a stale critical source
// (past its type's freshness threshold) must fail the doctor JSON report's
// `healthy` bit, surface as `source_fresh:<key>`, and make `--strict` error.
func TestDoctorStrictExitsOnStaleSource(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	origClock := doctorClock
	doctorClock = func() time.Time { return now }
	t.Cleanup(func() { doctorClock = origClock })

	seedSyncStatus(t, cfg, "gmail", now.Add(-30*time.Hour)) // stale: > 24h gmail threshold

	out := run(t, "doctor", "--json")
	var rep doctorReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("doctor --json must emit JSON: %v\noutput:\n%s", err, out)
	}
	if rep.Healthy {
		t.Fatalf("a stale critical source must make doctor report unhealthy:\n%s", out)
	}
	var found *doctorCheck
	for i, c := range rep.Checks {
		if c.Name == "source_fresh:gmail" {
			found = &rep.Checks[i]
		}
	}
	if found == nil {
		t.Fatalf("doctor --json missing the source_fresh:gmail check:\n%s", out)
	}
	if found.OK || !found.Critical {
		t.Fatalf("source_fresh:gmail check = %+v, want OK=false Critical=true", *found)
	}

	var out2 bytes.Buffer
	if err := Run(testCtx(t), []string{"doctor", "--strict"}, &out2, &out2, strings.NewReader("")); err == nil {
		t.Fatalf("doctor --strict must error when a critical source is stale; output:\n%s", out2.String())
	}
}

// TestFailedSyncNeverAdvancesWatermark (HEALTH-04 pin): a fresh failed attempt
// layered over an OLD success must classify as failed (not stale, not fresh) and
// must never fabricate a newer LastSuccessAt — the M-3 by-construction property
// (internal/memory/status.go) pinned so a future refactor cannot silently lose it.
func TestFailedSyncNeverAdvancesWatermark(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	oldSuccess := now.Add(-30 * time.Hour).UTC().Format(time.RFC3339) // itself > 24h: proves failed beats stale

	seedSyncStatusFull(t, cfg, "gmail", &memory.SyncStatus{
		Source:        "gmail",
		LastAttemptAt: now.Add(-5 * time.Minute).UTC().Format(time.RFC3339),
		LastError:     "database or disk is full (13)",
		LastSuccessAt: oldSuccess,
	})

	got := sourceHealthAll(cfg, now)
	if len(got) != 1 {
		t.Fatalf("want 1 source, got %+v", got)
	}
	h := got[0]
	if h.State != healthFailed {
		t.Fatalf("a fresh failed attempt over an old success must be %q, got %q: %+v", healthFailed, h.State, h)
	}
	if h.LastSuccessAt != oldSuccess {
		t.Fatalf("failed sync must not advance the success watermark: got %q, want %q", h.LastSuccessAt, oldSuccess)
	}

	st, err := memory.LoadStatus(syncStatusPathFor(cfg, Source{Name: "gmail", Type: "gmail"}))
	if err != nil {
		t.Fatalf("LoadStatus: %v", err)
	}
	if st.LastSuccessAt != oldSuccess {
		t.Fatalf("on-disk LastSuccessAt was mutated by a failed sync: got %q, want %q", st.LastSuccessAt, oldSuccess)
	}
}

// ---------------------------------------------------------------------------
// Packet B — the red first line + the pulse
// ---------------------------------------------------------------------------

// TestHealthBannerOrdering (HEALTH-02): worst-first ordering — failed > never >
// stale — with a tie-break on age descending, and "" when everything is fresh.
func TestHealthBannerOrdering(t *testing.T) {
	cases := []struct {
		name    string
		in      []sourceHealth
		wantKey string // "" means the banner must be empty
	}{
		{
			name:    "all fresh is empty",
			in:      []sourceHealth{{Key: "gmail", State: healthFresh}},
			wantKey: "",
		},
		{
			name: "failed beats never and stale",
			in: []sourceHealth{
				{Key: "imessage", State: healthStale, AgeHours: 100},
				{Key: "calendar", State: healthNever},
				{Key: "gmail", State: healthFailed, AgeHours: 2, LastError: "database or disk is full (13)"},
			},
			wantKey: "gmail",
		},
		{
			name: "never beats stale",
			in: []sourceHealth{
				{Key: "imessage", State: healthStale, AgeHours: 999},
				{Key: "calendar", State: healthNever},
			},
			wantKey: "calendar",
		},
		{
			name: "stale ties break on age descending",
			in: []sourceHealth{
				{Key: "a", State: healthStale, AgeHours: 30},
				{Key: "b", State: healthStale, AgeHours: 90},
			},
			wantKey: "b",
		},
	}
	for _, c := range cases {
		subRun(t, c.name, func(t *testing.T) {
			got := healthBannerFromSources(c.in)
			if c.wantKey == "" {
				if got != "" {
					t.Fatalf("banner = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, c.wantKey) {
				t.Fatalf("banner = %q, want it to name the worst source %q", got, c.wantKey)
			}
		})
	}
}

// TestHealthBannerEndToEnd exercises the healthBanner(cfg, now) convenience
// wrapper against real on-disk fixtures, and pins the exact shape of the
// rendered line: ONE line, "🔴 MORA HEALTH: " prefix, "Run: mora doctor" suffix.
func TestHealthBannerEndToEnd(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	if got := healthBanner(cfg, now); got != "" {
		t.Fatalf("no enabled sources: banner = %q, want empty", got)
	}

	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	if got := healthBanner(cfg, now); got != "" {
		t.Fatalf("fresh gmail: banner = %q, want empty", got)
	}

	seedSyncStatus(t, cfg, "gmail", now.Add(-52*time.Hour))
	got := healthBanner(cfg, now)
	if !strings.HasPrefix(got, "🔴 MORA HEALTH:") || !strings.HasSuffix(got, "Run: mora doctor") {
		t.Fatalf("banner = %q, want the red one-line alarm shape", got)
	}
	if !strings.Contains(got, "gmail") || !strings.Contains(got, "52h") {
		t.Fatalf("banner = %q, want it to name gmail and its 52h age", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("banner must be a SINGLE line, got %q", got)
	}
}

// TestDigestRendersBannerFirst (HEALTH-02): the daily digest's FIRST content
// line — right after the header, before "Fresh as of:" — is the red banner
// when a required source is unhealthy.
func TestDigestRendersBannerFirst(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	seedSyncStatus(t, cfg, "gmail", now.Add(-30*time.Hour)) // stale: > 24h gmail threshold

	d := deltaPreview(t, cfg, now)
	body := renderDigestBody(d)
	lines := strings.SplitN(body, "\n", 3)
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "# Mora digest") {
		t.Fatalf("digest header missing/misplaced:\n%s", body)
	}
	if !strings.HasPrefix(lines[1], "🔴 MORA HEALTH:") {
		t.Fatalf("banner must be the first content line after the header, got line 2 = %q\nfull body:\n%s", lines[1], body)
	}
}

// TestDigestOmitsBannerWhenHealthy: the flip side — a fully fresh vault renders
// no banner at all (byte-neutral for the common case).
func TestDigestOmitsBannerWhenHealthy(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))

	d := deltaPreview(t, cfg, now)
	body := renderDigestBody(d)
	if strings.Contains(body, "MORA HEALTH") {
		t.Fatalf("a healthy digest must not render a banner:\n%s", body)
	}
}

// TestDigestTightBudgetNeverEvictsASurvivor (▸CX budget accounting): the health
// banner's bytes must be reserved in budgetDigestForMarkdown's frame — pinned
// here at the TIGHTEST budget that holds the full render (header + banner +
// freshness + section + item) with ZERO slack. If the banner were rendered
// outside the reserved frame, that missing reservation would let
// budgetDigestForMarkdown (wrongly) mark the item a survivor while the final
// truncateRunes safety net silently clips it to make room for the
// unaccounted-for banner bytes — this test proves every id budgetDigestForMarkdown
// calls a survivor is FULLY present, byte-for-byte, in the actually-rendered output.
func TestDigestTightBudgetNeverEvictsASurvivor(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	seedSyncStatus(t, cfg, "gmail", now.Add(-30*time.Hour)) // stale -> non-empty banner
	digestSeed(t, cfg, "gmail", "Important thread", 1*time.Hour, now)

	d := deltaPreview(t, cfg, now)
	if healthBannerFromSources(d.SourceHealth) == "" {
		t.Fatalf("test setup: expected a non-empty banner, got SourceHealth=%+v", d.SourceHealth)
	}
	if len(d.Sections) == 0 || len(d.Sections[0].Items) == 0 {
		t.Fatalf("test setup: expected a surfaced gmail item, got sections=%+v", d.Sections)
	}
	itemID := d.Sections[0].Items[0].ID
	fullRenderBytes := len(renderDigestBody(d))

	// Walk budgets from the smallest possible up to a full render's worth,
	// stopping at the FIRST (tightest) budget where the item first becomes a
	// budget survivor. That is the razor's-edge case ▸CX calls out: if the
	// banner's bytes aren't reserved in the budget frame, this is exactly the
	// point where budgetDigestForMarkdown can mark the item a survivor while the
	// final truncateRunes safety net has no room left and silently clips it.
	var tight int
	var survived map[string]bool
	for b := 1; b <= fullRenderBytes+64; b++ {
		_, s := budgetDigestForMarkdown(d, b)
		if s[itemID] {
			tight, survived = b, s
			break
		}
	}
	if survived == nil {
		t.Fatalf("item %q never became a budget survivor at any budget up to %d bytes", itemID, fullRenderBytes+64)
	}

	budgeted, _ := budgetDigestForMarkdown(d, tight)
	out := renderDigest(d, tight) // the full pipeline, including the truncateRunes safety net

	var survivorLine string
	for _, s := range budgeted.Sections {
		for _, it := range s.Items {
			if it.ID == itemID {
				survivorLine = renderDigestItemLine(it)
			}
		}
	}
	if survivorLine == "" {
		t.Fatalf("item %q reported as survived but absent from the budgeted digest's own sections", itemID)
	}
	if !strings.Contains(out, survivorLine) {
		t.Fatalf("item %q marked a budget survivor at its tightest fitting budget (%d bytes) but its full line is missing from the rendered output (silently truncated):\nrendered:\n%s", itemID, tight, out)
	}
	if !strings.HasPrefix(strings.SplitN(out, "\n", 3)[1], "🔴 MORA HEALTH:") {
		t.Fatalf("the banner itself must still be the first content line at this tight budget:\n%s", out)
	}
}

// TestMeetingBriefRendersBanner (HEALTH-02): MCP meeting_prep returns the
// MeetingBrief struct directly, so it must carry the health snapshot — and the
// CLI/human render must show the banner as the first line, so a brief over a
// dead corpus is never confidently silent about it.
func TestMeetingBriefRendersBanner(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := testCtx(t)
	at := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)

	enableSources(t, cfg, "gmail")
	seedSyncStatusFull(t, cfg, "gmail", &memory.SyncStatus{
		Source:        "gmail",
		LastAttemptAt: at.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
		LastSuccessAt: at.Add(-2 * time.Hour).UTC().Format(time.RFC3339),
		LastError:     "database or disk is full (13)",
	})

	event := eventMemFull(
		"calendar_event/founder-sync",
		"Founder sync",
		at.Add(2*time.Hour).Format(time.RFC3339),
		map[string]string{"adit@example.com": "Adit", "neil@example.com": "Neil Patel"},
		"adit@example.com", "neil@example.com",
	)
	event.Source = "calendar_event/founder-sync"
	event.Provider = "google"
	event.ProviderID = "calendar_event/founder-sync"
	if err := writeMemory(cfg, event); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	brief, err := buildEventMeetingBrief(ctx, cfg, event.ID, at, 0, 8)
	if err != nil {
		t.Fatal(err)
	}
	if healthBannerFromSources(brief.SourceHealth) == "" {
		t.Fatalf("test setup: expected a non-empty banner in brief.SourceHealth=%+v", brief.SourceHealth)
	}

	var out bytes.Buffer
	if err := renderMeetingBrief(&out, brief); err != nil {
		t.Fatal(err)
	}
	first := strings.SplitN(out.String(), "\n", 2)[0]
	if !strings.HasPrefix(first, "🔴 MORA HEALTH:") {
		t.Fatalf("meeting brief must render the health banner as the first line, got:\n%s", out.String())
	}
}

// ---------------------------------------------------------------------------
// `mora doctor --pulse`
// ---------------------------------------------------------------------------

// TestDoctorPulseHealthyExit0: every enabled source fresh -> exit 0, no toast.
func TestDoctorPulseHealthyExit0(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	origClock := doctorClock
	doctorClock = func() time.Time { return now }
	t.Cleanup(func() { doctorClock = origClock })

	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))

	origGOOS := runtimeGOOS
	runtimeGOOS = func() string { return "darwin" }
	t.Cleanup(func() { runtimeGOOS = origGOOS })
	origRunner := doctorNotifyRunner
	t.Cleanup(func() { doctorNotifyRunner = origRunner })
	called := false
	doctorNotifyRunner = func(args ...string) error { called = true; return nil }

	var out bytes.Buffer
	err := Run(testCtx(t), []string{"doctor", "--pulse"}, &out, &out, strings.NewReader(""))
	if err != nil {
		t.Fatalf("doctor --pulse on a healthy vault must exit 0: %v\noutput:\n%s", err, out.String())
	}
	if called {
		t.Fatalf("doctor --pulse must not notify when every source is fresh")
	}
	if strings.Contains(out.String(), "MORA HEALTH") {
		t.Fatalf("doctor --pulse must not print a banner when healthy:\n%s", out.String())
	}
}

// TestDoctorPulseExit2AndNotifies (HEALTH-02 delivery): an unhealthy source
// makes `doctor --pulse` exit 2 (a TYPED exit code, not a bare error mapping to
// exit 1) and post a toast through the injectable notify runner — asserted
// directly on the captured argv, mirroring notify_test.go's recordingRunner.
func TestDoctorPulseExit2AndNotifies(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	origClock := doctorClock
	doctorClock = func() time.Time { return now }
	t.Cleanup(func() { doctorClock = origClock })

	enableSources(t, cfg, "gmail")
	seedSyncStatusFull(t, cfg, "gmail", &memory.SyncStatus{
		Source:        "gmail",
		LastAttemptAt: now.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
		LastSuccessAt: now.Add(-52 * time.Hour).UTC().Format(time.RFC3339),
		LastError:     "database or disk is full (13)",
	})

	origGOOS := runtimeGOOS
	runtimeGOOS = func() string { return "darwin" }
	t.Cleanup(func() { runtimeGOOS = origGOOS })
	origRunner := doctorNotifyRunner
	t.Cleanup(func() { doctorNotifyRunner = origRunner })
	var gotArgs []string
	doctorNotifyRunner = func(args ...string) error { gotArgs = append([]string(nil), args...); return nil }

	var out bytes.Buffer
	err := Run(testCtx(t), []string{"doctor", "--pulse"}, &out, &out, strings.NewReader(""))
	if err == nil {
		t.Fatalf("doctor --pulse must error when a source is unhealthy; output:\n%s", out.String())
	}
	code, ok := ExitCodeFor(err)
	if !ok || code != 2 {
		t.Fatalf("doctor --pulse error = %v (ExitCodeFor ok=%v code=%d), want a typed exit code 2", err, ok, code)
	}
	if !strings.Contains(out.String(), "🔴 MORA HEALTH:") {
		t.Fatalf("doctor --pulse must print the banner:\n%s", out.String())
	}
	if len(gotArgs) != 2 || gotArgs[0] != "-e" {
		t.Fatalf("notify runner argv = %#v, want [-e, <script>]", gotArgs)
	}
	if !strings.Contains(gotArgs[1], "display notification") || !strings.Contains(gotArgs[1], "gmail") {
		t.Fatalf("notify script must name the unhealthy source: %q", gotArgs[1])
	}
}

// TestDoctorPulseJSONEmitsOnlySources: `--pulse --json` emits ONLY the sources
// array — no banner text mixed into the JSON stream.
func TestDoctorPulseJSONEmitsOnlySources(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	origClock := doctorClock
	doctorClock = func() time.Time { return now }
	t.Cleanup(func() { doctorClock = origClock })

	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now.Add(-30*time.Hour)) // stale

	origGOOS := runtimeGOOS
	runtimeGOOS = func() string { return "darwin" }
	t.Cleanup(func() { runtimeGOOS = origGOOS })
	origRunner := doctorNotifyRunner
	t.Cleanup(func() { doctorNotifyRunner = origRunner })
	doctorNotifyRunner = func(args ...string) error { return nil }

	var out bytes.Buffer
	_ = Run(testCtx(t), []string{"doctor", "--pulse", "--json"}, &out, &out, strings.NewReader(""))

	var rep struct {
		Sources []sourceHealth `json:"sources"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("doctor --pulse --json must emit JSON with a sources array: %v\noutput:\n%s", err, out.String())
	}
	if len(rep.Sources) != 1 || rep.Sources[0].Key != "gmail" || rep.Sources[0].State != healthStale {
		t.Fatalf("unexpected sources array: %+v", rep.Sources)
	}
	if strings.Contains(out.String(), "MORA HEALTH") {
		t.Fatalf("--pulse --json must not mix banner text into the JSON output:\n%s", out.String())
	}
}

// TestStampSyncAttemptFailureAdvancesOnRepeatedIdenticalError (review fix): the
// six-day incident was exactly this shape — the SAME error ("database or disk
// is full (13)") recurring every hour. Comparing LastError by TEXT to decide
// "already stamped" cannot tell "the inner path stamped this during the
// current attempt" from "a PREVIOUS attempt failed with the same string" — so
// a repeated identical pre-Ingest failure must still advance LastAttemptAt
// (and ErrorCount) on every attempt, not just the first.
func TestStampSyncAttemptFailureAdvancesOnRepeatedIdenticalError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	s := Source{Name: "gmail", Type: "gmail"}
	path := syncStatusPathFor(cfg, s)

	sameErr := errors.New("database or disk is full (13)")
	t1 := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	stampSyncAttemptFailure(cfg, s, sameErr, t1, nil)

	st1, err := memory.LoadStatus(path)
	if err != nil {
		t.Fatalf("LoadStatus: %v", err)
	}
	if st1.LastAttemptAt != t1.UTC().Format(time.RFC3339) {
		t.Fatalf("first attempt: LastAttemptAt = %q, want %q", st1.LastAttemptAt, t1.UTC().Format(time.RFC3339))
	}
	if st1.ErrorCount != 1 {
		t.Fatalf("first attempt: ErrorCount = %d, want 1", st1.ErrorCount)
	}

	// A SECOND attempt, one hour later, fails with the EXACT SAME error string.
	t2 := t1.Add(1 * time.Hour)
	stampSyncAttemptFailure(cfg, s, sameErr, t2, nil)

	st2, err := memory.LoadStatus(path)
	if err != nil {
		t.Fatalf("LoadStatus: %v", err)
	}
	if st2.LastAttemptAt != t2.UTC().Format(time.RFC3339) {
		t.Fatalf("a repeated IDENTICAL failure must still advance LastAttemptAt: got %q, want %q (the six-day incident's exact shape)", st2.LastAttemptAt, t2.UTC().Format(time.RFC3339))
	}
	if st2.ErrorCount != 2 {
		t.Fatalf("ErrorCount must keep incrementing across repeated identical failures: got %d, want 2", st2.ErrorCount)
	}
	if st2.LastError != sameErr.Error() {
		t.Fatalf("LastError = %q, want %q", st2.LastError, sameErr.Error())
	}
}

// TestStampSyncAttemptFailureSkipsWhenInnerPathAlreadyStamped: the ORIGINAL
// protective intent must survive the fix above — when the inner path
// (memory.Ingest via persistSyncStatus) already stamped THIS attempt (its
// LastAttemptAt is at-or-after the attempt's start), the outer stamp must NOT
// re-load-and-save, so it can never clobber a checkpoint/counter update the
// inner path already persisted with a stale re-read.
func TestStampSyncAttemptFailureSkipsWhenInnerPathAlreadyStamped(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	s := Source{Name: "gmail", Type: "gmail"}
	path := syncStatusPathFor(cfg, s)

	attemptStart := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	innerStampedAt := attemptStart.Add(2 * time.Second) // the inner path ran AFTER attemptStart was captured
	if err := memory.SaveStatus(path, &memory.SyncStatus{
		Source: "gmail", LastAttemptAt: innerStampedAt.UTC().Format(time.RFC3339),
		LastError: "boom", ErrorCount: 1, ItemCount: 4, Checkpoint: "page-7",
	}); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}

	stampSyncAttemptFailure(cfg, s, errors.New("boom"), attemptStart, nil)

	st, err := memory.LoadStatus(path)
	if err != nil {
		t.Fatalf("LoadStatus: %v", err)
	}
	if st.Checkpoint != "page-7" || st.ItemCount != 4 {
		t.Fatalf("must not clobber the inner path's checkpoint/counters: %+v", st)
	}
	if st.LastAttemptAt != innerStampedAt.UTC().Format(time.RFC3339) {
		t.Fatalf("must not overwrite the inner path's own attempt stamp: got %q, want %q", st.LastAttemptAt, innerStampedAt.UTC().Format(time.RFC3339))
	}
	if st.ErrorCount != 1 {
		t.Fatalf("must not double-count the inner path's own error: ErrorCount = %d, want 1", st.ErrorCount)
	}
}

// TestStampSyncAttemptFailureSkipsWhenInnerPathStampsSameSecond (review
// fix, round 2): RFC3339-serialized SyncStatus timestamps have only SECOND
// resolution, but attemptStart is a real time.Now() capture and carries
// nanoseconds. If the inner path (memory.Ingest via persistSyncStatus) ran
// LATER within the SAME wall-clock second, its stamp round-trips through
// RFC3339 losing the sub-second offset — comparing that against an
// un-truncated attemptStart would wrongly read as "before attemptStart" and
// double-stamp (clobbering the checkpoint/counters and double-counting
// ErrorCount) even though the inner path genuinely already handled this
// attempt.
func TestStampSyncAttemptFailureSkipsWhenInnerPathStampsSameSecond(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	s := Source{Name: "gmail", Type: "gmail"}
	path := syncStatusPathFor(cfg, s)

	// attemptStart is captured mid-second (.900s), matching a real time.Now().
	// The inner path ran a little later in that SAME second (.950s) and its
	// RFC3339-formatted stamp drops the fractional part, persisting as :00.000.
	attemptStart := time.Date(2026, 7, 1, 9, 0, 0, 900_000_000, time.UTC)
	sameSecondInnerStamp := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	if err := memory.SaveStatus(path, &memory.SyncStatus{
		Source: "gmail", LastAttemptAt: sameSecondInnerStamp.UTC().Format(time.RFC3339),
		LastError: "boom", ErrorCount: 1, ItemCount: 4, Checkpoint: "page-7",
	}); err != nil {
		t.Fatalf("SaveStatus: %v", err)
	}

	stampSyncAttemptFailure(cfg, s, errors.New("boom"), attemptStart, nil)

	st, err := memory.LoadStatus(path)
	if err != nil {
		t.Fatalf("LoadStatus: %v", err)
	}
	if st.Checkpoint != "page-7" || st.ItemCount != 4 {
		t.Fatalf("a same-second inner stamp must not be mistaken for an earlier attempt (checkpoint/counters clobbered): %+v", st)
	}
	if st.ErrorCount != 1 {
		t.Fatalf("must not double-count the inner path's error for a same-second stamp: ErrorCount = %d, want 1", st.ErrorCount)
	}
}

// TestDoctorUsesInjectedClockForGoogleAuthRecency (review fix): the normal
// (non --pulse) doctor check/render path must use the SAME injected
// doctorClock for EVERY rendered line, including the Google-auth-recency
// line — a stray time.Now() there would give one pinned doctor invocation two
// different "now"s.
func TestDoctorUsesInjectedClockForGoogleAuthRecency(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	tokenDir := filepath.Join(cfg.ConfigDir, "tokens")
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tokenDir, "google.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	authAt := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	if err := google.RecordAuth(tokenDir, "google", authAt); err != nil {
		t.Fatalf("RecordAuth: %v", err)
	}

	// Pin doctorClock far from the real wall clock: if the auth-recency line
	// used time.Now() instead of the injected clock, "X ago" would reflect the
	// real (near-zero) elapsed time since RecordAuth just ran, not this
	// deliberately large 52h gap.
	now := authAt.Add(52 * time.Hour)
	origClock := doctorClock
	doctorClock = func() time.Time { return now }
	t.Cleanup(func() { doctorClock = origClock })

	out := run(t, "doctor")
	want := "last authed " + authAt.Format(time.RFC3339) + " (2 days ago)"
	if !strings.Contains(out, want) {
		t.Fatalf("doctor text output must use the injected clock for auth recency, want %q in:\n%s", want, out)
	}
}

// TestIssue223HealthBannerAndCompactState covers issue #223 requirements:
// - producer-only stale/failed/never -> degraded/yellow
// - source/index red precedence
// - healthy no banner
// - bounded deterministic per-source projection with omitted count
// - cached brief transition
// - strict doctor critical producer failure
// - unreadable ledger fail-closed
func TestIssue223HealthBannerAndCompactState(t *testing.T) {
	subRun(t, "producer_only_degraded_yellow", func(t *testing.T) {
		withTempHome(t)
		run(t, "init")
		cfg := mustConfig(t)
		now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

		mustSeedExpected(t, cfg, expectedProducer{Name: "ingest-hourly", IntervalSeconds: 3600, Source: producerSourceScheduled})
		old := now.Add(-11 * time.Hour).UTC().Format(time.RFC3339)
		mustSeedStatus(t, cfg, producerStatus{Name: "ingest-hourly", LastSuccessAt: old, LastAttemptAt: old, SuccessTimes: []string{old}})

		h := healthOf(cfg, now)
		if h.State != healthDegraded {
			t.Fatalf("healthOf.State = %q, want %q", h.State, healthDegraded)
		}

		ch := compactHealthOf(cfg, now)
		if ch.State != healthDegraded {
			t.Fatalf("compactHealthOf.State = %q, want %q", ch.State, healthDegraded)
		}
		if !strings.HasPrefix(ch.Banner, "🟡 MORA HEALTH:") || !strings.Contains(ch.Banner, "ingest-hourly") {
			t.Fatalf("compactHealth.Banner = %q, want yellow producer banner", ch.Banner)
		}
	})

	subRun(t, "source_index_red_precedence", func(t *testing.T) {
		withTempHome(t)
		run(t, "init")
		cfg := mustConfig(t)
		now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

		enableSources(t, cfg, "gmail")
		seedSyncStatusFull(t, cfg, "gmail", &memory.SyncStatus{
			Source:        "gmail",
			LastAttemptAt: now.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
			LastSuccessAt: now.Add(-48 * time.Hour).UTC().Format(time.RFC3339),
			LastError:     "connection refused",
		})

		mustSeedExpected(t, cfg, expectedProducer{Name: "ingest-hourly", IntervalSeconds: 3600, Source: producerSourceScheduled})
		old := now.Add(-11 * time.Hour).UTC().Format(time.RFC3339)
		mustSeedStatus(t, cfg, producerStatus{Name: "ingest-hourly", LastSuccessAt: old, LastAttemptAt: old, SuccessTimes: []string{old}})

		h := healthOf(cfg, now)
		if h.State != healthUnhealthy {
			t.Fatalf("healthOf.State = %q, want %q", h.State, healthUnhealthy)
		}

		banner := healthBanner(cfg, now)
		if !strings.HasPrefix(banner, "🔴 MORA HEALTH:") || !strings.Contains(banner, "gmail") {
			t.Fatalf("banner = %q, want RED source alarm outranking yellow producer warning", banner)
		}
	})

	subRun(t, "healthy_no_banner", func(t *testing.T) {
		withTempHome(t)
		run(t, "init")
		cfg := mustConfig(t)
		now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

		h := healthOf(cfg, now)
		if h.State != healthHealthy {
			t.Fatalf("healthOf.State = %q, want %q", h.State, healthHealthy)
		}

		banner := healthBanner(cfg, now)
		if banner != "" {
			t.Fatalf("banner = %q, want empty for healthy vault", banner)
		}
	})

	subRun(t, "two_long_keys_same_prefix", func(t *testing.T) {
		k1 := "shared_prefix_alpha"
		k2 := "shared_prefix_beta"
		h := Health{
			State: healthDegraded,
			Sources: []sourceHealth{
				{Key: k1, State: healthStale},
				{Key: k2, State: healthStale},
			},
			Index: indexHealth{State: idxFresh},
		}
		ch := compactHealthFrom(h)
		if len(ch.PerSource) != 2 {
			t.Fatalf("len(PerSource) = %d, want 2", len(ch.PerSource))
		}
		if ch.PerSource[k1] != healthStale || ch.PerSource[k2] != healthStale {
			t.Fatalf("PerSource missing exact shared-prefix keys: %+v", ch.PerSource)
		}
		if ch.SourcesOmitted != 0 {
			t.Fatalf("SourcesOmitted = %d, want 0", ch.SourcesOmitted)
		}
	})

	subRun(t, "extremely_long_escaped_key_omitted", func(t *testing.T) {
		// Key larger than compactSourceBytesCap (80 bytes)
		longKey := "very_long_source_key_that_exceeds_the_exact_json_byte_cap_of_80_bytes_all_by_itself_and_contains_escaped_quotes_\"quoted\"_and_newlines_\n_to_force_json_escaping_overflow"
		h := Health{
			State: healthDegraded,
			Sources: []sourceHealth{
				{Key: "gmail", State: healthStale},
				{Key: longKey, State: healthStale},
			},
			Index: indexHealth{State: idxFresh},
		}
		ch := compactHealthFrom(h)
		if ch.PerSource[longKey] != "" {
			t.Fatalf("longKey should be omitted due to byte cap, but got included: %q", ch.PerSource[longKey])
		}
		if ch.PerSource["gmail"] != healthStale {
			t.Fatalf("gmail should be included: %q", ch.PerSource["gmail"])
		}
		if ch.SourcesOmitted != 1 {
			t.Fatalf("SourcesOmitted = %d, want 1 for omitted longKey", ch.SourcesOmitted)
		}
	})

	subRun(t, "deterministic_selection", func(t *testing.T) {
		h := Health{
			State: healthDegraded,
			Sources: []sourceHealth{
				{Key: "z_fresh", State: healthFresh},
				{Key: "a_stale", State: healthStale},
				{Key: "b_failed", State: healthFailed},
				{Key: "c_never", State: healthNever},
				{Key: "d_fresh", State: healthFresh},
			},
			Index: indexHealth{State: idxFresh},
		}
		ch := compactHealthFrom(h)
		// b_failed (rank 0), c_never (rank 1), a_stale (rank 2) must be selected
		if ch.PerSource["b_failed"] != healthFailed || ch.PerSource["c_never"] != healthNever || ch.PerSource["a_stale"] != healthStale {
			t.Fatalf("PerSource deterministic selection wrong: %+v", ch.PerSource)
		}
	})

	subRun(t, "bounded_deterministic_per_source_projection", func(t *testing.T) {
		// Table case 1: >cap same-state sources proving oldest (AgeHours desc) selected
		hAge := Health{
			State: healthDegraded,
			Sources: []sourceHealth{
				{Key: "src_young", State: healthStale, AgeHours: 5},
				{Key: "src_oldest", State: healthStale, AgeHours: 100},
				{Key: "src_older", State: healthStale, AgeHours: 50},
				{Key: "src_newest", State: healthStale, AgeHours: 1},
			},
			Index: indexHealth{State: idxFresh},
		}
		chAge := compactHealthFrom(hAge)
		if len(chAge.PerSource) != compactSourceCap {
			t.Fatalf("len(PerSource) = %d, want %d", len(chAge.PerSource), compactSourceCap)
		}
		if chAge.SourcesOmitted != 1 {
			t.Fatalf("SourcesOmitted = %d, want 1", chAge.SourcesOmitted)
		}
		if _, ok := chAge.PerSource["src_oldest"]; !ok {
			t.Fatalf("src_oldest (AgeHours 100) must be selected: %+v", chAge.PerSource)
		}
		if _, ok := chAge.PerSource["src_older"]; !ok {
			t.Fatalf("src_older (AgeHours 50) must be selected: %+v", chAge.PerSource)
		}
		if _, ok := chAge.PerSource["src_young"]; !ok {
			t.Fatalf("src_young (AgeHours 5) must be selected: %+v", chAge.PerSource)
		}
		if _, ok := chAge.PerSource["src_newest"]; ok {
			t.Fatalf("src_newest (AgeHours 1) should be omitted, but was included: %+v", chAge.PerSource)
		}

		// Table case 2: escaped, multibyte, very long, and shared-prefix exact keys
		hComplex := Health{
			State: healthDegraded,
			Sources: []sourceHealth{
				{Key: "escaped_\"quote\"_\n", State: healthStale, AgeHours: 10},
				{Key: "multibyte_日本語_data", State: healthStale, AgeHours: 10},
				{Key: "prefix_shared_one", State: healthStale, AgeHours: 10},
				{Key: "prefix_shared_two", State: healthStale, AgeHours: 10},
			},
			Index: indexHealth{State: idxFresh},
		}
		ch1 := compactHealthFrom(hComplex)
		ch2 := compactHealthFrom(hComplex)

		// Assert deterministic repeat (byte-identical JSON)
		b1, err1 := json.Marshal(ch1)
		b2, err2 := json.Marshal(ch2)
		if err1 != nil || err2 != nil || string(b1) != string(b2) {
			t.Fatalf("deterministic repeat failed: b1=%s, b2=%s", b1, b2)
		}

		// Assert marshaled PerSource byte cap
		bMap, _ := json.Marshal(ch1.PerSource)
		if len(bMap) > compactSourceBytesCap {
			t.Fatalf("len(json.Marshal(PerSource)) = %d > compactSourceBytesCap %d", len(bMap), compactSourceBytesCap)
		}
	})

	subRun(t, "fresh_index_failed_share_stale_producer", func(t *testing.T) {
		withTempHome(t)
		run(t, "init")
		cfg := mustConfig(t)
		now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

		// Personal index fresh, but a subscription share index failed
		mustSeedExpected(t, cfg, expectedProducer{Name: "ingest-hourly", IntervalSeconds: 3600, Source: producerSourceScheduled})
		old := now.Add(-11 * time.Hour).UTC().Format(time.RFC3339)
		mustSeedStatus(t, cfg, producerStatus{Name: "ingest-hourly", LastSuccessAt: old, LastAttemptAt: old, SuccessTimes: []string{old}})

		h := Health{
			Sources:   []sourceHealth{{Key: "gmail", State: healthFresh}},
			Index:     indexHealth{State: idxFresh, Shares: []indexHealth{{State: idxFailed, LastError: "integrity digest mismatch"}}},
			Producers: producerHealthAll(cfg, now),
		}
		h.State = aggregateHealthState(h)

		if h.State != healthUnhealthy {
			t.Fatalf("h.State = %q, want unhealthy for failed share index", h.State)
		}
		banner := healthBannerFrom(h)
		if !strings.HasPrefix(banner, "🔴 MORA HEALTH:") || !strings.Contains(banner, "subscription index") {
			t.Fatalf("banner = %q, want RED share index alarm outranking yellow producer warning", banner)
		}
	})

	subRun(t, "digest_generated_failed_share_red_precedence", func(t *testing.T) {
		withTempHome(t)
		run(t, "init")
		cfg := mustConfig(t)
		now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

		// Seed stale producer
		mustSeedExpected(t, cfg, expectedProducer{Name: "ingest-hourly", IntervalSeconds: 3600, Source: producerSourceScheduled})
		old := now.Add(-11 * time.Hour).UTC().Format(time.RFC3339)
		mustSeedStatus(t, cfg, producerStatus{Name: "ingest-hourly", LastSuccessAt: old, LastAttemptAt: old, SuccessTimes: []string{old}})

		// Seed a registered broken subscription so shareIndexHealthAll returns idxFailed
		registerSub(t, cfg, "team")
		subDir := filepath.Join(filepath.Dir(cfg.VaultDir), "subs", "team")
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatalf("MkdirAll sub: %v", err)
		}
		if err := os.WriteFile(filepath.Join(subDir, "migrated"), []byte("1"), 0o644); err != nil {
			t.Fatalf("WriteFile migrated: %v", err)
		}

		d, err := buildDigest(cfg, now, briefOpts{})
		if err != nil {
			t.Fatalf("buildDigest: %v", err)
		}
		banner := renderDigestHealthBanner(d)
		if !strings.HasPrefix(banner, "🔴 MORA HEALTH:") || !strings.Contains(banner, "subscription index") {
			t.Fatalf("renderDigestHealthBanner = %q, want RED subscription index banner", banner)
		}
		payload := digestMCPPayload(cfg, d, 20000)
		hMap, ok := payload["health"].(compactHealth)
		if !ok || hMap.State != healthUnhealthy {
			t.Fatalf("digest payload health = %+v, want state=unhealthy", payload["health"])
		}
		if !strings.HasPrefix(hMap.Banner, "🔴 MORA HEALTH:") || !strings.Contains(hMap.Banner, "subscription index") {
			t.Fatalf("digest payload banner = %q, want RED subscription index banner", hMap.Banner)
		}
	})

	subRun(t, "daily_brief_failed_share_red_precedence", func(t *testing.T) {
		withTempHome(t)
		run(t, "init")
		cfg := mustConfig(t)
		now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

		mustSeedExpected(t, cfg, expectedProducer{Name: "ingest-hourly", IntervalSeconds: 3600, Source: producerSourceScheduled})
		old := now.Add(-11 * time.Hour).UTC().Format(time.RFC3339)
		mustSeedStatus(t, cfg, producerStatus{Name: "ingest-hourly", LastSuccessAt: old, LastAttemptAt: old, SuccessTimes: []string{old}})

		registerSub(t, cfg, "team")
		subDir := filepath.Join(filepath.Dir(cfg.VaultDir), "subs", "team")
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatalf("MkdirAll sub: %v", err)
		}
		if err := os.WriteFile(filepath.Join(subDir, "migrated"), []byte("1"), 0o644); err != nil {
			t.Fatalf("WriteFile migrated: %v", err)
		}

		d, err := buildDigest(cfg, now, briefOpts{advance: true})
		if err != nil {
			t.Fatalf("buildDigest delta: %v", err)
		}
		banner := renderDigestHealthBanner(d)
		if !strings.HasPrefix(banner, "🔴 MORA HEALTH:") || !strings.Contains(banner, "subscription index") {
			t.Fatalf("delta digest banner = %q, want RED subscription index banner", banner)
		}
	})

	subRun(t, "meeting_brief_failed_share_red_precedence", func(t *testing.T) {
		withTempHome(t)
		run(t, "init")
		cfg := mustConfig(t)
		now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

		mustSeedExpected(t, cfg, expectedProducer{Name: "ingest-hourly", IntervalSeconds: 3600, Source: producerSourceScheduled})
		old := now.Add(-11 * time.Hour).UTC().Format(time.RFC3339)
		mustSeedStatus(t, cfg, producerStatus{Name: "ingest-hourly", LastSuccessAt: old, LastAttemptAt: old, SuccessTimes: []string{old}})

		registerSub(t, cfg, "team")
		subDir := filepath.Join(filepath.Dir(cfg.VaultDir), "subs", "team")
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatalf("MkdirAll sub: %v", err)
		}
		if err := os.WriteFile(filepath.Join(subDir, "migrated"), []byte("1"), 0o644); err != nil {
			t.Fatalf("WriteFile migrated: %v", err)
		}

		brief, err := buildNextMeetingBrief(context.Background(), cfg, now, nil, 0, 0)
		if err != nil {
			t.Fatalf("buildNextMeetingBrief: %v", err)
		}
		if brief.Health.State != healthUnhealthy {
			t.Fatalf("meetingBrief.Health.State = %q, want unhealthy", brief.Health.State)
		}
		if !strings.HasPrefix(brief.Health.Banner, "🔴 MORA HEALTH:") || !strings.Contains(brief.Health.Banner, "subscription index") {
			t.Fatalf("meetingBrief banner = %q, want RED subscription index banner", brief.Health.Banner)
		}
	})

	subRun(t, "cached_brief_transition", func(t *testing.T) {
		withTempHome(t)
		run(t, "init")
		cfg := mustConfig(t)
		now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

		// 1. Cached brief with red banner, now healthy -> strips banner
		bodyRed := "# Brief\n🔴 MORA HEALTH: gmail — stale. Run: mora doctor\n\nContent line\n"
		got := reconcileCachedBriefHealth(cfg, now, bodyRed)
		if strings.Contains(got, "MORA HEALTH") {
			t.Fatalf("reconcileCachedBriefHealth did not strip red banner when healthy:\n%s", got)
		}

		// 2. Cached brief with yellow banner, now healthy -> strips banner
		bodyYellow := "# Brief\n🟡 MORA HEALTH: ingest-hourly stale. Run: mora doctor\n\nContent line\n"
		got = reconcileCachedBriefHealth(cfg, now, bodyYellow)
		if strings.Contains(got, "MORA HEALTH") {
			t.Fatalf("reconcileCachedBriefHealth did not strip yellow banner when healthy:\n%s", got)
		}

		// 3. Cached brief with red banner, now producer stale -> replaces red with yellow
		mustSeedExpected(t, cfg, expectedProducer{Name: "ingest-hourly", IntervalSeconds: 3600, Source: producerSourceScheduled})
		old := now.Add(-11 * time.Hour).UTC().Format(time.RFC3339)
		mustSeedStatus(t, cfg, producerStatus{Name: "ingest-hourly", LastSuccessAt: old, LastAttemptAt: old, SuccessTimes: []string{old}})

		got = reconcileCachedBriefHealth(cfg, now, bodyRed)
		if !strings.Contains(got, "🟡 MORA HEALTH:") || strings.Contains(got, "🔴 MORA HEALTH:") {
			t.Fatalf("reconcileCachedBriefHealth did not replace red with yellow banner:\n%s", got)
		}
		lines := strings.Split(got, "\n")
		healthLineCount := 0
		for _, l := range lines {
			if isHealthBannerLine(l) {
				healthLineCount++
			}
		}
		if healthLineCount != 1 {
			t.Fatalf("reconcileCachedBriefHealth resulted in %d health lines, want 1:\n%s", healthLineCount, got)
		}
	})

	subRun(t, "strict_doctor_critical_producer_failure", func(t *testing.T) {
		withTempHome(t)
		run(t, "init")
		cfg := mustConfig(t)
		now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

		mustSeedExpected(t, cfg, expectedProducer{Name: "pulse-daily", IntervalSeconds: 86400, Source: producerSourceScheduled})
		old := now.Add(-72 * time.Hour).UTC().Format(time.RFC3339)
		mustSeedStatus(t, cfg, producerStatus{Name: "pulse-daily", LastSuccessAt: old, LastAttemptAt: old, SuccessTimes: []string{old}})

		setDoctorClock(t, now)
		var strictOut bytes.Buffer
		err := Run(testCtx(t), []string{"doctor", "--strict"}, &strictOut, &strictOut, strings.NewReader(""))
		if err == nil {
			t.Fatalf("doctor --strict must fail on critical producer failure")
		}
	})

	subRun(t, "unreadable_ledger_fail_closed", func(t *testing.T) {
		withTempHome(t)
		run(t, "init")
		cfg := mustConfig(t)
		now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

		statusPath := producerStatusPath(cfg)
		if err := os.MkdirAll(filepath.Dir(statusPath), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(statusPath, []byte("INVALID_JSON{"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		h := healthOf(cfg, now)
		if h.State != healthUnhealthy {
			t.Fatalf("healthOf.State = %q, want %q for corrupt producer ledger", h.State, healthUnhealthy)
		}

		banner := healthBanner(cfg, now)
		if !strings.HasPrefix(banner, "🔴 MORA HEALTH:") || !strings.Contains(banner, "producer") {
			t.Fatalf("banner = %q, want RED fail-closed alarm for corrupt producer ledger", banner)
		}
	})

	subRun(t, "cap_banner_line_byte_cap_and_utf8", func(t *testing.T) {
		tests := []struct {
			name          string
			input         string
			wantBytes     int
			wantEllipsis  bool
			wantSanitized bool
		}{
			{name: "ascii_short", input: "🟡 MORA HEALTH: short banner", wantBytes: 30},
			{name: "ascii_boundary_239", input: strings.Repeat("a", 239), wantBytes: 239},
			{name: "ascii_exact_240", input: strings.Repeat("a", 240), wantBytes: 240},
			{name: "ascii_boundary_241", input: strings.Repeat("a", 241), wantBytes: 240, wantEllipsis: true},
			{name: "cjk_exact_240", input: strings.Repeat("界", 80), wantBytes: 240},
			{name: "cjk_over_240", input: strings.Repeat("界", 81), wantBytes: 240, wantEllipsis: true},
			{name: "emoji_exact_240", input: strings.Repeat("🚀", 60), wantBytes: 240},
			{name: "emoji_over_240", input: strings.Repeat("🚀", 61), wantBytes: 239, wantEllipsis: true},
			{name: "combining_over_240", input: strings.Repeat("e\u0301", 100), wantBytes: 240, wantEllipsis: true},
			{name: "escaped_error_input", input: strings.Repeat("driver said \"boom\" at C:\\tmp\\db\nnext\t", 10), wantBytes: 240, wantEllipsis: true, wantSanitized: true},
			{name: "invalid_utf8_short", input: string([]byte{'a', 0xff, 'b'}), wantBytes: 5, wantSanitized: true},
			{name: "invalid_utf8_over_cap", input: strings.Repeat(string([]byte{0xff})+"x", 100), wantBytes: 239, wantEllipsis: true, wantSanitized: true},
		}

		for _, tt := range tests {
			tt := tt
			subRun(t, tt.name, func(t *testing.T) {
				got := capBannerLine(tt.input)
				if len(got) != tt.wantBytes {
					t.Fatalf("len(capBannerLine(%s)) = %d bytes, want %d: %q", tt.name, len(got), tt.wantBytes, got)
				}
				if !utf8.ValidString(got) {
					t.Fatalf("capBannerLine(%s) produced invalid UTF-8: %q", tt.name, got)
				}
				if strings.HasSuffix(got, "…") != tt.wantEllipsis {
					t.Fatalf("capBannerLine(%s) ellipsis = %v, want %v: %q", tt.name, strings.HasSuffix(got, "…"), tt.wantEllipsis, got)
				}
				if tt.wantSanitized && strings.ContainsAny(got, "\n\r\t") {
					t.Fatalf("capBannerLine(%s) retained a line-breaking control: %q", tt.name, got)
				}
			})
		}
	})

	subRun(t, "producer_named_producers_vs_corrupt_ledger", func(t *testing.T) {
		now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
		cases := []struct {
			name   string
			state  string
			status *producerStatus
		}{
			{name: "never", state: prodNever},
			{name: "stale", state: prodStale, status: &producerStatus{
				Name: "producers", LastSuccessAt: now.Add(-10 * time.Hour).Format(time.RFC3339),
				LastAttemptAt: now.Add(-10 * time.Hour).Format(time.RFC3339),
				SuccessTimes:  []string{now.Add(-10 * time.Hour).Format(time.RFC3339)},
			}},
			{name: "failed", state: prodFailed, status: &producerStatus{
				Name: "producers", LastSuccessAt: now.Add(-10 * time.Hour).Format(time.RFC3339),
				LastAttemptAt: now.Add(-time.Hour).Format(time.RFC3339), LastError: "job timeout",
				SuccessTimes: []string{now.Add(-10 * time.Hour).Format(time.RFC3339)},
			}},
		}
		for _, tc := range cases {
			tc := tc
			subRun(t, "valid_"+tc.name, func(t *testing.T) {
				withTempHome(t)
				run(t, "init")
				cfg := mustConfig(t)
				mustSeedExpected(t, cfg, expectedProducer{Name: "producers", IntervalSeconds: 3600, Source: producerSourceScheduled})
				if tc.status != nil {
					mustSeedStatus(t, cfg, *tc.status)
				}

				h := healthOf(cfg, now)
				if h.State != healthDegraded {
					t.Fatalf("h.State = %q, want degraded for valid producer named producers in %s state", h.State, tc.state)
				}
				if len(h.Producers) != 1 || h.Producers[0].Subject != producerHealthSubjectProducer || h.Producers[0].State != tc.state {
					t.Fatalf("h.Producers = %+v, want one typed producer in state %s", h.Producers, tc.state)
				}
				banner := healthBannerFrom(h)
				if !strings.HasPrefix(banner, "🟡 MORA HEALTH:") || !strings.Contains(banner, "producers") {
					t.Fatalf("banner = %q, want YELLOW banner for valid producer named producers", banner)
				}
			})
		}

		for _, ledger := range []string{"expected", "status"} {
			ledger := ledger
			subRun(t, "corrupt_"+ledger, func(t *testing.T) {
				withTempHome(t)
				run(t, "init")
				cfg := mustConfig(t)
				if ledger == "status" {
					mustSeedExpected(t, cfg, expectedProducer{Name: "producers", IntervalSeconds: 3600, Source: producerSourceScheduled})
				}
				path := producerExpectedPath(cfg)
				if ledger == "status" {
					path = producerStatusPath(cfg)
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatalf("MkdirAll producer ledger: %v", err)
				}
				if err := os.WriteFile(path, []byte("NOT_VALID_JSON{"), 0o600); err != nil {
					t.Fatalf("WriteFile corrupt %s ledger: %v", ledger, err)
				}

				h := healthOf(cfg, now)
				if h.State != healthUnhealthy {
					t.Fatalf("h.State = %q, want unhealthy for corrupt %s ledger", h.State, ledger)
				}
				if len(h.Producers) != 1 || h.Producers[0].Subject != producerHealthSubjectLedger {
					t.Fatalf("h.Producers = %+v, want one typed ledger failure", h.Producers)
				}
				banner := healthBannerFrom(h)
				if !strings.HasPrefix(banner, "🔴 MORA HEALTH:") || !strings.Contains(banner, "producer ledger unreadable") {
					t.Fatalf("banner = %q, want RED unreadable-ledger banner", banner)
				}

				setDoctorClock(t, now)
				out := run(t, "doctor", "--json")
				var rep doctorReport
				if err := json.Unmarshal([]byte(out), &rep); err != nil {
					t.Fatalf("doctor --json: %v\n%s", err, out)
				}
				var found *doctorCheck
				for i := range rep.Checks {
					if rep.Checks[i].Name == "producer_ledger_readable" {
						found = &rep.Checks[i]
					}
				}
				if found == nil || found.OK || !found.Critical {
					t.Fatalf("producer_ledger_readable check = %+v, want failed critical", found)
				}
				var strictOut bytes.Buffer
				if err := Run(testCtx(t), []string{"doctor", "--strict"}, &strictOut, &strictOut, strings.NewReader("")); err == nil {
					t.Fatalf("doctor --strict must fail for corrupt %s ledger", ledger)
				}
			})
		}
	})

	subRun(t, "filesystem_distinct_source_identity", func(t *testing.T) {
		withTempHome(t)
		run(t, "init")
		cfg := mustConfig(t)
		now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

		dirDocs := t.TempDir()
		dirNotes := t.TempDir()

		sDocs := fsSource("docs", dirDocs, "personal")
		sNotes := fsSource("notes", dirNotes, "work")

		if got := instanceKeyForSource(sDocs); got != "filesystem" {
			t.Fatalf("global digest/watermark key = %q, want unchanged filesystem key", got)
		}
		if got := healthInstanceKeyForSource(sDocs); got != "filesystem:docs" {
			t.Fatalf("health-only key = %q, want filesystem:docs", got)
		}

		if err := saveSources(cfg, []Source{sDocs, sNotes}); err != nil {
			t.Fatalf("saveSources: %v", err)
		}

		// Save fresh status for filesystem:docs
		statusDocsPath := syncStatusPathFor(cfg, sDocs)
		if statusDocsPath == "" {
			t.Fatalf("syncStatusPathFor returned empty for sDocs")
		}
		if err := saveSyncStatusFn(statusDocsPath, &memory.SyncStatus{
			Source:        "docs",
			LastAttemptAt: now.Add(-10 * time.Minute).Format(time.RFC3339),
			LastSuccessAt: now.Add(-5 * time.Minute).Format(time.RFC3339),
		}); err != nil {
			t.Fatalf("save docs sync status: %v", err)
		}

		// Save failed status for filesystem:notes
		statusNotesPath := syncStatusPathFor(cfg, sNotes)
		if statusNotesPath == "" {
			t.Fatalf("syncStatusPathFor returned empty for sNotes")
		}
		if err := saveSyncStatusFn(statusNotesPath, &memory.SyncStatus{
			Source:        "notes",
			LastAttemptAt: now.Add(-10 * time.Minute).Format(time.RFC3339),
			LastSuccessAt: now.Add(-time.Hour).Format(time.RFC3339),
			LastError:     "permission denied",
			ErrorCount:    1,
		}); err != nil {
			t.Fatalf("save notes sync status: %v", err)
		}

		sh := sourceHealthAll(cfg, now)
		if len(sh) != 2 {
			t.Fatalf("sourceHealthAll returned %d entries, want 2: %+v", len(sh), sh)
		}
		if sh[0].Key != "filesystem:docs" || sh[1].Key != "filesystem:notes" {
			t.Fatalf("sourceHealthAll keys = [%s, %s], want [filesystem:docs, filesystem:notes]", sh[0].Key, sh[1].Key)
		}

		h := healthOf(cfg, now)
		if h.State != healthUnhealthy {
			t.Fatalf("h.State = %q, want unhealthy due to failed filesystem:notes source", h.State)
		}

		ch := compactHealthFrom(h)
		if ch.PerSource["filesystem:docs"] != healthFresh || ch.PerSource["filesystem:notes"] != healthFailed {
			t.Fatalf("PerSource map = %+v, want filesystem:docs=fresh and filesystem:notes=failed", ch.PerSource)
		}
	})
}
