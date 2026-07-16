package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// producer_test.go — Packet E / HEALTH-11: producer liveness. These prove a
// healthy vault and clean index cannot report green while nothing has consumed
// them, that the watchman does not deadlock on its own stamp, and that the
// producer ledger's cross-process lease does not lose updates.

func setProducerClock(t *testing.T, now time.Time) {
	t.Helper()
	orig := producerClock
	producerClock = func() time.Time { return now }
	t.Cleanup(func() { producerClock = orig })
}

func setDoctorClock(t *testing.T, now time.Time) {
	t.Helper()
	orig := doctorClock
	doctorClock = func() time.Time { return now }
	t.Cleanup(func() { doctorClock = orig })
}

func mustSeedExpected(t *testing.T, cfg Config, exps ...expectedProducer) {
	t.Helper()
	m := map[string]expectedProducer{}
	for _, e := range exps {
		m[e.Name] = e
	}
	if err := saveExpectedProducers(cfg, m); err != nil {
		t.Fatalf("saveExpectedProducers: %v", err)
	}
}

func mustSeedStatus(t *testing.T, cfg Config, sts ...producerStatus) {
	t.Helper()
	m := map[string]producerStatus{}
	for _, s := range sts {
		m[s.Name] = s
	}
	if err := saveProducerStatus(cfg, m); err != nil {
		t.Fatalf("saveProducerStatus: %v", err)
	}
}

func mustLoadStatus(t *testing.T, cfg Config) map[string]producerStatus {
	t.Helper()
	m, err := loadProducerStatus(cfg)
	if err != nil {
		t.Fatalf("loadProducerStatus: %v", err)
	}
	return m
}

// TestProducerStampsAtRealChokepoint drives each real producer command and proves
// it stamps its OWN outcome at its own chokepoint. Removing withProducerStamp at
// any one site turns exactly that producer's subtest red — the "individually
// load-bearing" contract (mutation matrix row 22).
func TestProducerStampsAtRealChokepoint(t *testing.T) {
	cases := []struct {
		producer string
		args     []string
	}{
		{"index-hourly", []string{"index", "rebuild", "--force"}},
		{"ingest-hourly", []string{"ingest", "run", "--all"}},
		{"backup-daily", []string{"backup"}},
		{"lint-weekly", []string{"lint"}},
		{"pulse-daily", []string{"pulse", "--advance"}},
		{"git-daily", []string{"sync", "git"}}, // no remote configured: a FAILED run must still stamp an attempt
	}
	for _, tc := range cases {
		t.Run(tc.producer, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			cfg := mustConfig(t)
			now := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
			setProducerClock(t, now)
			setBriefClockForTest(t, now)

			var out bytes.Buffer
			runErr := Run(context.Background(), tc.args, &out, &out, strings.NewReader(""))

			st := mustLoadStatus(t, cfg)
			ps, ok := st[tc.producer]
			if !ok || ps.LastAttemptAt == "" {
				t.Fatalf("%s: no stamp recorded at its chokepoint (run err=%v)\noutput:\n%s\nledger:%+v", tc.producer, runErr, out.String(), st)
			}
			if runErr == nil && ps.LastSuccessAt == "" {
				t.Fatalf("%s: a clean run must leave a SUCCESS stamp, got %+v", tc.producer, ps)
			}
		})
	}
}

func setBriefClockForTest(t *testing.T, now time.Time) {
	t.Helper()
	orig := briefClock
	briefClock = func() time.Time { return now }
	t.Cleanup(func() { briefClock = orig })
}

// TestDeadProducerFailsDoctor: an expected producer whose newest success is older
// than 2x its interval is mechanically unhealthy — doctor's producer_live:* check
// is critical and --strict returns nonzero (mutation matrix row 23).
func TestDeadProducerFailsDoctor(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	mustSeedExpected(t, cfg, expectedProducer{Name: "pulse-daily", IntervalSeconds: 86400, Source: producerSourceAdopted, AdoptedAt: now.Add(-96 * time.Hour).Format(time.RFC3339)})
	old := now.Add(-72 * time.Hour).UTC().Format(time.RFC3339) // 72h > 2x24h
	mustSeedStatus(t, cfg, producerStatus{Name: "pulse-daily", LastSuccessAt: old, LastAttemptAt: old, SuccessTimes: []string{old}})

	setDoctorClock(t, now)

	jsonOut := run(t, "doctor", "--json")
	var rep doctorReport
	if err := json.Unmarshal([]byte(jsonOut), &rep); err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, jsonOut)
	}
	if rep.Healthy {
		t.Fatalf("a producer stale for 72h must make doctor unhealthy:\n%s", jsonOut)
	}
	found := false
	for _, c := range rep.Checks {
		if c.Name == "producer_live:pulse-daily" {
			found = true
			if c.OK || !c.Critical {
				t.Fatalf("producer_live:pulse-daily must be a FAILED CRITICAL check: %+v", c)
			}
		}
	}
	if !found {
		t.Fatalf("doctor --json missing producer_live:pulse-daily:\n%s", jsonOut)
	}

	var strictOut bytes.Buffer
	if err := Run(context.Background(), []string{"doctor", "--strict"}, &strictOut, &strictOut, strings.NewReader("")); err == nil {
		t.Fatalf("doctor --strict must error with a dead producer:\n%s", strictOut.String())
	}
}

