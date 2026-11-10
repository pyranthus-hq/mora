package mora

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

var mcpPopupTimeout = 50 * time.Second

// approveHeldWriteByPopup keeps the staged proposal pending unless the owner
// explicitly accepts the form. Tool arguments never supply approval authority.
func approveHeldWriteByPopup(ctx context.Context, cfg Config, held any) any {
	if cfg.MCPApprovalPopupOff || configMCPWritePolicy(cfg) != mcpWritePolicyPropose {
		return held
	}
	result, ok := held.(map[string]any)
	if !ok {
		return held
	}
	fail := func(err error) any {
		result["popup"] = "approval failed: " + err.Error()
		return held
	}
	id := proposalIDOf(held)
	p, _, err := readMCPWriteProposal(cfg, id)
	if err != nil {
		return fail(err)
	}
	if _, redacted := redactProposalValue(p.Arguments); redacted {
		result["popup"] = "skipped: credential-like text"
		return held
	}
	at, err := time.Parse(time.RFC3339, p.ProposedAt)
	if err != nil {
		return fail(err)
	}
	m, err := mcpMemoryFromArgs(p.Arguments, at)
	if err != nil {
		return fail(err)
	}
	digest, err := mcpProposalArgumentsSHA256(p.Arguments)
	if err != nil {
		return fail(err)
	}
	// Claude Code 2.1 shows three message lines and one field-description
	// line, each cut at the window width. So every line here is short, the
	// text takes two lines, and the hint line carries flags and the counts.
	esc := func(r []rune) string { q := strconv.Quote(string(r)); return q[1 : len(q)-1] }
	title, text := []rune(m.Title), []rune(m.Text)
	target := strArg(p.Arguments, "target", "")
	// Budgets are in escaped characters and keep every line at 76 or less.
	width := func(r []rune) int { return len([]rune(esc(r))) }
	scope := popupClip([]rune(m.Scope), 26)
	var msg strings.Builder
	var shownTitle []rune
	if target != "" {
		shownTitle = popupClip(title, 40)
		fmt.Fprintf(&msg, "Correct %s as %s?\nin %s: \"%s\"", esc(popupClip([]rune(target), 30)), esc(popupClip([]rune(strArg(p.Arguments, "disposition", "")), 12)), esc(scope), esc(shownTitle))
	} else {
		kind := popupClip([]rune(m.Type), 8)
		shownTitle = popupClip(title, 60-width(scope)-width(kind))
		fmt.Fprintf(&msg, "Save \"%s\" as %s in %s?", esc(shownTitle), esc(kind), esc(scope))
	}
	shown := 0
	for lines := 3 - strings.Count(msg.String(), "\n") - 1; lines > 0 && shown < len(text); lines-- {
		n := popupFit(text[shown:], 72)
		if n == 0 {
			break
		}
		fmt.Fprintf(&msg, "\n\"%s\"", esc(text[shown:shown+n]))
		shown += n
	}
	var hint strings.Builder
	if reasons := tentativeHoldReasons(m); len(reasons) > 0 {
		hint.WriteString("flags: " + esc(popupClip([]rune(strings.Join(reasons, ", ")), 28)) + ". ")
	}
	// The owner must know that an accept also saves what the popup does not show.
	switch textCut, titleCut := shown < len(text), len(shownTitle) < len(title); {
	case textCut && titleCut:
		fmt.Fprintf(&hint, "Tick + Accept saves the whole note: text %d of %d characters shown, title %d of %d. ", shown, len(text), len(shownTitle), len(title))
	case textCut:
		fmt.Fprintf(&hint, "Tick + Accept saves the whole note: text %d of %d characters shown. ", shown, len(text))
	case titleCut:
		fmt.Fprintf(&hint, "Tick + Accept saves the whole note: title %d of %d characters shown. ", len(shownTitle), len(title))
	default:
		hint.WriteString("Tick + Accept saves it. ")
	}
	hint.WriteString("Else it waits.")
	yes, action, err := mcppkg.ElicitAnswer(ctx, msg.String(), "save", "Save this agent note?", hint.String(), mcpPopupTimeout)
	if errors.Is(err, mcppkg.ErrElicitationUnavailable) {
		return held
	}
	waiting := func(why string) any {
		result["popup"] = "not saved: " + why + ". The note waits for the owner (`mora mcp proposals list`). Tell the user. Do not send this same note again; other notes are fine."
		return held
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return waiting("no answer in " + mcpPopupTimeout.String())
	case err != nil:
		return waiting("the client could not show or return the popup")
	case yes:
	case action == "accept":
		return waiting("the box was not ticked")
	case action == "decline":
		return waiting("the user declined")
	case action == "cancel":
		return waiting("the user closed the popup")
	default:
		return waiting("the answer was not a clear yes")
	}
	current, err := loadConfigFor(ctx)
	if err != nil || configMCPWritePolicy(current) != mcpWritePolicyPropose || current.MCPApprovalPopupOff {
		return waiting("the write policy changed while the popup was open")
	}
	memoryID, err := decideMCPWriteProposalVia(ctx, cfg, id, "approve", "popup", digest)
	if err != nil {
		return fail(err)
	}
	return map[string]any{"proposal": map[string]any{"id": id, "status": "approved", "memory_id": memoryID}}
}

// popupFit returns how many leading runes of r fit in max characters once
// escaped, so a line never ends inside an escape and the shown count is exact.
func popupFit(r []rune, max int) int {
	used := 0
	for i, c := range r {
		w := len([]rune(strconv.Quote(string(c)))) - 2
		if used+w > max {
			return i
		}
		used += w
	}
	return len(r)
}

// popupClip cuts r to what fits in max escaped characters.
func popupClip(r []rune, max int) []rune {
	return r[:popupFit(r, max)]
}
