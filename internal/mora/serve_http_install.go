package mora

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
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const serveHTTPLabel = "com.mora.serve-http"

// serveHTTPPortFree reports whether the loopback port can be bound right now. It
// is a package var so tests can stub the preflight without depending on whatever
// is (or isn't) listening on the test host.
var serveHTTPPortFree = func(port int) bool {
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
func serveHTTPServicePort() int { return envPortOr(defaultHTTPPort) }

// serveHTTPService dispatches `mora serve http install|uninstall|status`.
func serveHTTPService(cfg Config, sub string, stdout io.Writer) error {
	switch sub {
	case "install":
		return installServeHTTP(cfg, stdout)
	case "uninstall":
		return uninstallServeHTTP(cfg, stdout)
	case "status":
		return statusServeHTTP(cfg, stdout)
	default:
		return fmt.Errorf("usage: mora serve http install|uninstall|status")
	}
}

// serveHTTPEnvVars snapshots the exported vars the daemon must carry — launchd,
// schtasks, and systemd all run with a bare environment. These are PATHS/config,
// never secrets: MORA_CONFIG_DIR pins a re-rooted install to the right vault
// (without it the daemon serves the DEFAULT vault, mirroring schedule.go's
// EnvironmentVariables snapshot), and MORA_PORT pins the port so preflight and
// the daemon agree.
func serveHTTPEnvVars() (keys, vals []string) {
	if v := os.Getenv("MORA_CONFIG_DIR"); v != "" {
		keys, vals = append(keys, "MORA_CONFIG_DIR"), append(vals, v)
	}
	// MORA_VAULT is the runtime vault override (wins over config.toml, issue
	// #66) — without the snapshot the daemon reverts to config.toml's vault.
	if v := os.Getenv("MORA_VAULT"); v != "" {
		keys, vals = append(keys, "MORA_VAULT"), append(vals, v)
	}
	if v := os.Getenv("MORA_PORT"); v != "" {
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
func serveHTTPPlist(cfg Config, exe string) string {
	keys, vals := serveHTTPEnvVars()
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
`, serveHTTPLabel, exe, envBlock,
		filepath.Join(cfg.StateDir, "serve-http.out.log"),
		filepath.Join(cfg.StateDir, "serve-http.err.log"))
}

// serveHTTPSystemdUnit renders the Linux systemd --user unit. Restart=on-failure
// mirrors the macOS SuccessfulExit:false semantics; RestartSec spaces out retries.
func serveHTTPSystemdUnit(cfg Config, exe string) string {
	keys, vals := serveHTTPEnvVars()
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
		filepath.Join(cfg.StateDir, "serve-http.out.log"),
		filepath.Join(cfg.StateDir, "serve-http.err.log"))
}

func launchAgentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

func systemdUserUnitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user", "mora-serve-http.service"), nil
}

func installServeHTTP(cfg Config, stdout io.Writer) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	port := serveHTTPServicePort()

	switch runtimeGOOS() {
	case "darwin":
		dir, err := launchAgentsDir()
		if err != nil {
			return err
		}
		uid := strconv.Itoa(os.Getuid())
		// Stop any existing instance first so its hold on the port is released
		// before we preflight; ignore "not loaded".
		_, _ = runScheduleCommand("launchctl", "bootout", "gui/"+uid+"/"+serveHTTPLabel)
		if !serveHTTPPortFree(port) {
			return fmt.Errorf("port %d is already in use — free it (another process is bound to 127.0.0.1:%d) or set MORA_PORT, then retry", port, port)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		plistPath := filepath.Join(dir, serveHTTPLabel+".plist")
		if err := atomicWrite(plistPath, []byte(serveHTTPPlist(cfg, exe)), 0o644); err != nil {
			return err
		}
		if out, err := runScheduleCommand("launchctl", "bootstrap", "gui/"+uid, plistPath); err != nil {
			return fmt.Errorf("launchctl bootstrap %s: %w: %s", serveHTTPLabel, err, strings.TrimSpace(string(out)))
		}
		okf(stdout, "installed + started launchd daemon %s (http://127.0.0.1:%d/, auto-restarts, starts at login)", serveHTTPLabel, port)
		return nil

	case "windows":
		// A logon task — schtasks has no daemon/KeepAlive semantics, so this
		// starts serve-http at each login but does NOT auto-restart on crash.
		args := []string{"/Create", "/TN", `Mora\serve-http`, "/SC", "ONLOGON", "/TR", `"` + exe + `" serve http`, "/F"}
		if out, err := runScheduleCommand("schtasks", args...); err != nil {
			return fmt.Errorf("schtasks create Mora\\serve-http: %w: %s", err, strings.TrimSpace(string(out)))
		}
		fmt.Fprintf(stdout, "installed Windows logon task Mora\\serve-http (starts at login; no crash auto-restart — for that use a real service). Start now: mora serve http\n")
		return nil

	default:
		// Linux/WSL: write the systemd --user unit and print the enable steps.
		// Enabling (and lingering, to survive logout) is left to the user because
		// it needs an active user bus this process may not have.
		unitPath, err := systemdUserUnitPath()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
			return err
		}
		if err := atomicWrite(unitPath, []byte(serveHTTPSystemdUnit(cfg, exe)), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wrote systemd user unit %s\nEnable + start it:\n  systemctl --user daemon-reload\n  systemctl --user enable --now mora-serve-http.service\nTo keep it running after logout:\n  loginctl enable-linger %q\n", unitPath, os.Getenv("USER"))
		return nil
	}
}

func uninstallServeHTTP(cfg Config, stdout io.Writer) error {
	switch runtimeGOOS() {
	case "darwin":
		dir, err := launchAgentsDir()
		if err != nil {
			return err
		}
		uid := strconv.Itoa(os.Getuid())
		_, _ = runScheduleCommand("launchctl", "bootout", "gui/"+uid+"/"+serveHTTPLabel)
		if err := os.Remove(filepath.Join(dir, serveHTTPLabel+".plist")); err != nil && !os.IsNotExist(err) {
			return err
		}
		okf(stdout, "uninstalled launchd daemon %s", serveHTTPLabel)
		return nil

	case "windows":
		if out, err := runScheduleCommand("schtasks", "/Delete", "/TN", `Mora\serve-http`, "/F"); err != nil {
			return fmt.Errorf("schtasks delete Mora\\serve-http: %w: %s", err, strings.TrimSpace(string(out)))
		}
		okf(stdout, "uninstalled Windows logon task Mora\\serve-http")
		return nil

	default:
		unitPath, err := systemdUserUnitPath()
		if err != nil {
			return err
		}
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
func statusServeHTTP(cfg Config, stdout io.Writer) error {
	port := serveHTTPServicePort()

	installed := false
	switch runtimeGOOS() {
	case "darwin":
		if dir, err := launchAgentsDir(); err == nil {
			installed = fileExists(filepath.Join(dir, serveHTTPLabel+".plist"))
		}
	case "windows":
		_, err := runScheduleCommand("schtasks", "/Query", "/TN", `Mora\serve-http`)
		installed = err == nil
	default:
		if unitPath, err := systemdUserUnitPath(); err == nil {
			installed = fileExists(unitPath)
		}
	}

	listening := serveHTTPHealthy(port)
	fmt.Fprintf(stdout, "service installed: %v\nlistening on 127.0.0.1:%d: %v\n", installed, port, listening)
	return nil
}

// serveHTTPHealthy probes GET /healthz on the loopback port with a short timeout.
func serveHTTPHealthy(port int) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}
