package mora

import (
	"bytes"
	"context"
	"encoding/json"
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

// errLoopLockHeld names the loop spine's live-lease sentinel at the CLI layer,
// where the Phase 1 error taxonomy wraps package sentinels in typed codes. It is
// the SAME value the spine returns, not a look-alike, so errors.Is still matches
// through a moraError wrap.
var errLoopLockHeld = looppkg.ErrLockHeld

type exitCodeError struct {
	code int
	msg  string
}

func (e exitCodeError) Error() string { return e.msg }
func (e exitCodeError) ExitCode() int { return e.code }

// ExitCodeFor reports the structured process exit code an error wants main() to
// use. Three carriers, in order: the local skip sentinel, a typed mora error
// (the Phase 1 error-code registry), and the loop spine's own SkipError.
func ExitCodeFor(err error) (int, bool) {
	var e exitCodeError
	if errors.As(err, &e) {
		return e.code, true
	}
	var moraErr moraError
	if errors.As(err, &moraErr) {
		return moraErr.ExitCode(), true
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

// ---------------------------------------------------------------------------
// receipts
// ---------------------------------------------------------------------------

// loopDoneReceipt is the machine form of a closed run. `loop done` printed only
// a human sentence before Plan 01-07, so a machine caller could not read back
// the period or the recorded failure reason of the close it had just made.
type loopDoneReceipt struct {
	LoopID    string `json:"loop_id"`
	RunID     string `json:"run_id"`
	Period    string `json:"period"`
	Status    string `json:"status"`
	LastError string `json:"last_error,omitempty"`
}

// loopListPayload carries the registered loops under a named key. Plan 01-07:
// the bare array moves under `loops` so the document is an object that can
// carry the receipt envelope.
type loopListPayload struct {
	Loops json.RawMessage `json:"loops"`
}

// loopCapture runs one loop-spine command and, under --json only, diverts its
// bare payload document away from stdout so this layer can republish it under
// the envelope. The spine (internal/loop) owns the payload fields; the CLI layer
// owns the contract, which keeps every `mora.loop.*` schema name spelled in
// package mora — where the registry's schema-name sweep looks for it. Human
// output is never intercepted.
func loopCapture(stdout io.Writer, jsonOut bool, fn func(out io.Writer) error) ([]byte, error) {
	if !jsonOut {
		return nil, fn(stdout)
	}
	var buf bytes.Buffer
	err := fn(&buf)
	return buf.Bytes(), err
}

// loopReemit republishes a captured bare payload under the versioned envelope.
// Re-wrapping the spine's own document, rather than rebuilding it here, keeps
// exactly one definition of each loop payload's field set.
func loopReemit(stdout io.Writer, schema string, captured []byte) error {
	trimmed := bytes.TrimSpace(captured)
	if len(trimmed) == 0 {
		return nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return fmt.Errorf("loop %s payload: %w", schema, err)
	}
	return emitReceipt(stdout, schema, 1, payload)
}

func loopBegin(cfg Config, id string, jsonOut bool, now time.Time, stdout io.Writer) error {
	captured, err := loopCapture(stdout, jsonOut, func(out io.Writer) error {
		return looppkg.Begin(cfg, id, jsonOut, now, out)
	})
	code, skipped := looppkg.ExitCodeFor(err)
	if err != nil && !skipped {
		// A real failure publishes no document; the error carries the reason.
		return err
	}
	// The skip document still ships; only then does the exit-10 skip return, so
	// the caller reads why it was gated before it sees the code.
	if emitErr := loopReemit(stdout, "mora.loop.begin", captured); emitErr != nil {
		return emitErr
	}
	if skipped {
		return exitCodeError{code: code} // empty msg => no stderr noise, exit 10
	}
	return nil
}

func loopDone(cfg Config, id, runID string, ok bool, reason string, jsonOut bool, now time.Time, stdout io.Writer) error {
	// Done's own output is a human sentence, not a document, so the receipt is
	// rebuilt from the terminal record it just wrote rather than re-wrapped.
	if _, err := loopCapture(stdout, jsonOut, func(out io.Writer) error {
		return looppkg.Done(cfg, id, runID, ok, reason, now, out)
	}); err != nil {
		return err
	}
	if !jsonOut {
		return nil
	}
	completed, found := loadRunRecord(cfg, id)
	if !found {
		return fmt.Errorf("loop %q closed but its terminal record is unreadable", id)
	}
	return emitReceipt(stdout, "mora.loop.done", 1, loopDoneReceipt{
		LoopID: id, RunID: completed.RunID, Period: completed.Period,
		Status: string(completed.Status), LastError: completed.LastError,
	})
}

func heartbeatLoopRun(cfg Config, id, runID string, now time.Time) error {
	return looppkg.HeartbeatRun(cfg, id, runID, now)
}

func loopHeartbeat(cfg Config, id, runID string, jsonOut bool, now time.Time, stdout io.Writer) error {
	captured, err := loopCapture(stdout, jsonOut, func(out io.Writer) error {
		return looppkg.HeartbeatCommand(cfg, id, runID, jsonOut, now, out)
	})
	if err != nil {
		return err
	}
	return loopReemit(stdout, "mora.loop.heartbeat", captured)
}

func withLoopRunEffectAt(cfg Config, id, runID string, effectNow time.Time, fn func() error) error {
	return looppkg.WithRunEffectAt(cfg, id, runID, effectNow, loopClock, fn)
}
func startLoopHeartbeat(cfg Config, id, runID string) func() {
	return looppkg.StartHeartbeat(cfg, id, runID, loopClock)
}

func loopStatus(cfg Config, id string, jsonOut bool, now time.Time, stdout io.Writer) error {
	captured, err := loopCapture(stdout, jsonOut, func(out io.Writer) error {
		return looppkg.Status(cfg, id, jsonOut, now, out)
	})
	if err != nil {
		return err
	}
	return loopReemit(stdout, "mora.loop.status", captured)
}

func loopRegister(cfg Config, id, cadence string, command []string, scheduleJob string, jsonOut bool, now time.Time, stdout io.Writer) error {
	reg, err := looppkg.Register(cfg, id, cadence, command, scheduleJob, now)
	if err != nil {
		return err
	}
	if jsonOut {
		// The registration as constructed IS the receipt: a caller that just
		// registered reads back the cadence and command it now owns.
		return emitReceipt(stdout, "mora.loop.register", 1, reg)
	}
	okf(stdout, "registered loop %q (cadence %s)", id, cadence)
	return nil
}

func loopList(cfg Config, jsonOut bool, now time.Time, stdout io.Writer) error {
	captured, err := loopCapture(stdout, jsonOut, func(out io.Writer) error {
		return looppkg.List(cfg, jsonOut, now, out)
	})
	if err != nil || !jsonOut {
		return err
	}
	// Plan 01-07: the bare array moves under `loops` so the payload is an object
	// that can carry the envelope.
	loops := bytes.TrimSpace(captured)
	if len(loops) == 0 {
		loops = []byte("[]")
	}
	return emitReceipt(stdout, "mora.loop.list", 1, loopListPayload{Loops: loops})
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

// loopPositionalID pulls the loop id out of the first positional argument. A
// dash-led argument is refused rather than accepted as an id: `mora loop begin
// --json` used to start a real run for a loop literally named "--json", so a
// machine caller asking for JSON silently mutated loop state.
func loopPositionalID(rest []string) (id string, flagArgs []string, ok bool) {
	if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
		return "", nil, false
	}
	return rest[0], rest[1:], true
}

// cmdLoop dispatches `mora loop begin|heartbeat|done|status|register|list`. The id is the
// first positional arg (so the documented `loop begin <id> --json` form works
// regardless of flag ordering); list takes no id. Mirrors cmdSchedule/cmdPulse
// (flag.ContinueOnError + io.Discard). now is the real wall clock (the only
// place a fresh time.Now() is taken — every helper receives it injected).
func cmdLoop(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	_ = ctx
	_ = stderr
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
		id, flagArgs, ok := loopPositionalID(rest)
		if !ok {
			return errors.New("usage: mora loop begin <id> [--json]")
		}
		fs := newFS("begin")
		jsonOut := fs.Bool("json", false, "json output")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		return loopBegin(cfg, id, *jsonOut, now, stdout)

	case "heartbeat":
		id, flagArgs, ok := loopPositionalID(rest)
		if !ok {
			return errors.New("usage: mora loop heartbeat <id> --run <run_id> [--json]")
		}
		fs := newFS("heartbeat")
		runID := fs.String("run", "", "the active run id from `loop begin`")
		jsonOut := fs.Bool("json", false, "json output")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		return loopHeartbeat(cfg, id, *runID, *jsonOut, now, stdout)

	case "done":
		id, flagArgs, ok := loopPositionalID(rest)
		if !ok {
			return errors.New(`usage: mora loop done <id> (--ok | --fail "reason")`)
		}
		fs := newFS("done")
		jsonOut := fs.Bool("json", false, "json output")
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
		return loopDone(cfg, id, *runID, *okFlag, *failReason, *jsonOut, now, stdout)

	case "status":
		id, flagArgs, ok := loopPositionalID(rest)
		if !ok {
			return errors.New("usage: mora loop status <id> [--json]")
		}
		fs := newFS("status")
		jsonOut := fs.Bool("json", false, "json output")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		return loopStatus(cfg, id, *jsonOut, now, stdout)

	case "register":
		id, flagArgs, ok := loopPositionalID(rest)
		if !ok {
			return errors.New("usage: mora loop register <id> [--json] [--cadence daily] [--command \"...\"] [--schedule-job <job>]")
		}
		fs := newFS("register")
		cadence := fs.String("cadence", loopDefaultCadence, "daily|hourly|weekly")
		command := fs.String("command", "", "argv the trigger runs (whitespace-separated)")
		scheduleJob := fs.String("schedule-job", "", "backing launchd job (e.g. pulse-daily)")
		jsonOut := fs.Bool("json", false, "json output")
		if err := fs.Parse(flagArgs); err != nil {
			return err
		}
		return loopRegister(cfg, id, *cadence, strings.Fields(*command), *scheduleJob, *jsonOut, now, stdout)

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
