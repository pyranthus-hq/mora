package mora

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	hookspkg "github.com/pyranthus-hq/mora/internal/hooks"
	indexstore "github.com/pyranthus-hq/mora/internal/index"
	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// The MCP confirm_memory tool is the chat-client twin of `mora confirm --mine`.
// The agent names a note; Mora asks the human through MCP elicitation
// (elicitation/create, form mode), which the client shows to the person and
// never to the model. Only an accept whose boolean field is exactly true makes
// the note owner-confirmed, through confirmMemoryVia, the same core the
// terminal path uses. Any other answer, a client without the capability, a
// timeout or an error leaves the note as it was.
//
// The known limit: a client can be configured to answer elicitations itself
// (Claude Code's Elicitation hooks can), and then the yes is not a person's.
// docs/guide.md states that in its trust section.

// mcpConfirmCaller names the elicitation path in the agent write log.
const mcpConfirmCaller = "mcp-elicitation"

// ownerConfirmElicitTimeout bounds how long the tool waits for the person. It
// stays under the 180-second tool-call limit Antigravity and Claude Desktop
// apply, so the agent gets Mora's reason rather than a client timeout.
// Package var so tests can shorten it.
var ownerConfirmElicitTimeout = 170 * time.Second

// Limits on what the prompt shows. A note that does not fit is never cut to
// fit: the person would confirm text they did not see. It is refused instead,
// and the terminal path, which shows any length, remains.
const (
	ownerPromptFieldRunes = 200
	ownerPromptMetaRunes  = 400
	ownerPromptTextRunes  = 2000
	ownerPromptTextLines  = 60
	ownerPromptErrRunes   = 120
	ownerPromptField      = "confirm"
	ownerPromptFieldTitle = "Yes, this is my rule"
)

func mcpConfirmMemory(ctx context.Context, cfg Config, args map[string]any) (any, error) {
	id := strings.TrimSpace(strArg(args, "id", ""))
	if id == "" {
		return nil, errors.New("confirm_memory: id is required")
	}
	m, raw, already, err := ownerConfirmCandidate(cfg, id)
	if err != nil {
		return nil, err
	}
	if already {
		return map[string]any{"id": m.ID, "owner_confirmed": true, "changed": false, "health": compactHealthOf(cfg, time.Now())}, nil
	}
	state := "the note is unchanged"
	if memory.IsTentative(m) {
		state = "the note stays tentative"
	}
	instead := fmt.Sprintf("the user can run `mora confirm --mine %s` in their own terminal", rulesShow(m.ID))
	if !mcppkg.ElicitAvailable(ctx) {
		return nil, fmt.Errorf("Not confirmed: this client did not declare MCP elicitation, so Mora cannot ask the user; %s. Instead %s.", state, instead)
	}
	prompt, err := ownerConfirmPrompt(m)
	if err != nil {
		return nil, fmt.Errorf("Not confirmed: %v; %s. Instead %s.", err, state, instead)
	}
	outcome, err := mcppkg.ElicitConfirm(ctx, prompt, ownerPromptField, ownerPromptFieldTitle, ownerConfirmElicitTimeout)
	if err != nil || outcome != mcppkg.ElicitYes {
		return nil, ownerConfirmRefusal(outcome, err, state, instead)
	}
	var warn bytes.Buffer
	m, logged, err := confirmMemoryVia(ctx, cfg, id, indexstore.ContentSHA256(raw), memory.OwnerConfirmedDigest(m), mcpConfirmCaller, &warn)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"id": m.ID, "owner_confirmed": true, "changed": true, "audit_logged": logged, "health": compactHealthOf(cfg, time.Now())}
	if w := strings.TrimSpace(warn.String()); w != "" {
		out["warnings"] = strings.Split(w, "\n")
	}
	return out, nil
}

