package mora

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	hookRecallDefaultThreshold = 0
	hookRecallLimit            = 3
	hookRecallByteLimit        = 800
	hookRecallTimeout          = 700 * time.Millisecond
	// hookMarker is appended to every installed command as a trailing shell
	// comment (Claude Code runs hook commands via the shell, so it is ignored
	// at execution). It lets install/uninstall/status identify Mora's own hooks
	// independently of the binary's name or path — matching on the command name
	// alone is fragile when the binary is renamed (e.g. mora-dev/mora-new).
	hookMarker = "#mora-managed"
)

var (
	hookNow            = time.Now
	hookResolveBrief   = resolveBrief
	hookSearchMemories = searchMemories
	hookExecutable     = os.Executable
)

type hookEnvelope struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

type sessionStartHookInput struct {
	Source string `json:"source"`
}

type recallHookInput struct {
	Prompt string `json:"prompt"`
}

type claudeCommandHook struct {
	Type    string                     `json:"type,omitempty"`
	Command string                     `json:"command,omitempty"`
	Timeout int                        `json:"timeout,omitempty"`
	Extra   map[string]json.RawMessage `json:"-"`
}

type claudeHookGroup struct {
	Matcher string                     `json:"matcher,omitempty"`
	Hooks   []claudeCommandHook        `json:"hooks"`
	Extra   map[string]json.RawMessage `json:"-"`
}

func (h *claudeCommandHook) UnmarshalJSON(body []byte) error {
	type known claudeCommandHook
	var k known
	if err := json.Unmarshal(body, &k); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return err
	}
	delete(raw, "type")
	delete(raw, "command")
	delete(raw, "timeout")
	*h = claudeCommandHook(k)
	h.Extra = raw
	return nil
}

func (h claudeCommandHook) MarshalJSON() ([]byte, error) {
	raw := cloneRawMessages(h.Extra)
	if h.Type != "" {
		b, err := json.Marshal(h.Type)
		if err != nil {
			return nil, err
		}
		raw["type"] = b
	}
	if h.Command != "" {
		b, err := json.Marshal(h.Command)
		if err != nil {
			return nil, err
		}
		raw["command"] = b
	}
	if h.Timeout != 0 {
		b, err := json.Marshal(h.Timeout)
		if err != nil {
			return nil, err
		}
		raw["timeout"] = b
	}
	return json.Marshal(raw)
}

func (g *claudeHookGroup) UnmarshalJSON(body []byte) error {
	type known claudeHookGroup
	var k known
	if err := json.Unmarshal(body, &k); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return err
	}
	delete(raw, "matcher")
	delete(raw, "hooks")
	*g = claudeHookGroup(k)
	g.Extra = raw
	return nil
}

func (g claudeHookGroup) MarshalJSON() ([]byte, error) {
	raw := cloneRawMessages(g.Extra)
	if g.Matcher != "" {
		b, err := json.Marshal(g.Matcher)
		if err != nil {
			return nil, err
		}
		raw["matcher"] = b
	}
	b, err := json.Marshal(g.Hooks)
	if err != nil {
		return nil, err
	}
	raw["hooks"] = b
	return json.Marshal(raw)
}

func cloneRawMessages(in map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in)+3)
	for k, v := range in {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out
}

func cmdHook(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) error {
	if len(args) == 0 {
		return errors.New("usage: mora hook session-start|recall|install|uninstall|status")
	}
	switch args[0] {
	case "session-start":
		return hookSessionStart(ctx, stdout, stdin)
	case "recall":
		return hookRecall(ctx, args[1:], stdout, stdin)
	case "install":
		return hookInstall(args[1:], stdout)
	case "uninstall":
		return hookUninstall(stdout)
	case "status":
		return hookStatus(stdout)
	default:
		return errors.New("usage: mora hook session-start|recall|install|uninstall|status")
	}
}

func hookSessionStart(ctx context.Context, stdout io.Writer, stdin io.Reader) error {
	var in sessionStartHookInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return nil
	}
	if in.Source == "compact" {
		return nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return nil
	}
	body, _, berr := hookResolveBrief(cfg, hookNow(), briefOpts{})
	if berr != nil {
		// C2 ▸A: hookSessionStart used to swallow EVERY resolveBrief error with
		// a silent return nil — a failed brief build injected nothing, and the
		// agent's session started with no signal at all that anything was
		// wrong. resolveBrief's own success path already carries a fresh
		// banner (reconcileCachedBriefHealth / the generate path's render), so
		// this is scoped to the one gap that isn't otherwise covered: inject
		// just the banner line instead of staying silent.
		if banner := healthBannerFrom(healthOf(cfg, hookNow())); banner != "" {
			_ = writeHookOutput(stdout, "SessionStart", banner)
		}
		return nil
	}
	if strings.TrimSpace(body) == "" {
		return nil
	}
	_ = writeHookOutput(stdout, "SessionStart", body)
	return nil
}

