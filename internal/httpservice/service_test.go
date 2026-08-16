package httpservice

import (
	"fmt"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type Config struct{ StateDir string }

var runtimeGOOS = func() string { return "linux" }
var runScheduleCommand = func(string, ...string) ([]byte, error) { return nil, nil }
var serveHTTPPortFree = func(int) bool { return true }

func environment() map[string]string {
	m := map[string]string{}
	for _, k := range []string{"MORA_CONFIG_DIR", "MORA_VAULT", "MORA_PORT"} {
		if v := os.Getenv(k); v != "" {
			m[k] = v
		}
	}
	return m
}
func port() int {
	if n, err := strconv.Atoi(os.Getenv("MORA_PORT")); err == nil && n > 0 {
		return n
	}
	return 8787
}
func options(cfg Config, out io.Writer, exe bool) Options {
	home, _ := os.UserHomeDir()
	path := ""
	if exe {
		path, _ = os.Executable()
	}
	return Options{StateDir: cfg.StateDir, GOOS: runtimeGOOS(), Home: home, Executable: path, User: os.Getenv("USER"), UID: os.Getuid(), Port: port(), Env: environment(), Stdout: out, RunCommand: runScheduleCommand, PortFree: serveHTTPPortFree, Healthy: func(int) bool { return false }}
}
func serveHTTPPlist(cfg Config, exe string) string { return Plist(cfg.StateDir, exe, environment()) }
func serveHTTPSystemdUnit(cfg Config, exe string) string {
	return SystemdUnit(cfg.StateDir, exe, environment())
}
func installServeHTTP(cfg Config, out io.Writer) error   { return Install(options(cfg, out, true)) }
func uninstallServeHTTP(cfg Config, out io.Writer) error { return Uninstall(options(cfg, out, false)) }
func serveHTTPService(cfg Config, sub string, out io.Writer) error {
	switch sub {
	case "install":
		return installServeHTTP(cfg, out)
	case "uninstall":
		return uninstallServeHTTP(cfg, out)
	case "status":
		return Status(options(cfg, out, false))
	default:
		return fmt.Errorf("usage: mora serve http install|uninstall|status")
	}
}

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

func TestWindowsInstallUninstallAndStatus(t *testing.T) {
	var calls [][]string
	run := func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return nil, nil
	}
	var out strings.Builder
	o := Options{GOOS: "windows", Executable: `C:\mora.exe`, Port: 8787, Stdout: &out, RunCommand: run, PortFree: func(int) bool { return true }, Healthy: func(int) bool { return true }}
	if err := Install(o); err != nil {
		t.Fatal(err)
	}
	if err := Status(o); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	if !sawCall(calls, "schtasks", "/Create") || !sawCall(calls, "schtasks", "/Delete") {
		t.Fatalf("calls=%v", calls)
	}
	if !strings.Contains(out.String(), "service installed: true") {
		t.Fatalf("out=%q", out.String())
	}
}
func TestLinuxUninstallAndDarwinStatus(t *testing.T) {
	home := t.TempDir()
	unit := systemdUserUnitPath(home)
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	o := Options{GOOS: "linux", Home: home, Port: 8787, Stdout: &out, Healthy: func(int) bool { return false }, RunCommand: func(string, ...string) ([]byte, error) { return nil, nil }}
	if err := Uninstall(o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Fatalf("unit remains: %v", err)
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", Label+".plist")
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, []byte("plist"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	o.GOOS = "darwin"
	o.Healthy = func(int) bool { return true }
	if err := Status(o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "service installed: true") || !strings.Contains(out.String(), "true") {
		t.Fatalf("status=%q", out.String())
	}
}

func TestHealthyRequiresHTTP200(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
	defer srv.Close()
	i := strings.LastIndex(srv.URL, ":")
	port, err := strconv.Atoi(srv.URL[i+1:])
	if err != nil {
		t.Fatal(err)
	}
	if !Healthy(port) {
		t.Fatal("200 health probe failed")
	}
	status = http.StatusServiceUnavailable
	if Healthy(port) {
		t.Fatal("non-200 health accepted")
	}
	if Healthy(1) {
		t.Fatal("closed port accepted")
	}
}
