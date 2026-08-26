package mora

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestSchedulePlistCarriesGoogleCredsEnv locks the launchd-env contract: launchd
// jobs do NOT inherit the user's shell environment, so a BYO-creds setup
// (MORA_GOOGLE_CREDENTIALS in the profile) silently breaks every scheduled
// Google sync — the job hits the embedded DEV_PLACEHOLDER client and the vault
// goes stale while `mora sync` from a terminal still works. The fix: the plist
// builder snapshots MORA_GOOGLE_CREDENTIALS (a PATH, not a secret) into an
// EnvironmentVariables dict at install time, and omits the dict entirely when
// the var is unset (embedded real creds need no override).
func TestSchedulePlistCarriesGoogleCredsEnv(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	t.Setenv("MORA_GOOGLE_CREDENTIALS", "/Users/x/creds/google-client.json")
	plist := schedulePlist(t, cfg, "ingest-hourly")
	if !strings.Contains(plist, "<key>EnvironmentVariables</key>") {
		t.Fatalf("plist missing EnvironmentVariables dict when MORA_GOOGLE_CREDENTIALS is set:\n%s", plist)
	}
	if !strings.Contains(plist, "<key>MORA_GOOGLE_CREDENTIALS</key><string>/Users/x/creds/google-client.json</string>") {
		t.Fatalf("plist missing the snapshotted creds path:\n%s", plist)
	}

	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")
	plist = schedulePlist(t, cfg, "ingest-hourly")
	if strings.Contains(plist, "EnvironmentVariables") {
		t.Fatalf("plist must omit EnvironmentVariables when the var is unset:\n%s", plist)
	}
}

// TestSchedulePlistCarriesConfigDirEnv locks the same launchd-env contract for
// MORA_CONFIG_DIR: a re-rooted install (scratch/isolated config) that installs a
// schedule must NOT have its job silently run against the DEFAULT vault. launchd
// won't inherit the exported var, so the plist builder snapshots it (a PATH, not
// a secret) into the same EnvironmentVariables dict. (Codex phase-0 review P1.)
func TestSchedulePlistCarriesConfigDirEnv(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	t.Setenv("MORA_CONFIG_DIR", "/Users/x/scratch/mora")
	plist := schedulePlist(t, cfg, "ingest-hourly")
	if !strings.Contains(plist, "<key>EnvironmentVariables</key>") {
		t.Fatalf("plist missing EnvironmentVariables dict when MORA_CONFIG_DIR is set:\n%s", plist)
	}
	if !strings.Contains(plist, "<key>MORA_CONFIG_DIR</key><string>/Users/x/scratch/mora</string>") {
		t.Fatalf("plist missing the snapshotted config dir — a re-rooted install's scheduled job would run against the default vault:\n%s", plist)
	}
}

type scheduleCall struct {
	name string
	args []string
}

func withScheduleRunner(t *testing.T, fn scheduleCommandRunner) *[]scheduleCall {
	t.Helper()
	orig := runScheduleCommand
	calls := []scheduleCall{}
	runScheduleCommand = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, scheduleCall{name: name, args: append([]string(nil), args...)})
		if fn != nil {
			return fn(name, args...)
		}
		return nil, nil
	}
	t.Cleanup(func() { runScheduleCommand = orig })
	return &calls
}

func argAfter(args []string, flag string) (string, bool) {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1], true
		}
	}
	return "", false
}

func hasArgPair(args []string, flag, value string) bool {
	got, ok := argAfter(args, flag)
	return ok && got == value
}

