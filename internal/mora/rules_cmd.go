package mora

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/mattn/go-isatty"
	"github.com/pyranthus-hq/mora/internal/rules"
)

const rulesUsage = "usage: mora rules add|test|approve|off|list|compile"

// rulesRefusal is printed when approve or off runs where an agent could have
// typed it. Only a person at their own terminal may change what warns.
const rulesRefusal = "refusing to change rule approval from a non-interactive shell or from inside an agent session. Run this in your own terminal."

// rulesAgentRefusal names the variable that marked the shell as an agent's.
const rulesAgentRefusal = "refusing to change rule approval from inside an agent session (%s is set). Run this in your own terminal."

// rulesInteractive and rulesReadConfirm are swapped in tests. Production asks
// for a real terminal on stdin, as confirmVaultRepoint does.
var (
	rulesInteractive = func(stdin io.Reader) bool {
		f, ok := stdin.(*os.File)
		return ok && isatty.IsTerminal(f.Fd())
	}
	rulesReadConfirm = func(stdin io.Reader) string {
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		return line
	}
	rulesNow = time.Now
)

func cmdRules(ctx context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) error {
	if len(args) == 0 {
		return errors.New(rulesUsage)
	}
	switch args[0] {
	case "add":
		return rulesAdd(ctx, args[1:], stdout)
	case "test":
		return rulesTest(ctx, args[1:], stdout)
	case "approve":
		return rulesApprove(ctx, args[1:], stdout, stdin)
	case "off":
		return rulesOff(ctx, args[1:], stdout, stdin)
	case "list":
		return rulesList(ctx, args[1:], stdout)
	case "compile":
		return rulesCompile(ctx, args[1:], stdout)
	default:
		return fmt.Errorf("unknown rules command %q; %s", args[0], rulesUsage)
	}
}

// parseRulesArgs lets flags and positionals mix (`approve ID --mode warn`).
// Go's flag package stops at the first positional, so parse again after it.
func parseRulesArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func rulesAdd(ctx context.Context, args []string, stdout io.Writer) error {
	const usage = "usage: mora rules add --template compound-cd | mora rules add --id ID --match RE [--exclude RE] [--unless RE] [--context subagent] --message TEXT [--title T]"
	fs := flag.NewFlagSet("rules add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	template := fs.String("template", "", "built-in template: "+strings.Join(rules.TemplateNames, ", "))
	id := fs.String("id", "", "rule id")
	match := fs.String("match", "", "RE2 pattern the command must match")
	exclude := fs.String("exclude", "", "RE2 pattern replaced by a space before matching")
	unless := fs.String("unless", "", "RE2 pattern that cancels a match in the rest of the same simple command")
	ruleContext := fs.String("context", "", `"subagent" to warn only inside subagents`)
	message := fs.String("message", "", "the warning text, one line")
	title := fs.String("title", "", "short title")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(usage)
	}
	var r rules.Rule
	if *template != "" {
		if *id != "" || *match != "" || *exclude != "" || *unless != "" || *ruleContext != "" || *message != "" || *title != "" {
			return errors.New(usage)
		}
		t, ok := rules.Template(*template, rulesNow())
		if !ok {
			return fmt.Errorf("unknown template %q; templates: %s", *template, strings.Join(rules.TemplateNames, ", "))
		}
		r = t
	} else {
		if *id == "" || *match == "" || *message == "" {
			return errors.New(usage)
		}
		r = rules.Rule{
			ID: *id, Title: *title, CreatedAt: rulesNow().UTC().Format(time.RFC3339), Source: "cli",
			Message: *message,
			Spec:    rules.Spec{Version: 1, Tool: "Bash", Match: *match, Exclude: *exclude, Unless: *unless, Context: *ruleContext},
		}
	}
	if err := rules.Validate(r); err != nil {
		return err
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	path := rules.RulePath(cfg.VaultDir, r.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := atomicCreate(path, rules.Render(r), 0o600); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("rule %s already exists at %s", r.ID, path)
		}
		return err
	}
	fmt.Fprintf(stdout, "added rule %s (inactive). Approve it in your own terminal: mora rules approve %s --mode warn\n", r.ID, r.ID)
	return nil
}

