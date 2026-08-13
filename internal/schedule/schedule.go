// Package schedule renders and installs Mora's OS scheduler definitions.
package schedule

import (
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/google"
)

var executable = os.Executable
var runtimeGOOS = func() string { return "" }
var commandRunner = func(name string, args ...string) ([]byte, error) { return exec.Command(name, args...).CombinedOutput() }

var commands = map[string]string{
	"pulse-daily": "schedule run pulse-daily", "index-hourly": "index rebuild",
	"backup-daily": "backup", "lint-weekly": "lint", "ingest-hourly": "ingest run --all",
	"git-daily": "sync git", "update-daily": "upgrade --scheduled-check", "doctor-pulse": "doctor --pulse",
}

// Seams contains the process/platform dependencies owned by the composition root.
type Seams struct {
	Success    func(io.Writer, string, ...any)
	GOOS       func() string
	Executable func() (string, error)
	RunCommand func(string, ...string) ([]byte, error)
	AppRoot    func(string) (string, bool)
}

// Configure installs the composition-root platform seams.
func Configure(seams Seams) {
	if seams.GOOS != nil {
		runtimeGOOS = seams.GOOS
	}
	if seams.Executable != nil {
		executable = seams.Executable
	}
	if seams.RunCommand != nil {
		commandRunner = seams.RunCommand
	}
	appRoot = seams.AppRoot
	success = seams.Success
}

var appRoot func(string) (string, bool)
var success func(io.Writer, string, ...any)

func RunAtLoad(job string) bool { return job != "pulse-daily" }

