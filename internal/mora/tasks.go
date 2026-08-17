package mora

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The `tasks` mutation receipts. Each mirrors the fact its human line prints,
// so an agent never has to read English to learn what changed.
type tasksSyncReceipt struct {
	Added   int  `json:"added"`
	Written bool `json:"written"`
}

type tasksAddReceipt struct {
	Task    string `json:"task"`
	Added   bool   `json:"added"`
	Domain  string `json:"domain"`
	Owner   string `json:"owner"`
	Pri     string `json:"pri"`
	Horizon string `json:"horizon"`
	Blocker string `json:"blocker"`
}

type tasksDoneReceipt struct {
	Task        string `json:"task"`
	RowsUpdated int    `json:"rows_updated"`
}

// splitLeadingPositionals cuts an argument list at the first dash-led argument,
// so a command whose positional may contain spaces can still accept trailing
// flags without folding them into the positional.
//
// A literal `--` ends flag interpretation: everything after it is positional,
// however it is spelled. Without that escape hatch the dash-led guard below
// makes a legitimately dash-led name — `tasks done -- -urgent` — unaddressable,
// which is a capability the guard was never meant to remove.
func splitLeadingPositionals(args []string) (positional, flags []string) {
	// `--` wins wherever it sits, so `tasks done --json -- -urgent` keeps both
	// the flag and the dash-led name. Scanning for it first is what makes that
	// work: the dash-led cut below would otherwise fire on `--json` at index 0
	// and never reach the terminator.
	for i, arg := range args {
		if arg == "--" {
			return args[i+1:], args[:i]
		}
	}
	for i, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return args[:i], args[i:]
		}
	}
	return args, nil
}