func hookRecall(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) error {
	threshold, ok := parseHookRecallArgs(args)
	if !ok {
		return nil
	}
	var in recallHookInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return nil
	}
	prompt := strings.TrimSpace(in.Prompt)
	if skipRecallPrompt(prompt) {
		return nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return nil
	}
	// C2 ▸A: hook recall injects into EVERY user prompt, so it gets the same
	// one-line treatment as session-start — the banner rides along whenever the
	// vault is unhealthy, on both the search-succeeded and search-failed paths,
	// instead of a failed/timed-out search silently injecting nothing.
	banner := healthBannerFrom(healthOf(cfg, hookNow()))
	searchCtx, cancel := context.WithTimeout(ctx, hookRecallTimeout)
	defer cancel()
	mems, serr := hookSearchMemories(searchCtx, cfg, prompt, "", hookRecallLimit*4)
	var body string
	if serr == nil && searchCtx.Err() == nil {
		body = formatRecallContext(mems, threshold, hookNow())
	}
	body = prependBannerLine(banner, body)
	if body == "" {
		return nil
	}
	_ = writeHookOutput(stdout, "UserPromptSubmit", body)
	return nil
}

// prependBannerLine puts the health banner on its own leading line above body,
// or returns body/banner alone when the other is empty.
func prependBannerLine(banner, body string) string {
	switch {
	case banner == "":
		return body
	case body == "":
		return banner
	default:
		return banner + "\n" + body
	}
}

func parseHookRecallArgs(args []string) (float64, bool) {
	fs := flag.NewFlagSet("mora hook recall", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	threshold := fs.Float64("threshold", hookRecallDefaultThreshold, "BM25 max-cost threshold")
	if err := fs.Parse(args); err != nil {
		return 0, false
	}
	return *threshold, true
}

func skipRecallPrompt(prompt string) bool {
	if utf8.RuneCountInString(prompt) < 12 {
		return true
	}
	if strings.HasPrefix(strings.TrimLeft(prompt, " \t\r\n"), "/") {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(prompt)) {
	case "yes", "no", "ok", "y", "n", "continue", "go", "k":
		return true
	default:
		return false
	}
}

func formatRecallContext(mems []Memory, threshold float64, now time.Time) string {
	var b strings.Builder
	count := 0
	for _, m := range mems {
		if count >= hookRecallLimit {
			break
		}
		// FTS5 bm25() is a lower-is-better cost and matching rows are commonly negative;
		// threshold is therefore a max-cost cutoff, keeping only scores <= threshold.
		if m.Score > threshold {
			continue
		}
		line := recallLine(m, now)
		if line == "" {
			continue
		}
		nextLen := b.Len() + len(line)
		if b.Len() == 0 {
			nextLen += len("[Mora recall]\n")
		} else {
			nextLen++
		}
		if nextLen > hookRecallByteLimit {
			break
		}
		if b.Len() == 0 {
			b.WriteString("[Mora recall]\n")
		} else {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		count++
	}
	return b.String()
}

func recallLine(m Memory, now time.Time) string {
	snippet := strings.Join(strings.Fields(m.Text), " ")
	if snippet == "" {
		snippet = strings.TrimSpace(m.Title)
	}
	if snippet == "" {
		return ""
	}
	snippet = clipRunes(snippet, 180)
	title := strings.TrimSpace(m.Title)
	if title == "" {
		title = m.ID
	}
	provenance := m.Source
	if provenance == "" {
		provenance = "memory"
	}
	if m.Scope != "" {
		provenance += "/" + m.Scope
	}
	return fmt.Sprintf("- %s [%s, age: %s, id: %s]: %s", title, provenance, memoryAge(m.CreatedAt, now), m.ID, snippet)
}

func clipRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	var b strings.Builder
	count := 0
	for _, r := range s {
		if count >= limit {
			break
		}
		b.WriteRune(r)
		count++
	}
	return strings.TrimSpace(b.String()) + "..."
}

func memoryAge(createdAt string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return "unknown"
	}
	if t.After(now) {
		return "in the future"
	}
	days := int(now.Sub(t).Hours() / 24)
	switch days {
	case 0:
		return "today"
	case 1:
		return "1d"
	default:
		return fmt.Sprintf("%dd", days)
	}
}

func writeHookOutput(stdout io.Writer, eventName, context string) error {
	out := hookEnvelope{HookSpecificOutput: hookSpecificOutput{
		HookEventName:     eventName,
		AdditionalContext: context,
	}}
	return json.NewEncoder(stdout).Encode(out)
}

func hookInstall(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("mora hook install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	threshold := fs.Float64("threshold", hookRecallDefaultThreshold, "BM25 max-cost threshold")
	if err := fs.Parse(args); err != nil {
		return err
	}
	exe, err := hookExecutable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}
	path, err := claudeSettingsPath()
	if err != nil {
		return err
	}
	settings, hooks, err := loadClaudeSettings(path)
	if err != nil {
		return err
	}
	upsertClaudeHook(hooks, "SessionStart", "session-start", claudeCommandHook{
		Type:    "command",
		Command: exe + " hook session-start " + hookMarker + ":session-start",
		Timeout: 15,
	})
	recallCommand := exe + " hook recall"
	if *threshold != hookRecallDefaultThreshold {
		recallCommand += " --threshold " + strconv.FormatFloat(*threshold, 'g', -1, 64)
	}
	recallCommand += " " + hookMarker + ":recall"
	upsertClaudeHook(hooks, "UserPromptSubmit", "recall", claudeCommandHook{
		Type:    "command",
		Command: recallCommand,
		Timeout: 10,
	})
	settings["hooks"] = hooks
	if err := writeClaudeSettings(path, settings); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "installed mora Claude hooks: SessionStart, UserPromptSubmit")
	return nil
}

