package mora

import (
	"runtime"

	notifypkg "github.com/pyranthus-hq/mora/internal/notify"
)

type notifyRunner = notifypkg.Runner

type urgentNote struct{ subtitle, body string }

func escapeAppleScriptString(s string) string { return notifypkg.EscapeAppleScriptString(s) }
func shouldNotify(goos string) bool           { return notifypkg.ShouldNotify(goos) }
func osascriptRunner(args ...string) error    { return notifypkg.OSAScriptRunner(args...) }
func notifyBrief(briefPath string, top *urgentNote, run notifyRunner, goos string) error {
	var payload *notifypkg.Urgent
	if top != nil {
		payload = &notifypkg.Urgent{Subtitle: top.subtitle, Body: top.body}
	}
	return notifypkg.Brief(briefPath, payload, run, goos)
}
func notifyHealthAlarm(banner string, run notifyRunner, goos string) error {
	return notifypkg.HealthAlarm(banner, run, goos)
}

var runtimeGOOS = func() string { return runtime.GOOS }

func notifyBriefDefault(briefPath string, top *urgentNote) error {
	return notifyBrief(briefPath, top, osascriptRunner, runtimeGOOS())
}
