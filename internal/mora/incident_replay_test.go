package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// incident_replay_test.go — Packet C (HEALTH-05): the real six-day silent
// freeze, replayed. The bug that shipped: a source that keeps FAILING every
// hour still shows an OLD-but-present LastSuccessAt, and the pre-Gate-1 digest
// only spelled "stale" in a heading nobody read. This proves the fix surfaces
// the same failure mode within 24h across every alarm surface, and that the
// alarm cannot be silenced by the very failure that causes it.

// TestSixDayFreezeSurfacesWithin24h replays the incident: gmail + imessage are
// healthy at T0, then every hour for 25 hours the sync attempt fails with the
// EXACT error the real incident logged ("database or disk is full (13)") while
// LastSuccessAt stays frozen at T0. At T0+25h — one hour past the tightest
// (24h) freshness threshold — every alarm surface must have caught it: the
// typed health classification, `doctor --json`/`--strict`, `doctor --pulse`
// (exit 2 + a toast), the daily brief, and the meeting brief.
func TestSixDayFreezeSurfacesWithin24h(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	t0 := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	enableSources(t, cfg, "gmail", "imessage")
	seedSyncStatus(t, cfg, "gmail", t0)
	seedSyncStatus(t, cfg, "imessage", t0)

	const incidentError = "database or disk is full (13)"
	gmailPath := syncStatusPathFor(cfg, Source{Name: "gmail", Type: "gmail"})
	imessagePath := syncStatusPathFor(cfg, Source{Name: "imessage", Type: "imessage"})
	// Hourly failed attempts for 25 hours: LastAttemptAt advances every hour,
	// LastError is the real incident's error, LastSuccessAt stays frozen at T0 —
	// exactly the shape that made the six-day freeze invisible pre-Gate-1.
	for h := 1; h <= 25; h++ {
		attemptAt := t0.Add(time.Duration(h) * time.Hour).UTC().Format(time.RFC3339)
		for _, path := range []string{gmailPath, imessagePath} {
			st, err := memory.LoadStatus(path)
			if err != nil {
				t.Fatalf("LoadStatus(%s): %v", path, err)
			}
			st.LastAttemptAt = attemptAt
			st.LastError = incidentError
			st.ErrorCount++
			if err := memory.SaveStatus(path, st); err != nil {
				t.Fatalf("SaveStatus(%s): %v", path, err)
			}
		}
	}

	now := t0.Add(25 * time.Hour)

	// (1) The typed classification: gmail must read failed (an active error, not
	// merely "old").
	health := sourceHealthAll(cfg, now)
	byKey := map[string]sourceHealth{}
	for _, h := range health {
		byKey[h.Key] = h
	}
	if h := byKey["gmail"]; h.State != healthFailed || h.LastError != incidentError {
		t.Fatalf("gmail health = %+v, want state %q with the incident error", h, healthFailed)
	}

	origClock := doctorClock
	doctorClock = func() time.Time { return now }
	t.Cleanup(func() { doctorClock = origClock })
	origGOOS := runtimeGOOS
	runtimeGOOS = func() string { return "darwin" }
	t.Cleanup(func() { runtimeGOOS = origGOOS })
	origRunner := doctorNotifyRunner
	t.Cleanup(func() { doctorNotifyRunner = origRunner })
	var toastArgs []string
	doctorNotifyRunner = func(args ...string) error { toastArgs = append([]string(nil), args...); return nil }

	// (2) `doctor --json` marks the gmail freshness check failed.
	jsonOut := run(t, "doctor", "--json")
	var rep doctorReport
	if err := json.Unmarshal([]byte(jsonOut), &rep); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, jsonOut)
	}
	if rep.Healthy {
		t.Fatalf("doctor --json must report unhealthy 25h into the freeze:\n%s", jsonOut)
	}
	found := false
	for _, c := range rep.Checks {
		if c.Name == "source_fresh:gmail" {
			found = true
			if c.OK {
				t.Fatalf("source_fresh:gmail must be OK=false: %+v", c)
			}
		}
	}
	if !found {
		t.Fatalf("doctor --json missing source_fresh:gmail:\n%s", jsonOut)
	}

	// (3) `doctor --strict` errors.
	var strictOut bytes.Buffer
	if err := Run(ctx, []string{"doctor", "--strict"}, &strictOut, &strictOut, strings.NewReader("")); err == nil {
		t.Fatalf("doctor --strict must error 25h into the freeze; output:\n%s", strictOut.String())
	}

	// (4) `doctor --pulse` exits 2 and the fake notify runner captured a toast.
	var pulseOut bytes.Buffer
	pulseErr := Run(ctx, []string{"doctor", "--pulse"}, &pulseOut, &pulseOut, strings.NewReader(""))
	if pulseErr == nil {
		t.Fatalf("doctor --pulse must error (exit 2) 25h into the freeze; output:\n%s", pulseOut.String())
	}
	if code, ok := ExitCodeFor(pulseErr); !ok || code != 2 {
		t.Fatalf("doctor --pulse error = %v, want a typed exit code 2", pulseErr)
	}
	if len(toastArgs) != 2 || !strings.Contains(toastArgs[1], "gmail") {
		t.Fatalf("doctor --pulse must post a toast naming the failing source, got argv=%#v", toastArgs)
	}

	// (5) The daily brief's FIRST content line — right after the "# Mora
	// digest" header — is the red banner.
	origBriefClock := briefClock
	briefClock = func() time.Time { return now }
	t.Cleanup(func() { briefClock = origBriefClock })
	briefOut := run(t, "brief")
	briefLines := strings.SplitN(briefOut, "\n", 3)
	if len(briefLines) < 2 || !strings.HasPrefix(briefLines[0], "# Mora digest") {
		t.Fatalf("daily brief header missing/misplaced:\n%s", briefOut)
	}
	if !strings.HasPrefix(briefLines[1], "🔴 MORA HEALTH:") {
		t.Fatalf("daily brief's first CONTENT line (after the header) must be the red banner, got %q\nfull brief:\n%s", briefLines[1], briefOut)
	}

	// (6) The meeting brief renders the banner too (there is no upcoming event in
	// this vault — the alarm must still surface, not vanish with an empty brief).
	mb, err := buildNextMeetingBrief(ctx, cfg, now, nil, 0, meetingBriefDefaultPerGuest)
	if err != nil {
		t.Fatalf("buildNextMeetingBrief: %v", err)
	}
	var mbOut bytes.Buffer
	if err := renderMeetingBrief(&mbOut, mb); err != nil {
		t.Fatalf("renderMeetingBrief: %v", err)
	}
	if !strings.HasPrefix(mbOut.String(), "🔴 MORA HEALTH:") {
		t.Fatalf("meeting brief must render the banner even with no upcoming event, got:\n%s", mbOut.String())
	}
}