func TestWindowsScheduleInstallUsesSchtasksAndConfigDir(t *testing.T) {
	withTempHome(t)
	withRuntimeGOOS(t, "windows")
	// Isolate the MORA_CONFIG_DIR-only case: clear any ambient BYO creds so the
	// /TR prefix assertion is deterministic regardless of the host environment.
	t.Setenv("MORA_GOOGLE_CREDENTIALS", "")
	t.Setenv("MORA_CONFIG_DIR", `C:\Users\Adit\AppData\Local\Mora`)
	calls := withScheduleRunner(t, nil)

	var out bytes.Buffer
	if err := installSchedule(&out, Config{}, "ingest-hourly"); err != nil {
		t.Fatalf("installSchedule windows: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("schedule runner calls = %d, want 1", len(*calls))
	}
	call := (*calls)[0]
	if call.name != "schtasks" {
		t.Fatalf("runner command = %q, want schtasks", call.name)
	}
	if len(call.args) == 0 || call.args[0] != "/Create" {
		t.Fatalf("schtasks args should start with /Create, got %#v", call.args)
	}
	if !hasArgPair(call.args, "/TN", `Mora\ingest-hourly`) {
		t.Fatalf("schtasks args missing Mora task name: %#v", call.args)
	}
	if !hasArgPair(call.args, "/SC", "HOURLY") || !hasArgPair(call.args, "/MO", "1") {
		t.Fatalf("ingest-hourly should map to hourly cadence, got %#v", call.args)
	}
	tr, ok := argAfter(call.args, "/TR")
	if !ok {
		t.Fatalf("schtasks args missing /TR: %#v", call.args)
	}
	if !strings.HasPrefix(tr, `cmd /c "set "MORA_CONFIG_DIR=C:\Users\Adit\AppData\Local\Mora"&& "`) {
		t.Fatalf("/TR did not carry MORA_CONFIG_DIR with cmd wrapper: %q", tr)
	}
	if !strings.Contains(tr, `" ingest run --all"`) {
		t.Fatalf("/TR missing scheduled mora args: %q", tr)
	}
	// cmd.exe does NOT unescape backslash-quote; a `\"` here fails to launch the
	// binary on every scheduled run (cmd's first/last-quote rule leaves a spurious
	// backslash on the exe path). The exe must be wrapped in PLAIN double quotes.
	if strings.Contains(tr, `\"`) {
		t.Fatalf("/TR must use PLAIN double quotes, not backslash-escaped: %q", tr)
	}
	if strings.Contains(out.String(), "cron line") {
		t.Fatalf("windows install must never print a Linux cron line; got:\n%s", out.String())
	}
}

// TestWindowsScheduleCarriesGoogleCredsEnv locks the schtasks-env contract for
// MORA_GOOGLE_CREDENTIALS — the Windows sibling of TestSchedulePlistCarriesGoogleCredsEnv.
// Scheduled tasks do not inherit the shell environment, so a BYO-creds setup
// whose var is only in the current session (not the persistent user env) would
// otherwise have every scheduled Google sync silently fall back to the embedded
// DEV_PLACEHOLDER client while terminal syncs keep working — a stale vault with
// no visible error. The /TR action must `set MORA_GOOGLE_CREDENTIALS` before the
// exe, exactly as the launchd plist snapshots it.
func TestWindowsScheduleCarriesGoogleCredsEnv(t *testing.T) {
	withTempHome(t)
	withRuntimeGOOS(t, "windows")
	t.Setenv("MORA_GOOGLE_CREDENTIALS", `C:\Users\Adit\creds\google-client.json`)
	calls := withScheduleRunner(t, nil)

	var out bytes.Buffer
	if err := installSchedule(&out, Config{}, "ingest-hourly"); err != nil {
		t.Fatalf("installSchedule windows: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("schedule runner calls = %d, want 1", len(*calls))
	}
	tr, ok := argAfter((*calls)[0].args, "/TR")
	if !ok {
		t.Fatalf("schtasks args missing /TR: %#v", (*calls)[0].args)
	}
	if !strings.Contains(tr, `set "MORA_GOOGLE_CREDENTIALS=C:\Users\Adit\creds\google-client.json"`) {
		t.Fatalf("windows scheduled task dropped MORA_GOOGLE_CREDENTIALS — BYO-creds scheduled Google sync would silently go stale: %q", tr)
	}
	if strings.Contains(tr, `\"`) {
		t.Fatalf("/TR must use PLAIN double quotes, not backslash-escaped: %q", tr)
	}
}

// TestWindowsScheduleQuotesEnvValueWithAmpersand guards #60 bug 1: a creds/config
// path containing a cmd.exe metacharacter (& is legal in Windows folder names)
// must be carried literally via the `set "VAR=value"` quoted idiom. Without the
// quotes cmd.exe parses the `&` as a command separator, truncating the `set` so
// the scheduled Google sync silently falls back to the embedded placeholder.
func TestWindowsScheduleQuotesEnvValueWithAmpersand(t *testing.T) {
	withTempHome(t)
	withRuntimeGOOS(t, "windows")
	t.Setenv("MORA_GOOGLE_CREDENTIALS", `C:\R&D\google-client.json`)
	calls := withScheduleRunner(t, nil)

	var out bytes.Buffer
	if err := installSchedule(&out, Config{}, "ingest-hourly"); err != nil {
		t.Fatalf("installSchedule windows: %v", err)
	}
	tr, ok := argAfter((*calls)[0].args, "/TR")
	if !ok {
		t.Fatalf("schtasks args missing /TR: %#v", (*calls)[0].args)
	}
	if !strings.Contains(tr, `set "MORA_GOOGLE_CREDENTIALS=C:\R&D\google-client.json"`) {
		t.Fatalf("env value containing `&` was not carried intact inside a quoted set — cmd.exe would split it: %q", tr)
	}
}

func TestWindowsScheduleCadenceMirrorsLaunchdCadence(t *testing.T) {
	tests := []struct {
		job  string
		want []string
	}{
		{"pulse-daily", []string{"/SC", "DAILY", "/ST", "08:00"}},
		{"backup-daily", []string{"/SC", "DAILY", "/ST", "02:00"}},
		{"git-daily", []string{"/SC", "DAILY", "/ST", "03:00"}},
		{"lint-weekly", []string{"/SC", "WEEKLY", "/D", "SUN", "/ST", "09:00"}},
		{"index-hourly", []string{"/SC", "HOURLY", "/MO", "1"}},
		{"ingest-hourly", []string{"/SC", "HOURLY", "/MO", "1"}},
	}
	for _, tt := range tests {
		subRun(t, tt.job, func(t *testing.T) {
			got := strings.Join(windowsScheduleCadenceArgs(tt.job), " ")
			for _, part := range tt.want {
				if !strings.Contains(got, part) {
					t.Fatalf("windows cadence for %s = %q, missing %q", tt.job, got, part)
				}
			}
		})
	}
}

func TestWindowsScheduleListQueriesKnownMoraTasks(t *testing.T) {
	withRuntimeGOOS(t, "windows")
	withScheduleRunner(t, func(name string, args ...string) ([]byte, error) {
		if hasArgPair(args, "/TN", `Mora\pulse-daily`) {
			return []byte("TaskName: Mora\\pulse-daily"), nil
		}
		return nil, errors.New("not found")
	})

	var out bytes.Buffer
	if err := listSchedules(&out, Config{}); err != nil {
		t.Fatalf("listSchedules windows: %v", err)
	}
	if strings.TrimSpace(out.String()) != `Mora\pulse-daily` {
		t.Fatalf("windows list output = %q, want Mora\\pulse-daily", out.String())
	}
}

func TestWindowsScheduleUninstallDeletesTask(t *testing.T) {
	withRuntimeGOOS(t, "windows")
	calls := withScheduleRunner(t, nil)

	var out bytes.Buffer
	if err := uninstallSchedule(&out, Config{}, "pulse-daily"); err != nil {
		t.Fatalf("uninstallSchedule windows: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("schedule runner calls = %d, want 1", len(*calls))
	}
	call := (*calls)[0]
	if call.name != "schtasks" || len(call.args) == 0 || call.args[0] != "/Delete" {
		t.Fatalf("delete command = %s %#v, want schtasks /Delete", call.name, call.args)
	}
	if !hasArgPair(call.args, "/TN", `Mora\pulse-daily`) || call.args[len(call.args)-1] != "/F" {
		t.Fatalf("delete args missing task name or /F: %#v", call.args)
	}
}
