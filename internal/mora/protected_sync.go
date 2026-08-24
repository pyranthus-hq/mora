package mora

import (
	"context"
	"os"
	"path/filepath"
	"time"

	protectedsyncpkg "github.com/pyranthus-hq/mora/internal/protectedsync"
)

const protectedSyncReceiptFlag = protectedsyncpkg.ReceiptFlag

type protectedSyncReceipt = protectedsyncpkg.Receipt

var protectedSyncExecutable = os.Executable
var protectedSyncRunOpen = func(ctx context.Context, args ...string) error { return realRun(ctx, "/usr/bin/open", args...) }
var protectedSyncNow = time.Now
var protectedSyncUserHomeDir = os.UserHomeDir
var errProtectedSyncDirect = protectedsyncpkg.ErrDirect

func protectedSyncSource(source string) bool { return protectedsyncpkg.IsSource(source) }

func writeProtectedSyncReceipt(cfg Config, r protectedSyncReceipt) error {
	return protectedsyncpkg.WriteReceipt(cfg.StateDir, r)
}

// protectedSyncAppRoot resolves the FDA-bearing Mora.app identity used for local
// protected-source reads. The normal app install exposes a PATH symlink into the
// bundle, so the running executable resolves directly to Mora.app. Keep a bounded
// fallback for standalone/legacy PATH binaries, though: if the canonical signed app
// is installed at one of Mora's supported application locations, launch that app
// rather than reading Messages/Calendar under the generic CLI identity.
//
// This is deliberately discovery-only. LaunchServices/TCC remains the authority for
// whether the discovered app actually has Full Disk Access; a failed app launch or
// protected read stays loud and is never retried under a different identity.
func protectedSyncAppRoot(exe string) (string, bool) {
	if root, ok := moraAppRoot(exe); ok {
		return root, true
	}
	var roots []string
	if home, err := protectedSyncUserHomeDir(); err == nil && home != "" {
		roots = append(roots, filepath.Join(home, "Applications", moraAppName))
	}
	roots = append(roots, filepath.Join(string(filepath.Separator), "Applications", moraAppName))
	for _, root := range roots {
		inner := filepath.Join(root, "Contents", "MacOS", "mora")
		info, err := os.Stat(inner)
		if err != nil || info.IsDir() {
			continue
		}
		if resolvedRoot, ok := moraAppRoot(inner); ok {
			return resolvedRoot, true
		}
	}
	return "", false
}

func relayProtectedSync(ctx context.Context, cfg Config, source string) (protectedSyncReceipt, error) {
	return protectedsyncpkg.Relay(ctx, protectedsyncpkg.Options{StateDir: cfg.StateDir, Source: source, GOOS: runtimeGOOS(), Executable: protectedSyncExecutable, AppRoot: protectedSyncAppRoot, RunOpen: protectedSyncRunOpen})
}
func protectedSyncReceiptArg(args []string) (string, []string, error) {
	return protectedsyncpkg.ParseArgs(args)
}
func realRun(ctx context.Context, name string, args ...string) error {
	_, err := realExec(ctx, "", name, args...)
	return err
}
