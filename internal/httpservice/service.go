package httpservice

// Managed OS service for `mora serve http` (issue #54 follow-up).
//
// `mora serve http` runs the loopback server in the foreground — fine for a quick
// session, but a sandboxed AI browser wants it ALWAYS reachable. This installs it
// as a per-user, auto-restarting daemon, mirroring the launchd/schtasks machinery
// in schedule.go — except serve-http is a long-running daemon, so it uses
// KeepAlive (macOS) / Restart=on-failure (systemd) instead of a periodic cadence.
//
// The plist/unit renderers are pure functions (no disk I/O) so tests can assert
// their contents without touching the real system; the launchctl/schtasks calls
// go through the mockable runScheduleCommand seam.

import (
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const Label = "com.mora.serve-http"

type Options struct {
	StateDir, GOOS, Home, Executable, User string
	UID, Port                              int
	Env                                    map[string]string
	Stdout                                 io.Writer
	RunCommand                             func(string, ...string) ([]byte, error)
	PortFree                               func(int) bool
	Healthy                                func(int) bool
	OK                                     func(io.Writer, string, ...any)
}

func (o Options) ok(format string, args ...any) {
	if o.OK != nil {
		o.OK(o.Stdout, format, args...)
		return
	}
	fmt.Fprintf(o.Stdout, "✓ "+format+"\n", args...)
}

// serveHTTPPortFree reports whether the loopback port can be bound right now. It
// is a package var so tests can stub the preflight without depending on whatever
// is (or isn't) listening on the test host.
func PortFree(port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// serveHTTPServicePort resolves the port the daemon will bind — the same
// resolution serveLoopbackHTTP uses (MORA_PORT, else the default), so preflight
// and the installed daemon agree.

// serveHTTPService dispatches `mora serve http install|uninstall|status`.

// serveHTTPEnvVars snapshots the exported vars the daemon must carry — launchd,
// schtasks, and systemd all run with a bare environment. These are PATHS/config,
// never secrets: MORA_CONFIG_DIR pins a re-rooted install to the right vault
// (without it the daemon serves the DEFAULT vault, mirroring schedule.go's
// EnvironmentVariables snapshot), and MORA_PORT pins the port so preflight and
// the daemon agree.
func EnvVars(env map[string]string) (keys, vals []string) {
	if v := env["MORA_CONFIG_DIR"]; v != "" {
		keys, vals = append(keys, "MORA_CONFIG_DIR"), append(vals, v)
	}
	// MORA_VAULT is the runtime vault override (wins over config.toml, issue
	// #66) — without the snapshot the daemon reverts to config.toml's vault.
	if v := env["MORA_VAULT"]; v != "" {
		keys, vals = append(keys, "MORA_VAULT"), append(vals, v)
	}
	if v := env["MORA_PORT"]; v != "" {
		keys, vals = append(keys, "MORA_PORT"), append(vals, v)
	}
	return keys, vals
}

// serveHTTPPlist renders the launchd agent deterministically. KeepAlive is a
// {SuccessfulExit:false} dict, NOT true: a crash (non-zero exit — e.g. the port
// got taken) is relaunched, but a deliberate `launchctl bootout` or Ctrl-C stays
// down instead of instantly respawning. ThrottleInterval widens launchd's default
// 10s respawn floor so a persistent bind failure backs off to every 30s rather
// than hammering.
func Plist(stateDir, exe string, environment map[string]string) string {
	keys, vals := EnvVars(environment)
	envBlock := ""
	if len(keys) > 0 {
		var b strings.Builder
		b.WriteString("<key>EnvironmentVariables</key><dict>")
		for i := range keys {
			fmt.Fprintf(&b, "<key>%s</key><string>%s</string>", keys[i], vals[i])
		}
		b.WriteString("</dict>\n")
		envBlock = b.String()
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>serve</string><string>http</string></array>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>ThrottleInterval</key><integer>30</integer>
%s<key>StandardOutPath</key><string>%s</string>
<key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, Label, exe, envBlock,
		filepath.Join(stateDir, "serve-http.out.log"),
		filepath.Join(stateDir, "serve-http.err.log"))
}

// serveHTTPSystemdUnit renders the Linux systemd --user unit. Restart=on-failure
// mirrors the macOS SuccessfulExit:false semantics; RestartSec spaces out retries.
func SystemdUnit(stateDir, exe string, environment map[string]string) string {
	keys, vals := EnvVars(environment)
	var env strings.Builder
	for i := range keys {
		fmt.Fprintf(&env, "Environment=%s=%s\n", keys[i], vals[i])
	}
	return fmt.Sprintf(`[Unit]
Description=mora loopback HTTP server for sandboxed AI browsers
After=default.target

[Service]
ExecStart=%s serve http
%sRestart=on-failure
RestartSec=10s
StandardOutput=append:%s
StandardError=append:%s

[Install]
WantedBy=default.target
`, exe, env.String(),
		filepath.Join(stateDir, "serve-http.out.log"),
		filepath.Join(stateDir, "serve-http.err.log"))
}

func launchAgentsDir(home string) string { return filepath.Join(home, "Library", "LaunchAgents") }

func systemdUserUnitPath(home string) string {
	return filepath.Join(home, ".config", "systemd", "user", "mora-serve-http.service")
}

func Install(o Options) error {
	exe, port, stdout := o.Executable, o.Port, o.Stdout

	switch o.GOOS {
	case "darwin":
		dir := launchAgentsDir(o.Home)
		uid := strconv.Itoa(o.UID)
		// Stop any existing instance first so its hold on the port is released
		// before we preflight; ignore "not loaded".
		_, _ = o.RunCommand("launchctl", "bootout", "gui/"+uid+"/"+Label)
		if !o.PortFree(port) {
			return fmt.Errorf("port %d is already in use — free it (another process is bound to 127.0.0.1:%d) or set MORA_PORT, then retry", port, port)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		plistPath := filepath.Join(dir, Label+".plist")
		if err := atomicio.Write(plistPath, []byte(Plist(o.StateDir, exe, o.Env)), 0o644); err != nil {
			return err
		}
		if out, err := o.RunCommand("launchctl", "bootstrap", "gui/"+uid, plistPath); err != nil {
			return fmt.Errorf("launchctl bootstrap %s: %w: %s", Label, err, strings.TrimSpace(string(out)))
		}
		o.ok("installed + started launchd daemon %s (http://127.0.0.1:%d/, auto-restarts, starts at login)", Label, port)
		return nil

	case "windows":
		// A logon task — schtasks has no daemon/KeepAlive semantics, so this
		// starts serve-http at each login but does NOT auto-restart on crash.
		args := []string{"/Create", "/TN", `Mora\serve-http`, "/SC", "ONLOGON", "/TR", `"` + exe + `" serve http`, "/F"}
		if out, err := o.RunCommand("schtasks", args...); err != nil {
			return fmt.Errorf("schtasks create Mora\\serve-http: %w: %s", err, strings.TrimSpace(string(out)))
		}
		fmt.Fprintf(stdout, "installed Windows logon task Mora\\serve-http (starts at login; no crash auto-restart — for that use a real service). Start now: mora serve http\n")
		return nil

	default:
		// Linux/WSL: write the systemd --user unit and print the enable steps.
		// Enabling (and lingering, to survive logout) is left to the user because
		// it needs an active user bus this process may not have.
		unitPath := systemdUserUnitPath(o.Home)
		if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
			return err
		}
		if err := atomicio.Write(unitPath, []byte(SystemdUnit(o.StateDir, exe, o.Env)), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wrote systemd user unit %s\nEnable + start it:\n  systemctl --user daemon-reload\n  systemctl --user enable --now mora-serve-http.service\nTo keep it running after logout:\n  loginctl enable-linger %q\n", unitPath, o.User)
		return nil
	}
}

func Uninstall(o Options) error {
	stdout := o.Stdout
	switch o.GOOS {
	case "darwin":
		dir := launchAgentsDir(o.Home)
		uid := strconv.Itoa(o.UID)
		_, _ = o.RunCommand("launchctl", "bootout", "gui/"+uid+"/"+Label)
		if err := os.Remove(filepath.Join(dir, Label+".plist")); err != nil && !os.IsNotExist(err) {
			return err
		}
		o.ok("uninstalled launchd daemon %s", Label)
		return nil

	case "windows":
		if out, err := o.RunCommand("schtasks", "/Delete", "/TN", `Mora\serve-http`, "/F"); err != nil {
			return fmt.Errorf("schtasks delete Mora\\serve-http: %w: %s", err, strings.TrimSpace(string(out)))
		}
		o.ok("uninstalled Windows logon task Mora\\serve-http")
		return nil

	default:
		unitPath := systemdUserUnitPath(o.Home)
		if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Fprintf(stdout, "removed %s\nDisable it:\n  systemctl --user disable --now mora-serve-http.service\n", unitPath)
		return nil
	}
}

// statusServeHTTP reports whether the service file is installed AND whether the
// server is actually answering on the loopback port (an installed-but-dead daemon
// is a real state, so we probe /healthz rather than trust the plist alone).
func Status(o Options) error {
	port, stdout := o.Port, o.Stdout

	installed := false
	switch o.GOOS {
	case "darwin":
		installed = fileExists(filepath.Join(launchAgentsDir(o.Home), Label+".plist"))
	case "windows":
		_, err := o.RunCommand("schtasks", "/Query", "/TN", `Mora\serve-http`)
		installed = err == nil
	default:
		installed = fileExists(systemdUserUnitPath(o.Home))
	}

	listening := o.Healthy(port)
	fmt.Fprintf(stdout, "service installed: %v\nlistening on 127.0.0.1:%d: %v\n", installed, port, listening)
	return nil
}

// serveHTTPHealthy probes GET /healthz on the loopback port with a short timeout.
func Healthy(port int) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

func fileExists(path string) bool { _, err := os.Stat(path); return err == nil }