func hookUninstall(stdout io.Writer) error {
	path, err := claudeSettingsPath()
	if err != nil {
		return err
	}
	settings, hooks, err := loadClaudeSettings(path)
	if err != nil {
		return err
	}
	for event, groups := range hooks {
		var kept []claudeHookGroup
		for _, group := range groups {
			var groupHooks []claudeCommandHook
			for _, h := range group.Hooks {
				if !strings.Contains(h.Command, hookMarker+":") {
					groupHooks = append(groupHooks, h)
				}
			}
			if len(groupHooks) > 0 {
				group.Hooks = groupHooks
				kept = append(kept, group)
			}
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}
	if err := writeClaudeSettings(path, settings); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "uninstalled mora Claude hooks")
	return nil
}

func hookStatus(stdout io.Writer) error {
	path, err := claudeSettingsPath()
	if err != nil {
		return err
	}
	_, hooks, err := loadClaudeSettings(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "SessionStart: %s\n", installedStatus(hooks, "SessionStart", "session-start"))
	fmt.Fprintf(stdout, "UserPromptSubmit: %s\n", installedStatus(hooks, "UserPromptSubmit", "recall"))
	return nil
}

func installedStatus(hooks map[string][]claudeHookGroup, event, sub string) string {
	if findClaudeHook(hooks[event], sub) >= 0 {
		return "installed"
	}
	return "not installed"
}

func claudeSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func loadClaudeSettings(path string) (map[string]any, map[string][]claudeHookGroup, error) {
	settings := map[string]any{}
	body, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// A dangling symlink also reads as ErrNotExist, but something IS at
		// the path — writing would silently replace the user's symlink (e.g.
		// into a dotfiles repo) with a mora-only regular file. Refuse. Only a
		// confirmed-absent path falls through to the create-fresh case; any
		// other Lstat failure means absence is unproven, so fail closed.
		if _, lerr := os.Lstat(path); lerr == nil {
			return nil, nil, fmt.Errorf("refusing to modify %s: it is a broken symlink: fix or remove it first", path)
		} else if !errors.Is(lerr, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("reading Claude settings %s: %w", path, lerr)
		}
		// No settings file yet: callers create a fresh one.
	case err != nil:
		// Fail closed: install/uninstall write back the full settings map, so
		// proceeding from an unread file would replace it with mora-only content.
		return nil, nil, fmt.Errorf("reading Claude settings %s: %w", path, err)
	default:
		if err := json.Unmarshal(body, &settings); err != nil {
			// Fail closed here too: a file that exists but does not parse as
			// strict JSON (JSONC comments, a trailing comma) must never be
			// silently treated as empty — writing back would wipe every
			// non-mora setting in it.
			return nil, nil, fmt.Errorf("refusing to modify %s: not valid JSON (%v): fix it or back it up first", path, err)
		}
	}
	hooks := map[string][]claudeHookGroup{}
	if raw, ok := settings["hooks"]; ok {
		if b, err := json.Marshal(raw); err == nil {
			if err := json.Unmarshal(b, &hooks); err != nil {
				return nil, nil, fmt.Errorf("malformed Claude settings hooks: %w", err)
			}
		} else {
			return nil, nil, fmt.Errorf("malformed Claude settings hooks: %w", err)
		}
	}
	return settings, hooks, nil
}

func writeClaudeSettings(path string, settings map[string]any) error {
	body, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return atomicio.Write(path, body, 0o600)
}

func upsertClaudeHook(hooks map[string][]claudeHookGroup, event, sub string, def claudeCommandHook) {
	groups := hooks[event]
	if idx := findClaudeHook(groups, sub); idx >= 0 {
		groupIdx, hookIdx := splitHookIndex(idx)
		groups[groupIdx].Hooks[hookIdx] = def
		hooks[event] = groups
		return
	}
	hooks[event] = append(groups, claudeHookGroup{Hooks: []claudeCommandHook{def}})
}

func findClaudeHook(groups []claudeHookGroup, sub string) int {
	marker := hookMarker + ":" + sub
	for groupIdx, group := range groups {
		for hookIdx, h := range group.Hooks {
			if strings.Contains(h.Command, marker) {
				return joinHookIndex(groupIdx, hookIdx)
			}
		}
	}
	return -1
}

func joinHookIndex(groupIdx, hookIdx int) int {
	return groupIdx<<16 | hookIdx
}

func splitHookIndex(idx int) (int, int) {
	return idx >> 16, idx & 0xffff
}
