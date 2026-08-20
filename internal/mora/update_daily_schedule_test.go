package mora

import (
	"strings"
	"testing"
)

func TestUpdateDailyScheduleContract(t *testing.T) {
	if got := scheduleCommands["update-daily"]; got != "upgrade --scheduled-check" {
		t.Fatalf("update-daily command = %q", got)
	}
	if got := launchdSchedule("update-daily"); !strings.Contains(got, "<integer>4</integer>") {
		t.Fatalf("update-daily launchd schedule = %q", got)
	}
	args := windowsScheduleCadenceArgs("update-daily")
	if !sameStrings(args, []string{"/SC", "DAILY", "/ST", "04:00"}) {
		t.Fatalf("update-daily Windows schedule = %v", args)
	}
}