// TestUnwritableStampStillAlarms (HEALTH-05, the fail-closed property): the
// alarm keys on the AGE of the last successfully recorded stamp, so it cannot
// be silenced by the very failure that is causing it — even when the sync
// process can no longer WRITE a fresh status update at all (a chmod on the
// status file itself does nothing, since SaveStatus writes <path>.tmp + rename,
// which bypasses target-file perms; the parent DIRECTORY has to be unwritable
// to block that write). On POSIX this is exercised with a real chmod; on
// Windows (where chmod semantics don't reliably deny a directory write) the
// injected saveSyncStatusFn seam simulates the same failure deterministically.
func TestUnwritableStampStillAlarms(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	t0 := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", t0)
	path := syncStatusPathFor(cfg, Source{Name: "gmail", Type: "gmail"})
	before, err := memory.LoadStatus(path)
	if err != nil {
		t.Fatalf("LoadStatus: %v", err)
	}

	if runtime.GOOS != "windows" {
		dir := filepath.Dir(path)
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatalf("chmod %s: %v", dir, err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // restore so TempDir cleanup can remove it

		// A real attempted write into the now-unwritable directory must fail —
		// proving the directory chmod, not the file's, is what matters here.
		if err := memory.SaveStatus(path, before); err == nil {
			t.Fatalf("SaveStatus into an unwritable parent directory unexpectedly succeeded")
		}
	} else {
		orig := saveSyncStatusFn
		t.Cleanup(func() { saveSyncStatusFn = orig })
		saveSyncStatusFn = func(string, *memory.SyncStatus) error { return errors.New("simulated write failure") }
	}

	// Simulate an attempt trying (and failing) to stamp a failure — the write
	// failure must be swallowed (best-effort) and must NOT corrupt or resurrect
	// the on-disk stamp.
	stampSyncAttemptFailure(cfg, Source{Name: "gmail", Type: "gmail"}, errors.New("disk full"), t0.Add(10*time.Hour), nil)

	after, err := memory.LoadStatus(path)
	if err != nil {
		t.Fatalf("LoadStatus after failed write attempt: %v", err)
	}
	if after.LastSuccessAt != before.LastSuccessAt || after.LastError != before.LastError {
		t.Fatalf("an unwritable stamp must be left EXACTLY as it was: before=%+v after=%+v", before, after)
	}

	// The alarm still fires purely by reading the frozen (unwritable) stamp's
	// age — 30h later, well past the 24h gmail threshold.
	now := t0.Add(30 * time.Hour)
	got := sourceHealthAll(cfg, now)
	if len(got) != 1 || got[0].State != healthStale {
		t.Fatalf("sourceHealthAll = %+v, want a single stale gmail entry (the unwritable stamp just gets older)", got)
	}
	if healthBannerFromSources(got) == "" {
		t.Fatalf("an unwritable stamp must still produce a banner")
	}
}
