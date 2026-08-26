package mora

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// mora_test.go — Plan 12-05 user-facing surface wiring:
//   - pulse --since-hours (SC#2: explicit ad-hoc window, NEVER advances)
//   - pulse --advance (D-02/SC#4: default-off; the ONLY committing path)
//   - installSchedule pulse-daily (durable wrapper, no RunAtLoad)
//   - sourceFreshness (key off SyncStatus.Source; include never-synced — SC#3 gap)

// briefSnapshotExists reports whether a watermark file was committed for an
// instance — the preview-safety probe (no snapshot ⇒ nothing advanced).
func briefSnapshotExists(cfg Config, key string) bool {
	_, err := os.Stat(briefPath(cfg, key))
	return err == nil
}

// runPulse drives `mora pulse <args...>` against a buffer (non-TTY, byte-clean)
// and returns its stdout.
func runPulse(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	full := append([]string{"pulse"}, args...)
	if err := Run(testCtx(t), full, &out, &out, nil); err != nil {
		t.Fatalf("pulse %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

// TestPulseSinceHoursRendersWindowAndNeverAdvances (SC#2): an explicit
// --since-hours run renders the plain window and writes NO watermark snapshot,
// even though delta mode would. The watermark only ever moves on --advance.
func TestPulseSinceHoursRendersWindowAndNeverAdvances(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Now()
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Quarterly review", 2*time.Hour, now)

	out := runPulse(t, "--digest", "--since-hours", "48")
	if !strings.Contains(out, "Mora digest") {
		t.Fatalf("pulse --digest --since-hours should render the digest header; got:\n%s", out)
	}
	if !strings.Contains(out, "last 48h") {
		t.Fatalf("pulse --since-hours 48 should render the explicit-window header (last 48h); got:\n%s", out)
	}
	if !strings.Contains(out, "Quarterly review") {
		t.Fatalf("pulse --since-hours should include the in-window memory; got:\n%s", out)
	}
	if briefSnapshotExists(cfg, "gmail") {
		t.Fatalf("an explicit --since-hours window must NEVER advance the watermark, but a snapshot was written")
	}
}

// TestPulsePreviewWritesNoSnapshot (D-02/SC#4): the default surface and
// --write are BOTH preview — neither advances the watermark. Only --advance does.
func TestPulsePreviewWritesNoSnapshot(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Now()
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Weekly sync", 1*time.Hour, now)

	// Plain preview.
	runPulse(t, "--digest")
	if briefSnapshotExists(cfg, "gmail") {
		t.Fatalf("pulse --digest (preview) must not advance the watermark")
	}
	// --write is decoupled (log.md only), still preview for the watermark.
	runPulse(t, "--write", "--digest")
	if briefSnapshotExists(cfg, "gmail") {
		t.Fatalf("pulse --write --digest must STILL be preview (write is log-only, decoupled from the watermark)")
	}
	// --write must have appended log.md (decoupled side-effect proves it ran).
	logBody, _ := os.ReadFile(cfg.VaultDir + "/log.md")
	if !strings.Contains(string(logBody), "pulse") {
		t.Fatalf("pulse --write should append a pulse line to log.md; got:\n%s", logBody)
	}
}

// TestPulseAdvanceCommitsWatermark (D-02/SC#4): --advance is the ONLY surface
// that commits the watermark.
func TestPulseAdvanceCommitsWatermark(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Now()
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Board deck", 1*time.Hour, now)

	runPulse(t, "--write", "--digest", "--advance")
	if !briefSnapshotExists(cfg, "gmail") {
		t.Fatalf("pulse --write --digest --advance must commit the watermark (snapshot written)")
	}
}

// TestPulseSinceHoursWithAdvanceStillNeverAdvances (SC#2 invariant): an
// explicit window NEVER advances even if --advance is also passed — the window
// path is ad-hoc and watermark-independent by construction.
func TestPulseSinceHoursWithAdvanceStillNeverAdvances(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enableSources(t, cfg, "gmail")
	now := time.Now()
	seedSyncStatus(t, cfg, "gmail", now.Add(-1*time.Hour))
	digestSeed(t, cfg, "gmail", "Ad hoc", 1*time.Hour, now)

	runPulse(t, "--write", "--digest", "--advance", "--since-hours", "24")
	if briefSnapshotExists(cfg, "gmail") {
		t.Fatalf("an explicit --since-hours window must never advance the watermark, even with --advance")
	}
}

// TestPulseDailyScheduleUsesDurableWrapperNoRunAtLoad: the installed job enters
// through `schedule run pulse-daily`, never invokes --advance directly, and
// still drops RunAtLoad so a login cannot trigger an extra daily attempt.
func TestPulseDailyScheduleUsesDurableWrapperNoRunAtLoad(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	plist := pulseDailyPlist(t, cfg)
	for _, token := range []string{"schedule", "run", "pulse-daily"} {
		if !strings.Contains(plist, "<string>"+token+"</string>") {
			t.Fatalf("pulse-daily must invoke durable wrapper token %q; plist:\n%s", token, plist)
		}
	}
	if strings.Contains(plist, "<string>--advance</string>") {
		t.Fatalf("pulse-daily plist must not bypass loop begin/done with direct --advance; plist:\n%s", plist)
	}
	if strings.Contains(plist, "<key>RunAtLoad</key>") {
		t.Fatalf("pulse-daily must NOT set RunAtLoad (reboot/login would re-consume the morning delta); plist:\n%s", plist)
	}
	// The 08:00 calendar interval is preserved.
	if !strings.Contains(plist, "StartCalendarInterval") {
		t.Fatalf("pulse-daily must keep its 08:00 calendar interval; plist:\n%s", plist)
	}
}

// TestNonPulseScheduleKeepsRunAtLoad: dropping RunAtLoad is scoped to
// pulse-daily (the committing job). A periodic refresh job like index-hourly is
// not a one-shot commit, so it keeps RunAtLoad (re-fire on login is fine there).
func TestNonPulseScheduleKeepsRunAtLoad(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	plist := schedulePlist(t, cfg, "index-hourly")
	if !strings.Contains(plist, "<key>RunAtLoad</key>") {
		t.Fatalf("index-hourly (a periodic refresh, not a one-shot commit) should keep RunAtLoad; plist:\n%s", plist)
	}
}

// TestSourceFreshnessKeysOffSourceAndIncludesNeverSynced (SC#3 gap): the
// freshness map keys off SyncStatus.Source (so imessage is not mis-keyed by a
// google- prefix strip), and a never-synced enabled source is INCLUDED (so it
// can read unavailable downstream) rather than silently dropped.
func TestSourceFreshnessKeysOffSourceAndIncludesNeverSynced(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Now()

	// imessage: a real status file is named imessage-<name>.json on disk, but its
	// Source field is "imessage". The OLD code stripped only a "google-" prefix,
	// leaving "imessage-<name>" — a mis-key. Keying off Source fixes it.
	seedSyncStatus(t, cfg, "imessage", now.Add(-1*time.Hour))
	// gmail: a never-synced source (status file present but LastSynced=="") must
	// be SURFACED, not dropped by the old LastSynced!="" guard.
	seedSyncStatusFull(t, cfg, "gmail", &memory.SyncStatus{Source: "gmail"}) // never synced

	fresh := sourceFreshness(cfg)
	if _, ok := fresh["imessage"]; !ok {
		t.Fatalf("sourceFreshness must key imessage off SyncStatus.Source (not a google- strip); got keys: %v", keysOf(fresh))
	}
	for k := range fresh {
		if strings.HasPrefix(k, "imessage-") {
			t.Fatalf("sourceFreshness mis-keyed imessage as %q (google-prefix strip bug); got keys: %v", k, keysOf(fresh))
		}
	}
	if _, ok := fresh["gmail"]; !ok {
		t.Fatalf("sourceFreshness must INCLUDE a never-synced source (so it can read unavailable), but gmail was dropped; got keys: %v", keysOf(fresh))
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// schedulePlist renders the launchd plist for a job without writing to disk so a
// test can assert its contents (command args, RunAtLoad). It exercises the same
// pure plist builder installSchedule uses.
func schedulePlist(t *testing.T, cfg Config, job string) string {
	t.Helper()
	plist, ok := schedulePlistFor(cfg, job)
	if !ok {
		t.Fatalf("schedulePlistFor(%q) returned !ok", job)
	}
	return plist
}

func pulseDailyPlist(t *testing.T, cfg Config) string {
	t.Helper()
	return schedulePlist(t, cfg, "pulse-daily")
}
