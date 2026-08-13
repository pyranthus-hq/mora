package mora

import (
	"context"
	"errors"
	"fmt"
	schedulepkg "github.com/pyranthus-hq/mora/internal/schedule"
	"io"
	"os"
)

var scheduleExecutable = os.Executable

func cmdSchedule(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mora schedule install|list|uninstall|run")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		return listSchedules(stdout, cfg)
	case "install":
		if len(args) != 2 {
			return errors.New("usage: mora schedule install <pulse-daily|doctor-pulse|index-hourly|backup-daily|lint-weekly|ingest-hourly|git-daily|update-daily>")
		}
		return installSchedule(stdout, cfg, args[1])
	case "uninstall":
		if len(args) != 2 {
			return errors.New("usage: mora schedule uninstall <pulse-daily|doctor-pulse|index-hourly|backup-daily|lint-weekly|ingest-hourly|git-daily|update-daily>")
		}
		return uninstallSchedule(stdout, cfg, args[1])
	case "run":
		if len(args) != 2 || args[1] != "pulse-daily" {
			return errors.New("usage: mora schedule run pulse-daily")
		}
		return runScheduledPulseDaily(ctx, cfg, stdout)
	default:
		return errors.New("usage: mora schedule install|list|uninstall|run")
	}
}

// runScheduledPulseDaily is the durable scheduled boundary for the only
// non-idempotent job. The scheduler no longer invokes pulse --advance directly:
// it opens the same daily-brief gate as the interactive skill, passes that run's
// identity into cmdPulse's heartbeat/commit fence, and records exactly one
// terminal transition before returning. Exit 10 is an idempotent scheduler
// success (today already completed), not a failed OS job.
func runScheduledPulseDaily(ctx context.Context, cfg Config, stdout io.Writer) error {
	const loopID = "daily-brief"
	now := loopClock()
	if err := loopBegin(cfg, loopID, false, now, stdout); err != nil {
		if code, ok := ExitCodeFor(err); ok && code == loopSkipExitCode {
			return nil
		}
		return fmt.Errorf("scheduled pulse gate: %w", err)
	}
	rec, found := loadRunRecord(cfg, loopID)
	if !found || rec.Status != loopRunRunning || rec.RunID == "" {
		return errors.New("scheduled pulse gate opened without a current running record")
	}

	pulseArgs := []string{
		"--write", "--digest", "--advance", "--sync", "--brief-file", "--notify",
		"--loop", loopID, "--loop-run", rec.RunID,
	}
	if err := cmdPulse(ctx, pulseArgs, stdout); err != nil {
		closeErr := loopDone(cfg, loopID, rec.RunID, false, err.Error(), loopClock(), stdout)
		return errors.Join(fmt.Errorf("scheduled pulse: %w", err), closeErr)
	}
	if err := loopDone(cfg, loopID, rec.RunID, true, "", loopClock(), stdout); err != nil {
		return fmt.Errorf("scheduled pulse completion: %w", err)
	}
	return nil
}

// scheduleRunAtLoad reports whether a job's plist should set RunAtLoad. It is
// FALSE for pulse-daily: that job is a one-shot daily COMMIT (it advances the
// watermark), so re-firing on every reboot/login would consume the morning delta
// before the user reads it. The durable daily-brief gate now rejects duplicate
// same-period fires too; dropping RunAtLoad remains defense in depth and avoids
// needless scheduler work.
// Periodic refresh jobs (index/ingest/backup/lint) are idempotent re-runs, so
// they keep RunAtLoad to catch up after a login.

func scheduleSeams() schedulepkg.Seams {
	return schedulepkg.Seams{
		GOOS: runtimeGOOS, Executable: scheduleExecutable, RunCommand: runScheduleCommand, AppRoot: moraAppRoot, Success: okf,
	}
}
func listSchedules(stdout io.Writer, cfg Config) error {
	return schedulepkg.List(stdout, cfg, scheduleSeams())
}
func installSchedule(stdout io.Writer, cfg Config, job string) error {
	return schedulepkg.Install(stdout, cfg, job, scheduleSeams())
}
func uninstallSchedule(stdout io.Writer, cfg Config, job string) error {
	return schedulepkg.Uninstall(stdout, cfg, job, scheduleSeams())
}

func schedulePlistFor(cfg Config, job string) (string, bool) {
	schedulepkg.Configure(scheduleSeams())
	return schedulepkg.PlistFor(cfg, job)
}
func launchdSchedule(job string) string { return schedulepkg.LaunchdSchedule(job) }
func plistArgs(args string) string      { return schedulepkg.PlistArgs(args) }
func windowsScheduleCadenceArgs(job string) []string {
	return schedulepkg.WindowsScheduleCadenceArgs(job)
}
