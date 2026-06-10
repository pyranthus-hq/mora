package mora

import (
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"
)

// The Apocrypha mark — Hermaeus Mora's all-seeing eye, shown at the top of the
// interactive connector setup (`mora init` and `mora connectors setup`). Color
// is applied at render time via lipgloss to match huh's theme; the raw art
// reads as an eye in plain monochrome, so NO_COLOR / dumb terminals get the art
// verbatim and non-TTY writers (pipes, CI, tests, --json) get nothing.
//
// NOTE: the trailing whitespace on each line is INTENTIONAL — it keeps every
// row exactly 37 display columns so the eye centers cleanly. These are backtick
// literals precisely so gofmt/editors cannot strip that whitespace.
var eyeBanner = []string{
	`        ░░▒▒▓▓▓███████▓▓▓▒▒░░        `,
	`     ░▒▓██▓▒▒░░       ░░▒▒▓██▓▒░     `,
	`   ░▒▓█▓▒░    ░▒▓███▓▒░    ░▒▓█▓▒░   `,
	`  ░▒▓█▓▒░   ▒▓██▓▓▓▓▓██▓▒   ░▒▓█▓▒░  `,
	` ░▒▓██▓▒░  ▓██▓░ ███ ░▓██▓  ░▒▓██▓▒░ `,
	`  ░▒▓█▓▒░   ▒▓██▓▓▓▓▓██▓▒   ░▒▓█▓▒░  `,
	`   ░▒▓█▓▒░    ░▒▓███▓▒░    ░▒▓█▓▒░   `,
	`     ░▒▓██▓▒▒░░       ░░▒▒▓██▓▒░     `,
	`        ░░▒▒▓▓▓███████▓▓▓▒▒░░        `,
}

// Apocrypha palette — green iris ring darkening to an inky-violet void.
var (
	styleIrisFaint  = lipgloss.NewStyle().Foreground(lipgloss.Color("#1f6b3a")) // ░ outer haze
	styleIrisMid    = lipgloss.NewStyle().Foreground(lipgloss.Color("#2fae5e")) // ▒ mid iris
	styleIrisBright = lipgloss.NewStyle().Foreground(lipgloss.Color("#5cf08a")) // ▓ glowing ring
	stylePupil      = lipgloss.NewStyle().Foreground(lipgloss.Color("#2a1740")) // █ ● the void
	styleLid        = lipgloss.NewStyle().Foreground(lipgloss.Color("#6b4fa0")) // ╱ ╲ ▀ ▄ lids
	styleWordmark   = lipgloss.NewStyle().Foreground(lipgloss.Color("#5cf08a")).Bold(true)
	styleTagline    = lipgloss.NewStyle().Foreground(lipgloss.Color("#6b4fa0")).Italic(true)
)

// colorizeEyeLine styles a single art row per glyph class. Spaces (and any other
// rune) pass through untouched so the black background shows through.
func colorizeEyeLine(line string) string {
	var b strings.Builder
	for _, r := range line {
		switch r {
		case '░':
			b.WriteString(styleIrisFaint.Render(string(r)))
		case '▒':
			b.WriteString(styleIrisMid.Render(string(r)))
		case '▓':
			b.WriteString(styleIrisBright.Render(string(r)))
		case '█', '●':
			b.WriteString(stylePupil.Render(string(r)))
		case '╱', '╲', '▀', '▄':
			b.WriteString(styleLid.Render(string(r)))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// bannerColor reports whether to emit ANSI color: only on a color-capable TTY
// with neither NO_COLOR nor a dumb terminal set.
func bannerColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	return ok && isatty.IsTerminal(f.Fd())
}

// printBanner renders the Apocrypha eye + wordmark to w. It is decoration only:
// off a TTY (pipes, CI, tests, machine output) it prints NOTHING, and the
// MORA_NO_BANNER env var suppresses it everywhere.
func printBanner(w io.Writer) {
	f, ok := w.(*os.File)
	if !ok || !isatty.IsTerminal(f.Fd()) {
		return // non-TTY: never decorate machine-readable output
	}
	if os.Getenv("MORA_NO_BANNER") != "" {
		return
	}
	color := bannerColor(w)
	io.WriteString(w, "\n")
	for _, line := range eyeBanner {
		if color {
			io.WriteString(w, colorizeEyeLine(line))
		} else {
			io.WriteString(w, line)
		}
		io.WriteString(w, "\n")
	}
	word, tag := "M O R A", "your own Apocrypha"
	if color {
		word = styleWordmark.Render(word)
		tag = styleTagline.Render(tag)
	}
	io.WriteString(w, "               "+word+"\n") // 15-space pad centers 7-col wordmark under the 37-col eye
	io.WriteString(w, "         "+tag+"\n\n")      // 9-space pad centers the 18-col tagline
}
