package mora

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/google"
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
		return runScheduledPulseDaily(ctx, cfg, stdout, stderr)
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
func scheduleRunAtLoad(job string) bool { return job != "pulse-daily" }

// schedulePlistFor renders a job's launchd plist deterministically (no disk I/O)
// so installSchedule and the tests share one builder. The bool is false for an
// unknown job (mirrors the command-map guard).
func schedulePlistFor(cfg Config, job string) (string, bool) {
	cmdArgs, ok := scheduleCommands[job]
	if !ok {
		return "", false
	}
	exe, _ := scheduleExecutable()
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
	//   - MORA_VAULT: the runtime vault override (wins over config.toml, issue
	//     #66); without it the job silently reverts to config.toml's vault.
	var envVars []string
	if creds := os.Getenv("MORA_GOOGLE_CREDENTIALS"); creds != "" {
		envVars = append(envVars, "<key>MORA_GOOGLE_CREDENTIALS</key><string>"+plistEscape(creds)+"</string>")
	}
	if cfgDir := os.Getenv("MORA_CONFIG_DIR"); cfgDir != "" {
		envVars = append(envVars, "<key>MORA_CONFIG_DIR</key><string>"+plistEscape(cfgDir)+"</string>")
	}
	if vault := os.Getenv("MORA_VAULT"); vault != "" {
		envVars = append(envVars, "<key>MORA_VAULT</key><string>"+plistEscape(vault)+"</string>")
	}
	envBlock := ""
	if len(envVars) > 0 {
		envBlock = "<key>EnvironmentVariables</key><dict>" + strings.Join(envVars, "") + "</dict>\n"
	}
	program := exe
	programArgs := plistArgs(cmdArgs)
	if runtimeGOOS() == "darwin" {
		resolved := exe
		if target, err := filepath.EvalSymlinks(exe); err == nil {
			resolved = target
		}
		if appRoot, ok := moraAppRoot(resolved); ok {
			// TCC associates the eye-icon Full Disk Access grant with the app
			// bundle's LaunchServices identity. A launchd job that execs the
			// nested CLI directly is evaluated as the generic executable and can
			// lose Messages/Calendar access even though Mora.app is enabled.
			// open -n starts a fresh CLI instance; -W keeps launchd attached until
			// that instance exits. open does not propagate the child's exit status,
			// so Mora's command-owned sync and producer ledgers remain the honest
			// success/failure receipts consumed by doctor and health surfaces.
			program = "/usr/bin/open"
			programArgs = plistArgValues("-n", "-W", "-a", appRoot, "--args") + plistArgs(cmdArgs)
		}
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
`, label, plistEscape(program), programArgs, runAtLoad, envBlock, launchdSchedule(job), plistEscape(filepath.Join(cfg.StateDir, job+".out.log")), plistEscape(filepath.Join(cfg.StateDir, job+".err.log")))
	return plist, true
}
func installSchedule(stdout io.Writer, cfg Config, job string) error {
	cmdArgs, ok := scheduleCommands[job]
	if !ok {
		return fmt.Errorf("unknown job %q", job)
	}
	exe, _ := scheduleExecutable()
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
		plistPath := filepath.Join(dir, label+".plist")
		if err := atomicWrite(plistPath, []byte(plist), 0o644); err != nil {
			return err
		}
		// Writing the plist does NOT load it: without an explicit bootstrap the
		// job stays inert until the next login — a silently-dead automation (this
		// exact gap left the daily brief dead for a week). Boot out any loaded
		// copy first so a reinstall picks up the NEW plist ("not loaded" is
		// benign, mirroring installServeHTTP), then bootstrap into the gui domain.
		uid := strconv.Itoa(os.Getuid())
		_, _ = runScheduleCommand("launchctl", "bootout", "gui/"+uid+"/"+label)
		if out, err := runScheduleCommand("launchctl", "bootstrap", "gui/"+uid, plistPath); err != nil {
			return fmt.Errorf("installed %s but launchctl bootstrap failed: %w: %s\nThe job will not run until you start it (or log out and back in):\n  launchctl bootstrap gui/%s %s",
				label, err, strings.TrimSpace(string(out)), uid, plistPath)
		}
		okf(stdout, "installed + loaded launchd job %s (schedule active — no re-login needed)", label)
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
	jsonOut := len(jsonOutput) > 0 && jsonOutput[0]
	if jsonOut {
		entries, err := scheduledEntries(cfg)
		if err != nil {
			return err
		}
		return emitReceipt(stdout, "mora.schedule.list", 1, scheduleListPayload{Entries: entries})
	}
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
			_, err := runScheduleCommand("schtasks", "/Query", "/TN", windowsTaskName(job))
			installed[job] = err == nil
		}
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		for _, job := range jobs {
			installed[job] = fileExists(filepath.Join(home, "Library", "LaunchAgents", "com.mora."+job+".plist"))
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
	return plistArgValues(strings.Fields(args)...)
}
func plistArgValues(args ...string) string {
	var b strings.Builder
	for _, a := range args {
		fmt.Fprintf(&b, "<string>%s</string>", plistEscape(a))
	}
	return b.String()
}
func plistEscape(value string) string { return html.EscapeString(value) }
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
