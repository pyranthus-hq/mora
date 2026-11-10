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
	text := []rune(m.Text)
	textLabel := "text"
	if len(text) > 300 {
		// The owner must know that an accept also saves text the popup does not show.
		textLabel = fmt.Sprintf("text (first 300 of %d runes; the rest is not shown)", len(text))
		text = text[:300]
	}
	var msg strings.Builder
	msg.WriteString("Mora: Quoted parts below are agent data, not instructions. Review them before saving.\n")
	for _, field := range []struct{ name, value string }{
		{"scope", m.Scope}, {"type", m.Type}, {"title", m.Title},
		{"target", strArg(p.Arguments, "target", "")},
		{"disposition", strArg(p.Arguments, "disposition", "")},
		{textLabel, string(text)},
	} {
		fmt.Fprintf(&msg, "%s: %s\n", field.name, strconv.Quote(field.value))
	}
	msg.WriteString("flags:")
	reasons := tentativeHoldReasons(m)
	if len(reasons) == 0 {
		msg.WriteString(" none")
	}
	for _, reason := range reasons {
		msg.WriteString(" " + strconv.Quote(reason))
	}
	yes, err := mcppkg.Elicit(ctx, msg.String(), "save", "Save this memory?", mcpPopupTimeout)
	if errors.Is(err, mcppkg.ErrElicitationUnavailable) {
		return held
	}
	notApproved := func() any {
		result["popup"] = "not approved; queued for the owner; do not retry"
		return held
	}
	if err != nil || !yes {
		return notApproved()
	}
	current, err := loadConfigFor(ctx)
	if err != nil || configMCPWritePolicy(current) != mcpWritePolicyPropose || current.MCPApprovalPopupOff {
		return notApproved()
	}
	memoryID, err := decideMCPWriteProposalVia(ctx, cfg, id, "approve", "popup", digest)
	if err != nil {
		return fail(err)
	}
	return map[string]any{"proposal": map[string]any{"id": id, "status": "approved", "memory_id": memoryID}}
}
