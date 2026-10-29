package mora

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	hookspkg "github.com/pyranthus-hq/mora/internal/hooks"
	"github.com/pyranthus-hq/mora/internal/memoryfile"
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
	CWD    string `json:"cwd"`
}

func cmdHook(ctx context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) error {
	if len(args) == 0 {
		return errors.New("usage: mora hook session-start|recall|install|uninstall|status")
	}
	switch args[0] {
	case "session-start":
		return hookSessionStart(ctx, stdout, stdin)
	case "recall":
		return hookRecall(ctx, args[1:], stdout, stdin)
	case "install":
		return hookInstall(ctx, args[1:], stdout)
	case "uninstall":
		fs := flag.NewFlagSet("hook uninstall", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		project := fs.Bool("project", false, "remove hooks from the current project")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
		return hookUninstall(ctx, stdout, *project)
	case "status":
		fs := flag.NewFlagSet("hook status", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		jsonOut := fs.Bool("json", false, "emit JSON")
		project := fs.Bool("project", false, "inspect hooks in the current project")
		if parseErr := fs.Parse(args[1:]); parseErr != nil {
			return newMoraError(errCodeUsageUnknownFlag, "usage", parseErr, "%v", parseErr)
		}
		if fs.NArg() != 0 {
			return newMoraError(errCodeUsageUnknownValue, "usage", nil, "unexpected argument %q", fs.Arg(0))
		}
		return hookStatus(ctx, stdout, *jsonOut, *project)
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
	cfg, err := loadConfigFor(ctx)
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
	opts, ok := parseHookRecallArgs(args)
	if !ok {
		return nil
	}
	var in recallHookInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return nil
	}
	if opts.scope != "" && !hookWithinProject(opts.projectDir, in.CWD) {
		return nil
	}
	prompt := strings.TrimSpace(in.Prompt)
	if skipRecallPrompt(prompt) {
		return nil
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return nil
	}
	// C2 ▸A: hook recall injects into EVERY user prompt, so it gets the same
	// one-line treatment as session-start — the banner rides along whenever the
	// vault is unhealthy, on both the search-succeeded and search-failed paths.
	// Scoped recall omits it because source errors may contain unrelated private data.
	var banner string
	if opts.scope == "" {
		banner = healthBannerFrom(healthOf(cfg, hookNow()))
	}
	searchCtx, cancel := context.WithTimeout(ctx, hookRecallTimeout)
	defer cancel()
	mems, serr := hookSearchMemories(searchCtx, cfg, prompt, opts.scope, hookRecallLimit*4)
	var body string
	if serr == nil && searchCtx.Err() == nil {
		if opts.scope != "" {
			kept := mems[:0]
			for _, m := range mems {
				if m.Scope == opts.scope {
					kept = append(kept, m)
				}
			}
			mems = kept
		}
		body = formatRecallContext(mems, opts.threshold, hookNow())
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

type hookRecallOptions struct {
	threshold  float64
	scope      string
	projectDir string
}

func parseHookRecallArgs(args []string) (hookRecallOptions, bool) {
	fs := flag.NewFlagSet("mora hook recall", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var opts hookRecallOptions
	fs.Float64Var(&opts.threshold, "threshold", hookRecallDefaultThreshold, "BM25 max-cost threshold")
	fs.StringVar(&opts.scope, "scope", "", "exact memory scope")
	fs.StringVar(&opts.projectDir, "project-dir", "", "project directory bound at installation")
	if err := fs.Parse(args); err != nil {
		return opts, false
	}
	if fs.NArg() != 0 || (opts.scope == "") != (opts.projectDir == "") || memoryfile.ValidateWriteScope(opts.scope) != nil {
		return opts, false
	}
	return opts, true
}

func hookWithinProject(root, cwd string) bool {
	if !filepath.IsAbs(root) || !filepath.IsAbs(cwd) {
		return false
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, cwd)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func writeHookOutput(stdout io.Writer, eventName, context string) error {
	out := hookEnvelope{HookSpecificOutput: hookSpecificOutput{
		HookEventName:     eventName,
		AdditionalContext: context,
	}}
	return json.NewEncoder(stdout).Encode(out)
}

func hookInstall(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("mora hook install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	threshold := fs.Float64("threshold", hookRecallDefaultThreshold, "BM25 max-cost threshold")
	project := fs.Bool("project", false, "install recall only in the current project")
	scope := fs.String("scope", "", "project memory scope (requires --project)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || (*project && (!strings.HasPrefix(*scope, "project:") || memoryfile.ValidateWriteScope(*scope) != nil)) || (!*project && *scope != "") {
		return errors.New("usage: mora hook install [--project --scope project:NAME] [--threshold NUMBER]")
	}
	exe, err := hookExecutable()
	if err != nil {
		return err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return err
	}
	path, err := claudeSettingsPath(ctx)
	if err != nil {
		return err
	}
	if *project {
		projectPath, err := claudeProjectSettingsPath()
		if err != nil {
			return err
		}
		start, recall, err := hookspkg.Status(path)
		if err != nil {
			return err
		}
		if start == "installed" || recall == "installed" {
			return errors.New("global Mora hooks read all scopes; run mora hook uninstall before installing project recall")
		}
		path = projectPath
		root := filepath.Dir(filepath.Dir(path))
		start, recall, err = hookspkg.Status(filepath.Join(root, ".claude", "settings.json"))
		if err != nil {
			return err
		}
		if start == "installed" || recall == "installed" {
			return errors.New("remove Mora hooks from this project's .claude/settings.json before installing project recall")
		}
		if err := hookspkg.InstallProjectRecall(path, exe, *scope, root, *threshold); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "installed Mora project recall: %s (%s)\n", *scope, path)
		return nil
	}
	if err := hookspkg.Install(path, exe, *threshold); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "installed mora Claude hooks: SessionStart, UserPromptSubmit")
	return nil
}

func hookUninstall(ctx context.Context, stdout io.Writer, project ...bool) error {
	path, err := claudeSettingsPath(ctx)
	if len(project) > 0 && project[0] {
		path, err = claudeProjectSettingsPath()
	}
	if err != nil {
		return err
	}
	if err := hookspkg.Uninstall(path); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "uninstalled mora Claude hooks")
	return nil
}

type hookStatusHarness struct {
	Name      string   `json:"name"`
	Installed bool     `json:"installed"`
	Events    []string `json:"events"`
}

type hookStatusPayload struct {
	Installed bool                `json:"installed"`
	Path      string              `json:"path"`
	Harnesses []hookStatusHarness `json:"harnesses"`
}

func hookStatus(ctx context.Context, stdout io.Writer, jsonOutput ...bool) error {
	jsonOut := len(jsonOutput) > 0 && jsonOutput[0]
	path, err := claudeSettingsPath(ctx)
	if len(jsonOutput) > 1 && jsonOutput[1] {
		path, err = claudeProjectSettingsPath()
	}
	if err != nil {
		return err
	}
	start, recall, err := hookspkg.Status(path)
	if err != nil {
		return err
	}
	if jsonOut {
		events := make([]string, 0, 2)
		if start == "installed" {
			events = append(events, "SessionStart")
		}
		if recall == "installed" {
			events = append(events, "UserPromptSubmit")
		}
		harnesses := make([]hookStatusHarness, 0, 1)
		harnesses = append(harnesses, hookStatusHarness{Name: "claude", Installed: len(events) > 0, Events: events})
		return emitReceipt(stdout, "mora.hook.status", 1, hookStatusPayload{
			Installed: len(events) > 0, Path: path, Harnesses: harnesses,
		})
	}
	fmt.Fprintf(stdout, "SessionStart: %s\n", start)
	fmt.Fprintf(stdout, "UserPromptSubmit: %s\n", recall)
	return nil
}

func claudeProjectSettingsPath() (string, error) {
	if os.Getenv("CLAUDE_CONFIG_DIR") != "" {
		return "", errors.New("project hooks do not support CLAUDE_CONFIG_DIR; use the default Claude configuration directory")
	}
	root, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	for dir := root; ; dir = filepath.Dir(dir) {
		entry, err := os.Lstat(filepath.Join(dir, ".git"))
		if err == nil {
			if !entry.IsDir() || entry.Mode()&os.ModeSymlink != 0 {
				return "", errors.New("project hooks do not support linked worktrees or .git files; install from the main checkout root")
			}
			if dir != root {
				return "", errors.New("run project hook commands from the repository root")
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return hookspkg.ProjectSettingsPath(root)
}

// claudeSettingsPath derives ~/.claude/settings.json from the context-resolved
// home so an injected test sandbox can never reach the developer's real Claude
// settings (that file has been wiped by a leak before — treat it as user data).
func claudeSettingsPath(ctx context.Context) (string, error) {
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return "", err
	}
	home := cfg.HomeDir()
	if home == "" {
		return "", errors.New("cannot resolve a home directory for Claude settings")
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}