// TestInteractiveRunsNeverAdoptAProducer: adoption fires only on NON-interactive
// runs. Three interactive successes across three days must NOT create an
// expectation — else a human debugging `mora index rebuild` would pin a cadence and
// redden the product forever (mutation matrix row 25). The non-interactive twin is
// the positive control.
func TestInteractiveRunsNeverAdoptAProducer(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	days := []time.Time{
		time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 12, 8, 0, 0, 0, time.UTC),
	}

	// Interactive (nonInteractive=false): never adopts.
	for _, d := range days {
		if err := withProducerStamp(cfg, "index-hourly", d, false, nil); err != nil {
			t.Fatalf("withProducerStamp: %v", err)
		}
	}
	exp, err := loadExpectedProducers(cfg)
	if err != nil {
		t.Fatalf("loadExpectedProducers: %v", err)
	}
	if _, adopted := exp["index-hourly"]; adopted {
		t.Fatalf("interactive runs must never adopt a producer; expected.json = %+v", exp)
	}

	// Non-interactive (the scheduled path): the SAME cadence DOES adopt — proving the
	// gate, not a broken predicate, is what suppressed adoption above.
	for _, d := range days {
		if err := withProducerStamp(cfg, "pulse-daily", d, true, nil); err != nil {
			t.Fatalf("withProducerStamp: %v", err)
		}
	}
	exp, _ = loadExpectedProducers(cfg)
	got, adopted := exp["pulse-daily"]
	if !adopted {
		t.Fatalf("three non-interactive daily successes must adopt pulse-daily; expected.json = %+v", exp)
	}
	if got.Source != producerSourceAdopted || got.IntervalSeconds < 3600 || got.IntervalSeconds > 7*24*3600 {
		t.Fatalf("adopted expectation malformed: %+v", got)
	}
}

