package mora

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/leasefile"
	looppkg "github.com/pyranthus-hq/mora/internal/loop"
	"io"
	"os"
	"strings"
	"time"
)

const (
	loopSkipExitCode          = looppkg.SkipExitCode
	loopDefaultCadence        = looppkg.DefaultCadence
	loopRunSchemaVersion      = looppkg.RunSchemaVersion
	loopRegistrySchemaVersion = looppkg.RegistrySchemaVersion
	loopLockTTL               = looppkg.LockTTL
	loopRunRunning            = looppkg.RunRunning
	loopRunSucceeded          = looppkg.RunSucceeded
	loopRunFailed             = looppkg.RunFailed
)

var loopClock = time.Now

type exitCodeError struct {
	code int
	msg  string
}

func (e exitCodeError) Error() string { return e.msg }
func (e exitCodeError) ExitCode() int { return e.code }
func ExitCodeFor(err error) (int, bool) {
	var e exitCodeError
	if errors.As(err, &e) {
		return e.code, true
	}
	return looppkg.ExitCodeFor(err)
}

type loopRunRecord = looppkg.RunRecord
type loopHealth = looppkg.Health
type loopLockBody = leasefile.Body

func newRunID(now time.Time) string               { return looppkg.NewRunID(now) }
func loopLatestPath(cfg Config, id string) string { return looppkg.LatestPath(cfg, id) }
func loopLockPath(cfg Config, id string) string   { return looppkg.LockPath(cfg, id) }

func periodFor(cadence string, now time.Time) string { return looppkg.PeriodFor(cadence, now) }
func saveRunRecord(cfg Config, rec loopRunRecord, now time.Time) error {
	return looppkg.SaveRecord(cfg, rec, now)
}
func loopJournalPath(cfg Config, id string) string { return looppkg.JournalPath(cfg, id) }
func loadRunRecord(cfg Config, id string) (loopRunRecord, bool) {
	return looppkg.LoadRunRecord(cfg, id)
}

func loopBegin(cfg Config, id string, jsonOut bool, now time.Time, stdout io.Writer) error {
	err := looppkg.Begin(cfg, id, jsonOut, now, stdout)
	if code, ok := looppkg.ExitCodeFor(err); ok {
		return exitCodeError{code: code}
	}
	return err
}
func loopDone(cfg Config, id, runID string, ok bool, reason string, now time.Time, stdout io.Writer) error {
	return looppkg.Done(cfg, id, runID, ok, reason, now, stdout)
}
func heartbeatLoopRun(cfg Config, id, runID string, now time.Time) error {
	return looppkg.HeartbeatRun(cfg, id, runID, now)
}
func loopHeartbeat(cfg Config, id, runID string, jsonOut bool, now time.Time, stdout io.Writer) error {
	return looppkg.HeartbeatCommand(cfg, id, runID, jsonOut, now, stdout)
}

func withLoopRunEffectAt(cfg Config, id, runID string, effectNow time.Time, fn func() error) error {
	return looppkg.WithRunEffectAt(cfg, id, runID, effectNow, loopClock, fn)
}
func startLoopHeartbeat(cfg Config, id, runID string) func() {
	return looppkg.StartHeartbeat(cfg, id, runID, loopClock)
}
func loopStatus(cfg Config, id string, jsonOut bool, now time.Time, stdout io.Writer) error {
	return looppkg.Status(cfg, id, jsonOut, now, stdout)
}
func loopRegister(cfg Config, id, cadence string, command []string, scheduleJob string, now time.Time, stdout io.Writer) error {
	if err := looppkg.Register(cfg, id, cadence, command, scheduleJob, now); err != nil {
		return err
	}
	okf(stdout, "registered loop %q (cadence %s)", id, cadence)
	return nil
}
func loopList(cfg Config, jsonOut bool, now time.Time, stdout io.Writer) error {
	return looppkg.List(cfg, jsonOut, now, stdout)
}

const leaseRemovalTimeout = leasefile.RemovalTimeout

var (
	leaseRemoveFn           = os.Remove
	leaseRemovalRetryableFn = atomicio.SharingViolationRetryable
)

