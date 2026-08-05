package mora

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestDarwinScheduleInstallBootstrapsJob locks the install→running contract:
// writing a plist into ~/Library/LaunchAgents does NOT load it into launchd, so
// an install that stops there is silently inert until the next login (the exact
// failure mode that left the daily-brief automation dead for a week). Install
// must bootout any previously-loaded copy (so a reinstall picks up the NEW
// plist) and then bootstrap the fresh plist into the gui domain — mirroring
// installServeHTTP.
func TestDarwinScheduleInstallBootstrapsJob(t *testing.T) {
	withTempHome(t)
	withRuntimeGOOS(t, "darwin")
	calls := withScheduleRunner(t, nil)

	var out bytes.Buffer
	if err := installSchedule(&out, Config{StateDir: t.TempDir()}, "ingest-hourly"); err != nil {
		t.Fatalf("installSchedule: %v", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	label := "com.mora.ingest-hourly"
	plistPath := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("plist not written: %v", err)
	}

	if len(*calls) != 2 {
		t.Fatalf("launchctl calls=%d %#v, want 2 (bootout then bootstrap)", len(*calls), *calls)
	}
	uid := strconv.Itoa(os.Getuid())
	bootout := (*calls)[0]
	wantTarget := "gui/" + uid + "/" + label
	if bootout.name != "launchctl" || len(bootout.args) != 2 || bootout.args[0] != "bootout" || bootout.args[1] != wantTarget {
		t.Fatalf("first call=%s %#v, want launchctl bootout %s", bootout.name, bootout.args, wantTarget)
	}
	bootstrap := (*calls)[1]
	if bootstrap.name != "launchctl" || len(bootstrap.args) != 3 ||
		bootstrap.args[0] != "bootstrap" || bootstrap.args[1] != "gui/"+uid || bootstrap.args[2] != plistPath {
		t.Fatalf("second call=%s %#v, want launchctl bootstrap gui/%s %s", bootstrap.name, bootstrap.args, uid, plistPath)
	}
	if !strings.Contains(out.String(), "installed + loaded launchd job "+label) {
		t.Fatalf("output should report the job as installed AND loaded, got:\n%s", out.String())
	}
}

func TestDarwinAppScheduleLaunchesBundleThroughLaunchServices(t *testing.T) {
	withRuntimeGOOS(t, "darwin")
	oldExecutable := scheduleExecutable
	scheduleExecutable = func() (string, error) {
		return "/Users/test/My Applications/Mora.app/Contents/MacOS/mora", nil
	}
	t.Cleanup(func() { scheduleExecutable = oldExecutable })

	plist, ok := schedulePlistFor(Config{StateDir: "/Users/test/state"}, "ingest-hourly")
	if !ok {
		t.Fatal("schedulePlistFor rejected ingest-hourly")
	}
	for _, want := range []string{
		"<string>/usr/bin/open</string>",
		"<string>-n</string><string>-W</string>",
		"<string>-a</string><string>/Users/test/My Applications/Mora.app</string>",
		"<string>--args</string><string>ingest</string><string>run</string><string>--all</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("app schedule missing %q:\n%s", want, plist)
		}
	}
	if strings.Contains(plist, "<string>/Users/test/My Applications/Mora.app/Contents/MacOS/mora</string>") {
		t.Fatalf("app schedule directly execs the nested CLI instead of the FDA-granted bundle:\n%s", plist)
	}
}

func TestDarwinAppScheduleEscapesXMLPaths(t *testing.T) {
	withRuntimeGOOS(t, "darwin")
	oldExecutable := scheduleExecutable
	scheduleExecutable = func() (string, error) {
		return "/Users/test/Mora & Memory/Mora.app/Contents/MacOS/mora", nil
	}
	t.Cleanup(func() { scheduleExecutable = oldExecutable })

	plist, ok := schedulePlistFor(Config{StateDir: "/Users/test/State & Logs"}, "ingest-hourly")
	if !ok {
		t.Fatal("schedulePlistFor rejected ingest-hourly")
	}
	for _, want := range []string{
		"<string>/Users/test/Mora &amp; Memory/Mora.app</string>",
		"<string>/Users/test/State &amp; Logs/ingest-hourly.out.log</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("app schedule missing escaped XML value %q:\n%s", want, plist)
		}
	}
}

