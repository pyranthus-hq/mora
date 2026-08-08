package mora

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const updateLeaseTTL = 2 * time.Hour

var (
	unattendedUpdateApply = runUnattendedAppUpdate
	unattendedVerifyApp   = verifyMoraAppBundle
	unattendedHealthCheck = func(ctx context.Context, cfg Config) error {
		return cmdDoctor(ctx, []string{"--strict", "--json"}, io.Discard)
	}
	unattendedPostHealthCheck = healthCheckWithNewBinary
	unattendedAppWritable     = appParentWritable
	unattendedSwapApps        = atomicSwapMoraAppDirectories
	unattendedVerifyArchive   = verifyAppArchiveChecksum
	unattendedExtractApp      = extractMoraAppArchive
	unattendedRebuild         = rebuildIndexWithNewBinary
	unattendedFailpoint       = func(string) error { return nil }
)

func updateLeasePath(cfg Config) string {
	return filepath.Join(cfg.StateDir, "update", "update.lock")
}

func acquireUpdateLease(cfg Config, now time.Time) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(updateLeasePath(cfg)), 0o700); err != nil {
		return nil, err
	}
	body, err := json.Marshal(loopLockBody{PID: os.Getpid(), AcquiredAt: now.UTC().Format(time.RFC3339)})
	if err != nil {
		return nil, err
	}
	path := updateLeasePath(cfg)
	for attempt := 0; attempt < 2; attempt++ {
		published, err := publishLockFile(path, body)
		if err != nil {
			return nil, err
		}
		if published {
			return loopLockReleaser(path, body), nil
		}
		reaped, err := reapStaleLockTTL(path, now, updateLeaseTTL)
		if err != nil {
			return nil, err
		}
		if !reaped {
			return nil, fmt.Errorf("another Mora update is active; retry later")
		}
	}
	return nil, fmt.Errorf("another Mora update is active; retry later")
}

func appParentWritable(appRoot string) bool {
	probe, err := os.MkdirTemp(filepath.Dir(appRoot), ".mora-update-write-probe.")
	if err != nil {
		return false
	}
	return os.Remove(probe) == nil
}