// TestDeadProducerSurfacesWithin24h is the E5 incident replay: the 7-day dead
// automation. pulse-daily is adopted from three non-interactive daily runs, then
// frozen; at interval*2+eps every alarm surface fires, and deleting the stamp (the
// dead-worktree analogue) reads `never`, not absent.
func TestDeadProducerSurfacesWithin24h(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	day1 := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	days := []time.Time{day1, day1.Add(24 * time.Hour), day1.Add(48 * time.Hour)}

	// Adopt pulse-daily via three non-interactive daily successes (E2), and drop a
	// dated brief artifact each day so the consumer-side detector (E3) has evidence.
	for _, d := range days {
		if err := withProducerStamp(cfg, "pulse-daily", d, true, nil); err != nil {
			t.Fatalf("withProducerStamp: %v", err)
		}
		artifact := filepath.Join(cfg.VaultDir, "briefs", d.UTC().Format("2006-01-02")+"-brief.md")
		if err := os.MkdirAll(filepath.Dir(artifact), 0o700); err != nil {
			t.Fatalf("mkdir briefs: %v", err)
		}
		if err := os.WriteFile(artifact, []byte("# Mora digest\n"), 0o644); err != nil {
			t.Fatalf("write artifact: %v", err)
		}
	}
	exp, _ := loadExpectedProducers(cfg)
	if _, ok := exp["pulse-daily"]; !ok {
		t.Fatalf("setup: pulse-daily should have adopted; expected.json=%+v", exp)
	}

	// A fresh source keeps the source arm green so the PRODUCER is the sole alarm.
	now := days[2].Add(48*time.Hour + time.Hour) // interval*2 + eps past the last success
	enableSources(t, cfg, "gmail")
	seedSyncStatus(t, cfg, "gmail", now)

	// (1) The typed classification: pulse-daily reads stale.
	prod := producerHealthAll(cfg, now)
	if len(prod) != 1 || prod[0].Name != "pulse-daily" || prod[0].State != prodStale {
		t.Fatalf("producerHealthAll = %+v, want one stale pulse-daily", prod)
	}

	setDoctorClock(t, now)
	origGOOS := runtimeGOOS
	runtimeGOOS = func() string { return "darwin" }
	t.Cleanup(func() { runtimeGOOS = origGOOS })
	origRunner := doctorNotifyRunner
	t.Cleanup(func() { doctorNotifyRunner = origRunner })
	var toastArgs []string
	doctorNotifyRunner = func(args ...string) error { toastArgs = append([]string(nil), args...); return nil }

	// (2) doctor --json: producer_live:pulse-daily fails; brief_artifact_fresh fails.
	var rep doctorReport
	if err := json.Unmarshal([]byte(run(t, "doctor", "--json")), &rep); err != nil {
		t.Fatalf("doctor --json: %v", err)
	}
	if rep.Healthy {
		t.Fatalf("dead producer must make doctor unhealthy")
	}
	assertFailingCheck(t, rep, "producer_live:pulse-daily")
	assertFailingCheck(t, rep, "brief_artifact_fresh")

	// (3) doctor --strict errors.
	var strictOut bytes.Buffer
	if err := Run(ctx, []string{"doctor", "--strict"}, &strictOut, &strictOut, strings.NewReader("")); err == nil {
		t.Fatalf("doctor --strict must error:\n%s", strictOut.String())
	}

	// (4) doctor --pulse exits 2 and posts a toast naming the dead producer.
	var pulseOut bytes.Buffer
	pulseErr := Run(ctx, []string{"doctor", "--pulse"}, &pulseOut, &pulseOut, strings.NewReader(""))
	if code, ok := ExitCodeFor(pulseErr); !ok || code != 2 {
		t.Fatalf("doctor --pulse err = %v, want exit 2\n%s", pulseErr, pulseOut.String())
	}
	if len(toastArgs) != 2 || !strings.Contains(toastArgs[1], "pulse-daily") {
		t.Fatalf("doctor --pulse must toast the dead producer, got argv=%#v", toastArgs)
	}

	// (5) The daily brief's first content line is the red banner.
	setBriefClockForTest(t, now)
	briefOut := run(t, "brief")
	briefLines := strings.SplitN(briefOut, "\n", 3)
	if len(briefLines) < 2 || !strings.HasPrefix(briefLines[1], "🔴 MORA HEALTH:") || !strings.Contains(briefLines[1], "pulse-daily") {
		t.Fatalf("daily brief's first content line must be the dead-producer banner, got:\n%s", briefOut)
	}

	// (6) Delete the stamp (the dead-worktree analogue): pulse-daily reads NEVER, not
	// absent — only possible because the expectation lives in a SEPARATE file (E2).
	if err := os.Remove(producerStatusPath(cfg)); err != nil {
		t.Fatalf("remove status: %v", err)
	}
	prod = producerHealthAll(cfg, now)
	if len(prod) != 1 || prod[0].State != prodNever {
		t.Fatalf("after deleting the stamp, pulse-daily must read never, got %+v", prod)
	}
}

func assertFailingCheck(t *testing.T, rep doctorReport, name string) {
	t.Helper()
	for _, c := range rep.Checks {
		if c.Name == name {
			if c.OK {
				t.Fatalf("%s must be a failing check: %+v", name, c)
			}
			return
		}
	}
	t.Fatalf("doctor --json missing check %q", name)
}

// TestPulseSelfRecoversInOneCadence: the watchman must not deadlock on its own
// stamp. A doctor-pulse stamp aged 7 days (connectors fresh) → exit 2 naming the
// missed cadence, AND the stamp advances on the exit-2 path; one cadence later →
// exit 0 (mutation matrix row 37).
func TestPulseSelfRecoversInOneCadence(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	mustSeedExpected(t, cfg, expectedProducer{Name: "doctor-pulse", IntervalSeconds: 86400, Source: producerSourceScheduled})
	stale := now.Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	mustSeedStatus(t, cfg, producerStatus{Name: "doctor-pulse", LastSuccessAt: stale, LastAttemptAt: stale, SuccessTimes: []string{stale}})

	setDoctorClock(t, now)

	// Run N: the stale watchman is reported (exit 2), then stamped on the exit-2 path.
	var out1 bytes.Buffer
	err1 := Run(ctx, []string{"doctor", "--pulse"}, &out1, &out1, strings.NewReader(""))
	if code, ok := ExitCodeFor(err1); !ok || code != 2 {
		t.Fatalf("run N: want exit 2 for the stale watchman, got %v\n%s", err1, out1.String())
	}
	if !strings.Contains(out1.String(), "doctor-pulse") {
		t.Fatalf("run N banner must name the missed watchman cadence:\n%s", out1.String())
	}
	st := mustLoadStatus(t, cfg)
	if got := st["doctor-pulse"].LastSuccessAt; got != now.UTC().Format(time.RFC3339) {
		t.Fatalf("run N must ADVANCE the doctor-pulse stamp on the exit-2 path: LastSuccessAt=%q want %q", got, now.UTC().Format(time.RFC3339))
	}

	// Run N+1 (same clock: the stamp is now fresh): exit 0, the watchman recovered
	// with no human touch, no --force, no reset.
	var out2 bytes.Buffer
	err2 := Run(ctx, []string{"doctor", "--pulse"}, &out2, &out2, strings.NewReader(""))
	if err2 != nil {
		t.Fatalf("run N+1: watchman must self-recover to exit 0, got %v\n%s", err2, out2.String())
	}
}

