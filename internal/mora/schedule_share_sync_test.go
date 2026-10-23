package mora

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func shareSyncUnitPaths(cfg Config) (service, timer string) {
	dir := filepath.Join(cfg.HomeDir(), ".config", "systemd", "user")
	return filepath.Join(dir, "mora-share-sync.service"), filepath.Join(dir, "mora-share-sync.timer")
}

// shareSyncScheduleConfig mirrors scheduleInstallTestConfig, which lives in a
// darwin-only file; these tests run on every platform.
func shareSyncScheduleConfig(t *testing.T) Config {
	t.Helper()
	cfg := Config{StateDir: t.TempDir()}
	cfg.SetHomeDir(mustConfig(t).HomeDir())
	return cfg
}

func callLine(c scheduleCall) string { return c.name + " " + strings.Join(c.args, " ") }

func TestShareSyncScheduleCommand(t *testing.T) {
	if got := scheduleCommands["share-sync"]; got != "share sync" {
		t.Fatalf("share-sync command = %q, want %q", got, "share sync")
	}
	if got := launchdSchedule("share-sync"); got != "<key>StartInterval</key><integer>900</integer>" {
		t.Fatalf("share-sync launchd schedule = %q, want a 15-minute StartInterval", got)
	}
	if args := strings.Join(windowsScheduleCadenceArgs("share-sync"), " "); args != "/SC MINUTE /MO 15" {
		t.Fatalf("share-sync Windows cadence = %q", args)
	}
}

// TestDarwinShareSyncPlist covers the launchd path: a 15-minute interval that
// runs at load, `mora share sync` as the program, logs in the state dir, and
// GIT_TERMINAL_PROMPT=0 so a missing git credential fails fast instead of
// hanging a launchd job on a prompt nobody can answer.
func TestDarwinShareSyncPlist(t *testing.T) {
	withRuntimeGOOS(t, "darwin")
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")
	t.Setenv("MORA_VAULT", "")
	t.Setenv("MORA_CONFIG_DIR", "/Users/x/scratch/mora")
	oldExecutable := scheduleExecutable
	scheduleExecutable = func() (string, error) { return "/usr/local/bin/mora", nil }
	t.Cleanup(func() { scheduleExecutable = oldExecutable })

	plist := schedulePlist(t, Config{StateDir: "/Users/x/state"}, "share-sync")
	for _, want := range []string{
		"<key>Label</key><string>com.mora.share-sync</string>",
		"<array><string>/usr/local/bin/mora</string><string>share</string><string>sync</string></array>",
		"<key>RunAtLoad</key><true/>",
		"<key>StartInterval</key><integer>900</integer>",
		"<key>MORA_CONFIG_DIR</key><string>/Users/x/scratch/mora</string>",
		"<key>GIT_TERMINAL_PROMPT</key><string>0</string>",
		"<key>StandardOutPath</key><string>/Users/x/state/share-sync.out.log</string>",
		"<key>StandardErrorPath</key><string>/Users/x/state/share-sync.err.log</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("share-sync plist missing %q:\n%s", want, plist)
		}
	}
	// Other jobs keep their exact environment: GIT_TERMINAL_PROMPT is share-sync only.
	if other := schedulePlist(t, Config{StateDir: "/Users/x/state"}, "ingest-hourly"); strings.Contains(other, "GIT_TERMINAL_PROMPT") {
		t.Fatalf("GIT_TERMINAL_PROMPT leaked into another job's plist:\n%s", other)
	}
}

func TestDarwinShareSyncInstallBootstrapsAndUninstallBootsOut(t *testing.T) {
	withTempHome(t)
	withRuntimeGOOS(t, "darwin")
	calls := withScheduleRunner(t, nil)
	cfg := shareSyncScheduleConfig(t)

	var out bytes.Buffer
	if err := installSchedule(&out, cfg, "share-sync"); err != nil {
		t.Fatalf("install: %v", err)
	}
	plistPath := filepath.Join(cfg.HomeDir(), "Library", "LaunchAgents", "com.mora.share-sync.plist")
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("plist not written: %v", err)
	}
	uid := strconv.Itoa(os.Getuid())
	if len(*calls) != 2 || callLine((*calls)[1]) != "launchctl bootstrap gui/"+uid+" "+plistPath {
		t.Fatalf("launchctl calls = %#v", *calls)
	}
	if err := uninstallSchedule(&out, cfg, "share-sync"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Fatalf("plist survived uninstall: %v", err)
	}
	if got := callLine((*calls)[2]); got != "launchctl bootout gui/"+uid+"/com.mora.share-sync" {
		t.Fatalf("uninstall call = %q", got)
	}
}

