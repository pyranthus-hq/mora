package mora

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	indexstore "github.com/pyranthus-hq/mora/internal/index"
	"github.com/pyranthus-hq/mora/internal/memory"
)

const (
	reviewUsage     = "usage: mora review [--list [--json] | --show saved|held ID --json]"
	reviewKindSaved = "saved"
	reviewKindHeld  = "held"
)

// reviewNote is one agent note that waits for the owner. Digest binds a held
// note to the exact arguments the owner saw.
type reviewNote struct {
	Kind       string
	ID         string
	Scope      string
	Type       string
	Title      string
	Text       string
	Source     string
	Provenance string // Derived by memory.DeriveProvenance: evidence, document, or authored.
	Truncated  bool
	Deleted    bool
	Meta       map[string]any
	Reasons    []string
	Digest     string
	// SHA is the content hash of the memory file the owner saw (saved notes).
	SHA string
	// Confirm is memory.OwnerConfirmedDigest of the shown note; keep binds the
	// owner-confirmed tag to it.
	Confirm string
	// Fields is memory.OwnerConfirmedFields as printed: every field the digest
	// binds. SHA (saved) or Digest (held) covers them in same.
	Fields [][2]string
}

type reviewRow struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Scope string `json:"scope"`
	Type  string `json:"type"`
	Title string `json:"title"`
}

type reviewReceipt struct {
	Notes []reviewRow `json:"notes"`
}

type reviewPreviewReceipt struct {
	Preview desktopReviewPreview `json:"preview"`
}

// same reports whether the owner would still see what was printed.
func (n reviewNote) same(o reviewNote) bool {
	return n.Kind == o.Kind && n.ID == o.ID && n.Scope == o.Scope && n.Type == o.Type &&
		n.Title == o.Title && n.Text == o.Text && n.Digest == o.Digest && n.SHA == o.SHA && n.Confirm == o.Confirm
}

