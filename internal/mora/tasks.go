package mora

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

func cmdTasks(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: mora tasks <sync [--write] | add <name> [flags] | done <name> | list [--json]>")
	}
	switch args[0] {
	case "sync":
		fs := flag.NewFlagSet("tasks sync", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		write := fs.Bool("write", false, "write")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		added, err := syncTasks(cfg, *write)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "tasks added: %d\n", added)
		return nil
	case "add":
		// Contract: the (quoted) task name is the first positional; flags follow it
		// (`tasks add "<name>" [--pri ...]`). Parsing flags from args[2:] avoids
		// Go's flag pkg stopping at the first non-flag arg, which would otherwise
		// fold a trailing `--pri P0` into the name.
		usage := errors.New("usage: mora tasks add <name> [--pri P1] [--domain ...] [--owner ...] [--horizon ...] [--blocker ...]")
		if len(args) < 2 {
			return usage
		}
		name := strings.TrimSpace(args[1])
		fs := flag.NewFlagSet("tasks add", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		domain := fs.String("domain", "memory", "domain")
		owner := fs.String("owner", "you", "owner")
		pri := fs.String("pri", "P1", "priority (P0|P1|P2)")
		horizon := fs.String("horizon", "this week", "horizon")
		blocker := fs.String("blocker", "None", "blocker")
		if err := fs.Parse(args[2:]); err != nil {
			return err
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
		name := strings.TrimSpace(strings.Join(args[1:], " "))
		if name == "" {
			return errors.New("usage: mora tasks done <name>")
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
