package mora

import (
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"path/filepath"
	"strings"
	"testing"
)

// stubScheduleRunner records every runScheduleCommand invocation and returns
// success, so install/uninstall never shell out to the real launchctl/schtasks.
func stubScheduleRunner(t *testing.T) *[][]string {
	t.Helper()
	orig := runScheduleCommand
	t.Cleanup(func() { runScheduleCommand = orig })
	var calls [][]string
	runScheduleCommand = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return nil, nil
	}
	return &calls
}

func sawCall(calls [][]string, name, sub string) bool {
	for _, c := range calls {
		if len(c) >= 2 && c[0] == name && c[1] == sub {
			return true
		}
	}
	return false
}

func stubGOOS(t *testing.T, goos string) {
	t.Helper()
	orig := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = orig })
	runtimeGOOS = func() string { return goos }
}

func stubPortFree(t *testing.T, free bool) {
	t.Helper()
	orig := serveHTTPPortFree
	t.Cleanup(func() { serveHTTPPortFree = orig })
	serveHTTPPortFree = func(int) bool { return free }
}

func TestServeHTTPPlistRenders(t *testing.T) {
	t.Setenv("MORA_CONFIG_DIR", "/tmp/isolated-vault")
	t.Setenv("MORA_PORT", "")
	cfg := Config{StateDir: "/state"}
	p := serveHTTPPlist(cfg, "/usr/local/bin/mora")

	for _, want := range []string{
		"<key>Label</key><string>com.mora.serve-http</string>",
		"<string>/usr/local/bin/mora</string><string>serve</string><string>http</string>",
		"<key>RunAtLoad</key><true/>",
		"<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>",
		"<key>ThrottleInterval</key><integer>30</integer>",
		"<key>MORA_CONFIG_DIR</key><string>/tmp/isolated-vault</string>",
		filepath.Join("/state", "serve-http.out.log"),
	} {
		if !strings.Contains(p, want) {
			t.Errorf("plist missing %q\n---\n%s", want, p)
		}
	}
	// KeepAlive must NOT be the bare-true form that respawns even on clean exit.
	if strings.Contains(p, "<key>KeepAlive</key><true/>") {
		t.Error("KeepAlive must be {SuccessfulExit:false}, not bare true")
	}
}

func TestServeHTTPSystemdUnitRenders(t *testing.T) {
	t.Setenv("MORA_CONFIG_DIR", "/tmp/v")
	t.Setenv("MORA_PORT", "8899")
	cfg := Config{StateDir: "/state"}
	u := serveHTTPSystemdUnit(cfg, "/opt/mora")
	for _, want := range []string{
		"ExecStart=/opt/mora serve http",
		"Restart=on-failure",
		"Environment=MORA_CONFIG_DIR=/tmp/v",
		"Environment=MORA_PORT=8899",
		"WantedBy=default.target",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("systemd unit missing %q\n---\n%s", want, u)
		}
	}
}

func TestInstallServeHTTPDarwin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir() reads %USERPROFILE% on Windows, not $HOME
	t.Setenv("MORA_PORT", "")
	stubGOOS(t, "darwin")
	stubPortFree(t, true)
	calls := stubScheduleRunner(t)

	var out strings.Builder
	if err := installServeHTTP(Config{StateDir: t.TempDir()}, &out); err != nil {
		t.Fatalf("install: %v", err)
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", "com.mora.serve-http.plist")
	if !genericutil.FileExists(plist) {
		t.Fatalf("expected plist at %s", plist)
	}
	if !sawCall(*calls, "launchctl", "bootout") {
		t.Error("expected a launchctl bootout (stop any old instance) before bootstrap")
	}
	if !sawCall(*calls, "launchctl", "bootstrap") {
		t.Error("expected a launchctl bootstrap to start the daemon")
	}
}

func TestInstallServeHTTPDarwinPortBusyAborts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir() reads %USERPROFILE% on Windows, not $HOME
	stubGOOS(t, "darwin")
	stubPortFree(t, false) // something already bound the port
	stubScheduleRunner(t)

	var out strings.Builder
	err := installServeHTTP(Config{StateDir: t.TempDir()}, &out)
	if err == nil {
		t.Fatal("expected install to abort when the port is busy")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("want a port-busy error, got: %v", err)
	}
	if genericutil.FileExists(filepath.Join(home, "Library", "LaunchAgents", "com.mora.serve-http.plist")) {
		t.Error("no plist should be written when preflight fails")
	}
}

func TestInstallServeHTTPLinuxWritesUnit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir() reads %USERPROFILE% on Windows, not $HOME
	stubGOOS(t, "linux")

	var out strings.Builder
	if err := installServeHTTP(Config{StateDir: t.TempDir()}, &out); err != nil {
		t.Fatalf("install: %v", err)
	}
	unit := filepath.Join(home, ".config", "systemd", "user", "mora-serve-http.service")
	if !genericutil.FileExists(unit) {
		t.Fatalf("expected systemd unit at %s", unit)
	}
	if !strings.Contains(out.String(), "systemctl --user enable --now") {
		t.Errorf("expected enable instructions in output, got: %s", out.String())
	}
}

func TestUninstallServeHTTPDarwinRemovesPlist(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir() reads %USERPROFILE% on Windows, not $HOME
	stubGOOS(t, "darwin")
	stubPortFree(t, true)
	calls := stubScheduleRunner(t)

	var out strings.Builder
	if err := installServeHTTP(Config{StateDir: t.TempDir()}, &out); err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := uninstallServeHTTP(Config{StateDir: t.TempDir()}, &out); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if genericutil.FileExists(filepath.Join(home, "Library", "LaunchAgents", "com.mora.serve-http.plist")) {
		t.Error("plist should be removed after uninstall")
	}
	if !sawCall(*calls, "launchctl", "bootout") {
		t.Error("uninstall should bootout the daemon")
	}
}

func TestServeHTTPServiceRejectsUnknownSub(t *testing.T) {
	var out strings.Builder
	if err := serveHTTPService(Config{}, "bogus", &out); err == nil {
		t.Fatal("expected an error for an unknown subcommand")
	}
}