// reviewTitle is the complete title, never cut or normalized, so what the
// owner compares is what the note says. Escape it only when printing.
func reviewTitle(m Memory) string {
	if strings.TrimSpace(m.Title) != "" {
		return m.Title
	}
	for _, line := range strings.Split(m.Text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func reviewNoteFromMemory(m Memory) reviewNote {
	return reviewNote{
		Kind: reviewKindSaved, ID: m.ID, Scope: m.Scope, Type: m.Type,
		Title: reviewTitle(m), Text: m.Text, Source: m.Source,
		Provenance: memory.DeriveProvenance(m), Truncated: m.Truncated, Deleted: m.DeletedAt != "", Meta: m.Meta,
		Confirm: memory.OwnerConfirmedDigest(m), Fields: memory.OwnerConfirmedFields(m),
	}
}

const reviewNoReason = "held under an earlier rule or write mode"

func reviewNoteFromProposal(p mcpWriteProposal) (reviewNote, error) {
	// A fixed clock keeps the note the same between print and keep; the only
	// field the clock reaches is a missing as_of, named as such below.
	m, err := mcpMemoryFromArgs(p.Arguments, time.Time{})
	if err != nil {
		return reviewNote{}, err
	}
	digest, err := mcpProposalArgumentsSHA256(p.Arguments)
	if err != nil {
		return reviewNote{}, err
	}
	n := reviewNoteFromMemory(m)
	n.Kind, n.ID, n.Reasons, n.Digest = reviewKindHeld, p.ID, tentativeHoldReasons(m), digest
	// created_at does not exist until the keep writes the memory.
	missingAsOf := strArg(p.Arguments, "as_of", "") == ""
	for i := range n.Fields {
		if n.Fields[i][0] == "created at" || (missingAsOf && n.Fields[i][0] == "decision as of") {
			n.Fields[i][1] = "the time you keep it"
		}
	}
	if len(n.Reasons) == 0 {
		// Held under an earlier rule, or staged by propose mode, which gives no reason.
		n.Reasons = []string{reviewNoReason}
	}
	return n, nil
}

// loadReviewNote reads one note again from disk.
func loadReviewNote(cfg Config, kind, id string) (reviewNote, error) {
	if kind == reviewKindSaved {
		found, err := findMemory(cfg, id)
		if err != nil {
			return reviewNote{}, err
		}
		raw, err := os.ReadFile(found.Path)
		if err != nil {
			return reviewNote{}, err
		}
		m, err := parseMemoryBytes(found.Path, raw)
		if err != nil {
			return reviewNote{}, err
		}
		if !memory.IsTentative(m) {
			return reviewNote{}, fmt.Errorf("memory %s is not tentative", m.ID)
		}
		n := reviewNoteFromMemory(m)
		n.SHA = indexstore.ContentSHA256(raw)
		return n, nil
	}
	p, _, err := readMCPWriteProposal(cfg, id)
	if err != nil {
		return reviewNote{}, err
	}
	if d, err := readMCPProposalDecision(cfg, id); err != nil {
		return reviewNote{}, err
	} else if d != nil {
		return reviewNote{}, proposalDecisionConflict(d)
	}
	return reviewNoteFromProposal(p)
}

// listReviewNotes returns saved notes first, then held ones.
func listReviewNotes(cfg Config, stderr io.Writer) ([]reviewNote, error) {
	rows, err := listTentativeMemories(cfg)
	if err != nil {
		return nil, err
	}
	notes := []reviewNote{}
	for _, r := range rows {
		n, err := loadReviewNote(cfg, reviewKindSaved, r.ID)
		if err != nil {
			fmt.Fprintf(stderr, "warning: %s\n", rulesShow(fmt.Sprintf("skipping memory %s: %v", r.ID, err)))
			continue
		}
		notes = append(notes, n)
	}
	ps, err := listMCPWriteProposals(cfg)
	if err != nil {
		return nil, err
	}
	for _, p := range ps {
		d, err := readMCPProposalDecision(cfg, p.ID)
		if err != nil {
			return nil, err
		}
		if d != nil {
			continue
		}
		n, err := reviewNoteFromProposal(p)
		if err != nil {
			fmt.Fprintf(stderr, "warning: %s\n", rulesShow(fmt.Sprintf("skipping invalid proposal %s: %v", p.ID, err)))
			continue
		}
		notes = append(notes, n)
	}
	return notes, nil
}

// reviewShowText escapes each line, so only newlines can move the cursor.
func reviewShowText(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = "  " + rulesShow(l)
	}
	return strings.Join(lines, "\n")
}

func printReviewNote(w io.Writer, n reviewNote) {
	fmt.Fprintf(w, "\n%s %s\n", n.Kind, rulesShow(n.ID))
	fmt.Fprintf(w, "scope: %s\ntype: %s\ntitle: %s\n", rulesShow(n.Scope), rulesShow(n.Type), rulesShow(n.Title))
	for _, f := range n.Fields {
		switch f[0] {
		case "scope", "type", "title", "text": // printed in their own places
		default:
			fmt.Fprintf(w, "%s: %s\n", f[0], rulesShow(f[1]))
		}
	}
	if len(n.Reasons) > 0 {
		reasons := make([]string, len(n.Reasons))
		for i, r := range n.Reasons {
			reasons[i] = rulesShow(r)
		}
		fmt.Fprintf(w, "held because: %s\n", strings.Join(reasons, ", "))
	}
	fmt.Fprintf(w, "text:\n%s\n", reviewShowText(n.Text))
}

// reviewAct keeps or rejects through the same cores the single-note commands
// use, and prints its own escaped result line instead of their output.
func reviewAct(ctx context.Context, cfg Config, n reviewNote, keep bool, stdout, stderr io.Writer) error {
	warn := &bytes.Buffer{}
	defer func() { reviewWarnings(stderr, warn) }()
	id := rulesShow(n.ID)
	switch {
	case n.Kind == reviewKindSaved && keep:
		m, _, err := confirmMemory(ctx, cfg, n.ID, n.SHA, n.Confirm, warn)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "owner-confirmed %s (%s)\n", rulesShow(m.ID), rulesShow(n.Title))
		return nil
	case n.Kind == reviewKindSaved:
		m, err := findMemory(cfg, n.ID)
		if err != nil {
			return err
		}
		_, _, reconcile, err := undoMemory(ctx, cfg, m, false, n.SHA, warn)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "undid %s (%s)\n", rulesShow(m.ID), rulesShow(n.Title))
		if reconcile {
			if err := reconcileAuthoredWrites(ctx, cfg); err != nil {
				fmt.Fprintf(warn, "warning: memory removed and dropped from search, but full index reconciliation is pending: %v\n", err)
			}
		}
		return nil
	}
	action, opts := "reject", []createOption(nil)
	if keep {
		action, opts = "approve", []createOption{withOwnerConfirmed()}
	}
	memoryID, err := decideMCPWriteProposalVia(ctx, cfg, n.ID, action, "", n.Digest, opts...)
	if err != nil {
		return err
	}
	if keep {
		fmt.Fprintf(stdout, "Approved %s as owner-confirmed memory %s.\n", id, rulesShow(memoryID))
		m, err := findMemory(cfg, memoryID)
		if err == nil {
			err = appendAgentWriteLog(cfg, time.Now(), "owner-confirm", m, "cli")
		}
		if err != nil {
			fmt.Fprintf(warn, "warning: memory %s is owner-confirmed, but %s could not be appended: %v\n", memoryID, agentWriteLogName, err)
		}
	} else {
		fmt.Fprintf(stdout, "Rejected %s.\n", id)
	}
	return nil
}

// reviewWarnings prints captured core warnings with each line escaped.
func reviewWarnings(w io.Writer, buf *bytes.Buffer) {
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line != "" {
			fmt.Fprintln(w, rulesShow(line))
		}
	}
}

