package mora

// companion_read.go is the CLI surface of the companion reader (graph node
// N12's third caller, alongside the phone listener and the tests). The
// desktop app drives these three read verbs directly against the same
// companionReader the loopback listener uses, so the CLI and the phone cannot
// drift: each verb emits exactly the document the listener serves on
// /v1/companion/<verb>.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
)

const companionHealthUsage = "usage: mora companion health [--json]"

func cmdCompanionHealth(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("companion health", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return newMoraError(errCodeUsageUnknownFlag, "usage", err, "%v", err)
	}
	if fs.NArg() != 0 {
		return newCodedError(errCodeUsageUnknownValue, nil,
			"%s (unexpected argument %q)", companionHealthUsage, fs.Arg(0))
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	out, err := newCompanionReader(cfg).Health(ctx)
	if err != nil {
		return newCodedError(errCodeInternalUnexpected, err, "companion health: %v", err)
	}
	if *jsonOut {
		return emitCompanionDocument(stdout, out)
	}
	fmt.Fprintf(stdout, "state\t%s\n", out.State)
	fmt.Fprintf(stdout, "policy\t%s\n", out.Policy)
	fmt.Fprintf(stdout, "index\t%s\t%d memories\n", out.Index.State, out.Index.Memories)
	for _, s := range out.Sources {
		fmt.Fprintf(stdout, "source\t%s\t%s\n", s.Key, s.State)
	}
	return nil
}

// emitCompanionDocument prints one wire document. The envelope is the
// projection's own Header, so emitReceipt (which stamps a CLI receipt schema)
// is deliberately not used here.
func emitCompanionDocument(w io.Writer, doc any) error {
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}
