package mora

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

func emitRetention(stdout io.Writer, jsonOut bool, schema string, value any) error {
	if jsonOut {
		return emitReceipt(stdout, schema, 1, value)
	}
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(b))
	return err
}

// retentionDecisionFlagsFirst preserves value-taking flag pairs while allowing
// the documented positional-first form. The shared flagsFirst helper is only
// safe for boolean flags.
func retentionDecisionFlagsFirst(args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--json" || strings.HasPrefix(a, "--json=") {
			flags = append(flags, a)
			continue
		}
		if a == "--action" || a == "--class" || a == "--summary" {
			flags = append(flags, a)
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		if strings.HasPrefix(a, "--action=") || strings.HasPrefix(a, "--class=") || strings.HasPrefix(a, "--summary=") {
			flags = append(flags, a)
			continue
		}
		positional = append(positional, a)
	}
	return append(flags, positional...)
}

func cmdRetention(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: mora retention report|decide|execute|recover|verify")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "report":
		fs := flag.NewFlagSet("retention report", flag.ContinueOnError)
		fs.SetOutput(stderr)
		older := fs.Int("older-than-days", retentionDefaultOlderDays, "candidate age threshold")
		recovery := fs.Int("recovery-days", retentionDefaultRecoverDay, "encrypted recovery window")
		jsonOut := fs.Bool("json", false, "emit JSON receipt")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
		report, err := buildRetentionReport(cfg, time.Now(), *older, *recovery)
		if err != nil {
			return err
		}
		return emitRetention(stdout, *jsonOut, "mora.retention.report", report)
	case "decide":
		fs := flag.NewFlagSet("retention decide", flag.ContinueOnError)
		fs.SetOutput(stderr)
		action := fs.String("action", "", "keep|change-class|compact|delete")
		class := fs.String("class", "", "new class for change-class")
		summary := fs.String("summary", "", "durable summary for compact")
		jsonOut := fs.Bool("json", false, "emit JSON receipt")
		if err := fs.Parse(retentionDecisionFlagsFirst(args[1:])); err != nil {
			return err
		}
		if fs.NArg() != 2 {
			return fmt.Errorf("usage: mora retention decide <report-id> <memory-id> --action <decision>")
		}
		report, err := decideRetentionCandidate(cfg, fs.Arg(0), fs.Arg(1), retentionDecision{Action: *action, Class: *class, Summary: *summary})
		if err != nil {
			return err
		}
		return emitRetention(stdout, *jsonOut, "mora.retention.decision", report)
	case "execute":
		fs := flag.NewFlagSet("retention execute", flag.ContinueOnError)
		fs.SetOutput(stderr)
		yes := fs.Bool("yes", false, "confirm destructive retention mutation")
		jsonOut := fs.Bool("json", false, "emit JSON receipt")
		if err := fs.Parse(flagsFirst(args[1:])); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: mora retention execute <report-id> --yes")
		}
		if !*yes {
			return fmt.Errorf("refusing retention execution without --yes")
		}
		receipt, err := executeRetentionReport(ctx, cfg, fs.Arg(0), time.Now())
		if err != nil {
			return err
		}
		return emitRetention(stdout, *jsonOut, "mora.retention.execution", receipt)
	case "recover":
		fs := flag.NewFlagSet("retention recover", flag.ContinueOnError)
		fs.SetOutput(stderr)
		yes := fs.Bool("yes", false, "confirm recovery mutation")
		jsonOut := fs.Bool("json", false, "emit JSON receipt")
		if err := fs.Parse(flagsFirst(args[1:])); err != nil {
			return err
		}
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: mora retention recover <manifest-id> --yes")
		}
		if !*yes {
			return fmt.Errorf("refusing retention recovery without --yes")
		}
		receipt, err := recoverRetentionManifest(ctx, cfg, fs.Arg(0), time.Now())
		if err != nil {
			return err
		}
		return emitRetention(stdout, *jsonOut, "mora.retention.recovery", receipt)
	case "verify":
		fs := flag.NewFlagSet("retention verify", flag.ContinueOnError)
		fs.SetOutput(stderr)
		jsonOut := fs.Bool("json", false, "emit JSON receipt")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
		receipt, err := verifyRetentionIntegrity(ctx, cfg)
		if err != nil {
			return err
		}
		if !receipt.Healthy {
			return fmt.Errorf("retention integrity verification failed: %+v", receipt)
		}
		return emitRetention(stdout, *jsonOut, "mora.retention.integrity", receipt)
	default:
		return fmt.Errorf("unknown retention command %q", args[0])
	}
}
