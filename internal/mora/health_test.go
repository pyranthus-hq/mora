package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
	if err := Run(context.Background(), []string{"doctor", "--strict"}, &out2, &out2, strings.NewReader("")); err == nil {
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
		t.Run(c.name, func(t *testing.T) {
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
	ctx := context.Background()
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
	err := Run(context.Background(), []string{"doctor", "--pulse"}, &out, &out, strings.NewReader(""))
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
	err := Run(context.Background(), []string{"doctor", "--pulse"}, &out, &out, strings.NewReader(""))
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
	_ = Run(context.Background(), []string{"doctor", "--pulse", "--json"}, &out, &out, strings.NewReader(""))

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
