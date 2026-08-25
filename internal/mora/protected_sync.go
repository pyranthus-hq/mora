package mora

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	protectedsyncpkg "github.com/pyranthus-hq/mora/internal/protectedsync"
)

const protectedSyncReceiptFlag = protectedsyncpkg.ReceiptFlag

type protectedSyncReceipt = protectedsyncpkg.Receipt

var protectedSyncExecutable = os.Executable
var protectedSyncProcessRunner = defaultSourceProcessRunner()
var protectedSyncRunOpen = func(ctx context.Context, args ...string) error {
	return runSourceProcess(ctx, protectedSyncProcessRunner, "/usr/bin/open", args...)
}
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

func newProtectedSyncToken() (string, error) { return protectedsyncpkg.NewToken(nil) }

func readProtectedSyncReceipt(cfg Config, token, source string, minCompletedAt ...time.Time) (protectedSyncReceipt, error) {
	if len(minCompletedAt) > 0 {
		return protectedsyncpkg.ReadReceiptAfter(cfg.StateDir, token, source, minCompletedAt[0])
	}
	return protectedsyncpkg.ReadReceipt(cfg.StateDir, token, source)
}

func protectedSyncApp() (string, bool) {
	if runtimeGOOS() != "darwin" {
		return "", false
	}
	exe, err := protectedSyncExecutable()
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return protectedSyncAppRoot(exe)
}

// relayProtectedIngest gives scheduled all-source ingest the same token-bound
// app receipt protocol as protected connector syncs. It is intentionally separate
// from protectedSyncSource: `mora sync ingest-hourly` must never become valid.
func relayProtectedIngest(ctx context.Context, cfg Config, all bool, source string) error {
	app, ok := protectedSyncApp()
	if !ok {
		return errProtectedSyncDirect
	}
	token, err := newProtectedSyncToken()
	if err != nil {
		return err
	}
	launchTime := protectedSyncNow().UTC()
	args := []string{"ingest", "run"}
	expectedSource := "ingest-hourly"
	if all {
		args = append(args, "--all")
	} else {
		args = append(args, "--source", source)
		expectedSource = source
	}
	args = append(args, protectedSyncReceiptFlag, token)
	openArgs := append([]string{"-n", "-W", "-a", app, "--args"}, args...)
	if err := protectedSyncRunOpen(ctx, openArgs...); err != nil {
		return fmt.Errorf("launching Mora.app for ingest: %w", err)
	}
	r, err := readProtectedSyncReceipt(cfg, token, expectedSource, launchTime)
	if err != nil {
		return err
	}
	if r.Error != "" {
		return fmt.Errorf("Mora.app ingest failed: %s", r.Error)
	}
	return nil
}

// validateProducerReceipt is a defense-in-depth producer-ledger check used by
// regression coverage. The relay's token and current completion time are the
// invocation authority; this rejects an independently updated status ledger.
func validateProducerReceipt(cfg Config, name string, launchTime time.Time) error {
	status, err := loadProducerStatus(cfg)
	if err != nil {
		return fmt.Errorf("loading producer status: %w", err)
	}
	ps, ok := status[name]
	if !ok || ps.LastAttemptAt == "" || ps.LastSuccessAt == "" {
		return fmt.Errorf("producer receipt for %s was not recorded", name)
	}
	if ps.LastError != "" || ps.LastAttemptAt != ps.LastSuccessAt {
		return fmt.Errorf("producer %s failed: %s", name, ps.LastError)
	}
	attemptTime, err := time.Parse(time.RFC3339, ps.LastAttemptAt)
	if err != nil || attemptTime.Before(launchTime.UTC().Truncate(time.Second)) {
		return fmt.Errorf("producer %s receipt %s is older than invocation launch time %s", name, ps.LastAttemptAt, launchTime.Format(time.RFC3339))
	}
	successTime, err := time.Parse(time.RFC3339, ps.LastSuccessAt)
	if err != nil || successTime.Before(launchTime.UTC().Truncate(time.Second)) {
		return fmt.Errorf("producer %s success receipt %s is older than invocation launch time %s", name, ps.LastSuccessAt, launchTime.Format(time.RFC3339))
	}
	return nil
}