// schedulePlistFor renders a job's launchd plist deterministically (no disk I/O)
// so installSchedule and the tests share one builder. The bool is false for an
// unknown job (mirrors the command-map guard).
func PlistFor(cfg config.Config, job string) (string, bool) {
	cmdArgs, ok := commands[job]
	if !ok {
		return "", false
	}
	exe, _ := executable()
	label := "com.mora." + job
	runAtLoad := ""
	if RunAtLoad(job) {
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
		envVars = append(envVars, "<key>MORA_GOOGLE_CREDENTIALS</key><string>"+PlistEscape(creds)+"</string>")
	}
	if cfgDir := os.Getenv("MORA_CONFIG_DIR"); cfgDir != "" {
		envVars = append(envVars, "<key>MORA_CONFIG_DIR</key><string>"+PlistEscape(cfgDir)+"</string>")
	}
	if vault := os.Getenv("MORA_VAULT"); vault != "" {
		envVars = append(envVars, "<key>MORA_VAULT</key><string>"+PlistEscape(vault)+"</string>")
	}
	envBlock := ""
	if len(envVars) > 0 {
		envBlock = "<key>EnvironmentVariables</key><dict>" + strings.Join(envVars, "") + "</dict>\n"
	}
	program := exe
	programArgs := PlistArgs(cmdArgs)
	if runtimeGOOS() == "darwin" {
		resolved := exe
		if target, err := filepath.EvalSymlinks(exe); err == nil {
			resolved = target
		}
		if appRoot, ok := appRoot(resolved); ok {
			// TCC associates the eye-icon Full Disk Access grant with the app
			// bundle's LaunchServices identity. A launchd job that execs the
			// nested CLI directly is evaluated as the generic executable and can
			// lose Messages/Calendar access even though Mora.app is enabled.
			// open -n starts a fresh CLI instance; -W keeps launchd attached until
			// that instance exits. open does not propagate the child's exit status,
			// so Mora's command-owned sync and producer ledgers remain the honest
			// success/failure receipts consumed by doctor and health surfaces.
			program = "/usr/bin/open"
			programArgs = PlistArgValues("-n", "-W", "-a", appRoot, "--args") + PlistArgs(cmdArgs)
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
`, label, PlistEscape(program), programArgs, runAtLoad, envBlock, LaunchdSchedule(job), PlistEscape(filepath.Join(cfg.StateDir, job+".out.log")), PlistEscape(filepath.Join(cfg.StateDir, job+".err.log")))
	return plist, true
}
func Install(stdout io.Writer, cfg config.Config, job string, seams Seams) error {
	Configure(seams)
	cmdArgs, ok := commands[job]
	if !ok {
		return fmt.Errorf("unknown job %q", job)
	}
	exe, _ := executable()
	if runtimeGOOS() == "windows" {
		args := WindowsScheduleCreateArgs(job, exe, cmdArgs)
		if out, err := commandRunner("schtasks", args...); err != nil {
			return fmt.Errorf("schtasks create %s: %w: %s", WindowsTaskName(job), err, strings.TrimSpace(string(out)))
		}
		success(stdout, "installed Windows scheduled task %s", WindowsTaskName(job))
		return nil
	}
	if runtimeGOOS() == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir := filepath.Join(home, "Library", "LaunchAgents")
		label := "com.mora." + job
		plist, _ := PlistFor(cfg, job)
		plistPath := filepath.Join(dir, label+".plist")
		if err := atomicio.Write(plistPath, []byte(plist), 0o644); err != nil {
			return err
		}
		// Writing the plist does NOT load it: without an explicit bootstrap the
		// job stays inert until the next login — a silently-dead automation (this
		// exact gap left the daily brief dead for a week). Boot out any loaded
		// copy first so a reinstall picks up the NEW plist ("not loaded" is
		// benign, mirroring installServeHTTP), then bootstrap into the gui domain.
		uid := strconv.Itoa(os.Getuid())
		_, _ = commandRunner("launchctl", "bootout", "gui/"+uid+"/"+label)
		if out, err := commandRunner("launchctl", "bootstrap", "gui/"+uid, plistPath); err != nil {
			return fmt.Errorf("installed %s but launchctl bootstrap failed: %w: %s\nThe job will not run until you start it (or log out and back in):\n  launchctl bootstrap gui/%s %s",
				label, err, strings.TrimSpace(string(out)), uid, plistPath)
		}
		success(stdout, "installed + loaded launchd job %s (schedule active — no re-login needed)", label)
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
func List(stdout io.Writer, cfg config.Config, seams Seams) error {
	Configure(seams)
	if runtimeGOOS() == "windows" {
		jobs := make([]string, 0, len(commands))
		for job := range commands {
			jobs = append(jobs, job)
		}
		sort.Strings(jobs)
		for _, job := range jobs {
			if _, err := commandRunner("schtasks", "/Query", "/TN", WindowsTaskName(job)); err == nil {
				fmt.Fprintln(stdout, WindowsTaskName(job))
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
func Uninstall(stdout io.Writer, cfg config.Config, job string, seams Seams) error {
	Configure(seams)
	if _, ok := commands[job]; !ok {
		return fmt.Errorf("unknown job %q", job)
	}
	if runtimeGOOS() == "windows" {
		if out, err := commandRunner("schtasks", "/Delete", "/TN", WindowsTaskName(job), "/F"); err != nil {
			return fmt.Errorf("schtasks delete %s: %w: %s", WindowsTaskName(job), err, strings.TrimSpace(string(out)))
		}
		success(stdout, "uninstalled Windows scheduled task %s", WindowsTaskName(job))
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
		_, _ = commandRunner("launchctl", "bootout", "gui/"+uid+"/"+label)
		if err := os.Remove(filepath.Join(home, "Library", "LaunchAgents", label+".plist")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		success(stdout, "uninstalled launchd job %s", label)
		return nil
	}
	fmt.Fprintln(stdout, "Linux: remove the cron line or systemd user timer you installed for this job.")
	return nil
}
func WindowsTaskName(job string) string {
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
func WindowsTaskCommand(exe, cmdArgs string) string {
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
func WindowsScheduleCreateArgs(job, exe, cmdArgs string) []string {
	args := []string{"/Create", "/TN", WindowsTaskName(job), "/TR", WindowsTaskCommand(exe, cmdArgs)}
	args = append(args, WindowsScheduleCadenceArgs(job)...)
	args = append(args, "/F")
	return args
}
func WindowsScheduleCadenceArgs(job string) []string {
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
	case "update-daily":
		return []string{"/SC", "DAILY", "/ST", "04:00"}
	default:
		return []string{"/SC", "HOURLY", "/MO", "1"}
	}
}
func PlistArgs(args string) string {
	return PlistArgValues(strings.Fields(args)...)
}
func PlistArgValues(args ...string) string {
	var b strings.Builder
	for _, a := range args {
		fmt.Fprintf(&b, "<string>%s</string>", PlistEscape(a))
	}
	return b.String()
}
func PlistEscape(value string) string { return html.EscapeString(value) }
func LaunchdSchedule(job string) string {
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
	case "update-daily":
		return "<key>StartCalendarInterval</key><dict><key>Hour</key><integer>4</integer><key>Minute</key><integer>0</integer></dict>"
	case "lint-weekly":
		return "<key>StartCalendarInterval</key><dict><key>Weekday</key><integer>0</integer><key>Hour</key><integer>9</integer><key>Minute</key><integer>0</integer></dict>"
	default:
		return "<key>StartInterval</key><integer>3600</integer>"
	}
}
