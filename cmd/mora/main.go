package main

import (
	"context"
	"errors"
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
		// Honor a structured exit code (e.g. `mora loop begin` returns exit 10 on
		// an already-succeeded period; its payload is already on stdout). A blank
		// message means the command already emitted its output — don't double-print.
		var coded interface{ ExitCode() int }
		if errors.As(err, &coded) {
			if msg := err.Error(); msg != "" {
				fmt.Fprintln(os.Stderr, msg)
			}
			os.Exit(coded.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