func runUnattendedAppUpdate(ctx context.Context, cfg Config, receipt *updateReceipt, now time.Time, stdout io.Writer) error {
	version := receipt.LatestVersion
	if receipt.ApplyVersion == version && receipt.ApplyOutcome == "deferred" && receipt.ApplyErrorCode == "app_unwritable" {
		return maybeNotifyUpdate(cfg, receipt, now)
	}
	startApplyAttempt(receipt, version, now)
	if err := saveUpdateReceipt(cfg, *receipt); err != nil {
		return err
	}
	deferApply := func(code string) error {
		receipt.ApplyOutcome = "deferred"
		receipt.ApplyErrorCode = code
		receipt.RollbackOutcome = "not_needed"
		receipt.RebuildOutcome = "not_run"
		if err := saveUpdateReceipt(cfg, *receipt); err != nil {
			return err
		}
		return maybeNotifyUpdate(cfg, receipt, now)
	}
	failBeforeSwap := func(code string, cause error) error {
		receipt.ApplyOutcome = "failed_before_swap"
		receipt.ApplyErrorCode = code
		receipt.RollbackOutcome = "not_needed"
		receipt.RebuildOutcome = "not_run"
		if err := saveUpdateReceipt(cfg, *receipt); err != nil {
			return errors.Join(cause, fmt.Errorf("persisting pre-swap failure receipt: %w", err))
		}
		return cause
	}

	if runtimeGOOS() != "darwin" || runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64" {
		return deferApply("not_verified_app")
	}
	exe, err := updatePolicyExecutable()
	if err != nil {
		return failBeforeSwap("not_verified_app", fmt.Errorf("locating executable: %w", err))
	}
	if resolved, err := updatePolicyEvalLinks(exe); err == nil {
		exe = resolved
	}
	appRoot, ok := moraAppRoot(exe)
	if !ok {
		return deferApply("not_verified_app")
	}
	arch := runtime.GOARCH
	currentVersion := strings.TrimPrefix(BuildVersion, "v")
	if !canonicalStableVersion(currentVersion) {
		return deferApply("not_verified_app")
	}
	if err := unattendedVerifyApp(ctx, appRoot, currentVersion, arch); err != nil {
		return failBeforeSwap("installed_verify_failed", fmt.Errorf("installed Mora.app verification failed: %w", err))
	}
	if err := unattendedFailpoint("after_installed_verify"); err != nil {
		return failBeforeSwap("failpoint", err)
	}
	if err := unattendedHealthCheck(ctx, cfg); err != nil {
		return deferApply("unsafe_health")
	}
	if !unattendedAppWritable(appRoot) {
		return deferApply("app_unwritable")
	}

	token := firstNonEmpty(os.Getenv("MORA_GITHUB_TOKEN"), os.Getenv("GITHUB_TOKEN"), os.Getenv("GH_TOKEN"))
	source, err := newAppReleaseSource(token)
	if err != nil {
		return failBeforeSwap("download_failed", fmt.Errorf("setting up app release source: %w", err))
	}
	candidate, found, err := detectLatestAppRelease(ctx, source, arch)
	if err != nil {
		return failBeforeSwap("download_failed", fmt.Errorf("checking signed app release: %w", err))
	}
	if !found || candidate.version != version || !canonicalStableVersion(candidate.version) {
		return failBeforeSwap("state_changed", fmt.Errorf("published app release changed before apply; planned %s", version))
	}

	parent := filepath.Dir(appRoot)
	stageDir, err := os.MkdirTemp(parent, ".mora-app-unattended.")
	if err != nil {
		return deferApply("app_unwritable")
	}
	preserveStage := false
	defer func() {
		if !preserveStage {
			_ = os.RemoveAll(stageDir)
		}
	}()
	archivePath := filepath.Join(stageDir, candidate.assetName)
	checksumPath := filepath.Join(stageDir, moraAppChecksumFilename)
	if err := downloadAppReleaseFile(ctx, candidate.assetURL, token, archivePath, maxMoraAppArchiveBytes); err != nil {
		return failBeforeSwap("download_failed", fmt.Errorf("downloading signed app: %w", err))
	}
	if err := downloadAppReleaseFile(ctx, candidate.checksumURL, token, checksumPath, maxChecksumBytes); err != nil {
		return failBeforeSwap("download_failed", fmt.Errorf("downloading app checksum: %w", err))
	}
	if err := unattendedVerifyArchive(archivePath, checksumPath, candidate.assetName); err != nil {
		return failBeforeSwap("download_failed", err)
	}
	if err := unattendedFailpoint("after_download"); err != nil {
		return failBeforeSwap("failpoint", err)
	}
	extractDir := filepath.Join(stageDir, "expanded")
	if err := os.Mkdir(extractDir, 0o700); err != nil {
		return failBeforeSwap("staged_verify_failed", err)
	}
	stagedApp, err := unattendedExtractApp(ctx, archivePath, extractDir)
	if err != nil {
		return failBeforeSwap("staged_verify_failed", err)
	}
	if err := unattendedVerifyApp(ctx, stagedApp, candidate.version, arch); err != nil {
		return failBeforeSwap("staged_verify_failed", fmt.Errorf("staged Mora.app verification failed: %w", err))
	}
	if err := unattendedFailpoint("after_staged_verify"); err != nil {
		return failBeforeSwap("failpoint", err)
	}
	// Plan/apply fence: identity and health are observations, never capabilities.
	// Re-run both immediately before the only mutating operation.
	if err := unattendedVerifyApp(ctx, appRoot, currentVersion, arch); err != nil {
		return failBeforeSwap("state_changed", fmt.Errorf("installed app changed before swap: %w", err))
	}
	if err := unattendedHealthCheck(ctx, cfg); err != nil {
		return deferApply("unsafe_health")
	}
	if err := unattendedFailpoint("before_swap"); err != nil {
		return failBeforeSwap("failpoint", err)
	}
	if err := unattendedSwapApps(appRoot, stagedApp); err != nil {
		return failBeforeSwap("swap_failed", err)
	}

	rollback := func(code string, cause error) error {
		receipt.ApplyErrorCode = code
		if receipt.RebuildOutcome == "" {
			receipt.RebuildOutcome = "not_run"
		}
		persistFailure := func(original error) error {
			if saveErr := saveUpdateReceipt(cfg, *receipt); saveErr != nil {
				return errors.Join(original, fmt.Errorf("persisting rollback evidence: %w", saveErr))
			}
			return original
		}
		if rollbackErr := unattendedSwapApps(appRoot, stagedApp); rollbackErr != nil {
			preserveStage = true
			receipt.ApplyOutcome = "rollback_failed"
			receipt.RollbackOutcome = "failed"
			failure := errors.Join(
				fmt.Errorf("unattended update failed: %w", cause),
				fmt.Errorf("rollback swap failed; previous app preserved at %s: %w", stagedApp, rollbackErr),
			)
			return persistFailure(failure)
		}
		receipt.ApplyOutcome = "rolled_back"
		receipt.RollbackOutcome = "succeeded"
		if verifyErr := unattendedVerifyApp(ctx, appRoot, currentVersion, arch); verifyErr != nil {
			preserveStage = true
			receipt.ApplyOutcome = "rollback_failed"
			receipt.RollbackOutcome = "failed"
			return persistFailure(errors.Join(cause, fmt.Errorf("rollback restored an unverifiable app; recovery app preserved at %s: %w", stagedApp, verifyErr)))
		}
		if receipt.RebuildOutcome == "succeeded" {
			if _, repairErr := unattendedRebuild(ctx, filepath.Join(appRoot, "Contents", "MacOS", "mora")); repairErr != nil {
				preserveStage = true
				receipt.ApplyOutcome = "rollback_failed"
				receipt.ApplyErrorCode = "rollback_rebuild_failed"
				receipt.RollbackOutcome = "failed"
				return persistFailure(errors.Join(cause, fmt.Errorf("app rollback succeeded but old index compatibility repair failed; recovery app preserved at %s: %w", stagedApp, repairErr)))
			}
		}
		return persistFailure(cause)
	}

	if err := unattendedFailpoint("after_swap"); err != nil {
		return rollback("failpoint", err)
	}
	if err := unattendedVerifyApp(ctx, appRoot, candidate.version, arch); err != nil {
		return rollback("post_swap_verify_failed", err)
	}
	newExecutable := filepath.Join(appRoot, "Contents", "MacOS", "mora")
	rebuilt, err := unattendedRebuild(ctx, newExecutable)
	if err != nil {
		receipt.RebuildOutcome = "failed"
		return rollback("rebuild_failed", err)
	}
	if rebuilt {
		receipt.RebuildOutcome = "succeeded"
	} else {
		receipt.RebuildOutcome = "not_needed"
	}
	if err := unattendedPostHealthCheck(ctx, newExecutable); err != nil {
		return rollback("post_health_failed", err)
	}
	if err := unattendedFailpoint("after_post_health"); err != nil {
		return rollback("failpoint", err)
	}
	receipt.ApplyOutcome = "updated"
	receipt.ApplyErrorCode = ""
	receipt.RollbackOutcome = "not_needed"
	receipt.AppliedAt = updatePolicyClock().UTC().Format(time.RFC3339)
	receipt.UpdateAvailable = false
	receipt.NotificationErrorCode = ""
	// A prior availability reminder is not post-apply evidence. Clear it before
	// persisting success; the checked completion notification below replaces it.
	receipt.LastNotifiedAt = ""
	receipt.LastNotifiedVersion = ""
	if err := saveUpdateReceipt(cfg, *receipt); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "✓ automatically updated Mora.app to %s\n", candidate.version)
	return notifyAutomaticUpdateSuccess(cfg, receipt, candidate.version, updatePolicyClock().UTC())
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

func startApplyAttempt(receipt *updateReceipt, version string, now time.Time) {
	receipt.ApplyVersion = version
	receipt.ApplyAttemptAt = now.Format(time.RFC3339)
	receipt.AppliedAt = ""
	receipt.ApplyOutcome = "in_progress"
	receipt.ApplyErrorCode = ""
	receipt.RollbackOutcome = ""
	receipt.RebuildOutcome = ""
}

func healthCheckWithNewBinary(ctx context.Context, executable string) error {
	cmd := exec.CommandContext(ctx, executable, "doctor", "--strict", "--json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("post-update health check failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func rebuildIndexWithNewBinary(ctx context.Context, executable string) (bool, error) {
	cmd := exec.CommandContext(ctx, executable, "index", "rebuild", "--if-needed")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("post-update index check failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return !strings.Contains(string(output), "rebuild not needed"), nil
}