func cmdTasks(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mora tasks <sync [--write] | add <name> [flags] | done <name> | list [--json]>")
	}
	switch args[0] {
	case "sync":
		fs := flag.NewFlagSet("tasks sync", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		write := fs.Bool("write", false, "write")
		asJSON := fs.Bool("json", false, "json")
		if err := fs.Parse(args[1:]); err != nil {
			return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		added, err := syncTasks(cfg, *write)
		if err != nil {
			return err
		}
		if *asJSON {
			return emitReceipt(stdout, "mora.tasks.sync", 1, tasksSyncReceipt{Added: added, Written: *write})
		}
		fmt.Fprintf(stdout, "tasks added: %d\n", added)
		return nil
	case "add":
		// Contract: the (quoted) task name is the first positional; flags follow it
		// (`tasks add "<name>" [--pri ...]`). Parsing flags from args[2:] avoids
		// Go's flag pkg stopping at the first non-flag arg, which would otherwise
		// fold a trailing `--pri P0` into the name.
		usage := errors.New("usage: mora tasks add <name> [--json] [--pri P1] [--domain ...] [--owner ...] [--horizon ...] [--blocker ...]")
		// A flag in the name slot is a caller error, never a task name: `mora
		// tasks add --json` used to create a live task literally called
		// "--json", so a machine caller asking for JSON silently mutated the
		// vault. Refuse instead.
		// A literal `--` ends flag interpretation for the name slot, so a
		// legitimately dash-led title stays addressable: `tasks add -- -urgent`.
		rest := args[1:]
		literalName := len(rest) > 0 && rest[0] == "--"
		if literalName {
			rest = rest[1:]
		}
		if len(rest) == 0 || (!literalName && strings.HasPrefix(rest[0], "-")) {
			return usage
		}
		name := strings.TrimSpace(rest[0])
		fs := flag.NewFlagSet("tasks add", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		domain := fs.String("domain", "memory", "domain")
		owner := fs.String("owner", "you", "owner")
		pri := fs.String("pri", "P1", "priority (P0|P1|P2)")
		horizon := fs.String("horizon", "this week", "horizon")
		blocker := fs.String("blocker", "None", "blocker")
		asJSON := fs.Bool("json", false, "json")
		if err := fs.Parse(rest[1:]); err != nil {
			return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
		}
		// Same silent-drop hazard as `done`: an unquoted multiword title used to
		// keep only its first word and discard the rest without a word.
		if extra := fs.Args(); len(extra) > 0 {
			return newMoraError(errCodeUsageUnknownFlag, "usage", nil,
				"usage: mora tasks add <name> [flags] (unexpected argument %q — quote a name containing spaces)", extra[0])
		}
		if name == "" {
			return usage
		}
		// The task name is the row identity and a "|" would break the table, so
		// reject it rather than silently corrupt live-tasks.md.
		if strings.Contains(name, "|") {
			return errors.New("task name must not contain '|'")
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		added, err := addTask(cfg, LiveTask{Task: name, Domain: *domain, Owner: *owner, Pri: *pri, Horizon: *horizon, Blocker: *blocker})
		if err != nil {
			return err
		}
		if *asJSON {
			return emitReceipt(stdout, "mora.tasks.add", 1, tasksAddReceipt{
				Task: name, Added: added, Domain: *domain, Owner: *owner,
				Pri: *pri, Horizon: *horizon, Blocker: *blocker,
			})
		}
		if !added {
			fmt.Fprintf(stdout, "task exists: %s\n", name)
			return nil
		}
		fmt.Fprintf(stdout, "task added: %s\n", name)
		return nil
	case "list":
		fs := flag.NewFlagSet("tasks list", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		asJSON := fs.Bool("json", false, "json")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		tasks, err := listTasks(cfg)
		if err != nil {
			return err
		}
		if *asJSON {
			b, err := json.Marshal(tasks)
			if err != nil {
				return err
			}
			fmt.Fprintln(stdout, string(b))
			return nil
		}
		printHealthBannerLine(stdout, cfg, time.Now())
		for _, lt := range tasks {
			fmt.Fprintf(stdout, "%-8s %-10s %s\n", lt.Pri, lt.Status, lt.Task)
		}
		return nil
	case "done":
		// The name is every leading positional joined (task names carry spaces),
		// so flags start at the first dash-led argument. A bare `tasks done
		// --json` therefore has no name and is a usage error, rather than
		// closing a task literally called "--json".
		positional, flagArgs := splitLeadingPositionals(args[1:])
		fs := flag.NewFlagSet("tasks done", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		asJSON := fs.Bool("json", false, "json")
		if err := fs.Parse(flagArgs); err != nil {
			return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
		}
		// Go's flag package stops at the first non-flag argument and parks the
		// rest in Args(). Left unchecked, `tasks done a b --json junk` closes
		// "a b" and drops "junk" without a word — closing a task the caller did
		// not finish naming. Refuse instead of guessing.
		if rest := fs.Args(); len(rest) > 0 {
			return newMoraError(errCodeUsageUnknownFlag, "usage", nil,
				"usage: mora tasks done <name> [--json] (unexpected argument %q after flags — quote a name containing spaces, or pass it after `--`)", rest[0])
		}
		name := strings.TrimSpace(strings.Join(positional, " "))
		if name == "" {
			return errors.New("usage: mora tasks done <name> [--json]")
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		updated, err := markTaskDone(cfg, name)
		if err != nil {
			return err
		}
		if updated == 0 {
			return fmt.Errorf("no live task matched %q (it must already be a row in live-tasks.md — run `mora tasks sync --write` to seed P0 items first)", name)
		}
		if *asJSON {
			return emitReceipt(stdout, "mora.tasks.done", 1, tasksDoneReceipt{Task: name, RowsUpdated: updated})
		}
		// Task name is the row identity (no task IDs; syncTasks dedups by name).
		// Surface the count so closing multiple same-named rows is never silent.
		if updated > 1 {
			fmt.Fprintf(stdout, "task done: %s (%d rows)\n", name, updated)
		} else {
			fmt.Fprintf(stdout, "task done: %s\n", name)
		}
		return nil
	default:
		return errors.New("usage: mora tasks <sync [--write] | add <name> [flags] | done <name> | list [--json]>")
	}
}
func syncTasks(cfg Config, write bool) (int, error) {
	p0, err := parseP0(filepath.Join(cfg.VaultDir, "priority-map.md"))
	if err != nil {
		return 0, err
	}
	livePath := filepath.Join(cfg.VaultDir, "live-tasks.md")
	bodyBytes, err := os.ReadFile(livePath)
	if err != nil {
		return 0, err
	}
	body := string(bodyBytes)
	added := 0
	var rows []string
	for _, task := range p0 {
		if strings.Contains(body, "| "+task+" |") {
			continue
		}
		rows = append(rows, fmt.Sprintf("| %s | memory | you | P0 | queued | None | this week | %s |", task, time.Now().Format("2006-01-02")))
		added++
	}
	if write && added > 0 {
		body = strings.TrimRight(body, "\n") + "\n" + strings.Join(rows, "\n") + "\n"
		if err := atomicWrite(livePath, []byte(body), 0o644); err != nil {
			return 0, err
		}
	}
	return added, nil
}
func parseP0(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var inP0 bool
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "## ") {
			inP0 = strings.Contains(strings.ToLower(line), "p0")
			continue
		}
		if !inP0 {
			continue
		}
		m := p0Re.FindStringSubmatch(strings.TrimSpace(line))
		if len(m) == 3 {
			out = append(out, strings.TrimSpace(m[2]))
		}
	}
	return out, nil
}
func isTerminalStatus(status string) bool {
	return terminalTaskStatuses[strings.ToLower(strings.TrimSpace(status))]
}
func staleTasks(cfg Config, days int) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(cfg.VaultDir, "live-tasks.md"))
	if err != nil {
		return nil, err
	}
	var stale []string
	cutoff := time.Now().AddDate(0, 0, -days)
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "| ") || strings.Contains(line, "Last touched") || strings.Contains(line, "---") {
			continue
		}
		cols := tableCols(line)
		if len(cols) < 8 {
			continue
		}
		// Status-aware (issue #19): a terminal-state row is closed work, not stale.
		if isTerminalStatus(cols[4]) {
			continue
		}
		t, err := time.Parse("2006-01-02", cols[7])
		if err == nil && t.Before(cutoff) {
			stale = append(stale, cols[0])
		}
	}
	return stale, nil
}