// TestLinuxShareSyncInstallWritesUnitsAndEnablesTimer is the Linux contract: a
// systemd --user service + timer pair is written, the manager reloads, and the
// timer is enabled AND started in the same step, so the job is live without a
// re-login (the launchd lesson: a written-but-unloaded job is silently dead).
func TestLinuxShareSyncInstallWritesUnitsAndEnablesTimer(t *testing.T) {
	withTempHome(t)
	withRuntimeGOOS(t, "linux")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/test")
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")
	t.Setenv("MORA_VAULT", "/srv/my vault")
	t.Setenv("MORA_CONFIG_DIR", "/home/x/.config/mora-scratch")
	oldExecutable := scheduleExecutable
	scheduleExecutable = func() (string, error) { return "/home/x/.local/bin/mora", nil }
	t.Cleanup(func() { scheduleExecutable = oldExecutable })
	calls := withScheduleRunner(t, func(name string, args ...string) ([]byte, error) {
		if name == "loginctl" {
			return []byte("yes\n"), nil
		}
		return nil, nil
	})
	cfg := shareSyncScheduleConfig(t)

	var out bytes.Buffer
	if err := installSchedule(&out, cfg, "share-sync"); err != nil {
		t.Fatalf("install: %v\n%s", err, out.String())
	}
	servicePath, timerPath := shareSyncUnitPaths(cfg)
	service, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatalf("service unit not written: %v", err)
	}
	timer, err := os.ReadFile(timerPath)
	if err != nil {
		t.Fatalf("timer unit not written: %v", err)
	}
	for _, want := range []string{
		"Type=oneshot\n",
		`ExecStart="/home/x/.local/bin/mora" share sync` + "\n",
		`Environment="MORA_CONFIG_DIR=/home/x/.config/mora-scratch"` + "\n",
		`Environment="MORA_VAULT=/srv/my vault"` + "\n",
		`Environment="GIT_TERMINAL_PROMPT=0"` + "\n",
		"StandardOutput=append:" + filepath.Join(cfg.StateDir, "share-sync.out.log") + "\n",
		"StandardError=append:" + filepath.Join(cfg.StateDir, "share-sync.err.log") + "\n",
	} {
		if !strings.Contains(string(service), want) {
			t.Fatalf("service unit missing %q:\n%s", want, service)
		}
	}
	for _, want := range []string{
		"OnActiveSec=1min\n",
		"OnUnitActiveSec=15min\n",
		"Unit=mora-share-sync.service\n",
		"WantedBy=timers.target\n",
	} {
		if !strings.Contains(string(timer), want) {
			t.Fatalf("timer unit missing %q:\n%s", want, timer)
		}
	}
	wantCalls := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable --now mora-share-sync.timer",
		"loginctl show-user " + strconv.Itoa(os.Getuid()) + " --property=Linger --value",
	}
	if len(*calls) != len(wantCalls) {
		t.Fatalf("calls = %#v, want %v", *calls, wantCalls)
	}
	for i, c := range *calls {
		if callLine(c) != wantCalls[i] {
			t.Fatalf("call %d = %q, want %q", i, callLine(c), wantCalls[i])
		}
	}
	if !strings.Contains(out.String(), "installed + started systemd user timer mora-share-sync.timer") {
		t.Fatalf("install output:\n%s", out.String())
	}
	if strings.Contains(out.String(), "enable-linger") {
		t.Fatalf("lingering user must not get the linger warning:\n%s", out.String())
	}
}

// TestLinuxShareSyncInstallWarnsWithoutLinger: on a headless box (devbox) the
// user manager stops when the last session ends unless lingering is on, and the
// timer stops with it. Install must say so, with the fix, but not change it.
func TestLinuxShareSyncInstallWarnsWithoutLinger(t *testing.T) {
	withTempHome(t)
	withRuntimeGOOS(t, "linux")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/test")
	t.Setenv("USER", "adit")
	calls := withScheduleRunner(t, func(name string, args ...string) ([]byte, error) {
		if name == "loginctl" {
			return []byte("no\n"), nil
		}
		return nil, nil
	})
	var out bytes.Buffer
	if err := installSchedule(&out, shareSyncScheduleConfig(t), "share-sync"); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(out.String(), "loginctl enable-linger adit") {
		t.Fatalf("missing linger warning:\n%s", out.String())
	}
	for _, c := range *calls {
		if c.name == "loginctl" && len(c.args) > 0 && c.args[0] == "enable-linger" {
			t.Fatalf("install must never enable lingering itself: %#v", *calls)
		}
	}
}

