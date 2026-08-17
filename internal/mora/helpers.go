package mora

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// ptr returns a pointer to b. Used to set Source.Enabled on freshly-constructed
// literals — leaving Enabled nil would grandfather to true on next load (D-11).
func ptr(b bool) *bool { return &b }

// isInteractive reports whether r is a real terminal (character device). It uses
// only the stdlib: in production stdin is *os.File (os.Stdin) and we check for
// ModeCharDevice; in tests/pipes stdin is a strings.Reader or a redirected file,
// so this returns false. This keeps interactive consent/menus from blocking on a
// non-TTY without adding a go-isatty dependency (deferred to Plan 04).
func isInteractive(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// truncateRunes returns s clipped to at most max bytes, never splitting a
// multi-byte UTF-8 rune (a raw s[:max] could leave an invalid trailing byte).
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
func emit(w io.Writer, v any, jsonOut bool) error {
	if jsonOut {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(w, string(b))
		return nil
	}
	sty := newStyler(w, jsonOut)
	switch x := v.(type) {
	case Memory:
		fmt.Fprintf(w, "%s\t%s\t%s\n", sty.dim(x.ID), sty.dim(x.Scope), ownedTitle(x))
	case []Memory:
		for _, m := range x {
			fmt.Fprintf(w, "%s\t%s\t%s\n", sty.dim(m.ID), sty.dim(m.Scope), ownedTitle(m))
		}
	case []catalogRow:
		for _, r := range x {
			// Off-path stays byte-identical ("enabled"/"disabled"); glyph + color
			// only appear on a real TTY.
			state := "disabled"
			if r.Enabled {
				state = "enabled"
			}
			if sty.on {
				if r.Enabled {
					state = sty.ok("● enabled")
				} else {
					state = sty.dim("○ disabled")
				}
			}
			fmt.Fprintf(w, "%s\t%s\t%s\n", r.Type, r.Name, state)
		}
	default:
		fmt.Fprintf(w, "%v\n", v)
	}
	return nil
}
func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

// isHelpFlag reports whether a subcommand arg is a help request. Used by subcommands
// (sync, search) that otherwise treat a leading flag as data and act on it.
func isHelpFlag(s string) bool {
	return s == "--help" || s == "-h" || s == "help"
}

// refuseDashLedPositional closes the bug class Plans 01-05 and 01-07 each found
// a fresh instance of: a dash-led argument landing in a POSITIONAL slot.
// `tasks add --json` created a task named "--json", `loop begin --json` started
// a run for a loop of that name, and `sources add --json` registered a live
// source typed and named "--json" — three independent discoveries of one
// defect, each found only because somebody executed that exact path.
//
// The call sites parse their arguments very differently (flag.FlagSet with
// flagsFirst, hand-rolled loops, positional-then-flags splits), so a shared
// PARSER would not fit. The CHECK is identical everywhere, so this is the
// shared piece. Phase 01-10's matrix should assert the CLASS is closed by
// driving every positional-taking path, not that N instances were patched.
func refuseDashLedPositional(command, slot, value string) error {
	if !strings.HasPrefix(value, "-") {
		return nil
	}
	return newCodedError(errCodeUsageMissingArgument, nil,
		"%s: %q is a flag, not the %s — pass the %s first, or use `--` to end flag interpretation",
		command, value, slot, slot)
}
