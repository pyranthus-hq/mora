package mora

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	healthpkg "github.com/pyranthus-hq/mora/internal/health"
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
	producerSourceScheduled = "scheduled"
	producerSourceAdopted   = "adopted"
)

// producerStatus is one producer's evidence row in status.json. SuccessTimes is
// the RAW ring the adoption predicate reads (do not pre-dedupe by day — that would
// destroy the inter-run-gap distribution the median is computed over).
type producerStatus = healthpkg.ProducerStatus

// expectedProducer is one expectation row in expected.json — the durable "we
// expect this to keep running" state that survives a deleted stamp (E2).
type expectedProducer = healthpkg.ExpectedProducer

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
func producerStampErrorPath(cfg Config, name string) string {
	return filepath.Join(filepath.Dir(producerStatusPath(cfg)), name+".stamp_error.json")
}

type producerStampError struct {
	Producer    string `json:"producer"`
	AttemptedAt string `json:"attempted_at"`
	Error       string `json:"error"`
}

// mutateProducersFn is injectable so stamp failures are tested without weakening
// the real ledger's cross-process mutation semantics.
var mutateProducersFn = mutateProducers

func withProducerStamp(cfg Config, name string, now time.Time, nonInteractive bool, fn func() error) error {
	var runErr error
	if fn != nil {
		runErr = fn()
	}
	if name == "" {
		return runErr
	}
	stampErr := mutateProducersFn(cfg, func(m map[string]producerStatus) error {
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
		errRec := producerStampError{Producer: name, AttemptedAt: now.UTC().Format(time.RFC3339), Error: stampErr.Error()}
		if b, marshalErr := json.Marshal(errRec); marshalErr == nil {
			_ = atomicio.Write(producerStampErrorPath(cfg, name), b, 0o600)
		}
		stampFailure := fmt.Errorf("producer stamp for %s failed: %w", name, stampErr)
		if runErr != nil {
			return errors.Join(runErr, stampFailure)
		}
		return stampFailure
	}
	_ = os.Remove(producerStampErrorPath(cfg, name))
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
	stampErr := withProducerStamp(cfg, name, now, nonInteractive, func() error {
		if errp != nil {
			return *errp
		}
		return nil
	})
	if stampErr != nil && errp != nil {
		if *errp == nil {
			*errp = stampErr
		} else if !strings.Contains((*errp).Error(), stampErr.Error()) {
			*errp = errors.Join(*errp, stampErr)
		}
	}
}

func appendSuccessTime(times []string, now time.Time) []string {
	return healthpkg.AppendSuccessTime(times, now)
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
func adoptInterval(raw []string) (int, bool) { return healthpkg.AdoptInterval(raw) }

// producerHealthAll computes the producer arm: one record per EXPECTED producer,
// state derived from the evidence ledger over the injected now. Fail-closed: an
// unreadable ledger reports a typed ledger failure, never a reserved producer
// name and never green.
func producerHealthAll(cfg Config, now time.Time) []producerHealth {
	expected, err := loadExpectedProducers(cfg)
	if err != nil {
		return healthpkg.ProducerLedgerFailure(err)
	}
	status, err := loadProducerStatus(cfg)
	if err != nil {
		return healthpkg.ProducerLedgerFailure(err)
	}
	classified := healthpkg.ClassifyProducers(expected, status, now, producerDefaultInterval)
	for i := range classified {
		stampBytes, readErr := os.ReadFile(producerStampErrorPath(cfg, classified[i].Name))
		if readErr != nil {
			continue
		}
		var stampErr producerStampError
		if json.Unmarshal(stampBytes, &stampErr) != nil || stampErr.AttemptedAt == "" || stampErr.Error == "" {
			continue
		}
		attemptedAt, parseErr := time.Parse(time.RFC3339, stampErr.AttemptedAt)
		lastSuccessAt, successErr := time.Parse(time.RFC3339, classified[i].LastSuccessAt)
		if parseErr != nil || (successErr == nil && attemptedAt.Before(lastSuccessAt)) {
			continue // a later successful stamp supersedes this retained sidecar.
		}
		classified[i].State = prodFailed
		classified[i].LastError = "producer stamp failed: " + stampErr.Error
		classified[i].LastAttemptAt = stampErr.AttemptedAt
	}
	return classified
}

// attemptAfterSuccess reports whether the latest ATTEMPT postdates the latest
// success — i.e. the most recent run failed, so the recorded LastError is live.

// briefArtifactFresh is the consumer-side detector (E3): it needs no registration.
// If >=1 dated brief artifact exists (proving the user uses the surface) and the
// newest is older than 2x the daily brief cadence, the surface is stale even if no
// producer was ever registered. This alone would have caught the SEV with zero
// configuration. Returns (ok, present).
func briefArtifactFresh(cfg Config, now time.Time) (bool, bool) {
	return healthpkg.BriefArtifactFresh(cfg.VaultDir, now)
}

// forgetProducerLedger retires a producer (`mora doctor --forget-producer <name>`):
// it removes both the expectation and the evidence, so an adoption you regret — or a
// job you truly stopped — is not a permanent red banner. Idempotent.
func forgetProducerLedger(cfg Config, name string) error {
	_ = os.Remove(producerStampErrorPath(cfg, name))
	if err := mutateProducers(cfg, func(m map[string]producerStatus) error {
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
