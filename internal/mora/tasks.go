package mora

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
			// Plan 01-07: the bare array moves under `tasks` so the payload can
			// carry the schema envelope.
			out := make([]LiveTask, 0, len(tasks))
			out = append(out, tasks...)
			return emitReceipt(stdout, "mora.tasks.list", 1, tasksListPayload{Tasks: out})
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

// tasksListPayload carries the live task rows under a named key.
type tasksListPayload struct {
	Tasks []LiveTask `json:"tasks"`
}
