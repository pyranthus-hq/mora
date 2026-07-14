package mora

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// notify.go — SC#3: a best-effort, GUI/desktop-gated native macOS notification
// pointing the user at a freshly persisted daily brief (DIGH-06).
//
// Design (D13-1, D13-2): the toast posts via `osascript` — a macOS SYSTEM binary
// invoked through os/exec, the EXACT shell-out-to-a-system-binary precedent in
// internal/google/oauth.go openBrowser (`exec.Command("open", url).Start()`). It
// is NOT a Go module dependency, so the no-new-deps invariant holds. A missing or
// erroring osascript degrades SILENTLY — a failed toast must never fail the brief.
//
// WHY NOT a TTY gate (load-bearing): the pulse-daily LaunchAgent redirects stdout
// to a log file (schedulePlistFor sets StandardOutPath in mora.go), so an
// isTTYWriter/isInteractive check is FALSE in exactly the scheduled run we DO want
// to notify from — yet a LaunchAgent runs in the user's GUI session, so osascript
// works. The gate is therefore: caller opt-in flag (owned by 13-03) ∧
// GOOS == "darwin" ∧ MORA_NO_NOTIFY unset. This file owns the GOOS + env gate only.

// shouldNotify reports whether a native notification may be posted. The goos arg
// is injectable (defaulting to runtime.GOOS on the real binary) so the gate is
// unit-testable off-darwin: a non-darwin goos is a silent no-op, never an error.
//
// MORA_NO_NOTIFY is the user-facing opt-out (mirroring banner.go's MORA_NO_BANNER /
// render.go's MORA_NO_COLOR convention): set it to any non-empty value to suppress
// the toast everywhere.
func shouldNotify(goos string) bool {
	return goos == "darwin" && os.Getenv("MORA_NO_NOTIFY") == ""
}

// notifyRunner is the injectable exec seam. Its production value (osascriptRunner)
// shells out to osascript fire-and-forget; tests inject a fake to capture the argv
// WITHOUT spawning a process or writing any bytes.
type notifyRunner func(args ...string) error

// osascriptRunner is the production runner: it invokes the macOS system binary
// `osascript` fire-and-forget (.Start(), no blocking wait), mirroring oauth.go
// openBrowser. osascript is NOT a Go dependency.
func osascriptRunner(args ...string) error {
	return exec.Command("osascript", args...).Start()
}

// escapeAppleScriptString sanitizes a value for safe interpolation into an
// AppleScript double-quoted string literal (T-13-05, script injection). It strips
// control / newline characters (which could inject AppleScript statements) and
// backslash-escapes `\` and `"` so the value cannot break out of the string. The
// brief path is the ONLY interpolated value and is internally derived, but it is
// escaped defensively.
func escapeAppleScriptString(s string) string {
	// Strip control chars (incl. newline/CR/tab) — they have no place in a path
	// subtitle and could otherwise terminate the script line / inject statements.
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	// Backslash first (so we don't double-escape the escape we add for quotes),
	// then the double-quote.
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

// urgentNote is the enriched-toast payload (issue #62 follow-on): the top Urgent-shelf
// item's one-line summary, so the user acts on a deadline without opening the brief.
// nil => the content-free "Daily brief ready" toast (no urgent item this run). It is
// on-device only — carried into osascript, never sent off the machine.
type urgentNote struct {
	subtitle string // the item's subject / title
	body     string // its deadline-anchored one-line ask
}

// notifyBrief posts a best-effort native macOS notification (SC#3, DIGH-06). It is
// gated by shouldNotify(goos): on any non-darwin OS, or when MORA_NO_NOTIFY is set, it
// is a SILENT no-op (returns nil, never an error) and the runner is never invoked.
//
// When top is nil it names the persisted brief at briefPath ("Daily brief ready" +
// path subtitle). When top is set (issue #62 follow-on) it leads with the top Urgent
// item — title "Mora · ⚠ Urgent", the item's subject as subtitle, and its
// deadline-anchored ask as the body — so the deadline is visible on the lock screen.
// Both the subtitle and body are run through escapeAppleScriptString (the item text is
// user-derived, so this is the script-injection boundary). Still on-device: osascript
// is a local system binary, no bytes leave the machine.
//
// run is injectable (production: osascriptRunner). If run errors the error is SWALLOWED
// and nil is returned: a failed toast must NEVER fail the brief (D13-1, T-13-06).
// notifyBrief writes NOTHING to any io.Writer (byte-clean invariant, T-13-07).
func notifyBrief(briefPath string, top *urgentNote, run notifyRunner, goos string) error {
	if !shouldNotify(goos) {
		return nil // silent no-op: non-darwin or opted out — never an error
	}
	title, subtitle, body := "Mora", escapeAppleScriptString(briefPath), "Daily brief ready"
	if top != nil {
		title = "Mora · ⚠ Urgent"
		subtitle = escapeAppleScriptString(top.subtitle)
		body = escapeAppleScriptString(top.body)
	}
	script := `display notification "` + body + `" with title "` + title + `" subtitle "` + subtitle + `"`
	// Best-effort: swallow any runner error — a missing/failing osascript must not
	// abort the brief.
	_ = run("-e", script)
	return nil
}

// runtimeGOOS is the production OS value; the seam exists so the real binary reads
// runtime.GOOS while tests inject an arbitrary goos into OS-gated branches.
var runtimeGOOS = func() string { return runtime.GOOS }

// notifyHealthAlarm posts a best-effort native toast naming the health banner
// (HEALTH-02 delivery, `doctor --pulse`). Gated identically to notifyBrief —
// shouldNotify(goos): a silent no-op on non-darwin or when MORA_NO_NOTIFY is
// set, the runner never invoked. A failed/missing osascript is swallowed
// (D13-1): a toast failure must never fail the pulse check itself. The banner
// text is user/error-derived (carries LastError), so it is escaped exactly
// like notifyBrief's urgent-note text (T-13-05, script injection).
func notifyHealthAlarm(banner string, run notifyRunner, goos string) error {
	if !shouldNotify(goos) {
		return nil
	}
	script := `display notification "` + escapeAppleScriptString(banner) + `" with title "Mora · Health"`
	_ = run("-e", script)
	return nil
}

// notifyBriefDefault is the production entry point the integration caller (13-03)
// invokes after persisting a brief: it wires the real seams (osascriptRunner +
// runtime.GOOS) into notifyBrief. It inherits notifyBrief's best-effort,
// silent-no-op, byte-clean contract — it can never fail or contaminate the brief.
func notifyBriefDefault(briefPath string, top *urgentNote) error {
	return notifyBrief(briefPath, top, osascriptRunner, runtimeGOOS())
}
