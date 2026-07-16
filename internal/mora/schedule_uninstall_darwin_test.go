package mora

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestDarwinScheduleUninstallBootsOutLoadedJob(t *testing.T) {
	withTempHome(t)
	withRuntimeGOOS(t, "darwin")
	calls := withScheduleRunner(t, func(string, ...string) ([]byte, error) {
		return []byte("not loaded"), errors.New("not loaded")
	})
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	label := "com.mora.pulse-daily"
	plist := filepath.Join(home, "Library", "LaunchAgents", label+".plist")
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plist, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := uninstallSchedule(&out, Config{}, "pulse-daily"); err != nil {
		t.Fatalf("uninstallSchedule: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("launchctl calls=%d, want 1", len(*calls))
	}
	call := (*calls)[0]
	wantTarget := "gui/" + strconv.Itoa(os.Getuid()) + "/" + label
	if call.name != "launchctl" || len(call.args) != 2 || call.args[0] != "bootout" || call.args[1] != wantTarget {
		t.Fatalf("bootout call=%s %#v, want launchctl bootout %s", call.name, call.args, wantTarget)
	}
	if _, err := os.Stat(plist); !os.IsNotExist(err) {
		t.Fatalf("plist still exists after uninstall: %v", err)
	}
}
