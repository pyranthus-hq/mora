package mora

import (
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/rules"
)

// The PreToolUse hook runs before every Bash call, so it must fail open: on
// any error, panic or timeout it prints nothing, writes nothing to stderr and
// returns nil. It reads config and one capped file (active.json) and never
// touches SQLite, the index, the vault or the network.
var (
	hookPreToolNow        = time.Now
	hookPreToolLoadConfig = loadConfigFor
	hookPreToolLoad       = rules.LoadActive // swapped to panic in tests
	hookPreToolBudget     = 1500 * time.Millisecond
)

// hookPreToolMaxInput caps the stdin read. Larger input is ignored.
const hookPreToolMaxInput = 1 << 20

// hookPreToolDeps is what the worker needs, read from the package vars on the
// main goroutine. A worker that outlives its budget then never reads a var a
// later caller (or test) is swapping.
type hookPreToolDeps struct {
	now        func() time.Time
	loadConfig func(context.Context) (Config, error)
	load       func(string) (rules.ActiveSet, bool, error)
	version    string
}

func hookPreToolUse(ctx context.Context, stdout io.Writer, stdin io.Reader) (err error) {
	defer func() { _ = recover(); err = nil }()
	if os.Getenv("MORA_PRETOOL_OFF") == "1" {
		return nil
	}
	deps := hookPreToolDeps{now: hookPreToolNow, loadConfig: hookPreToolLoadConfig, load: hookPreToolLoad, version: BuildVersion}
	ch := make(chan string, 1)
	go func() {
		defer func() {
			if recover() != nil {
				ch <- ""
			}
		}()
		ch <- hookPreToolWork(ctx, stdin, deps)
	}()
	timer := time.NewTimer(hookPreToolBudget)
	defer timer.Stop()
	// Only this goroutine writes stdout, so a late worker can never print a
	// partial line.
	select {
	case msg := <-ch:
		if msg != "" {
			_ = writeHookOutput(stdout, "PreToolUse", msg)
		}
	case <-timer.C:
	}
	return nil
}

// hookPreToolWork parses the call, matches it against the active rules, logs
// one row and returns the warning text ("" for nothing to say).
func hookPreToolWork(ctx context.Context, stdin io.Reader, deps hookPreToolDeps) string {
	raw, err := io.ReadAll(io.LimitReader(stdin, hookPreToolMaxInput+1))
	if err != nil || len(raw) > hookPreToolMaxInput {
		return ""
	}
	action, harness, ok := rules.ParsePreTool(raw)
	if !ok {
		return ""
	}
	cfg, err := deps.loadConfig(ctx)
	if err != nil || cfg.StateDir == "" {
		return ""
	}
	logPath := rules.LogPath(cfg.StateDir)
	set, present, lerr := deps.load(rules.ActivePath(cfg.StateDir))
	if !present {
		return ""
	}
	row := rules.NewRow(deps.now(), harness, action)
	row.Mora = deps.version
	if lerr != nil {
		row.Note = "active_invalid"
		_ = rules.Append(logPath, row)
		return ""
	}
	if len(set.Rules) == 0 {
		// Rows are logged only while a rule is active: they are the
		// denominator of the warned and silent groups.
		return ""
	}
	row.RuleVersion = set.Version

	var notes []string
	matchers := make([]*rules.Matcher, 0, len(set.Rules))
	byID := map[string]rules.ActiveRule{}
	for _, r := range set.Rules {
		if r.Expired(deps.now()) {
			notes = append(notes, "rule_expired:"+r.ID)
			continue
		}
		m, merr := rules.NewMatcher(r.ID, r.Spec())
		if merr != nil || rules.ValidateMode(r.Mode) != nil || r.Mode == rules.ModeOff {
			notes = append(notes, "rule_invalid:"+r.ID)
			continue
		}
		if _, dup := byID[r.ID]; dup {
			notes = append(notes, "rule_invalid:"+r.ID)
			continue
		}
		matchers = append(matchers, m)
		byID[r.ID] = r
		row.Rules = append(row.Rules, rules.RuleRef{ID: r.ID, H: rules.ShortHash(r.Hash), Mode: r.Mode})
		if r.Holdout {
			row.Holdout = append(row.Holdout, r.ID)
		}
	}
	row.Note = strings.Join(notes, ",")

	row.Matched = rules.Match(matchers, action)
	var msgs []string
	for _, id := range row.Matched {
		r := byID[id]
		if r.Mode != rules.ModeWarn || (r.Holdout && row.Group != rules.GroupWarned) {
			continue
		}
		row.Warned = append(row.Warned, id)
		msgs = append(msgs, r.Message)
	}
	_ = rules.Append(logPath, row)
	return strings.Join(msgs, "\n")
}