// markTaskDone flips the live-tasks.md row whose Task (col 0) equals name to a
// terminal Status and stamps today as the completion date (Last touched, col 7).
// The row is KEPT — it is the closed-record that makes completion resurrection-
// safe: syncTasks sees the still-present row and refuses to re-add the P0 item
// (issue #19), without needing a separate closed-task ledger. Returns the number
// of rows updated (0 => not found).
func markTaskDone(cfg Config, name string) (int, error) {
	livePath := filepath.Join(cfg.VaultDir, "live-tasks.md")
	b, err := os.ReadFile(livePath)
	if err != nil {
		return 0, err
	}
	name = strings.TrimSpace(name)
	today := time.Now().Format("2006-01-02")
	lines := strings.Split(string(b), "\n")
	updated := 0
	for i, line := range lines {
		if !strings.HasPrefix(line, "| ") || strings.Contains(line, "Last touched") || strings.Contains(line, "---") {
			continue
		}
		cols := tableCols(line)
		if len(cols) < 8 || cols[0] != name {
			continue
		}
		cols[4] = "done"
		cols[7] = today
		lines[i] = "| " + strings.Join(cols, " | ") + " |"
		updated++
	}
	if updated == 0 {
		return 0, nil
	}
	if err := atomicWrite(livePath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return 0, err
	}
	return updated, nil
}

// listTasks parses live-tasks.md into rows (header/separator lines skipped).
func listTasks(cfg Config) ([]LiveTask, error) {
	b, err := os.ReadFile(filepath.Join(cfg.VaultDir, "live-tasks.md"))
	if err != nil {
		return nil, err
	}
	out := []LiveTask{}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "| ") || strings.Contains(line, "Last touched") || strings.Contains(line, "---") {
			continue
		}
		cols := tableCols(line)
		if len(cols) < 8 {
			continue
		}
		out = append(out, LiveTask{
			Task: cols[0], Domain: cols[1], Owner: cols[2], Pri: cols[3],
			Status: cols[4], Blocker: cols[5], Horizon: cols[6], LastTouched: cols[7],
		})
	}
	return out, nil
}

// addTask appends a queued live-task row. It is idempotent by Task name (the row
// identity, matching syncTasks's dedup): if a row with that name already exists
// it is a no-op and reports added=false, so a daily automation re-running the
// brief write-back never mints duplicates. Last touched is stamped today.
func addTask(cfg Config, lt LiveTask) (bool, error) {
	livePath := filepath.Join(cfg.VaultDir, "live-tasks.md")
	bodyBytes, err := os.ReadFile(livePath)
	if err != nil {
		return false, err
	}
	body := string(bodyBytes)
	// Idempotency by EXACT Task-name (col 0), not a substring scan of the whole
	// table — so a name that happens to appear in another row's Blocker/Horizon
	// cell, or that is a prefix of another task, does not falsely suppress the add.
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "| ") || strings.Contains(line, "Last touched") || strings.Contains(line, "---") {
			continue
		}
		if cols := tableCols(line); len(cols) >= 1 && cols[0] == lt.Task {
			return false, nil
		}
	}
	row := fmt.Sprintf("| %s | %s | %s | %s | queued | %s | %s | %s |",
		lt.Task, lt.Domain, lt.Owner, lt.Pri, lt.Blocker, lt.Horizon, time.Now().Format("2006-01-02"))
	body = strings.TrimRight(body, "\n") + "\n" + row + "\n"
	if err := atomicWrite(livePath, []byte(body), 0o644); err != nil {
		return false, err
	}
	return true, nil
}
func tableCols(line string) []string {
	raw := strings.Split(strings.Trim(line, "|"), "|")
	out := make([]string, 0, len(raw))
	for _, c := range raw {
		out = append(out, strings.TrimSpace(c))
	}
	return out
}