func leaseRemovalOptions() leasefile.RemovalOptions {
	return leasefile.RemovalOptions{Remove: leaseRemoveFn, Retryable: leaseRemovalRetryableFn, Backoff: sourcesAcquireBackoff, Now: time.Now, Sleep: time.Sleep, Timeout: leaseRemovalTimeout}
}
func publishLockFile(path string, body []byte) (bool, error) { return leasefile.Publish(path, body) }
func loopLockReleaser(path string, observed []byte) func() {
	return leasefile.Releaser(path, observed, leaseRemovalOptions())
}
func removeLeaseFileGuarded(path string) error { return leasefile.Remove(path, leaseRemovalOptions()) }
func reapStaleLockTTL(path string, now time.Time, ttl time.Duration) (bool, error) {
	return leasefile.Reap(path, now, ttl, leaseRemovalOptions())
}
func releaseLockFileFor(path, owner string) { leasefile.Release(path, owner, leaseRemovalOptions()) }
func heartbeatLockFileFor(path, owner string, now time.Time) bool {
	return leasefile.Heartbeat(path, owner, now)
}
func cmdLoop(ctx context.Context, args []string, stdout io.Writer) error {
	_ = ctx
	if len(args) == 0 {
		return errors.New("usage: mora loop begin|heartbeat|done|status|register|list <id> [flags]")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	now := loopClock()
	sub, rest := args[0], args[1:]

	newFS := func(name string) *flag.FlagSet {
		fs := flag.NewFlagSet("loop "+name, flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		return fs
	}

	switch sub {
	case "begin":
		if len(rest) == 0 {
			return errors.New("usage: mora loop begin <id> [--json]")
		}
		id, flagArgs := rest[0], rest[1:]
		fs := newFS("begin")
		jsonOut := fs.Bool("json", false, "json output")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		return loopBegin(cfg, id, *jsonOut, now, stdout)

	case "heartbeat":
		if len(rest) == 0 {
			return errors.New("usage: mora loop heartbeat <id> --run <run_id> [--json]")
		}
		id, flagArgs := rest[0], rest[1:]
		fs := newFS("heartbeat")
		runID := fs.String("run", "", "the active run id from `loop begin`")
		jsonOut := fs.Bool("json", false, "json output")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		return loopHeartbeat(cfg, id, *runID, *jsonOut, now, stdout)

	case "done":
		if len(rest) == 0 {
			return errors.New(`usage: mora loop done <id> (--ok | --fail "reason")`)
		}
		id, flagArgs := rest[0], rest[1:]
		fs := newFS("done")
		okFlag := fs.Bool("ok", false, "mark the run succeeded")
		failReason := fs.String("fail", "", "mark the run failed with a short reason")
		runID := fs.String("run", "", "the run id from `loop begin` (guards against closing a superseded run)")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		if !*okFlag && *failReason == "" {
			return errors.New(`mora loop done: pass --ok or --fail "reason"`)
		}
		if *okFlag && *failReason != "" {
			return errors.New("mora loop done: pass --ok OR --fail, not both")
		}
		return loopDone(cfg, id, *runID, *okFlag, *failReason, now, stdout)

	case "status":
		if len(rest) == 0 {
			return errors.New("usage: mora loop status <id> [--json]")
		}
		id, flagArgs := rest[0], rest[1:]
		fs := newFS("status")
		jsonOut := fs.Bool("json", false, "json output")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		return loopStatus(cfg, id, *jsonOut, now, stdout)

	case "register":
		if len(rest) == 0 {
			return errors.New("usage: mora loop register <id> [--cadence daily] [--command \"...\"] [--schedule-job <job>]")
		}
		id, flagArgs := rest[0], rest[1:]
		fs := newFS("register")
		cadence := fs.String("cadence", loopDefaultCadence, "daily|hourly|weekly")
		command := fs.String("command", "", "argv the trigger runs (whitespace-separated)")
		scheduleJob := fs.String("schedule-job", "", "backing launchd job (e.g. pulse-daily)")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		return loopRegister(cfg, id, *cadence, strings.Fields(*command), *scheduleJob, now, stdout)

	case "list":
		fs := newFS("list")
		jsonOut := fs.Bool("json", false, "json output")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		return loopList(cfg, *jsonOut, now, stdout)

	default:
		return fmt.Errorf("unknown loop subcommand %q (want begin|heartbeat|done|status|register|list)", sub)
	}
}
