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

func resolveReal(path string) (string, error) {
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	// The path may not exist yet (e.g. a vault directory that has not been
	// created). Walk up to the nearest existing ancestor, resolve it, then
	// re-append the missing components -- without this, /var/foo (where
	// /var -> /private/var on macOS) and /private/var/foo compare as different
	// paths, and a deep not-yet-created path under a symlinked prefix
	// (link/missing/state where link -> vault) escapes overlap detection.
	// Climbing is allowed ONLY past missing components: any other failure
	// (e.g. permission denied on a symlinked ancestor) is returned so callers
	// fail closed instead of comparing a wrong lexical path.
	prefix := filepath.Clean(path)
	var missing []string
	for {
		parent := filepath.Dir(prefix)
		if parent == prefix {
			return filepath.Clean(path), nil
		}
		missing = append(missing, filepath.Base(prefix))
		prefix = parent
		real, err := filepath.EvalSymlinks(prefix)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				real = filepath.Join(real, missing[i])
			}
			return real, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
	}
}

// PathsDisjoint reports whether tokenDir resolves outside vault. An
// unresolvable path (other than not-yet-created) fails closed: the pair is
// reported as NOT disjoint so data-safety gates refuse rather than guess.
func PathsDisjoint(vault, tokenDir string) bool {
	rv, errV := resolveReal(vault)
	rt, errT := resolveReal(tokenDir)
	if errV != nil || errT != nil {
		return false
	}
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

// WhatsAppSeams supplies the platform and filesystem boundary for the readiness probe.
type WhatsAppSeams struct {
	GOOS          func() string
	DBPath        func() string
	Stat          func(string) (os.FileInfo, error)
	ProbeReadable func(string) (bool, error)
}

// PrintWhatsAppReadiness probes and prints the stable WhatsApp readiness
// checklist: macOS-only gate, ChatStorage.sqlite presence, then a REAL read
// probe (never os.Stat) as the Full Disk Access signal — the same contract as
// the iMessage block above.
func PrintWhatsAppReadiness(stdout io.Writer, seams WhatsAppSeams) bool {
	goos := seams.GOOS()
	if goos != "darwin" {
		fmt.Fprintln(stdout, "warn whatsapp_macos")
		fmt.Fprintf(stdout, "WhatsApp ingest only runs on macOS — skipping ChatStorage.sqlite checks on %s.\n", goos)
		return false
	}
	fmt.Fprintln(stdout, "ok   whatsapp_macos")
	path := seams.DBPath()
	if _, err := seams.Stat(path); err != nil {
		fmt.Fprintln(stdout, "warn whatsapp_chat_storage")
		fmt.Fprintln(stdout, "No WhatsApp database found. Install and open WhatsApp Desktop, then re-run `mora doctor`.")
		return false
	}
	fmt.Fprintln(stdout, "ok   whatsapp_chat_storage")
	readable, err := seams.ProbeReadable(path)
	if readable && err == nil {
		fmt.Fprintln(stdout, "ok   whatsapp_full_disk_access")
		fmt.Fprintln(stdout, "WhatsApp is ready to sync. Run `mora ingest run --source whatsapp`.")
		return true
	}
	fmt.Fprintln(stdout, "warn whatsapp_full_disk_access")
	fmt.Fprintln(stdout, "WhatsApp ChatStorage.sqlite exists but could not be read. Grant Full Disk Access to Mora.app, then re-run `mora doctor`.")
	if err != nil {
		fmt.Fprintf(stdout, "     %v\n", err)
	}
	return false
}
