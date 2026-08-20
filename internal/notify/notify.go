// Package notify owns best-effort local desktop notification rendering and dispatch.
package notify

import (
	"os"
	"os/exec"
	"strings"
)

// Runner invokes the local notification system binary.
type Runner func(args ...string) error

// Urgent is the optional deadline-rich notification payload.
type Urgent struct{ Subtitle, Body string }

// ShouldNotify applies the macOS and MORA_NO_NOTIFY gates.
func ShouldNotify(goos string) bool { return goos == "darwin" && os.Getenv("MORA_NO_NOTIFY") == "" }

// OSAScriptRunner starts the native macOS osascript command without waiting.
func OSAScriptRunner(args ...string) error { return exec.Command("osascript", args...).Start() }

// EscapeAppleScriptString sanitizes a value for an AppleScript string literal.
func EscapeAppleScriptString(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

// Brief posts a gated, escaped, best-effort brief notification.
func Brief(briefPath string, top *Urgent, run Runner, goos string) error {
	if !ShouldNotify(goos) {
		return nil
	}
	title, subtitle, body := "Mora", EscapeAppleScriptString(briefPath), "Daily brief ready"
	if top != nil {
		title = "Mora · ⚠ Urgent"
		subtitle = EscapeAppleScriptString(top.Subtitle)
		body = EscapeAppleScriptString(top.Body)
	}
	script := `display notification "` + body + `" with title "` + title + `" subtitle "` + subtitle + `"`
	_ = run("-e", script)
	return nil
}

// HealthAlarm posts a gated, escaped, best-effort health notification.
func HealthAlarm(banner string, run Runner, goos string) error {
	if !ShouldNotify(goos) {
		return nil
	}
	script := `display notification "` + EscapeAppleScriptString(banner) + `" with title "Mora · Health"`
	_ = run("-e", script)
	return nil
}
