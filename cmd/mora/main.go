package main

import (
	"context"
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
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
