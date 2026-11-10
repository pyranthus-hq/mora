package mora

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	hookspkg "github.com/pyranthus-hq/mora/internal/hooks"
	"github.com/pyranthus-hq/mora/internal/memory"
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
	hookSearchMemories = hookRecallSearch
	hookExecutable     = os.Executable
)

const hookMarker = hookspkg.Marker

type claudeHookGroup = hookspkg.Group

type hookEnvelope struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
	// SystemMessage is shown to the user in Claude Code; additionalContext goes
	// to the model only.
	SystemMessage string `json:"systemMessage,omitempty"`
}

type hookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

type sessionStartHookInput struct {
	Source string `json:"source"`
	CWD    string `json:"cwd"`
}

type recallHookInput struct {
	Prompt string `json:"prompt"`
	CWD    string `json:"cwd"`
}

func cmdHook(ctx context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) error {
	if len(args) == 0 {
		return errors.New("usage: mora hook session-start|recall|session-end|pre-tool-use|install|uninstall|status")
	}
	switch args[0] {
	case "session-start":
		return hookSessionStart(ctx, args[1:], stdout, stdin)
	case "recall":
		return hookRecall(ctx, args[1:], stdout, stdin)
	case "session-end":
		return hookSessionEnd(ctx, args[1:], stdin)
	case "pre-tool-use":
		return hookPreToolUse(ctx, stdout, stdin)
	case "install":
		return hookInstall(ctx, args[1:], stdout)
	case "uninstall":
		fs := flag.NewFlagSet("hook uninstall", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		project := fs.Bool("project", false, "remove hooks from the current project")
		preTool := fs.Bool("pre-tool", false, "remove only the PreToolUse warning hook")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
		if *preTool {
			if *project {
				return errors.New("usage: mora hook uninstall --pre-tool (the PreToolUse hook is global only)")
			}
			return hookUninstallPreTool(ctx, stdout)
		}
		return hookUninstall(ctx, stdout, *project)
	case "status":
		fs := flag.NewFlagSet("hook status", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		jsonOut := fs.Bool("json", false, "emit JSON")
		project := fs.Bool("project", false, "inspect hooks in the current project")
		preTool := fs.Bool("pre-tool", false, "show only the PreToolUse warning hook")
		if parseErr := fs.Parse(args[1:]); parseErr != nil {
			return newMoraError(errCodeUsageUnknownFlag, "usage", parseErr, "%v", parseErr)
		}
		if fs.NArg() != 0 {
			return newMoraError(errCodeUsageUnknownValue, "usage", nil, "unexpected argument %q", fs.Arg(0))
		}
		if *preTool && *project {
			return newMoraError(errCodeUsageUnknownValue, "usage", nil, "--pre-tool cannot be combined with --project: the PreToolUse hook is global only")
		}
		if *preTool && !*jsonOut {
			// --json always carries pre_tool_use, so only text mode narrows.
			return hookStatusPreTool(ctx, stdout)
		}
		return hookStatus(ctx, stdout, *jsonOut, *project)
	default:
		return errors.New("usage: mora hook session-start|recall|session-end|pre-tool-use|install|uninstall|status")
	}
}

// parseProjectDirArg reads the one optional flag of session-start and
// session-end. A project install writes it; ok is false on any other argument.
func parseProjectDirArg(name string, args []string) (dir string, ok bool) {
	fs := flag.NewFlagSet("mora hook "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&dir, "project-dir", "", "project directory bound at installation")
	return dir, fs.Parse(args) == nil && fs.NArg() == 0
}

// hookProjectGate is the fail-closed check of a project-installed hook: cwd must
// be inside the installed folder and the folder must still be bound. Every other
// case, including an unreadable bindings file, means "do nothing".
func hookProjectGate(cfg Config, projectDir, cwd string) bool {
	if !hookWithinProject(projectDir, cwd) {
		return false
	}
	_, _, found, err := lookupProject(cfg, cwd)
	return err == nil && found
}

func hookSessionStart(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) error {
	projectDir, ok := parseProjectDirArg("session-start", args)
	if !ok {
		return nil
	}
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
	if projectDir != "" && !hookProjectGate(cfg, projectDir, in.CWD) {
		return nil // a project install never falls back to the daily brief
	}
	// A folder bound to a project gets that project's owner rules instead of the
	// daily brief. No binding file, or no match for cwd, keeps the daily brief.
	// A binding store that cannot be read, or a brief that cannot be built, shows
	// one diagnostic line: the daily brief holds personal mail and calendar.
	if filepath.IsAbs(in.CWD) {
		scope, root, found, lerr := lookupProject(cfg, in.CWD)
		text := ""
		var emitted []string
		switch {
		case lerr != nil:
			text = `Mora: the project bindings file cannot be read, so no brief is shown. Run "mora project list".`
		case found:
			var perr error
			if text, emitted, perr = buildProjectBrief(cfg, scope, root, hookNow()); perr != nil {
				text = fmt.Sprintf(`Mora: the brief for %s could not be built. Run "mora doctor".`, clipProjectScope(scope))
				emitted = nil
			}
		}
		if text != "" {
			werr := writeHookOutput(stdout, "SessionStart", prependBannerLine(boundHealthBanner(cfg), text))
			if len(emitted) > 0 {
				_ = appendGiven(cfg, givenLine{Surface: "hook-session-start", Kind: "project-brief", Caller: "claude-code", Dir: root, Scope: givenScope(scope), Written: boolPtr(werr == nil)}, emitted, len(emitted))
			}
			return nil
		}
	}
	// No binding for this folder: the daily brief leaves out every project record.
	body, _, berr := hookResolveBrief(cfg, hookNow(), briefOpts{excludeProjectScopes: true})
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
	if notice := reviewNotice(ctx, cfg); notice != "" {
		body += "\n" + notice
	}
	werr := writeHookOutput(stdout, "SessionStart", body)
	// The daily brief is a digest, not a list of notes: no ids, count 0.
	_ = appendGiven(cfg, givenLine{Surface: "hook-session-start", Kind: "daily-brief", Caller: "claude-code", Dir: givenHookDir(in.CWD), Written: boolPtr(werr == nil)}, nil, 0)
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
	// A binding file that cannot be read gives no recall and writes nothing.
	scopeFilter, bound, ok := recallScopeFilter(cfg, opts, in.CWD)
	if !ok {
		return nil
	}
	// C2 ▸A: hook recall injects into EVERY user prompt, so it gets the same
	// one-line treatment as session-start — the banner rides along whenever the
	// vault is unhealthy, on both the search-succeeded and search-failed paths.
	// A folder with a binding gets one fixed line, and the --scope form without a
	// binding gets none, because source errors may contain unrelated private data.
	var banner string
	switch {
	case bound:
		banner = boundHealthBanner(cfg)
	case opts.scope == "":
		banner = healthBannerFrom(healthOf(cfg, hookNow()))
	}
	searchCtx, cancel := context.WithTimeout(ctx, hookRecallTimeout)
	defer cancel()
	mems, serr := hookSearchMemories(searchCtx, cfg, prompt, "", hookRecallLimit*4, scopeFilter)
	var body string
	var recalledIDs []string
	if serr == nil && searchCtx.Err() == nil {
		kept := mems[:0]
		for _, m := range mems {
			// Recall speaks with Mora's voice inside the agent's prompt, so a
			// write nobody has confirmed never rides it.
			if scopeFilter.ScopeAllowed(m.Scope) && !memory.IsTentative(m) {
				kept = append(kept, m)
			}
		}
		mems = kept
		body, recalledIDs = hookspkg.FormatRecallContextIDs(mems, prompt, opts.threshold, hookNow())
	}
	body = prependBannerLine(banner, body)
	if body == "" {
		return nil
	}
	werr := writeHookOutputWithMessage(stdout, "UserPromptSubmit", body, hookspkg.SteppedInLine(len(recalledIDs)))
	if len(recalledIDs) > 0 {
		_ = appendGiven(cfg, givenLine{Surface: "hook-recall", Kind: "recall", Caller: "claude-code", Dir: givenHookDir(in.CWD), Scope: givenScope(opts.scope), Written: boolPtr(werr == nil)}, recalledIDs, len(recalledIDs))
	}
	return nil
}

// prependBannerLine puts the health banner on its own leading line above body,
// or returns body/banner alone when the other is empty.

func skipRecallPrompt(prompt string) bool { return hookspkg.SkipRecallPrompt(prompt) }
func formatRecallContext(mems []Memory, threshold float64, now time.Time) string {
	return hookspkg.FormatRecallContext(mems, threshold, now)
}

// hookRecallSearch is searchMemories with the hook's score domain restored.
// --threshold is a BM25 max cost, but once the Gmail/message segment arm
// participates searchMemoriesObserved returns positive RRF scores, and every
// one of them is above the default threshold of 0, so a prompt that matched
// an email or chat body used to inject nothing. The fused rank order is kept;
// each row's Score is put back to its parent-grain BM25 cost. A row admitted
// only by the segment arm has no parent cost and gets 0, which the default
// threshold keeps and a stricter (negative) threshold drops.
func hookRecallSearch(ctx context.Context, cfg Config, query, scope string, limit int, filters ...searchFilters) ([]Memory, error) {
	var obs searchMemoryObservation
	mems, err := searchMemoriesObserved(ctx, cfg, query, scope, limit, &obs, filters...)
	if err != nil || !obs.ScoreFused {
		return mems, err
	}
	for i := range mems {
		mems[i].Score = obs.BM25[mems[i].ID]
	}
	return mems, nil
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

// boundHealthLine replaces the detailed health banner in a folder with a
// project binding: the banner can name files and accounts of other projects.
const boundHealthLine = `Mora: memory health needs attention. Run "mora doctor".`

// boundHealthBanner is boundHealthLine, shown only when the detailed banner
// would not be empty.
func boundHealthBanner(cfg Config) string {
	if healthBannerFrom(healthOf(cfg, hookNow())) == "" {
		return ""
	}
	return boundHealthLine
}

// recallScopeFilter is the scope limit for notes that appear without being asked
// for. The binding of the hook cwd decides everything: its project and global,
// plus personal only when that binding is owner_only. With no binding, the
// installed --scope (cwd is inside its directory, checked by the caller) means
// that project plus global, never personal; with neither, everything except
// project notes. The bindings store is read first: when it cannot be read,
// whatever the cwd is, ok is false and recall stays silent. bound reports that a
// binding applied.
func recallScopeFilter(cfg Config, opts hookRecallOptions, cwd string) (f searchFilters, bound, ok bool) {
	var b projectBinding
	var err error
	if filepath.IsAbs(cwd) {
		b, bound, err = lookupProjectBinding(cfg, cwd)
	} else {
		_, err = loadProjectBindings(cfg)
	}
	switch {
	case err != nil:
		return searchFilters{}, false, false
	case bound:
		f.Scopes = []string{b.Scope, "global"}
		if b.OwnerOnly {
			f.Scopes = append(f.Scopes, "personal")
		}
	case opts.scope != "":
		f.Scopes = []string{opts.scope, "global"}
	default:
		f.NoProjectScopes = true
	}
	return f, bound, true
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
	return writeHookOutputWithMessage(stdout, eventName, context, "")
}

// writeHookOutputWithMessage also sets systemMessage, the line Claude Code
// shows to the user. An empty message is omitted.
func writeHookOutputWithMessage(stdout io.Writer, eventName, context, message string) error {
	out := hookEnvelope{HookSpecificOutput: hookSpecificOutput{
		HookEventName:     eventName,
		AdditionalContext: context,
	}, SystemMessage: message}
	return json.NewEncoder(stdout).Encode(out)
}

func hookInstall(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("mora hook install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	threshold := fs.Float64("threshold", hookRecallDefaultThreshold, "BM25 max-cost threshold")
	project := fs.Bool("project", false, "install recall only in the current project")
	scope := fs.String("scope", "", "project memory scope (requires --project)")
	preTool := fs.Bool("pre-tool", false, "install only the PreToolUse warning hook")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *preTool {
		if fs.NArg() != 0 || *project || *scope != "" {
			return errors.New("usage: mora hook install --pre-tool (the PreToolUse hook is global only)")
		}
		return hookInstallPreTool(ctx, stdout)
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
		globalHooks, err := hookStates(path)
		if err != nil {
			return err
		}
		root := filepath.Dir(filepath.Dir(projectPath))
		sharedHooks, err := hookStates(filepath.Join(root, ".claude", "settings.json"))
		if err != nil {
			return err
		}
		if sharedHooks.any() {
			return errors.New("remove Mora hooks from this project's .claude/settings.json before installing project recall")
		}
		// Bind first so a failed binding leaves no hook file behind.
		if _, _, err := bindProjectDir(ctx, root, *scope); err != nil {
			return err
		}
		// The global hooks decide by cwd, so they already serve this folder.
		if globalHooks.any() {
			note := ""
			if missing := globalHooks.missing(); len(missing) > 0 {
				note = fmt.Sprintf(" (missing globally: %s; run mora hook install)", strings.Join(missing, ", "))
			}
			// Old local Mora hooks would run beside the global ones, possibly
			// for another scope. Foreign hooks stay.
			if local, lerr := hookStates(projectPath); lerr == nil && local.any() {
				if err := hookspkg.Uninstall(projectPath); err != nil {
					return err
				}
				note += "; removed this folder's local Mora hooks"
			}
			fmt.Fprintf(stdout, "global Mora hooks already serve this folder; binding recorded%s\n", note)
			return nil
		}
		if err := hookspkg.InstallProjectRecall(projectPath, exe, *scope, root, *threshold); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "installed Mora project hooks: SessionStart, UserPromptSubmit, SessionEnd: %s (%s)\n", *scope, projectPath)
		return nil
	}
	if err := hookspkg.Install(path, exe, *threshold); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "installed mora Claude hooks: SessionStart, UserPromptSubmit, SessionEnd")
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
	Installed  bool                `json:"installed"`
	Path       string              `json:"path"`
	Harnesses  []hookStatusHarness `json:"harnesses"`
	PreToolUse string              `json:"pre_tool_use"`
	// Global and Project list the three hooks of each level. Project is
	// omitted when this folder cannot hold project hooks. BoundScope is the
	// project the current folder is bound to, or empty.
	Global     hookLevelStatus  `json:"global"`
	Project    *hookLevelStatus `json:"project,omitempty"`
	BoundScope string           `json:"bound_scope"`
}

type hookLevelStatus struct {
	SessionStart     string `json:"session_start"`
	UserPromptSubmit string `json:"user_prompt_submit"`
	SessionEnd       string `json:"session_end"`
}

func (s hookLevelStatus) list() [3]string {
	return [3]string{s.SessionStart, s.UserPromptSubmit, s.SessionEnd}
}

var hookNames = [3]string{"SessionStart", "UserPromptSubmit", "SessionEnd"}

func (s hookLevelStatus) any() bool {
	for _, v := range s.list() {
		if v == "installed" {
			return true
		}
	}
	return false
}

func (s hookLevelStatus) missing() (out []string) {
	for i, v := range s.list() {
		if v != "installed" {
			out = append(out, hookNames[i])
		}
	}
	return out
}

// hookStates reads the three Mora hooks of one Claude settings file.
func hookStates(path string) (hookLevelStatus, error) {
	start, recall, err := hookspkg.Status(path)
	if err != nil {
		return hookLevelStatus{}, err
	}
	end, err := hookspkg.StatusEnd(path)
	return hookLevelStatus{SessionStart: start, UserPromptSubmit: recall, SessionEnd: end}, err
}

func hookStatus(ctx context.Context, stdout io.Writer, jsonOutput ...bool) error {
	jsonOut := len(jsonOutput) > 0 && jsonOutput[0]
	project := len(jsonOutput) > 1 && jsonOutput[1]
	path, err := claudeSettingsPath(ctx)
	if project {
		path, err = claudeProjectSettingsPath()
	}
	if err != nil {
		return err
	}
	states, err := hookStates(path)
	if err != nil {
		return err
	}
	// The PreToolUse hook is global only, so read it from the global settings
	// even under --project. A global file that cannot be read makes it
	// "unknown" instead of failing a status read of another file.
	preTool, preToolErr := "unknown", error(nil)
	if globalPath, gerr := claudeSettingsPath(ctx); gerr != nil {
		preToolErr = gerr
	} else if st, perr := hookspkg.PreToolStatus(globalPath); perr != nil {
		preToolErr = perr
	} else {
		preTool = st
	}
	global := states
	var projectStates *hookLevelStatus
	if project {
		projectStates = &states
		if gp, gerr := claudeSettingsPath(ctx); gerr == nil {
			if g, serr := hookStates(gp); serr == nil {
				global = g
			}
		}
	} else if pp, perr := claudeProjectSettingsPath(); perr == nil {
		if ps, serr := hookStates(pp); serr == nil {
			projectStates = &ps
		}
	}
	bound := ""
	if cfg, cerr := loadConfigFor(ctx); cerr == nil {
		if wd, werr := os.Getwd(); werr == nil {
			bound, _, _, _ = lookupProject(cfg, wd)
		}
	}
	if jsonOut {
		events := make([]string, 0, 3)
		for i, v := range states.list() {
			if v == "installed" {
				events = append(events, hookNames[i])
			}
		}
		harnesses := make([]hookStatusHarness, 0, 1)
		harnesses = append(harnesses, hookStatusHarness{Name: "claude", Installed: len(events) > 0, Events: events})
		return emitReceipt(stdout, "mora.hook.status", 1, hookStatusPayload{
			Installed: len(events) > 0, Path: path, Harnesses: harnesses, PreToolUse: preTool,
			Global: global, Project: projectStates, BoundScope: bound,
		})
	}
	for i, v := range states.list() {
		fmt.Fprintf(stdout, "%s: %s\n", hookNames[i], v)
	}
	if bound == "" {
		fmt.Fprintln(stdout, "Folder: not bound to a project")
	} else {
		fmt.Fprintf(stdout, "Folder: bound to %s\n", clipProjectScope(bound))
	}
	if preToolErr != nil {
		fmt.Fprintf(stdout, "PreToolUse: unknown (%v)\n", preToolErr)
		return nil
	}
	fmt.Fprintf(stdout, "PreToolUse: %s\n", preTool)
	return nil
}

// hookInstallPreTool adds or replaces only Mora's PreToolUse (Bash) entry in
// the global Claude settings. The SessionStart and recall hooks are untouched.
func hookInstallPreTool(ctx context.Context, stdout io.Writer) error {
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
	if err := hookspkg.InstallPreTool(path, exe); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "installed mora Claude hook: PreToolUse (Bash)")
	return nil
}

func hookStatusPreTool(ctx context.Context, stdout io.Writer) error {
	path, err := claudeSettingsPath(ctx)
	if err != nil {
		return err
	}
	status, err := hookspkg.PreToolStatus(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "PreToolUse: %s\n", status)
	return nil
}

func hookUninstallPreTool(ctx context.Context, stdout io.Writer) error {
	path, err := claudeSettingsPath(ctx)
	if err != nil {
		return err
	}
	if err := hookspkg.UninstallSub(path, hookspkg.PreToolSub); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "uninstalled mora Claude hook: PreToolUse")
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