func cmdReview(ctx context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) error {
	if len(args) > 0 && args[0] == "desktop" {
		return cmdReviewDesktop(ctx, args[1:], stdout)
	}
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	list := fs.Bool("list", false, "list agent notes that wait for review")
	show := fs.String("show", "", "show one saved or held note for a desktop preview")
	jsonOut := fs.Bool("json", false, "json")
	if err := fs.Parse(reviewFlagsFirst(args)); err != nil {
		return err
	}
	showSet := false
	fs.Visit(func(f *flag.Flag) {
		showSet = showSet || f.Name == "show"
	})
	if showSet {
		if *list || !*jsonOut || fs.NArg() != 1 || (*show != reviewKindSaved && *show != reviewKindHeld) {
			return errors.New(reviewUsage)
		}
		cfg, err := loadConfigFor(ctx)
		if err != nil {
			return err
		}
		preview, err := loadDesktopReviewPreview(cfg, *show, fs.Arg(0))
		if err != nil {
			return errDesktopReviewUnavailable
		}
		return emitReceipt(stdout, "mora.review.preview", 1, reviewPreviewReceipt{Preview: preview})
	}
	if fs.NArg() != 0 || (*jsonOut && !*list) {
		return errors.New(reviewUsage)
	}
	if !*list {
		if err := requireVerifiedOwnerAction(ownerAction{Kind: "review.decide"}); err != nil {
			return err
		}
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	notes, err := listReviewNotes(cfg, stderr)
	if err != nil {
		return err
	}
	if *list {
		return printReviewList(notes, *jsonOut, stdout)
	}
	if len(notes) == 0 {
		fmt.Fprintln(stdout, "No agent notes wait for review.")
		return nil
	}
	// Approving a held note starts a rebuild that exit would kill.
	defer drainReconcileOrWarn(stderr)
	return walkReviewNotes(ctx, cfg, notes, bufio.NewReader(stdin), stdout, stderr)
}

// reviewFlagsFirst keeps --show's value attached while still accepting --json
// after its two positional values.
func reviewFlagsFirst(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--show" || args[i] == "-show" {
			flags = append(flags, args[i])
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		if strings.HasPrefix(args[i], "-") {
			flags = append(flags, args[i])
		} else {
			pos = append(pos, args[i])
		}
	}
	return append(flags, pos...)
}

func printReviewList(notes []reviewNote, jsonOut bool, stdout io.Writer) error {
	rows := make([]reviewRow, 0, len(notes))
	for _, n := range notes {
		rows = append(rows, reviewRow{ID: n.ID, Kind: n.Kind, Scope: n.Scope, Type: n.Type, Title: n.Title})
	}
	if jsonOut {
		return emitReceipt(stdout, "mora.review", 1, reviewReceipt{Notes: rows})
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "No agent notes wait for review.")
		return nil
	}
	for _, r := range rows {
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\n", rulesShow(r.ID), r.Kind, rulesShow(r.Scope), rulesShow(r.Type), rulesShow(r.Title))
	}
	return nil
}

func walkReviewNotes(ctx context.Context, cfg Config, notes []reviewNote, in *bufio.Reader, stdout, stderr io.Writer) error {
	kept, rejected, skipped := 0, 0, 0
	quit := false
	for _, n := range notes {
		if quit {
			break
		}
		cur := n
		for decided := false; !decided && !quit; {
			printReviewNote(stdout, cur)
			ans, ok := askReview(in, stdout)
			switch ans {
			case "s":
				skipped++
				decided = true
			case "q":
				quit = true
			default: // "k" or "r"
				fresh, err := loadReviewNote(cfg, cur.Kind, cur.ID)
				if err != nil {
					fmt.Fprintf(stderr, "error: %s\n", rulesShow(fmt.Sprintf("%s: %v", cur.ID, err)))
					decided = true
					break
				}
				if !fresh.same(cur) {
					fmt.Fprintf(stdout, "\nThis note changed after it was shown. Nothing was done. Showing it again.\n")
					cur = fresh
					break
				}
				if err := reviewAct(ctx, cfg, cur, ans == "k", stdout, stderr); err != nil {
					fmt.Fprintf(stderr, "error: %s\n", rulesShow(fmt.Sprintf("%s: %v", cur.ID, err)))
				} else if ans == "k" {
					kept++
				} else {
					rejected++
				}
				decided = true
			}
			if !ok {
				quit = true
			}
		}
	}
	fmt.Fprintf(stdout, "\nkept %d, rejected %d, skipped %d, left %d\n", kept, rejected, skipped, len(notes)-kept-rejected-skipped)
	return nil
}

// askReview reads until it has a valid answer. It returns "q" and false at end of input.
func askReview(in *bufio.Reader, stdout io.Writer) (string, bool) {
	for {
		fmt.Fprint(stdout, "[k]eep [r]eject [s]kip [q]uit: ")
		line, err := in.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "k", "keep":
			return "k", err == nil
		case "r", "reject":
			return "r", err == nil
		case "s", "skip":
			return "s", err == nil
		case "q", "quit":
			return "q", err == nil
		}
		if err != nil {
			fmt.Fprintln(stdout)
			return "q", false
		}
	}
}
