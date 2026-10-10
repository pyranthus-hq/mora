package schedule

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/config"
)

// SystemdUnitName is the systemd unit stem for a job ("mora-share-sync").
func SystemdUnitName(job string) string { return "mora-" + job }

func systemdUserDir(cfg config.Config) string {
	return filepath.Join(cfg.HomeDir(), ".config", "systemd", "user")
}

func systemdUnitPaths(cfg config.Config, job string) (service, timer string) {
	dir := systemdUserDir(cfg)
	return filepath.Join(dir, SystemdUnitName(job)+".service"), filepath.Join(dir, SystemdUnitName(job)+".timer")
}

// SystemdTimerInstalled reports whether the job's timer unit is on disk.
func SystemdTimerInstalled(cfg config.Config, job string) bool {
	_, timer := systemdUnitPaths(cfg, job)
	_, err := os.Stat(timer)
	return err == nil
}

func sortedSystemdJobs() []string {
	jobs := make([]string, 0, len(systemdJobs))
	for job := range systemdJobs {
		jobs = append(jobs, job)
	}
	sort.Strings(jobs)
	return jobs
}

// systemdInterval is how long the timer waits after each run.
func systemdInterval(job string) string {
	switch job {
	case "share-sync":
		return "15min"
	default:
		return "1h"
	}
}

// systemdQuote renders one value as a double-quoted systemd unit word: `\` and
// `"` are backslash-escaped and `%` is doubled so specifiers never expand.
func systemdQuote(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	return `"` + strings.ReplaceAll(v, "%", "%%") + `"`
}

// SystemdUnitsFor renders a job's systemd --user service and timer
// deterministically (no disk I/O). The bool is false for a job that has no
// systemd timer.
func SystemdUnitsFor(cfg config.Config, job string) (service, timer string, ok bool) {
	cmdArgs, known := commands[job]
	if !known || !HasSystemdTimer(job) {
		return "", "", false
	}
	exe, _ := executable()
	var env strings.Builder
	for _, kv := range jobEnv(job) {
		fmt.Fprintf(&env, "Environment=%s\n", systemdQuote(kv[0]+"="+kv[1]))
	}
	logPath := func(suffix string) string {
		return strings.ReplaceAll(filepath.Join(cfg.StateDir, job+suffix), "%", "%%")
	}
	service = fmt.Sprintf(`[Unit]
Description=Mora %s (mora %s)

[Service]
Type=oneshot
ExecStart=%s %s
%sStandardOutput=append:%s
StandardError=append:%s
`, job, cmdArgs, systemdQuote(exe), cmdArgs, env.String(), logPath(".out.log"), logPath(".err.log"))
	// OnActiveSec fires shortly after the timer starts (install, login, or
	// manager start), the systemd twin of launchd's RunAtLoad. OnUnitActiveSec
	// then repeats relative to the last run.
	timer = fmt.Sprintf(`[Unit]
Description=Run Mora %s every %s

[Timer]
OnActiveSec=1min
OnUnitActiveSec=%s
Unit=%s.service

[Install]
WantedBy=timers.target
`, job, systemdInterval(job), systemdInterval(job), SystemdUnitName(job))
	return service, timer, true
}

// systemctlUser runs `systemctl --user`. An agent or ssh shell often lacks
// XDG_RUNTIME_DIR even though the user manager is running, and systemctl then
// reports "Failed to connect to user scope bus". Point it at the standard
// per-user runtime dir when that dir has a bus socket.
func systemctlUser(args ...string) ([]byte, error) {
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		dir := filepath.Join("/run/user", strconv.Itoa(os.Getuid()))
		if _, err := os.Stat(filepath.Join(dir, "bus")); err == nil {
			_ = os.Setenv("XDG_RUNTIME_DIR", dir)
		}
	}
	return commandRunner("systemctl", append([]string{"--user"}, args...)...)
}

func installSystemdTimer(stdout io.Writer, cfg config.Config, job string) error {
	if cfg.HomeDir() == "" {
		return errors.New("cannot resolve a home directory for systemd user units")
	}
	service, timer, _ := SystemdUnitsFor(cfg, job)
	servicePath, timerPath := systemdUnitPaths(cfg, job)
	if err := os.MkdirAll(filepath.Dir(servicePath), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return err
	}
	if err := atomicio.Write(servicePath, []byte(service), 0o644); err != nil {
		return err
	}
	if err := atomicio.Write(timerPath, []byte(timer), 0o644); err != nil {
		return err
	}
	timerUnit := SystemdUnitName(job) + ".timer"
	// Writing the units does not start anything. Reload and enable --now in the
	// same step so the timer is live immediately; a written-but-inactive timer
	// would be a silently dead job.
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", timerUnit}} {
		if out, err := systemctlUser(args...); err != nil {
			return fmt.Errorf("wrote %s and %s but `systemctl --user %s` failed: %w: %s\nThe job will not run until you start it from a session with a systemd user manager:\n  systemctl --user daemon-reload\n  systemctl --user enable --now %s",
				servicePath, timerPath, strings.Join(args, " "), err, strings.TrimSpace(string(out)), timerUnit)
		}
	}
	success(stdout, "installed + started systemd user timer %s (every %s; log: %s)", timerUnit, systemdInterval(job), filepath.Join(cfg.StateDir, job+".out.log"))
	// Without lingering, the user manager (and this timer) stops when the user's
	// last session ends: on a headless server the job would only run while
	// someone is logged in. Report it; enabling linger is the user's call.
	uid := strconv.Itoa(os.Getuid())
	if out, err := commandRunner("loginctl", "show-user", uid, "--property=Linger", "--value"); err == nil && strings.TrimSpace(string(out)) == "no" {
		user := os.Getenv("USER")
		if user == "" {
			user = uid
		}
		fmt.Fprintf(stdout, "warning: lingering is off for %s, so this timer stops when your last session ends.\nTo keep it running after logout:\n  loginctl enable-linger %s\n", user, user)
	}
	return nil
}

func uninstallSystemdTimer(stdout io.Writer, cfg config.Config, job string) error {
	if cfg.HomeDir() == "" {
		return errors.New("cannot resolve a home directory for systemd user units")
	}
	timerUnit := SystemdUnitName(job) + ".timer"
	servicePath, timerPath := systemdUnitPaths(cfg, job)
	if _, err := os.Stat(timerPath); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(servicePath); errors.Is(err, os.ErrNotExist) {
			success(stdout, "systemd user timer %s is not installed", timerUnit)
			return nil
		}
	}
	// Removing the unit files does not stop a running timer. Disable --now first.
	disableOut, disableErr := systemctlUser("disable", "--now", timerUnit)
	for _, p := range []string{timerPath, servicePath} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if disableErr != nil {
		return fmt.Errorf("removed %s and %s but `systemctl --user disable --now %s` failed: %w: %s\nStop the loaded timer from a session with a systemd user manager:\n  systemctl --user stop %s\n  systemctl --user daemon-reload",
			timerPath, servicePath, timerUnit, disableErr, strings.TrimSpace(string(disableOut)), timerUnit)
	}
	if out, err := systemctlUser("daemon-reload"); err != nil {
		return fmt.Errorf("removed %s but `systemctl --user daemon-reload` failed: %w: %s", timerUnit, err, strings.TrimSpace(string(out)))
	}
	success(stdout, "uninstalled systemd user timer %s", timerUnit)
	return nil
}
