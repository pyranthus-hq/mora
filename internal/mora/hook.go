package mora

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	hookspkg "github.com/pyranthus-hq/mora/internal/hooks"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	hookRecallDefaultThreshold = 0
	hookRecallLimit            = hookspkg.RecallLimit
	hookRecallByteLimit        = hookspkg.RecallByteLimit
	hookRecallTimeout          = 700 * time.Millisecond
)

var (
	hookNow            = time.Now
	hookResolveBrief   = resolveBrief
	hookSearchMemories = searchMemories
	hookExecutable     = os.Executable
)

const hookMarker = hookspkg.Marker

type claudeHookGroup = hookspkg.Group

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

func skipRecallPrompt(prompt string) bool { return hookspkg.SkipRecallPrompt(prompt) }
func formatRecallContext(mems []Memory, threshold float64, now time.Time) string {
	return hookspkg.FormatRecallContext(mems, threshold, now)
}
func prependBannerLine(banner, body string) string { return hookspkg.PrependBanner(banner, body) }

func parseHookRecallArgs(args []string) (float64, bool) {
	fs := flag.NewFlagSet("mora hook recall", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	threshold := fs.Float64("threshold", hookRecallDefaultThreshold, "BM25 max-cost threshold")
	if err := fs.Parse(args); err != nil {
		return 0, false
	}
	return *threshold, true
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
	if err := hookspkg.Install(path, exe, *threshold); err != nil {
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
	if err := hookspkg.Uninstall(path); err != nil {
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
	start, recall, err := hookspkg.Status(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "SessionStart: %s\n", start)
	fmt.Fprintf(stdout, "UserPromptSubmit: %s\n", recall)
	return nil
}

func claudeSettingsPath() (string, error) { return hookspkg.SettingsPath() }
