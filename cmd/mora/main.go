package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/pyranthus-hq/mora/internal/mora"
)

// Injected at release time via -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	mora.BuildVersion = version
	mora.BuildCommit = commit
	mora.BuildDate = date

	ctx := context.Background()
	if err := mora.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Stdin); err != nil {
		// JSON callers consume the final document even when the command already
		// emitted a partial receipt or progress. Human invocations retain the
		// grandfathered sentinel exit statuses below.
		for _, arg := range os.Args[1:] {
			if arg == "--json" {
				code, message := mora.ErrorDetails(err)
				_ = json.NewEncoder(os.Stdout).Encode(struct {
					Schema        string `json:"schema"`
					SchemaVersion int    `json:"schema_version"`
					Code          string `json:"code"`
					Message       string `json:"message"`
				}{"mora.error", 1, code, message})
				if msg := err.Error(); msg != "" {
					fmt.Fprintln(os.Stderr, msg)
				}
				os.Exit(1)
			}
		}
		// Honor a structured exit code (e.g. `mora loop begin` returns exit 10 on
		// an already-succeeded period; its payload is already on stdout). A blank
		// message means the command already emitted its output — don't double-print.
		// ExitCodeFor matches ONLY mora's own sentinel, so a wrapped *exec.ExitError
		// from a failed git/schtasks subprocess can't hijack the exit status.
		if code, ok := mora.ExitCodeFor(err); ok {
			if msg := err.Error(); msg != "" {
				fmt.Fprintln(os.Stderr, msg)
			}
			os.Exit(code)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
