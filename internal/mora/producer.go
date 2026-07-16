package mora

import (
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
)

// Producer liveness (Packet E / HEALTH-11): the arm that convicts a healthy vault
// and a clean index of serving nothing. Its two files are DISJOINT by design (E2):
//   - status.json   — EVIDENCE: what actually ran, stamped at each producer's OWN
//     chokepoint (E1), so launchd, cron, an external orchestrator, and a human all
//     record identically.
//   - expected.json — EXPECTATION: what SHOULD run. E5's replay deletes the stamp
//     to model a dead worktree; an expectation inferred from the stamp would be
//     erased by the very event it must detect, so the alarm would delete itself.
//
// A never-expected producer is simply absent from Health.Producers (a user who
// scheduled nothing is never nagged). A producer whose brief ran daily for a month
// and then stopped IS surfaced — that is the recorded 7-day dead-automation SEV.

const (
	// producerSuccessRing bounds the raw success-time sample list kept per producer.
	// The adoption predicate needs the raw (un-deduped) samples to derive the median
	// inter-run gap, so the ring is truncated (never day-deduped) inside the lease.
	producerSuccessRing = 10
	// producerStaleMultiplier: a producer is stale once its newest success is older
	// than N x its interval. 2x matches E3's artifact rule and E5's replay (stale at
	// interval x2 + eps) and gives a scheduled job one full missed cycle of grace
	// before it reddens, so it never flaps at the cadence boundary.
	producerStaleMultiplier = 2

	producerAdoptMinSuccesses    = 3
	producerAdoptMinGap          = time.Hour
	producerAdoptMinDistinctDays = 3
	producerIntervalFloor        = time.Hour
	producerIntervalCeil         = 7 * 24 * time.Hour

	producerSourceScheduled = "scheduled"
	producerSourceAdopted   = "adopted"
)

// producerStatus is one producer's evidence row in status.json. SuccessTimes is
// the RAW ring the adoption predicate reads (do not pre-dedupe by day — that would
// destroy the inter-run-gap distribution the median is computed over).
type producerStatus struct {
	Name            string   `json:"name"`
	LastAttemptAt   string   `json:"last_attempt_at,omitempty"`
	LastSuccessAt   string   `json:"last_success_at,omitempty"`
	LastError       string   `json:"last_error,omitempty"`
	SuccessTimes    []string `json:"success_times"`
	IntervalSeconds int      `json:"interval_seconds"`
	Source          string   `json:"source"`
}

// expectedProducer is one expectation row in expected.json — the durable "we
// expect this to keep running" state that survives a deleted stamp (E2).
type expectedProducer struct {
	Name            string `json:"name"`
	IntervalSeconds int    `json:"interval_seconds"`
	Source          string `json:"source"` // declared | scheduled | adopted
	AdoptedAt       string `json:"adopted_at,omitempty"`
}

// jobDefaultIntervalSeconds is the scheduled cadence of each known job (from
// launchdSchedule / windowsScheduleCadenceArgs), used as the expectation interval
// for a declared/scheduled producer. Adoption instead uses the OBSERVED median
// inter-run gap, which is why adopting does not need this table.
var jobDefaultIntervalSeconds = map[string]int{
	"pulse-daily":   86400,
	"index-hourly":  3600,
	"ingest-hourly": 3600,
	"backup-daily":  86400,
	"git-daily":     86400,
	"lint-weekly":   604800,
	"doctor-pulse":  86400,
}

func producerDefaultInterval(name string) int {
	if s, ok := jobDefaultIntervalSeconds[name]; ok {
		return s
	}
	return 86400
}

// writerIsTTY reports whether w is an interactive terminal — the signal that a run
// is a human at a prompt, not launchd/cron/an orchestrator. Mirrors bannerColor's
// probe (banner.go:73) without the color env gating.
func writerIsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isatty.IsTerminal(f.Fd())
}

// producerArgHasFlag / producerArgFlagValue parse a bare `mora <cmd>` argv for the
// producer plumbing flags (they are consulted BEFORE the command's flag.FlagSet so
// a legacy pre-flag plist is handled identically to a new --producer install).
func producerArgHasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == "--"+name || a == "-"+name || strings.HasPrefix(a, "--"+name+"=") {
			return true
		}
	}
	return false
}