// rulesTest prints hit or miss for one command. It never logs and never
// needs an approval, so it is safe to run from an agent session.
func rulesTest(ctx context.Context, args []string, stdout io.Writer) error {
	const usage = "usage: mora rules test <id> -- <command> | mora rules test --match RE [--exclude RE] [--unless RE] [--context subagent] [--agent ID] -- <command>. " +
		`Patterns are Go RE2: \b and \w are ASCII only, and Unicode spaces are turned into plain spaces before matching`
	before, command, found := cutArgs(args, "--")
	if !found || strings.TrimSpace(command) == "" {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet("rules test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	match := fs.String("match", "", "RE2 pattern")
	exclude := fs.String("exclude", "", "RE2 pattern replaced by a space before matching")
	unless := fs.String("unless", "", "RE2 pattern that cancels a match")
	ruleContext := fs.String("context", "", `"subagent" to match only inside subagents`)
	agent := fs.String("agent", "", "pretend the command came from this subagent id")
	pos, err := parseRulesArgs(fs, before)
	if err != nil {
		return err
	}
	var id string
	var spec rules.Spec
	switch {
	case len(pos) == 1 && *match == "" && *exclude == "" && *unless == "" && *ruleContext == "":
		cfg, err := loadConfigFor(ctx)
		if err != nil {
			return err
		}
		r, err := rules.ReadRule(cfg.VaultDir, pos[0])
		if err != nil {
			return err
		}
		id, spec = r.ID, r.Spec
	case len(pos) == 0 && *match != "":
		id, spec = "test", rules.Spec{Version: 1, Tool: "Bash", Match: *match, Exclude: *exclude, Unless: *unless, Context: *ruleContext}
	default:
		return errors.New(usage)
	}
	m, err := rules.NewMatcher(id, spec)
	if err != nil {
		return err
	}
	if m.Hit(rules.Action{AgentID: *agent, Command: command}) {
		fmt.Fprintln(stdout, "hit")
	} else {
		fmt.Fprintln(stdout, "miss")
	}
	return nil
}

// cutArgs splits args at the first sep and joins what follows with spaces.
func cutArgs(args []string, sep string) ([]string, string, bool) {
	for i, a := range args {
		if a == sep {
			return args[:i], strings.Join(args[i+1:], " "), true
		}
	}
	return args, "", false
}

// rulesAgentEnv lists the variables Claude Code and Codex set in the shells
// they start. It is not a CODEX_ prefix check: CODEX_HOME, CODEX_API_KEY and
// similar are user settings that people export in their own shell profile.
var rulesAgentEnv = []string{
	"CLAUDECODE",
	"CODEX_SANDBOX",
	"CODEX_SANDBOX_NETWORK_DISABLED",
	"CODEX_THREAD_ID",
	"CODEX_CI",
}

// rulesAgentSession returns the first agent-session variable that is set, or
// "" when none is.
func rulesAgentSession() string {
	for _, key := range rulesAgentEnv {
		if os.Getenv(key) != "" {
			return key
		}
	}
	return ""
}

// rulesRequireTerminal refuses unless a person is at their own terminal.
func rulesRequireTerminal(stdin io.Reader) error {
	if key := rulesAgentSession(); key != "" {
		return fmt.Errorf(rulesAgentRefusal, key)
	}
	if !rulesInteractive(stdin) {
		return errors.New(rulesRefusal)
	}
	return nil
}

// rulesConfirm shows what is about to change and requires the rule id to be
// typed back.
func rulesConfirm(stdout io.Writer, stdin io.Reader, id string, lines [][2]string) error {
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	for _, l := range lines {
		fmt.Fprintf(tw, "%s:\t%s\n", l[0], rulesShow(l[1]))
	}
	_ = tw.Flush()
	fmt.Fprintf(stdout, "Type the rule id (%s) to confirm: ", id)
	if strings.TrimSpace(rulesReadConfirm(stdin)) != id {
		fmt.Fprintln(stdout)
		return errors.New("the typed id did not match; nothing changed")
	}
	return nil
}

// rulesShow quotes a value that holds a rune the terminal would not print as
// itself, such as a carriage return, ESC or a bidi control, so the screen
// cannot show a different pattern from the one being approved.
func rulesShow(v string) string {
	if strings.IndexFunc(v, func(r rune) bool { return !unicode.IsGraphic(r) }) >= 0 {
		return strconv.Quote(v)
	}
	return v
}

func rulesApprove(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) error {
	const usage = "usage: mora rules approve <id> --mode warn|shadow [--holdout]"
	fs := flag.NewFlagSet("rules approve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	mode := fs.String("mode", "", "warn or shadow")
	holdout := fs.Bool("holdout", false, "warn only the hash-selected half of sessions")
	pos, err := parseRulesArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *mode == "" {
		return errors.New(usage)
	}
	if err := rules.ValidateMode(*mode); err != nil {
		return err
	}
	if *mode == rules.ModeOff {
		return errors.New("use mora rules off <id> to turn a rule off")
	}
	id := pos[0]
	if err := rulesRequireTerminal(stdin); err != nil {
		return err
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	r, err := rules.ReadRule(cfg.VaultDir, id)
	if err != nil {
		return err
	}
	if err := rules.Validate(r); err != nil {
		return err
	}
	if r.ID != id {
		return fmt.Errorf("rule file id %q does not match its file name %q", r.ID, id)
	}
	hash := rules.Hash(r)
	if err := rulesConfirm(stdout, stdin, id, [][2]string{
		{"rule", r.ID}, {"hash", rules.ShortHash(hash)},
		{"match", r.Spec.Match}, {"exclude", orNone(r.Spec.Exclude)}, {"unless", orNone(r.Spec.Unless)},
		{"context", orNone(r.Spec.Context)}, {"message", r.Message},
		{"mode", *mode}, {"holdout", yesNo(*holdout)},
	}); err != nil {
		return err
	}
	if err := rules.AppendApproval(cfg.StateDir, rules.Approval{RuleID: id, RuleHash: hash, Mode: *mode, Holdout: *holdout, Via: "cli-tty"}); err != nil {
		return err
	}
	set, err := rules.Compile(cfg.VaultDir, cfg.StateDir)
	if err != nil {
		return err
	}
	label := *mode
	if *holdout {
		label += ", holdout"
	}
	fmt.Fprintf(stdout, "approved %s (%s). Active rules: %d\n", id, label, len(set.Rules))
	return nil
}

func rulesOff(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) error {
	fs := flag.NewFlagSet("rules off", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	pos, err := parseRulesArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: mora rules off <id>")
	}
	id := pos[0]
	if err := rulesRequireTerminal(stdin); err != nil {
		return err
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	approvals, err := rules.LoadApprovals(cfg.StateDir)
	if err != nil {
		return err
	}
	a, ok := approvals[id]
	if !ok || a.Mode == rules.ModeOff {
		return fmt.Errorf("rule %s is not approved; nothing to turn off", id)
	}
	if err := rulesConfirm(stdout, stdin, id, [][2]string{{"rule", id}, {"mode", a.Mode + " -> off"}}); err != nil {
		return err
	}
	if err := rules.AppendApproval(cfg.StateDir, rules.Approval{RuleID: id, RuleHash: a.RuleHash, Mode: rules.ModeOff, Via: "cli-tty"}); err != nil {
		return err
	}
	set, err := rules.Compile(cfg.VaultDir, cfg.StateDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "turned off %s. Active rules: %d\n", id, len(set.Rules))
	return nil
}

type rulesListRow struct {
	ID      string `json:"id"`
	Mode    string `json:"mode"`
	Holdout bool   `json:"holdout"`
	Hash    string `json:"hash"`
	State   string `json:"state"`
}

type rulesListPayload struct {
	Rules []rulesListRow `json:"rules"`
}

func rulesList(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("rules list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
	}
	if fs.NArg() != 0 {
		return newMoraError(errCodeUsageUnknownValue, "usage", nil, "unexpected argument %q", fs.Arg(0))
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	rows, err := rulesListRows(cfg)
	if err != nil {
		return err
	}
	if *jsonOut {
		return emitReceipt(stdout, "mora.rules.list", 1, rulesListPayload{Rules: rows})
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "no rules. Add one with: mora rules add --template compound-cd")
		return nil
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tMODE\tHOLDOUT\tHASH\tSTATE")
	for _, r := range rows {
		hash := r.Hash
		if hash == "" {
			hash = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.ID, r.Mode, yesNo(r.Holdout), hash, r.State)
	}
	return tw.Flush()
}

// rulesListRows joins the vault rule files with this device's approvals.
func rulesListRows(cfg Config) ([]rulesListRow, error) {
	approvals, err := rules.LoadApprovals(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for id := range approvals {
		ids[id] = true
	}
	entries, err := os.ReadDir(filepath.Join(cfg.VaultDir, "rules"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		// A bad file name still shows up, as "invalid: invalid rule id".
		if id, ok := strings.CutSuffix(e.Name(), ".md"); ok && !e.IsDir() {
			ids[id] = true
		}
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	rows := make([]rulesListRow, 0, len(sorted))
	for _, id := range sorted {
		a, ok := approvals[id]
		if !ok {
			a = rules.Approval{RuleID: id, Mode: rules.ModeOff}
		}
		r, state := rules.Check(cfg.VaultDir, a)
		row := rulesListRow{ID: id, Mode: a.Mode, Holdout: a.Holdout && a.Mode != rules.ModeOff, State: state}
		if r.ID != "" {
			row.Hash = rules.ShortHash(rules.Hash(r))
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// rulesCompile rebuilds active.json from the approvals. It needs no terminal
// because it can only drop rules whose file changed, never add one.
func rulesCompile(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("rules compile", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: mora rules compile")
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	set, err := rules.Compile(cfg.VaultDir, cfg.StateDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "compiled %s. Active rules: %d\n", rules.ActivePath(cfg.StateDir), len(set.Rules))
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