// TestPulseExecStampNeverMasksASickSource: the execution stamp means "the pulse
// ran", not "everything is healthy". With gmail failing, run N and N+1 both exit 2
// and both name gmail (the stamp did not silence a live failure), and a plain
// `mora doctor` leaves the producer record byte-identical (only --pulse stamps)
// (mutation matrix row 37 twin).
func TestPulseExecStampNeverMasksASickSource(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	now := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	enableSources(t, cfg, "gmail")
	seedSyncStatusFull(t, cfg, "gmail", &memory.SyncStatus{
		Source:        "gmail",
		LastSynced:    now.Add(-72 * time.Hour).UTC().Format(time.RFC3339),
		LastSuccessAt: now.Add(-72 * time.Hour).UTC().Format(time.RFC3339),
		LastAttemptAt: now.Add(-1 * time.Hour).UTC().Format(time.RFC3339),
		LastError:     "database or disk is full (13)",
		ErrorCount:    5,
		ItemCount:     1,
	})
	// A healthy watchman stamp, so doctor-pulse itself is never the alarm.
	fresh := now.UTC().Format(time.RFC3339)
	mustSeedExpected(t, cfg, expectedProducer{Name: "doctor-pulse", IntervalSeconds: 86400, Source: producerSourceScheduled})
	mustSeedStatus(t, cfg, producerStatus{Name: "doctor-pulse", LastSuccessAt: fresh, LastAttemptAt: fresh, SuccessTimes: []string{fresh}})

	setDoctorClock(t, now)

	for i, label := range []string{"run N", "run N+1"} {
		var out bytes.Buffer
		err := Run(ctx, []string{"doctor", "--pulse"}, &out, &out, strings.NewReader(""))
		if code, ok := ExitCodeFor(err); !ok || code != 2 {
			t.Fatalf("%s: want exit 2 (gmail sick), got %v\n%s", label, err, out.String())
		}
		if !strings.Contains(out.String(), "gmail") {
			t.Fatalf("%s (i=%d): the execution stamp must NOT mask the live gmail failure:\n%s", label, i, out.String())
		}
	}

	// A plain `mora doctor` (not --pulse) must not advance the watchman stamp — a dev
	// running doctor once cannot silence it for a cadence.
	before, _ := json.Marshal(mustLoadStatus(t, cfg)["doctor-pulse"])
	_ = run(t, "doctor")
	after, _ := json.Marshal(mustLoadStatus(t, cfg)["doctor-pulse"])
	if !bytes.Equal(before, after) {
		t.Fatalf("plain `mora doctor` must not stamp doctor-pulse: before=%s after=%s", before, after)
	}
}

// TestProducerLedgerNoLostUpdateAcrossProcesses proves the producer-ledger lease
// serializes concurrent PROCESSES (subprocess re-exec, not goroutines — the
// standing #108 lesson). Six processes each stamp a DIFFERENT producer into the one
// shared status.json; without the lease the last rename would drop the others
// (mutation matrix row 38).
func TestProducerLedgerNoLostUpdateAcrossProcesses(t *testing.T) {
	if name := os.Getenv("MORA_PRODUCER_STAMP_CHILD"); name != "" {
		cfg, err := loadConfig()
		if err != nil {
			os.Exit(11)
		}
		now, perr := time.Parse(time.RFC3339, os.Getenv("MORA_PRODUCER_STAMP_NOW"))
		if perr != nil {
			os.Exit(12)
		}
		if serr := withProducerStamp(cfg, name, now, true, nil); serr != nil {
			os.Exit(13)
		}
		os.Exit(0)
	}

	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	const k = 6
	base := time.Date(2026, 7, 10, 8, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	errs := make([]error, k)
	for i := 0; i < k; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestProducerLedgerNoLostUpdateAcrossProcesses$")
			cmd.Env = append(os.Environ(),
				fmt.Sprintf("MORA_PRODUCER_STAMP_CHILD=p%d", i),
				"MORA_PRODUCER_STAMP_NOW="+base.Add(time.Duration(i)*time.Hour).UTC().Format(time.RFC3339))
			if out, err := cmd.CombinedOutput(); err != nil {
				errs[i] = fmt.Errorf("child p%d: %v\n%s", i, err, out)
			}
		}()
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}

	st := mustLoadStatus(t, cfg)
	for i := 0; i < k; i++ {
		name := fmt.Sprintf("p%d", i)
		if _, ok := st[name]; !ok {
			t.Fatalf("lost update: producer %s missing after %d concurrent processes; ledger has %d entries: %v", name, k, len(st), st)
		}
	}
}
