package mora

import (
	"context"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/google"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

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
			return errors.New("usage: mora schedule install <pulse-daily|doctor-pulse|index-hourly|backup-daily|lint-weekly|ingest-hourly|git-daily>")
		}
		return installSchedule(stdout, cfg, args[1])
	case "uninstall":
		if len(args) != 2 {
			return errors.New("usage: mora schedule uninstall <pulse-daily|doctor-pulse|index-hourly|backup-daily|lint-weekly|ingest-hourly|git-daily>")
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
func scheduleRunAtLoad(job string) bool { return job != "pulse-daily" }

// schedulePlistFor renders a job's launchd plist deterministically (no disk I/O)
// so installSchedule and the tests share one builder. The bool is false for an
// unknown job (mirrors the command-map guard).
func schedulePlistFor(cfg Config, job string) (string, bool) {
	cmdArgs, ok := scheduleCommands[job]
	if !ok {
		return "", false
	}
	exe, _ := os.Executable()
	label := "com.mora." + job
	runAtLoad := ""
	if scheduleRunAtLoad(job) {
		runAtLoad = "<key>RunAtLoad</key><true/>\n"
	}
	// launchd jobs do NOT inherit the user's shell environment, so any exported
	// var the job depends on must be snapshotted into the plist at install time
	// (these are PATHS, not secrets):
	//   - MORA_GOOGLE_CREDENTIALS: without it a BYO-creds setup silently hits the
	//     embedded DEV_PLACEHOLDER client on every scheduled Google sync while
	//     terminal syncs keep working — the vault goes stale with no visible error.
	//   - MORA_CONFIG_DIR: without it a re-rooted (scratch/isolated) install's job
	//     runs against the DEFAULT vault — syncing/advancing the wrong installation.
	var envVars []string
	if creds := os.Getenv("MORA_GOOGLE_CREDENTIALS"); creds != "" {
		envVars = append(envVars, "<key>MORA_GOOGLE_CREDENTIALS</key><string>"+creds+"</string>")
	}
	if cfgDir := os.Getenv("MORA_CONFIG_DIR"); cfgDir != "" {
		envVars = append(envVars, "<key>MORA_CONFIG_DIR</key><string>"+cfgDir+"</string>")
	}
	envBlock := ""
	if len(envVars) > 0 {
		envBlock = "<key>EnvironmentVariables</key><dict>" + strings.Join(envVars, "") + "</dict>\n"
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string>%s</array>
%s%s%s
<key>StandardOutPath</key><string>%s</string>
<key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, label, exe, plistArgs(cmdArgs), runAtLoad, envBlock, launchdSchedule(job), filepath.Join(cfg.StateDir, job+".out.log"), filepath.Join(cfg.StateDir, job+".err.log"))
	return plist, true
}
func installSchedule(stdout io.Writer, cfg Config, job string) error {
	cmdArgs, ok := scheduleCommands[job]
	if !ok {
		return fmt.Errorf("unknown job %q", job)
	}
	exe, _ := os.Executable()
	if runtimeGOOS() == "windows" {
		args := windowsScheduleCreateArgs(job, exe, cmdArgs)
		if out, err := runScheduleCommand("schtasks", args...); err != nil {
			return fmt.Errorf("schtasks create %s: %w: %s", windowsTaskName(job), err, strings.TrimSpace(string(out)))
		}
		okf(stdout, "installed Windows scheduled task %s", windowsTaskName(job))
		return nil
	}
	if runtimeGOOS() == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir := filepath.Join(home, "Library", "LaunchAgents")
		label := "com.mora." + job
		plist, _ := schedulePlistFor(cfg, job)
		if err := atomicWrite(filepath.Join(dir, label+".plist"), []byte(plist), 0o644); err != nil {
			return err
		}
		okf(stdout, "installed launchd job %s", label)
		return nil
	}
	// Linux / WSL2: launchd unavailable. cron also won't inherit the shell env,
	// so a re-rooted install must carry MORA_CONFIG_DIR on the cron line or the
	// job runs against the default vault (mirrors the launchd EnvironmentVariables
	// snapshot above).
	cronEnv := ""
	if cfgDir := os.Getenv("MORA_CONFIG_DIR"); cfgDir != "" {
		cronEnv = "MORA_CONFIG_DIR=" + cfgDir + " "
	}
	if google.IsWSL() {
		fmt.Fprintf(stdout, "WSL detected: no launchd. Add to crontab or run manually:\n  */60 * * * * %s%s %s\nOr just run `mora sync google` when you want fresh data.\n", cronEnv, exe, cmdArgs)
		return nil
	}
	fmt.Fprintf(stdout, "Linux: launchd unavailable. cron line:\n  */60 * * * * %s%s %s\nOr a systemd user timer. Or run `mora sync google` manually.\n", cronEnv, exe, cmdArgs)
	return nil
}
func listSchedules(stdout io.Writer, cfg Config) error {
	if runtimeGOOS() == "windows" {
		jobs := make([]string, 0, len(scheduleCommands))
		for job := range scheduleCommands {
			jobs = append(jobs, job)
		}
		sort.Strings(jobs)
		for _, job := range jobs {
			if _, err := runScheduleCommand("schtasks", "/Query", "/TN", windowsTaskName(job)); err == nil {
				fmt.Fprintln(stdout, windowsTaskName(job))
			}
		}
		return nil
	}
	if runtimeGOOS() == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		matches, _ := filepath.Glob(filepath.Join(home, "Library", "LaunchAgents", "com.mora.*.plist"))
		for _, m := range matches {
			fmt.Fprintln(stdout, filepath.Base(m))
		}
		return nil
	}
	fmt.Fprintln(stdout, "cron listing not implemented")
	return nil
}
func uninstallSchedule(stdout io.Writer, cfg Config, job string) error {
	if _, ok := scheduleCommands[job]; !ok {
		return fmt.Errorf("unknown job %q", job)
	}
	if runtimeGOOS() == "windows" {
		if out, err := runScheduleCommand("schtasks", "/Delete", "/TN", windowsTaskName(job), "/F"); err != nil {
			return fmt.Errorf("schtasks delete %s: %w: %s", windowsTaskName(job), err, strings.TrimSpace(string(out)))
		}
		okf(stdout, "uninstalled Windows scheduled task %s", windowsTaskName(job))
		return nil
	}
	if runtimeGOOS() == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		label := "com.mora." + job
		// Removing a plist does not stop a job already loaded into launchd. Boot
		// out the service first; "not loaded" is benign because uninstall is
		// idempotent and the plist still needs to be removed.
		uid := strconv.Itoa(os.Getuid())
		_, _ = runScheduleCommand("launchctl", "bootout", "gui/"+uid+"/"+label)
		if err := os.Remove(filepath.Join(home, "Library", "LaunchAgents", label+".plist")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		okf(stdout, "uninstalled launchd job %s", label)
		return nil
	}
	fmt.Fprintln(stdout, "Linux: remove the cron line or systemd user timer you installed for this job.")
	return nil
}
func windowsTaskName(job string) string {
	return `Mora\` + job
}

// windowsTaskCommand builds the schtasks /TR action string. Scheduled tasks do
// NOT inherit the user's shell environment, so any exported var the job depends
// on must be snapshotted into the action at install time — mirroring the launchd
// EnvironmentVariables block in schedulePlistFor:
//   - MORA_GOOGLE_CREDENTIALS: without it a BYO-creds setup silently hits the
//     embedded DEV_PLACEHOLDER client on every scheduled Google sync while
//     terminal syncs keep working — the vault goes stale with no visible error.
//   - MORA_CONFIG_DIR: without it a re-rooted install's job runs against the
//     DEFAULT vault.
//
// When any var is carried, the action is wrapped in `cmd /c "..."`. cmd.exe
// strips only the FIRST and LAST quote of that string (there are >2 quotes plus
// the special `&` char), so the inner exe path is protected with PLAIN double
// quotes: cmd.exe does NOT treat backslash as a quote escape (`\"` would break
// launch every run), and there is no space before `&&` or cmd.exe folds it into
// the env value.
//
// The env VALUES use the `set "VAR=value"` quoted idiom so metacharacters in a
// path — `&` (legal in folder names, e.g. C:\R&D\creds.json), spaces, `<`, `>`,
// `|` — are taken literally instead of being parsed by cmd.exe as command
// separators, which would silently truncate the `set` (and, for creds, fall the
// job back to the embedded placeholder). The inner quotes stay balanced, so cmd
// /c's first/last-quote strip leaves them intact. Residual (rare): a literal `%`
// is still expanded by cmd.exe (paths rarely contain one), and schtasks truncates
// /TR at ~261 chars — a very long MORA_CONFIG_DIR would need a wrapper script.
func windowsTaskCommand(exe, cmdArgs string) string {
	var sets []string
	if creds := os.Getenv("MORA_GOOGLE_CREDENTIALS"); creds != "" {
		sets = append(sets, `set "MORA_GOOGLE_CREDENTIALS=`+creds+`"`)
	}
	if cfgDir := os.Getenv("MORA_CONFIG_DIR"); cfgDir != "" {
		sets = append(sets, `set "MORA_CONFIG_DIR=`+cfgDir+`"`)
	}
	if len(sets) == 0 {
		return `"` + exe + `" ` + cmdArgs
	}
	return `cmd /c "` + strings.Join(sets, "&& ") + `&& "` + exe + `" ` + cmdArgs + `"`
}
func windowsScheduleCreateArgs(job, exe, cmdArgs string) []string {
	args := []string{"/Create", "/TN", windowsTaskName(job), "/TR", windowsTaskCommand(exe, cmdArgs)}
	args = append(args, windowsScheduleCadenceArgs(job)...)
	args = append(args, "/F")
	return args
}
func windowsScheduleCadenceArgs(job string) []string {
	switch job {
	case "index-hourly", "ingest-hourly":
		return []string{"/SC", "HOURLY", "/MO", "1"}
	case "lint-weekly":
		return []string{"/SC", "WEEKLY", "/D", "SUN", "/ST", "09:00"}
	case "pulse-daily":
		return []string{"/SC", "DAILY", "/ST", "08:00"}
	case "doctor-pulse":
		return []string{"/SC", "DAILY", "/ST", "09:00"}
	case "backup-daily":
		return []string{"/SC", "DAILY", "/ST", "02:00"}
	case "git-daily":
		return []string{"/SC", "DAILY", "/ST", "03:00"}
	default:
		return []string{"/SC", "HOURLY", "/MO", "1"}
	}
}
func plistArgs(args string) string {
	var b strings.Builder
	for _, a := range strings.Fields(args) {
		fmt.Fprintf(&b, "<string>%s</string>", a)
	}
	return b.String()
}
func launchdSchedule(job string) string {
	switch job {
	case "index-hourly", "ingest-hourly":
		return "<key>StartInterval</key><integer>3600</integer>"
	case "pulse-daily":
		return "<key>StartCalendarInterval</key><dict><key>Hour</key><integer>8</integer><key>Minute</key><integer>0</integer></dict>"
	case "doctor-pulse":
		return "<key>StartCalendarInterval</key><dict><key>Hour</key><integer>9</integer><key>Minute</key><integer>0</integer></dict>"
	case "backup-daily":
		return "<key>StartCalendarInterval</key><dict><key>Hour</key><integer>2</integer><key>Minute</key><integer>0</integer></dict>"
	case "git-daily":
		return "<key>StartCalendarInterval</key><dict><key>Hour</key><integer>3</integer><key>Minute</key><integer>0</integer></dict>"
	case "lint-weekly":
		return "<key>StartCalendarInterval</key><dict><key>Weekday</key><integer>0</integer><key>Hour</key><integer>9</integer><key>Minute</key><integer>0</integer></dict>"
	default:
		return "<key>StartInterval</key><integer>3600</integer>"
	}
}
