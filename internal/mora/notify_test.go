package mora

import "testing"

func TestNotifyBriefDefault_OptOut_SilentNoOp(t *testing.T) {
	// notifyBriefDefault is the production entry point 13-03 wires (real
	// osascriptRunner + runtime.GOOS). Under MORA_NO_NOTIFY it is a guaranteed
	// silent no-op on EVERY platform — so this exercises the real wiring without
	// ever spawning osascript or firing a toast, and asserts the best-effort
	// contract (returns nil, never an error).
	t.Setenv("MORA_NO_NOTIFY", "1")
	if err := notifyBriefDefault("briefs/2026-06-08-brief.md", nil); err != nil {
		t.Fatalf("notifyBriefDefault (opted out) = %v, want nil", err)
	}
}
