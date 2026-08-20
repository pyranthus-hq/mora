package mora

import (
	"context"
	protectedsyncpkg "github.com/pyranthus-hq/mora/internal/protectedsync"
	"os"
	"time"
)

const protectedSyncReceiptFlag = protectedsyncpkg.ReceiptFlag

type protectedSyncReceipt = protectedsyncpkg.Receipt

var protectedSyncExecutable = os.Executable
var protectedSyncRunOpen = func(ctx context.Context, args ...string) error { return realRun(ctx, "/usr/bin/open", args...) }
var protectedSyncNow = time.Now
var errProtectedSyncDirect = protectedsyncpkg.ErrDirect

func protectedSyncSource(source string) bool { return protectedsyncpkg.IsSource(source) }

func writeProtectedSyncReceipt(cfg Config, r protectedSyncReceipt) error {
	return protectedsyncpkg.WriteReceipt(cfg.StateDir, r)
}

func relayProtectedSync(ctx context.Context, cfg Config, source string) error {
	return protectedsyncpkg.Relay(ctx, protectedsyncpkg.Options{StateDir: cfg.StateDir, Source: source, GOOS: runtimeGOOS(), Executable: protectedSyncExecutable, AppRoot: moraAppRoot, RunOpen: protectedSyncRunOpen})
}
func protectedSyncReceiptArg(args []string) (string, []string, error) {
	return protectedsyncpkg.ParseArgs(args)
}
func realRun(ctx context.Context, name string, args ...string) error {
	_, err := realExec(ctx, "", name, args...)
	return err
}
