package mora

import (
	"context"
	"errors"
	"flag"
	"fmt"
	schedulepkg "github.com/pyranthus-hq/mora/internal/schedule"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

var scheduleExecutable = os.Executable

func cmdSchedule(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mora schedule install|list|uninstall|run")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("schedule list", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		jsonOut := fs.Bool("json", false, "emit JSON")
		if parseErr := fs.Parse(args[1:]); parseErr != nil {
			return newMoraError(errCodeUsageUnknownFlag, "usage", parseErr, "%v", parseErr)
		}
		if fs.NArg() != 0 {
			return newMoraError(errCodeUsageUnknownValue, "usage", nil, "unexpected argument %q", fs.Arg(0))
		}
		return listSchedules(stdout, cfg, *jsonOut)
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
		jsonOut := false
		rest := make([]string, 0, len(args))
		for _, a := range args {
			if a == "--json" {
				jsonOut = true
				continue
			}
			rest = append(rest, a)
		}
		args = rest
		if len(args) != 2 || args[1] != "pulse-daily" {
			return errors.New("usage: mora schedule run pulse-daily")
		}
		// Under --json the scheduled run's own progress is a diagnostic; the
		// receipt is the document. The job's exit status is unchanged.
		out := stdout
		if jsonOut {
			out = stderr
		}
		if err := runScheduledPulseDaily(ctx, cfg, out, stderr); err != nil {
			return err
		}
		_ = appendTraceEvent(cfg, traceEvent{CorrelationID: queryCorrelationID(), Stage: traceStageSchedule, Status: "completed"})
		if jsonOut {
			return emitReceipt(stdout, "mora.schedule.run", 1, scheduleRunReceipt{Job: args[1], Ran: true})
		}
		return nil
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
// scheduleRunReceipt is the machine form of one scheduled job invocation. A
// skipped once-per-period run is still `ran: true` at the CLI level — the gate
// is reported by the loop receipts, not by this one.
type scheduleRunReceipt struct {
	Job string `json:"job"`
	Ran bool   `json:"ran"`
}

func runScheduledPulseDaily(ctx context.Context, cfg Config, stdout, stderr io.Writer) error {
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
	if err := cmdPulse(ctx, pulseArgs, stdout, stderr); err != nil {
		closeErr := loopDone(cfg, loopID, rec.RunID, false, err.Error(), false, loopClock(), stdout)
		return errors.Join(fmt.Errorf("scheduled pulse: %w", err), closeErr)
	}
	if err := loopDone(cfg, loopID, rec.RunID, true, "", false, loopClock(), stdout); err != nil {
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

type scheduleListEntry struct {
	Name      string `json:"name"`
	Cadence   string `json:"cadence"`
	NextRun   string `json:"next_run"`
	Installed bool   `json:"installed"`
}

type scheduleListPayload struct {
	Entries []scheduleListEntry `json:"entries"`
}

func listSchedules(stdout io.Writer, cfg Config, jsonOutput ...bool) error {
	// Plan 01-04: `--json` reports every known job with its cadence, its
	// deterministic next run, and whether it is installed — not just the
	// installed plist filenames the human listing prints.
	if len(jsonOutput) > 0 && jsonOutput[0] {
		entries, err := scheduledEntries(cfg)
		if err != nil {
			return err
		}
		return emitReceipt(stdout, "mora.schedule.list", 1, scheduleListPayload{Entries: entries})
	}
	return schedulepkg.List(stdout, cfg, scheduleSeams())
}
func installSchedule(stdout io.Writer, cfg Config, job string) error {
	return schedulepkg.Install(stdout, cfg, job, scheduleSeams())
}

func scheduledEntries(cfg Config) ([]scheduleListEntry, error) {
	jobs := make([]string, 0, len(scheduleCommands))
	for job := range scheduleCommands {
		jobs = append(jobs, job)
	}
	sort.Strings(jobs)
	entries := make([]scheduleListEntry, 0, len(jobs))
	installed := make(map[string]bool, len(jobs))
	switch runtimeGOOS() {
	case "windows":
		for _, job := range jobs {
			_, err := runScheduleCommand("schtasks", "/Query", "/TN", schedulepkg.WindowsTaskName(job))
			installed[job] = err == nil
		}
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		for _, job := range jobs {
			_, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", "com.mora."+job+".plist"))
			installed[job] = err == nil
		}
	}
	for _, job := range jobs {
		entries = append(entries, scheduleListEntry{
			Name: job, Cadence: scheduleCadence(job), NextRun: nextScheduleRun(job, producerClock()), Installed: installed[job],
		})
	}
	return entries, nil
}

func scheduleCadence(job string) string {
	switch job {
	case "index-hourly", "ingest-hourly":
		return "hourly"
	case "pulse-daily":
		return "daily at 08:00"
	case "doctor-pulse":
		return "daily at 09:00"
	case "backup-daily":
		return "daily at 02:00"
	case "git-daily":
		return "daily at 03:00"
	case "lint-weekly":
		return "weekly"
	default:
		return "unknown"
	}
}

func nextScheduleRun(job string, now time.Time) string {
	local := now.Local()
	if job == "index-hourly" || job == "ingest-hourly" {
		return local.Truncate(time.Hour).Add(time.Hour).Format(time.RFC3339)
	}
	hour := 8
	weekday := -1
	switch job {
	case "doctor-pulse":
		hour = 9
	case "backup-daily":
		hour = 2
	case "git-daily":
		hour = 3
	case "lint-weekly":
		hour, weekday = 9, int(time.Sunday)
	}
	next := time.Date(local.Year(), local.Month(), local.Day(), hour, 0, 0, 0, local.Location())
	if weekday >= 0 {
		for int(next.Weekday()) != weekday || !next.After(local) {
			next = next.AddDate(0, 0, 1)
		}
	} else if !next.After(local) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Format(time.RFC3339)
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