func producerArgFlagValue(args []string, name string) string {
	for i, a := range args {
		if strings.HasPrefix(a, "--"+name+"=") {
			return strings.TrimPrefix(a, "--"+name+"=")
		}
		if a == "--"+name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// producerNameFor resolves the producer identity for a run: the explicit
// --producer=<job> token (new installs; scheduleCommands appends it) overrides the
// argv-derived default (a legacy pre-flag plist keeps firing its old flag-less
// argv, so the derived default is what keeps the alarm alive for it — E2b).
func producerNameFor(derived string, args []string) string {
	if v := producerArgFlagValue(args, "producer"); v != "" {
		return v
	}
	return derived
}

// producerNonInteractive reports whether this run counts toward ADOPTION cadence:
// non-interactive (stdout is not a TTY) OR an explicit --scheduled / --producer
// token. An interactive terminal run never adopts, so a human running `mora index
// rebuild` three times while debugging cannot pin a ~2-minute cadence and redden
// the product forever (E2).
func producerNonInteractive(w io.Writer, args []string) bool {
	if producerArgHasFlag(args, "scheduled") || producerArgFlagValue(args, "producer") != "" {
		return true
	}
	return !writerIsTTY(w)
}

// withProducerStamp wraps a producer command's OWN success path (E1) — never the
// scheduler, so every runner stamps identically. fn runs OUTSIDE the ledger lease
// (it may be a multi-minute rebuild); only the tiny stamp RMW takes the
// cross-process lease. `now` is injected (determinism rule — this file never calls
// time.Now()). A stamp-write failure is best-effort: it warns and never turns fn's
// own success into a failure (mirrors stampSyncAttemptFailure, health.go:274).
//
// nonInteractive gates ADOPTION only: the evidence is recorded for every run, but a
// producer is added to expected.json (what turns a later silence into a red banner)
// only from non-interactive runs.
func withProducerStamp(cfg Config, name string, now time.Time, nonInteractive bool, fn func() error) error {
	var runErr error
	if fn != nil {
		runErr = fn()
	}
	if name == "" {
		return runErr
	}
	stampErr := mutateProducers(cfg, now, func(m map[string]producerStatus) error {
		ps := m[name]
		ps.Name = name
		ps.LastAttemptAt = now.UTC().Format(time.RFC3339)
		if runErr != nil {
			ps.LastError = runErr.Error()
		} else {
			ps.LastError = ""
			ps.LastSuccessAt = now.UTC().Format(time.RFC3339)
			ps.SuccessTimes = appendSuccessTime(ps.SuccessTimes, now)
		}
		m[name] = ps
		// Adoption is evaluated INSIDE the lease so the expectation write is
		// serialized with the evidence it is derived from. Best-effort (never aborts
		// the evidence save).
		if runErr == nil && nonInteractive {
			maybeAdoptProducer(cfg, name, ps, now)
		}
		return nil
	})
	if stampErr != nil {
		warnf(os.Stderr, "producer stamp for %s failed (health may lag): %v", name, stampErr)
	}
	return runErr
}

// stampChokepoint is the deferred wrapper each producer command registers at its
// OWN success path (E1). `derived` is the argv-derived job identity (which keeps a
// legacy pre-flag plist's alarm alive — E2b); a --producer=<job> token overrides
// it. It is deferred with the command's named return error so it records success or
// failure exactly as the command finished. Removing this call at any one producer
// site turns exactly that producer's TestProducerStampsAtRealChokepoint red.
func stampChokepoint(cfg Config, stdout io.Writer, args []string, derived string, now time.Time, errp *error) {
	name := producerNameFor(derived, args)
	nonInteractive := producerNonInteractive(stdout, args)
	_ = withProducerStamp(cfg, name, now, nonInteractive, func() error {
		if errp != nil {
			return *errp
		}
		return nil
	})
}

func appendSuccessTime(times []string, now time.Time) []string {
	times = append(times, now.UTC().Format(time.RFC3339))
	if len(times) > producerSuccessRing {
		times = times[len(times)-producerSuccessRing:]
	}
	return times
}

// maybeAdoptProducer records an adopted expectation when the raw success history
// qualifies (E2). It never OVERRIDES a declared/scheduled expectation. Best-effort.
func maybeAdoptProducer(cfg Config, name string, ps producerStatus, now time.Time) {
	interval, ok := adoptInterval(ps.SuccessTimes)
	if !ok {
		return
	}
	exp, err := loadExpectedProducers(cfg)
	if err != nil {
		return
	}
	if cur, exists := exp[name]; exists && cur.Source != producerSourceAdopted {
		return // an explicit declared/scheduled expectation already governs
	}
	adoptedAt := now.UTC().Format(time.RFC3339)
	if cur, exists := exp[name]; exists && cur.AdoptedAt != "" {
		adoptedAt = cur.AdoptedAt
	}
	exp[name] = expectedProducer{
		Name:            name,
		IntervalSeconds: interval,
		Source:          producerSourceAdopted,
		AdoptedAt:       adoptedAt,
	}
	if err := saveExpectedProducers(cfg, exp); err != nil {
		warnf(os.Stderr, "producer adoption for %s failed: %v", name, err)
	}
}

// adoptInterval derives an adopted cadence from raw success timestamps (E2): only
// when there are >=3 successes whose consecutive gaps are each >=1h and which span
// >=3 distinct UTC days. The interval is the MEDIAN inter-run gap, clamped to
// [1h,7d]. A sub-1h gap anywhere means an interactive burst, not a cadence.
func adoptInterval(rawTimes []string) (int, bool) {
	ts := make([]time.Time, 0, len(rawTimes))
	for _, s := range rawTimes {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			ts = append(ts, t.UTC())
		}
	}
	if len(ts) < producerAdoptMinSuccesses {
		return 0, false
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i].Before(ts[j]) })
	days := map[string]struct{}{}
	gaps := make([]time.Duration, 0, len(ts)-1)
	for i, t := range ts {
		days[t.Format("2006-01-02")] = struct{}{}
		if i > 0 {
			g := t.Sub(ts[i-1])
			if g < producerAdoptMinGap {
				return 0, false
			}
			gaps = append(gaps, g)
		}
	}
	if len(days) < producerAdoptMinDistinctDays {
		return 0, false
	}
	med := medianDuration(gaps)
	if med < producerIntervalFloor {
		med = producerIntervalFloor
	}
	if med > producerIntervalCeil {
		med = producerIntervalCeil
	}
	return int(med.Seconds()), true
}

