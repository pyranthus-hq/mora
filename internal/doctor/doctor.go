// Package doctor owns reusable diagnostic probes and presentation helpers.
package doctor

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Check is one named health probe; critical failures gate strict mode.
type Check struct {
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Critical bool   `json:"critical"`
}

func resolveReal(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return filepath.Clean(path)
}

// PathsDisjoint reports whether tokenDir resolves outside vault.
func PathsDisjoint(vault, tokenDir string) bool {
	rv, rt := resolveReal(vault), resolveReal(tokenDir)
	return !strings.HasPrefix(rt+string(os.PathSeparator), rv+string(os.PathSeparator)) && rt != rv
}

// LooksSynced reports whether a path names a common cloud-sync location.
func LooksSynced(path string) bool {
	for _, marker := range []string{"com~apple~CloudDocs", "Dropbox", "Google Drive", "OneDrive", "Sync"} {
		if strings.Contains(path, marker) {
			return true
		}
	}
	return false
}

// FailSummary returns the deterministic strict-mode critical failure summary.
func FailSummary(checks []Check) string {
	var failed []string
	for _, check := range checks {
		if check.Critical && !check.OK {
			failed = append(failed, check.Name)
		}
	}
	return fmt.Sprintf("%d critical check(s) failed: %s", len(failed), strings.Join(failed, ", "))
}

// HumanizeAgo renders a non-negative age using doctor's stable minute/hour/day grammar.
func HumanizeAgo(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		n := int(d / time.Minute)
		return fmt.Sprintf("%d %s ago", n, plural(n, "minute"))
	case d < 24*time.Hour:
		n := int(d / time.Hour)
		return fmt.Sprintf("%d %s ago", n, plural(n, "hour"))
	default:
		n := int(d / (24 * time.Hour))
		return fmt.Sprintf("%d %s ago", n, plural(n, "day"))
	}
}
func plural(n int, word string) string {
	if n == 1 {
		return word
	}
	return word + "s"
}

// IMessageSeams supplies the platform and filesystem boundary for the readiness probe.
type IMessageSeams struct {
	GOOS          func() string
	ChatDBPath    func() string
	Stat          func(string) (os.FileInfo, error)
	ProbeReadable func(string) (bool, error)
}

// PrintIMessageReadiness probes and prints the stable iMessage readiness checklist.
func PrintIMessageReadiness(stdout io.Writer, setupVariant bool, seams IMessageSeams) bool {
	goos := seams.GOOS()
	if goos != "darwin" {
		fmt.Fprintln(stdout, "warn imessage_macos")
		fmt.Fprintf(stdout, "iMessage ingest only runs on macOS — skipping chat.db checks on %s.\n", goos)
		return false
	}
	fmt.Fprintln(stdout, "ok   imessage_macos")
	path := seams.ChatDBPath()
	exists := false
	if _, err := seams.Stat(path); err == nil {
		exists = true
	}
	if exists {
		fmt.Fprintln(stdout, "ok   imessage_chat_db")
	} else {
		fmt.Fprintln(stdout, "warn imessage_chat_db")
	}
	readable := false
	if exists {
		readable, _ = seams.ProbeReadable(path)
	}
	if readable {
		fmt.Fprintln(stdout, "ok   imessage_full_disk_access")
		fmt.Fprintln(stdout, "iMessage is ready to sync. Run `mora sync imessage`.")
		return true
	}
	fmt.Fprintln(stdout, "warn imessage_full_disk_access")
	fmt.Fprintln(stdout)
	if !exists {
		fmt.Fprintln(stdout, "No Messages database found at ~/Library/Messages/chat.db.")
		fmt.Fprintln(stdout, "Open the Messages app and sign in to iMessage, then re-run `mora doctor`.")
		return false
	}
	final := "  4. Re-run `mora doctor` to confirm."
	if setupVariant {
		final = "  4. Re-run `mora doctor` to confirm, then `mora sync imessage`."
	}
	fmt.Fprintln(stdout, "iMessage needs Full Disk Access to read your Messages database.")
	fmt.Fprintln(stdout, "chat.db exists but could not be read (permission denied) — Full Disk Access is not granted.")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "To grant it:")
	fmt.Fprintln(stdout, "  1. Open System Settings → Privacy & Security → Full Disk Access.")
	fmt.Fprintln(stdout, "  2. Click the + button and add ~/Applications/Mora.app (not Terminal, Claude, or your editor).")
	fmt.Fprintln(stdout, "     If Mora.app is already listed, toggle it OFF and back ON.")
	fmt.Fprintln(stdout, "  3. Fully quit and reopen Mora.app.")
	fmt.Fprintln(stdout, final)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Mora only ever READS the database — it never writes to or modifies your Messages.")
	return false
}
