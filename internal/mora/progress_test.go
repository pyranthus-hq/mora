package mora

import (
	"bytes"
	"strings"
	"testing"
)

// TestProgressByteCleanOnPipe locks the byte-clean invariant on the live
// counter: a non-TTY stream (pipes, launchd logs, CI) gets plain appended
// lines on the lean 500 cadence and NO carriage returns or ANSI, and done()
// always settles the honest final count.
func TestProgressByteCleanOnPipe(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf, "gmail-pyranthus", "threads")
	if p.tty {
		t.Fatalf("bytes.Buffer must never be detected as a TTY")
	}
	for i := 0; i < 303; i++ {
		p.tick()
	}
	p.done()
	out := buf.String()
	if strings.ContainsAny(out, "\r\x1b") {
		t.Fatalf("control bytes reached a non-TTY stream:\n%q", out)
	}
	if got := strings.Count(out, "\n"); got != 2 {
		// 303 < 500 → no intermediate line; exactly start + done.
		t.Fatalf("want exactly start+final lines for 303 items, got %d lines:\n%s", got, out)
	}
	if !strings.Contains(out, "gmail-pyranthus: syncing…") {
		t.Fatalf("missing start line: %q", out)
	}
	if !strings.Contains(out, "gmail-pyranthus: 303 threads synced") {
		t.Fatalf("done() must settle the final count, got: %q", out)
	}
}

// TestProgressTTYSpinnerAndSettle locks the alive-feeling path: the TTY line
// repaints in place (carriage return + a spinner frame + the live count — the
// fix for "the command looks dead"), and done() settles a ✓ final line. The
// animator goroutine is timer-driven; the test drives paint() directly so it
// is deterministic, and done() must tolerate a nil animator.
func TestProgressTTYSpinnerAndSettle(t *testing.T) {
	var buf bytes.Buffer
	// Construct directly to force the TTY branch; isTTYWriter needs a real
	// terminal fd. No animator goroutine — paint() is driven by hand.
	p := &progress{out: &buf, label: "gmail-pyranthus", noun: "threads", tty: true}
	p.paint() // zero items: spinner + "syncing…"
	for i := 0; i < 30; i++ {
		p.tick()
	}
	p.paint() // repaint with the live count
	p.done()
	out := buf.String()
	if !strings.Contains(out, "\r") {
		t.Fatalf("TTY mode must repaint in place, got: %q", out)
	}
	hasFrame := false
	for _, f := range spinnerFrames {
		if strings.Contains(out, f) {
			hasFrame = true
			break
		}
	}
	if !hasFrame {
		t.Fatalf("TTY repaint must carry a spinner frame, got: %q", out)
	}
	if !strings.Contains(out, "gmail-pyranthus: 30 threads…") {
		t.Fatalf("repaint must carry the live count, got: %q", out)
	}
	if !strings.Contains(out, "✓ gmail-pyranthus: 30 threads synced") {
		t.Fatalf("done() must settle the ✓ line, got: %q", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("TTY mode must hold ONE rewritten line until done, got: %q", out)
	}
}
