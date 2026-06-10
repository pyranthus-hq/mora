package mora

import (
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"
)

// colorEnabled is the single gate that decides whether human-facing ANSI styling
// may be written to w. It MUST be consulted before any styled output: it keeps
// escape codes out of pipes, file redirects, the --json path, and the MCP stdio
// transport (where stray ANSI would corrupt an agent's context or break JSON
// parsing). Resolve it once per command and thread the result down; never style
// when it returns false.
//
// It mirrors isInteractive (mora.go): production stdout is *os.File and we test
// ModeCharDevice; tests/pipes use a buffer or redirected file and fail the check.
func colorEnabled(w io.Writer, jsonOut bool) bool {
	if jsonOut {
		return false
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("MORA_NO_COLOR") != "" {
		return false
	}
	if t := os.Getenv("TERM"); t == "" || t == "dumb" {
		return false
	}
	return isTTYWriter(w)
}

// isTTYWriter reports whether w is a real terminal. It uses go-isatty (already a
// dependency) rather than an os.ModeCharDevice test, because ModeCharDevice is
// also true for /dev/null and other character devices — redirecting there would
// otherwise wrongly enable ANSI and violate the byte-clean invariant.
func isTTYWriter(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// --- semantic styling -------------------------------------------------------
//
// One lightweight visual language for every human-facing surface a real user
// touches: sync freshness, doctor checks, connector/list tables, the digest.
// Everything routes through the colorEnabled gate, so on a pipe / --json / the
// MCP transport every method returns its input unchanged (byte-clean).
//
// We deliberately do NOT use glamour for the digest: its default style wraps
// every space in its own ANSI span (heavy "boxed" look + large token cost), the
// opposite of what a quick briefing wants. Markdown-document rendering is held
// back for genuinely rich bodies (read/think) behind a hand-trimmed style.
//
// Colors use the 16-slot ANSI palette (1=red … 6=cyan) rather than hardcoded hex
// so they inherit the user's terminal theme instead of clashing with it.

type styler struct{ on bool }

func newStyler(w io.Writer, jsonOut bool) styler { return styler{on: colorEnabled(w, jsonOut)} }

var (
	styAccent = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true) // primary label
	styDim    = lipgloss.NewStyle().Faint(true)                                // secondary (ids, timestamps)
	styOK     = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))            // green
	styWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))            // yellow
	styBad    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))            // red
)

func (s styler) accent(t string) string { return s.apply(styAccent, t) }
func (s styler) dim(t string) string    { return s.apply(styDim, t) }
func (s styler) ok(t string) string     { return s.apply(styOK, t) }
func (s styler) warn(t string) string   { return s.apply(styWarn, t) }
func (s styler) bad(t string) string    { return s.apply(styBad, t) }

func (s styler) apply(st lipgloss.Style, t string) string {
	if !s.on {
		return t
	}
	return st.Render(t)
}

// styleDigestTTY layers the visual language onto the digest Markdown for a human
// terminal. It is a removable skin over the raw renderDigest() output — the SAME
// string the MCP `digest` tool returns — so when styling is off it returns raw
// verbatim and the machine path stays byte-identical.
//
// On a TTY it: turns "# …"/"## …" headers into accented labels (markdown markers
// dropped from the human view), dims the freshness line, dims the trailing
// "(id: …)" on each item, and — Phase 12 (M-6) — registers a case for EVERY new
// delta sentinel so the human brief never renders half-styled:
//   - "- [new] " / "- [updated] " item prefixes get an accent on the change tag;
//   - the "- +N more since last brief" guard line is dimmed;
//   - the per-section three-state headings (no-changes / stale / unavailable) are
//     colored by severity (dim / warn / bad) instead of a flat accent.
//
// The byte-clean invariant holds: when sty.on is false every method returns its
// input unchanged, so --json / MCP / non-TTY output stays raw Markdown.
func styleDigestTTY(raw string, sty styler) string {
	if !sty.on {
		return raw
	}
	lines := strings.Split(raw, "\n")
	for i, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "# "):
			lines[i] = sty.accent(strings.TrimPrefix(ln, "# "))
		case strings.HasPrefix(ln, "## "):
			lines[i] = styleSectionHeading(strings.TrimPrefix(ln, "## "), sty)
		case strings.HasPrefix(ln, "Fresh as of:"):
			lines[i] = sty.dim(ln)
		// "+N more since last brief" guard line (M-6) — dim it; it's a count, not an item.
		case strings.HasPrefix(ln, "- +") && strings.Contains(ln, "more since last brief"):
			lines[i] = sty.dim(ln)
		// "[new]"/"[updated]" item prefixes (M-6) — accent the change tag, dim the id.
		case strings.HasPrefix(ln, "- [new] "), strings.HasPrefix(ln, "- [updated] "):
			lines[i] = styleChangeItem(ln, sty)
		case strings.HasPrefix(ln, "- "):
			lines[i] = dimIDSuffix(ln, sty)
		}
	}
	return strings.Join(lines, "\n")
}

// styleSectionHeading colors a "## " heading by its three-state sentinel (M-6):
// a healthy/delta heading accents normally, "no changes" dims, "stale" warns,
// "unavailable" reads bad. The markdown marker is already trimmed by the caller.
func styleSectionHeading(heading string, sty styler) string {
	switch {
	case strings.Contains(heading, "unavailable"):
		return sty.bad(heading)
	case strings.Contains(heading, "stale"):
		return sty.warn(heading)
	case strings.Contains(heading, "no changes since last brief"):
		return sty.dim(heading)
	default:
		return sty.accent(heading)
	}
}

// styleChangeItem accents the leading "[new]"/"[updated]" change tag and dims the
// trailing "(id: …)" so the human brief reads the delta state at a glance while
// the agent-only id stays de-emphasised.
func styleChangeItem(ln string, sty styler) string {
	for _, tag := range []string{"[new]", "[updated]"} {
		prefix := "- " + tag + " "
		if strings.HasPrefix(ln, prefix) {
			rest := dimIDSuffix("- "+ln[len(prefix):], sty)
			rest = strings.TrimPrefix(rest, "- ")
			return "- " + sty.accent(tag) + " " + rest
		}
	}
	return dimIDSuffix(ln, sty)
}

// dimIDSuffix dims a trailing " (id: …)" so the human-facing bullet de-emphasises
// the agent-only identifier without removing it.
func dimIDSuffix(ln string, sty styler) string {
	idx := strings.LastIndex(ln, " (id:")
	if idx == -1 || !strings.HasSuffix(ln, ")") {
		return ln
	}
	return ln[:idx] + " " + sty.dim(ln[idx+1:])
}