func medianDuration(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// producerHealthAll computes the producer arm: one record per EXPECTED producer,
// state derived from the evidence ledger over the injected now. Fail-closed: an
// unreadable ledger reports a synthetic failed producer, never green.
func producerHealthAll(cfg Config, now time.Time) []producerHealth {
	expected, err := loadExpectedProducers(cfg)
	if err != nil {
		return []producerHealth{{Name: "producers", State: prodFailed, LastError: err.Error()}}
	}
	status, serr := loadProducerStatus(cfg)
	if serr != nil {
		return []producerHealth{{Name: "producers", State: prodFailed, LastError: serr.Error()}}
	}
	names := make([]string, 0, len(expected))
	for n := range expected {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]producerHealth, 0, len(names))
	for _, name := range names {
		exp := expected[name]
		interval := exp.IntervalSeconds
		if interval <= 0 {
			interval = producerDefaultInterval(name)
		}
		ph := producerHealth{Name: name, IntervalSeconds: interval, Source: exp.Source}
		st, ok := status[name]
		ph.LastSuccessAt = st.LastSuccessAt
		ph.LastAttemptAt = st.LastAttemptAt
		ph.LastError = st.LastError
		last, lerr := time.Parse(time.RFC3339, st.LastSuccessAt)
		if !ok || st.LastSuccessAt == "" || lerr != nil {
			ph.State = prodNever
			out = append(out, ph)
			continue
		}
		age := now.UTC().Sub(last.UTC())
		if age < 0 {
			age = 0
		}
		ph.AgeHours = int(age.Hours())
		switch {
		case st.LastError != "" && attemptAfterSuccess(st):
			ph.State = prodFailed
		case age >= time.Duration(interval)*time.Second*producerStaleMultiplier:
			ph.State = prodStale
		default:
			ph.State = prodFresh
		}
		out = append(out, ph)
	}
	return out
}

// attemptAfterSuccess reports whether the latest ATTEMPT postdates the latest
// success — i.e. the most recent run failed, so the recorded LastError is live.
func attemptAfterSuccess(st producerStatus) bool {
	a, aerr := time.Parse(time.RFC3339, st.LastAttemptAt)
	s, serr := time.Parse(time.RFC3339, st.LastSuccessAt)
	if aerr != nil {
		return st.LastError != ""
	}
	if serr != nil {
		return true
	}
	return a.After(s)
}

// briefArtifactFresh is the consumer-side detector (E3): it needs no registration.
// If >=1 dated brief artifact exists (proving the user uses the surface) and the
// newest is older than 2x the daily brief cadence, the surface is stale even if no
// producer was ever registered. This alone would have caught the SEV with zero
// configuration. Returns (ok, present).
func briefArtifactFresh(cfg Config, now time.Time) (ok bool, present bool) {
	matches, _ := filepath.Glob(filepath.Join(cfg.VaultDir, "briefs", "*-brief.md"))
	newest := time.Time{}
	for _, m := range matches {
		datePart := strings.TrimSuffix(filepath.Base(m), "-brief.md")
		d, derr := time.Parse("2006-01-02", datePart)
		if derr != nil {
			continue
		}
		present = true
		if d.After(newest) {
			newest = d
		}
	}
	if !present {
		return true, false
	}
	stale := now.UTC().Sub(newest.UTC()) >= producerStaleMultiplier*24*time.Hour
	return !stale, true
}

// forgetProducerLedger retires a producer (`mora doctor --forget-producer <name>`):
// it removes both the expectation and the evidence, so an adoption you regret — or a
// job you truly stopped — is not a permanent red banner. Idempotent.
func forgetProducerLedger(cfg Config, name string, now time.Time) error {
	if err := mutateProducers(cfg, now, func(m map[string]producerStatus) error {
		delete(m, name)
		return nil
	}); err != nil {
		return err
	}
	exp, err := loadExpectedProducers(cfg)
	if err != nil {
		return err
	}
	delete(exp, name)
	return saveExpectedProducers(cfg, exp)
}
