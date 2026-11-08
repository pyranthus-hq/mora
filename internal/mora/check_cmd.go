package mora

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/corrections"
	"github.com/pyranthus-hq/mora/internal/sessions"
)

const (
	checkHeader = "mora check (read-only, counts only, safe to paste)"
	// maxSinceHours keeps --since inside what a time.Duration can hold.
	maxSinceHours = 100 * 366 * 24
)

// cmdCheck counts rejected or interrupted commands in local Claude Code and
// Codex transcripts. It reads them with os.Open only and writes nothing, and its
// output holds counts, dates and fixed table text, so it is safe to paste.
func cmdCheck(ctx context.Context, args []string, stdout, _ io.Writer) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sinceFlag := fs.String("since", "", "only look back this far: 30d, 24h or 2026-11-01")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
	}
	if fs.NArg() != 0 {
		return newMoraError(errCodeUsageUnknownValue, "usage", nil, "unexpected argument %q", fs.Arg(0))
	}
	var since time.Time
	var err error
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "since" {
			since, err = parseSince(*sinceFlag, time.Now())
		}
	})
	if err != nil {
		return newMoraError(errCodeUsageUnknownValue, "usage", err, "%v", err)
	}
	// Only the home folder matters here. The config keeps it on every error, and
	// such an error can hold a path or a config value, so it is not passed on.
	cfg, _ := loadConfigFor(ctx)
	if err := hermeticityGuard(cfg); err != nil {
		return err
	}
	corpus := corrections.NewCorpus(since)
	st, err := sessions.Scan(ctx, sessions.Options{Home: cfg.HomeDir(), Since: since}, corpus)
	if err != nil {
		return err
	}
	report := corpus.Report(st)
	if *jsonOut {
		return emitReceipt(stdout, "mora.check", 1, report)
	}
	printCheck(stdout, report)
	return nil
}

// parseSince accepts <n>d, <n>h and YYYY-MM-DD. A date is midnight UTC, because
// report dates are UTC.
func parseSince(value string, now time.Time) (time.Time, error) {
	usage := errors.New("--since wants <n>d, <n>h or YYYY-MM-DD, such as 30d, 24h or 2026-11-01")
	if t, err := time.Parse(time.DateOnly, value); err == nil && !t.IsZero() {
		return t, nil
	}
	hoursPerUnit := 0
	switch {
	case strings.HasSuffix(value, "d"):
		hoursPerUnit = 24
	case strings.HasSuffix(value, "h"):
		hoursPerUnit = 1
	default:
		return time.Time{}, usage
	}
	n, err := strconv.Atoi(value[:len(value)-1])
	if err != nil || n <= 0 || n > maxSinceHours/hoursPerUnit {
		return time.Time{}, usage
	}
	return now.Add(-time.Duration(n*hoursPerUnit) * time.Hour), nil
}

func printCheck(w io.Writer, r corrections.Report) {
	line := func(format string, args ...any) { fmt.Fprintf(w, format+"\n", args...) }
	line("%s", checkHeader)
	notes := r.Read
	if notes.FilesRead+notes.EmptyFiles+notes.UnreadableFiles+notes.FilesSkippedOld+notes.SymlinksSkipped == 0 {
		line("No Claude Code or Codex transcripts found.")
		return
	}
	line("%s", checkWindow(r.Window))
	claude, codex := r.Harnesses[0], r.Harnesses[1]
	c, rep, not := r.Corrections, r.Repeat, r.NotCounted
	line("")
	line("Sessions read")
	line("  Claude Code: sessions %d, subagent files %d, Bash calls %d", claude.Sessions, claude.SubagentFiles, claude.Calls)
	line("  Codex: sessions %d, shell calls %d, no rejection signal", codex.Sessions, codex.Calls)
	line("")
	line("Tier A corrections (you rejected or interrupted a tool call)")
	line("  Bash commands rejected: %d", c.BashRejected)
	line("  Bash commands interrupted: %d", c.BashInterrupted)
	line("  Other tools rejected or interrupted: %d (rules cover Bash only)", c.OtherRejected+c.OtherInterrupted)
	line("  User actions behind these: %d", c.UserActions)
	line("")
	line("Repeat after correction")
	line("  Commands rejected or interrupted: %d", rep.Scored)
	line("  With a later session: %d", rep.Eligible)
	if rep.Eligible == 0 {
		line("  No rate yet: no rejected or interrupted command has a later session.")
	} else {
		line("  Ran again in a later session, same command: %s", checkRate(rep.SameCommand, rep.Eligible))
		line("  Baseline, same command, never rejected or interrupted: %s", checkRate(rep.BaselineRepeated, rep.BaselinePairs))
		line("  Ran again in a later session, same shape: %s", checkRate(rep.SameShape, rep.Eligible))
		line("  Baseline, same shape, never rejected or interrupted: %s", checkRate(rep.BaselineShape, rep.BaselinePairs))
	}
	if rep.Eligible >= 1 && rep.Eligible <= 9 {
		line("  Small sample: read these as counts, not as a rate.")
	}
	line("")
	line("Top repeated patterns (same command / same shape)")
	if len(r.Top) == 0 {
		line("  none")
	}
	for _, p := range r.Top {
		line("  %s: %d / %d", p.Shape, p.SameCommand, p.SameShape)
	}
	line("")
	line("Not counted as corrections")
	line("  Blocked by a rule or hook: %d", not.BlockedByRule)
	line("  Blocked by auto mode: %d", not.BlockedByAutoMode)
	line("  Cancelled beside an interrupted call: %d", not.CancelledSiblings)
	line("  Interrupted inside a subagent: %d", not.InterruptedInSubagent)
	line("  Interrupted by shutdown or a non-human turn: %d", not.InterruptedNotHuman)
	line("  Interrupted between tool calls: %d", not.InterruptedBetweenCalls)
	line("  Older rejections with no typed field: %d", not.UntypedRejections)
	line("  Unknown denial kinds: %d", not.UnknownDenialKinds)
	line("")
	line("Read notes")
	line("  Files read: %d", notes.FilesRead)
	line("  Empty files: %d", notes.EmptyFiles)
	line("  Unreadable files: %d", notes.UnreadableFiles)
	if notes.SymlinksSkipped > 0 {
		line("  Symlinks skipped: %d", notes.SymlinksSkipped)
	}
	line("  Unknown records skipped: %d", notes.UnknownRecords)
	line("  Malformed lines skipped: %d", notes.MalformedLines)
	line("  Partial last lines: %d", notes.PartialLastLines)
	line("  Oversize lines skipped: %d", notes.OversizeLines)
	line("  Copied calls removed: %d", notes.CopiedCalls)
	line("  Codex commands not readable: %d", notes.CodexCommandsUnreadable)
	line("  Codex turn interrupts, not tied to a command: %d", notes.CodexTurnInterrupts)
	line("  Codex patches declined, not commands: %d", notes.CodexPatchesDeclined)
	line("")
	line("A rejection does not always mean the command was wrong. Read the rate beside the baseline.")
}

func checkWindow(w corrections.Window) string {
	out := "Window: all transcripts on disk"
	if w.Since != "" {
		out = "Window: since " + w.Since
	}
	if w.First != "" {
		out += ", " + w.First + " to " + w.Last
	}
	return out
}

// checkRate prints n of d. It prints no percentage when there is no data.
func checkRate(n, d int) string {
	if d == 0 {
		return "no data"
	}
	return fmt.Sprintf("%d of %d (%.1f%%)", n, d, 100*float64(n)/float64(d))
}