// ownerConfirmRefusal is the one line the agent gets for every answer but yes.
func ownerConfirmRefusal(outcome mcppkg.ElicitOutcome, err error, state, instead string) error {
	switch {
	case errors.Is(err, mcppkg.ErrElicitationUnavailable):
		return fmt.Errorf("Not confirmed: this client did not declare MCP elicitation, so Mora cannot ask the user; %s. Instead %s.", state, instead)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("Not confirmed: nobody answered Mora's prompt within %s; %s. The user can ask again, or %s.", ownerConfirmElicitTimeout, state, instead)
	case err != nil:
		return fmt.Errorf("Not confirmed: the prompt failed (%s); %s. Instead %s.", hookspkg.ClipRunes(flatProjectBrief(err.Error()), ownerPromptErrRunes), state, instead)
	}
	switch outcome {
	case mcppkg.ElicitDeclined:
		// A client that cannot show prompts may decline on its own, so this
		// does not claim the person said no.
		return fmt.Errorf("Not confirmed: Mora's prompt came back declined; %s. Do not ask again unless the user asks; if they saw no prompt, %s.", state, instead)
	case mcppkg.ElicitCancelled:
		return fmt.Errorf("Not confirmed: the prompt was dismissed or cancelled; %s. The user can ask again, or %s.", state, instead)
	case mcppkg.ElicitNo:
		return fmt.Errorf("Not confirmed: the user did not tick yes in Mora's prompt; %s. The user can ask again, or %s.", state, instead)
	default:
		return fmt.Errorf("Not confirmed: the client's answer to Mora's prompt was not readable; %s. Instead %s.", state, instead)
	}
}

// ownerConfirmPrompt renders every field OwnerConfirmedDigest binds, in the
// same order `mora confirm --mine` shows them, with the text last. Each value
// must survive the project brief's flattening (flatProjectBrief) and the
// terminal escaping `mora read` and `mora review` use (rulesShow) unchanged.
// When either would alter a value, the prompt could not show what will be
// saved, so the note is refused instead of shown altered. Text lines are
// indented, so no line of a note can pass for a field line.
func ownerConfirmPrompt(m Memory) (string, error) {
	if !projectBriefIDPattern.MatchString(m.ID) {
		return "", errors.New("its id cannot be shown safely in a chat prompt")
	}
	var b strings.Builder
	b.WriteString("Mora: an agent asks to save this note as YOUR rule. Agents will treat its exact text as your own words.\n\n")
	fmt.Fprintf(&b, "id: %s\n", m.ID)
	text := ""
	for _, f := range memory.OwnerConfirmedFields(m) {
		if f[0] == "text" {
			text = f[1]
			continue
		}
		limit := ownerPromptFieldRunes
		if f[0] == "meta" {
			limit = ownerPromptMetaRunes
		}
		if utf8.RuneCountInString(f[1]) > limit {
			return "", fmt.Errorf("its %s is too long to show in a chat prompt (limit %d characters)", f[0], limit)
		}
		if flatProjectBrief(f[1]) != f[1] || rulesShow(f[1]) != f[1] {
			return "", fmt.Errorf("its %s holds line breaks, control or invisible characters, or repeated spaces that a chat prompt cannot show exactly", f[0])
		}
		fmt.Fprintf(&b, "%s: %s\n", f[0], f[1])
	}
	if n := utf8.RuneCountInString(text); n > ownerPromptTextRunes {
		return "", fmt.Errorf("its text is too long to show in a chat prompt (%d characters; limit %d)", n, ownerPromptTextRunes)
	}
	lines := strings.Split(text, "\n")
	if len(lines) > ownerPromptTextLines {
		return "", fmt.Errorf("its text has too many lines to show in a chat prompt (%d; limit %d)", len(lines), ownerPromptTextLines)
	}
	b.WriteString("text:\n")
	for _, line := range lines {
		if rulesShow(line) != line || hasInvisibleUnicode(line) {
			return "", errors.New("its text holds tabs, control or invisible characters that a chat prompt cannot show exactly")
		}
		b.WriteString("  " + line + "\n")
	}
	b.WriteString("\nTick yes and accept only if this is your own rule. Decline if you did not ask for it.")
	return b.String(), nil
}
