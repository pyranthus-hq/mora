package mora

import (
	"context"
	"errors"
	"fmt"
	updatepkg "github.com/pyranthus-hq/mora/internal/update"
	"io"
	"os"
	"runtime"
	"time"
)

var (
	unattendedUpdateApply = runUnattendedAppUpdate
	unattendedVerifyApp   = verifyMoraAppBundle
	unattendedHealthCheck = func(ctx context.Context, cfg Config) error {
		return cmdDoctor(ctx, []string{"--strict", "--json"}, io.Discard, io.Discard)
	}
	unattendedPostHealthCheck = updatepkg.HealthCheckWithNewBinary
	unattendedAppWritable     = updatepkg.AppParentWritable
	unattendedSwapApps        = atomicSwapMoraAppDirectories
	unattendedVerifyArchive   = verifyAppArchiveChecksum
	unattendedExtractApp      = extractMoraAppArchive
	unattendedRebuild         = updatepkg.RebuildIndexWithNewBinary
	unattendedFailpoint       = func(string) error { return nil }
)

func updateLeasePath(cfg Config) string { return updatepkg.LeasePath(cfg.StateDir) }
func acquireUpdateLease(cfg Config, now time.Time) (func(), error) {
	return updatepkg.AcquireLease(cfg.StateDir, now)
}
func runUnattendedAppUpdate(ctx context.Context, cfg Config, receipt *updateReceipt, now time.Time, stdout io.Writer) error {
	token := firstNonEmpty(os.Getenv("MORA_GITHUB_TOKEN"), os.Getenv("GITHUB_TOKEN"), os.Getenv("GH_TOKEN"))
	return updatepkg.Apply(ctx, updatepkg.ApplyOptions{Store: updateReceiptStore(cfg), Receipt: receipt, Now: now, CurrentVersion: BuildVersion, GOOS: runtimeGOOS(), GOARCH: runtime.GOARCH, Stdout: stdout, Executable: updatePolicyExecutable, EvalSymlinks: updatePolicyEvalLinks, AppRoot: moraAppRoot, VerifyApp: unattendedVerifyApp, Health: func(ctx context.Context) error { return unattendedHealthCheck(ctx, cfg) }, PostHealth: unattendedPostHealthCheck, Writable: unattendedAppWritable, Discover: func(ctx context.Context, arch string) (updatepkg.Candidate, bool, error) {
		source, err := newAppReleaseSource(token)
		if err != nil {
			return updatepkg.Candidate{}, false, fmt.Errorf("setting up app release source: %w", err)
		}
		c, found, err := detectLatestAppRelease(ctx, source, arch)
		if err != nil {
			return updatepkg.Candidate{}, false, fmt.Errorf("checking signed app release: %w", err)
		}
		return updatepkg.Candidate{Version: c.version, AssetName: c.assetName, AssetURL: c.assetURL, ChecksumURL: c.checksumURL, ArchiveLimit: maxMoraAppArchiveBytes, ChecksumLimit: maxChecksumBytes}, found, nil
	}, Download: func(ctx context.Context, url, destination string, limit int64) error {
		return downloadAppReleaseFile(ctx, url, token, destination, limit)
	}, VerifyArchive: unattendedVerifyArchive, Extract: unattendedExtractApp, Swap: unattendedSwapApps, Rebuild: unattendedRebuild, Failpoint: unattendedFailpoint, NotifyAvailability: func(r *updateReceipt, at time.Time) error { return maybeNotifyUpdate(cfg, r, at) }, NotifySuccess: func(r *updateReceipt, version string, at time.Time) error {
		return notifyAutomaticUpdateSuccess(cfg, r, version, at)
	}, Clock: updatePolicyClock, ChecksumFilename: moraAppChecksumFilename})
}
func notifyAutomaticUpdateSuccess(cfg Config, receipt *updateReceipt, version string, now time.Time) error {
	if !shouldNotify(runtimeGOOS()) {
		return nil
	}
	script := `display notification "Mora updated to ` + escapeAppleScriptString(version) + `" with title "Mora · Update complete"`
	if err := updateNotificationRun("-e", script); err != nil {
		receipt.NotificationErrorCode = "notification_failed"
		notifyErr := fmt.Errorf("Mora.app updated but success notification failed: %w", err)
		if saveErr := saveUpdateReceipt(cfg, *receipt); saveErr != nil {
			return errors.Join(notifyErr, fmt.Errorf("persisting success-notification failure receipt: %w", saveErr))
		}
		return notifyErr
	}
	receipt.LastNotifiedAt = now.Format(time.RFC3339)
	receipt.LastNotifiedVersion = version
	receipt.NotificationErrorCode = ""
	return saveUpdateReceipt(cfg, *receipt)
}