func TestDarwinStandaloneScheduleStillExecsBinaryDirectly(t *testing.T) {
	withRuntimeGOOS(t, "darwin")
	oldExecutable := scheduleExecutable
	scheduleExecutable = func() (string, error) { return "/Users/test/.local/bin/mora", nil }
	t.Cleanup(func() { scheduleExecutable = oldExecutable })

	plist, ok := schedulePlistFor(Config{StateDir: "/Users/test/state"}, "ingest-hourly")
	if !ok {
		t.Fatal("schedulePlistFor rejected ingest-hourly")
	}
	if !strings.Contains(plist, "<string>/Users/test/.local/bin/mora</string>") || strings.Contains(plist, "<string>/usr/bin/open</string>") {
		t.Fatalf("standalone schedule must retain direct execution:\n%s", plist)
	}
}

// TestDarwinScheduleInstallBootoutNotLoadedIsBenign: on a FIRST install nothing
// is loaded, so the pre-bootstrap bootout fails with "not loaded" — that must
// not abort the install (bootout is only there to make reinstalls pick up the
// new plist).
func TestDarwinScheduleInstallBootoutNotLoadedIsBenign(t *testing.T) {
	withTempHome(t)
	withRuntimeGOOS(t, "darwin")
	withScheduleRunner(t, func(name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "bootout" {
			return []byte("Boot-out failed: 5: Input/output error"), errors.New("exit status 5")
		}
		return nil, nil
	})

	var out bytes.Buffer
	if err := installSchedule(&out, Config{StateDir: t.TempDir()}, "ingest-hourly"); err != nil {
		t.Fatalf("installSchedule must tolerate bootout of a not-loaded job: %v", err)
	}
	if !strings.Contains(out.String(), "installed + loaded") {
		t.Fatalf("expected success output, got:\n%s", out.String())
	}
}

// TestDarwinScheduleInstallBootstrapFailureIsLoud: if bootstrap fails the
// install must NOT report success — the plist is on disk but the job will not
// run until the next login. The error must carry the exact manual activation
// command so the user can start it without re-logging.
func TestDarwinScheduleInstallBootstrapFailureIsLoud(t *testing.T) {
	withTempHome(t)
	withRuntimeGOOS(t, "darwin")
	withScheduleRunner(t, func(name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "bootstrap" {
			return []byte("Bootstrap failed: 125: Domain does not support specified action"), errors.New("exit status 125")
		}
		return nil, nil
	})

	var out bytes.Buffer
	err := installSchedule(&out, Config{StateDir: t.TempDir()}, "ingest-hourly")
	if err == nil {
		t.Fatal("expected a non-nil error when launchctl bootstrap fails")
	}
	home, herr := os.UserHomeDir()
	if herr != nil {
		t.Fatal(herr)
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "com.mora.ingest-hourly.plist")
	uid := strconv.Itoa(os.Getuid())
	wantCmd := "launchctl bootstrap gui/" + uid + " " + plistPath
	if !strings.Contains(err.Error(), wantCmd) {
		t.Fatalf("error must include the manual activation command %q, got: %v", wantCmd, err)
	}
	if !strings.Contains(err.Error(), "exit status 125") {
		t.Fatalf("error must wrap the underlying launchctl failure, got: %v", err)
	}
	// The plist stays installed so the manual command (or next login) can start it.
	if _, statErr := os.Stat(plistPath); statErr != nil {
		t.Fatalf("plist should remain on disk after a bootstrap failure: %v", statErr)
	}
	if strings.Contains(out.String(), "installed + loaded") {
		t.Fatalf("must not claim the job started when bootstrap failed, got:\n%s", out.String())
	}
}
