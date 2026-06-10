package mora

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// progress is the live per-item counter for long backfills. On a real terminal
// it animates ONE line in place — a Charm-styled (lipgloss-tinted) braille
// spinner repainting on a timer plus the running count, so even the dead time
// between API pages visibly moves — and settles a final ✓ line. On a non-TTY
// (pipes, launchd logs, CI) it appends a plain line every pipeStep items — the
// byte-clean invariant: control characters and ANSI never reach a non-TTY
// stream. done() always settles the final count on its own line.
type progress struct {
	out   io.Writer
	label string // source name, e.g. "gmail-pyranthus"
	noun  string // "threads" | "events" | "conversations"
	tty   bool
	sty   styler

	mu    sync.Mutex
	count int
	shown int // pipe mode: last appended count
	frame int
	stop  chan struct{}
	wg    sync.WaitGroup
}

const (
	progressPipeStep = 500                    // appended lines: keep launchd/CI logs lean
	spinnerInterval  = 120 * time.Millisecond // repaint cadence — motion without flicker
)

// spinnerFrames are the braille frames Charm's bubbles/spinner ships as "Dot";
// hand-rolled here so the line-oriented CLI gets the motion WITHOUT adopting
// the bubbletea event loop (which takes over the terminal — wrong shape for a
// scrolling ingest log).
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func newProgress(out io.Writer, label, noun string) *progress {
	p := &progress{out: out, label: label, noun: noun}
	if out == nil {
		return p
	}
	p.tty = isTTYWriter(out)
	p.sty = newStyler(out, false)
	if p.tty {
		p.stop = make(chan struct{})
		p.wg.Add(1)
		go p.animate()
		return p
	}
	fmt.Fprintf(out, "  %s: syncing…\n", label)
	return p
}

// animate repaints the in-place line on a timer so the spinner moves even when
// no items are arriving (page fetches, API latency). TTY-only goroutine;
// stopped (and awaited) by done().
func (p *progress) animate() {
	defer p.wg.Done()
	t := time.NewTicker(spinnerInterval)
	defer t.Stop()
	p.paint()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.paint()
		}
	}
}

// paint renders the current spinner frame + count. Holds the lock for the
// whole write so done()'s final line can never interleave with a repaint.
func (p *progress) paint() {
	p.mu.Lock()
	defer p.mu.Unlock()
	frame := p.sty.accent(spinnerFrames[p.frame%len(spinnerFrames)])
	p.frame++
	if p.count == 0 {
		fmt.Fprintf(p.out, "\r  %s %s: syncing…", frame, p.label)
		return
	}
	fmt.Fprintf(p.out, "\r  %s %s: %d %s…", frame, p.label, p.count, p.noun)
}

// tick counts one written item. TTY rendering is timer-driven (animate); pipe
// mode appends a plain line per step boundary.
func (p *progress) tick() {
	if p.out == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.count++
	if p.tty {
		return // the animator repaints with the new count on its next frame
	}
	if p.count-p.shown >= progressPipeStep {
		p.shown = p.count
		fmt.Fprintf(p.out, "  %s: %d %s…\n", p.label, p.count, p.noun)
	}
}

// done stops the animator and settles the final line. On a TTY it overwrites
// the in-place counter (clearing to end-of-line); on a non-TTY it appends.
// Zero items still reports honestly.
func (p *progress) done() {
	if p.out == nil {
		return
	}
	if p.tty {
		if p.stop != nil {
			close(p.stop)
			p.wg.Wait()
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		// Trailing spaces clear any longer leftover frame — deliberately NOT an
		// ANSI erase (\x1b[K), so a TTY with NO_COLOR set stays escape-free.
		fmt.Fprintf(p.out, "\r  %s %s: %d %s synced      \n", p.sty.ok("✓"), p.label, p.count, p.noun)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.out, "  %s: %d %s synced\n", p.label, p.count, p.noun)
}

// warnf and okf are the ONE visual language every human-facing command speaks
// (the uniform-style rule, 2026-06-10): a yellow "warn:" prefix for recoverable
// problems, a green "✓" for completed actions. Both route through newStyler so
// the byte-clean invariant holds — plain text on pipes/JSON/non-TTY.
func warnf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	sty := newStyler(w, false)
	fmt.Fprintf(w, "%s %s\n", sty.warn("warn:"), fmt.Sprintf(format, args...))
}

func okf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	sty := newStyler(w, false)
	fmt.Fprintf(w, "%s %s\n", sty.ok("✓"), fmt.Sprintf(format, args...))
}