// TestLinuxShareSyncInstallFailsLoudlyWithoutUserBus: when systemctl --user
// cannot reach a user manager, the units are on disk but nothing runs. That is
// an error with the exact commands to finish, never a green "installed".
func TestLinuxShareSyncInstallFailsLoudlyWithoutUserBus(t *testing.T) {
	withTempHome(t)
	withRuntimeGOOS(t, "linux")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/test")
	withScheduleRunner(t, func(name string, args ...string) ([]byte, error) {
		if name == "systemctl" {
			return []byte("Failed to connect to user scope bus via local transport: Connection refused"), errors.New("exit status 1")
		}
		return nil, nil
	})
	cfg := shareSyncScheduleConfig(t)
	var out bytes.Buffer
	err := installSchedule(&out, cfg, "share-sync")
	if err == nil {
		t.Fatalf("install without a user bus must fail; output:\n%s", out.String())
	}
	for _, want := range []string{"Connection refused", "systemctl --user enable --now mora-share-sync.timer"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should contain %q, got:\n%v", want, err)
		}
	}
	if strings.Contains(out.String(), "installed + started") {
		t.Fatalf("must not claim success:\n%s", out.String())
	}
}

func TestLinuxShareSyncUninstallDisablesAndRemovesUnits(t *testing.T) {
	withTempHome(t)
	withRuntimeGOOS(t, "linux")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/test")
	calls := withScheduleRunner(t, func(name string, args ...string) ([]byte, error) {
		if name == "loginctl" {
			return []byte("yes\n"), nil
		}
		return nil, nil
	})
	cfg := shareSyncScheduleConfig(t)
	var out bytes.Buffer
	if err := installSchedule(&out, cfg, "share-sync"); err != nil {
		t.Fatalf("install: %v", err)
	}
	*calls = (*calls)[:0]
	if err := uninstallSchedule(&out, cfg, "share-sync"); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	servicePath, timerPath := shareSyncUnitPaths(cfg)
	for _, p := range []string{servicePath, timerPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s survived uninstall: %v", p, err)
		}
	}
	wantCalls := []string{
		"systemctl --user disable --now mora-share-sync.timer",
		"systemctl --user daemon-reload",
	}
	if len(*calls) != len(wantCalls) {
		t.Fatalf("calls = %#v, want %v", *calls, wantCalls)
	}
	for i, c := range *calls {
		if callLine(c) != wantCalls[i] {
			t.Fatalf("call %d = %q, want %q", i, callLine(c), wantCalls[i])
		}
	}
	if !strings.Contains(out.String(), "uninstalled systemd user timer mora-share-sync.timer") {
		t.Fatalf("uninstall output:\n%s", out.String())
	}
}

// TestLinuxOtherJobsStillPrintCronLine pins scope: only share-sync installs a
// systemd timer. The other jobs keep their documented Linux behaviour.
func TestLinuxOtherJobsStillPrintCronLine(t *testing.T) {
	withTempHome(t)
	withRuntimeGOOS(t, "linux")
	calls := withScheduleRunner(t, nil)
	var out bytes.Buffer
	if err := installSchedule(&out, shareSyncScheduleConfig(t), "ingest-hourly"); err != nil {
		t.Fatalf("install: %v", err)
	}
	if len(*calls) != 0 || !strings.Contains(out.String(), "cron line") {
		t.Fatalf("calls=%#v out=%s", *calls, out.String())
	}
}

func TestLinuxScheduleListReportsInstalledShareSync(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	withRuntimeGOOS(t, "linux")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/test")
	withScheduleRunner(t, func(name string, args ...string) ([]byte, error) {
		if name == "loginctl" {
			return []byte("yes\n"), nil
		}
		return nil, nil
	})
	cfg := mustConfig(t)
	var out bytes.Buffer
	if err := installSchedule(&out, cfg, "share-sync"); err != nil {
		t.Fatalf("install: %v", err)
	}

	listed := run(t, "schedule", "list")
	if !strings.Contains(listed, "mora-share-sync.timer") {
		t.Fatalf("schedule list should show the installed timer:\n%s", listed)
	}
	var receipt struct {
		Entries []scheduleListEntry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(run(t, "schedule", "list", "--json")), &receipt); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range receipt.Entries {
		if e.Name == "share-sync" {
			found = true
			if !e.Installed || e.Cadence != "every 15 minutes" {
				t.Fatalf("share-sync entry = %#v", e)
			}
		}
	}
	if !found {
		t.Fatalf("share-sync missing from schedule list --json: %#v", receipt.Entries)
	}
}
