package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/leasefile"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const LeaseTTL = 2 * time.Hour

type leaseBody struct {
	PID        int    `json:"pid"`
	AcquiredAt string `json:"acquired_at"`
}

func LeasePath(stateDir string) string { return filepath.Join(stateDir, "update", "update.lock") }
func AcquireLease(stateDir string, now time.Time) (func(), error) {
	path := LeasePath(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	body, err := json.Marshal(leaseBody{os.Getpid(), now.UTC().Format(time.RFC3339)})
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		published, err := leasefile.Publish(path, body)
		if err != nil {
			return nil, err
		}
		if published {
			return leasefile.Releaser(path, body, leasefile.DefaultRemovalOptions()), nil
		}
		reaped, err := leasefile.Reap(path, now, LeaseTTL, leasefile.DefaultRemovalOptions())
		if err != nil {
			return nil, err
		}
		if !reaped {
			return nil, fmt.Errorf("another Mora update is active; retry later")
		}
	}
	return nil, fmt.Errorf("another Mora update is active; retry later")
}

type Candidate struct {
	Version, AssetName, AssetURL, ChecksumURL string
	ArchiveLimit, ChecksumLimit               int64
}
type ApplyOptions struct {
	Store                        Store
	Receipt                      *Receipt
	Now                          time.Time
	CurrentVersion, GOOS, GOARCH string
	Stdout                       io.Writer
	Executable                   func() (string, error)
	EvalSymlinks                 func(string) (string, error)
	AppRoot                      func(string) (string, bool)
	VerifyApp                    func(context.Context, string, string, string) error
	Health                       func(context.Context) error
	PostHealth                   func(context.Context, string) error
	Writable                     func(string) bool
	Discover                     func(context.Context, string) (Candidate, bool, error)
	Download                     func(context.Context, string, string, int64) error
	VerifyArchive                func(string, string, string) error
	Extract                      func(context.Context, string, string) (string, error)
	Swap                         func(string, string) error
	Rebuild                      func(context.Context, string) (bool, error)
	Failpoint                    func(string) error
	NotifyAvailability           func(*Receipt, time.Time) error
	NotifySuccess                func(*Receipt, string, time.Time) error
	Clock                        func() time.Time
	ChecksumFilename             string
}

func AppParentWritable(appRoot string) bool {
	probe, err := os.MkdirTemp(filepath.Dir(appRoot), ".mora-update-write-probe.")
	if err != nil {
		return false
	}
	return os.Remove(probe) == nil
}
func Apply(ctx context.Context, o ApplyOptions) error {
	receipt, now, stdout := o.Receipt, o.Now, o.Stdout
	version := receipt.LatestVersion
	if receipt.ApplyVersion == version && receipt.ApplyOutcome == "deferred" && receipt.ApplyErrorCode == "app_unwritable" {
		return o.NotifyAvailability(receipt, now)
	}
	startApplyAttempt(receipt, version, now)
	if err := o.Store.Save(*receipt); err != nil {
		return err
	}
	deferApply := func(code string) error {
		receipt.ApplyOutcome = "deferred"
		receipt.ApplyErrorCode = code
		receipt.RollbackOutcome = "not_needed"
		receipt.RebuildOutcome = "not_run"
		if err := o.Store.Save(*receipt); err != nil {
			return err
		}
		return o.NotifyAvailability(receipt, now)
	}
	failBeforeSwap := func(code string, cause error) error {
		receipt.ApplyOutcome = "failed_before_swap"
		receipt.ApplyErrorCode = code
		receipt.RollbackOutcome = "not_needed"
		receipt.RebuildOutcome = "not_run"
		if err := o.Store.Save(*receipt); err != nil {
			return errors.Join(cause, fmt.Errorf("persisting pre-swap failure receipt: %w", err))
		}
		return cause
	}

	if o.GOOS != "darwin" || o.GOARCH != "arm64" && o.GOARCH != "amd64" {
		return deferApply("not_verified_app")
	}
	exe, err := o.Executable()
	if err != nil {
		return failBeforeSwap("not_verified_app", fmt.Errorf("locating executable: %w", err))
	}
	if resolved, err := o.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	appRoot, ok := o.AppRoot(exe)
	if !ok {
		return deferApply("not_verified_app")
	}
	arch := o.GOARCH
	currentVersion := strings.TrimPrefix(o.CurrentVersion, "v")
	if !CanonicalStableVersion(currentVersion) {
		return deferApply("not_verified_app")
	}
	if err := o.VerifyApp(ctx, appRoot, currentVersion, arch); err != nil {
		return failBeforeSwap("installed_verify_failed", fmt.Errorf("installed Mora.app verification failed: %w", err))
	}
	if err := o.Failpoint("after_installed_verify"); err != nil {
		return failBeforeSwap("failpoint", err)
	}
	if err := o.Health(ctx); err != nil {
		return deferApply("unsafe_health")
	}
	if !o.Writable(appRoot) {
		return deferApply("app_unwritable")
	}

	candidate, found, err := o.Discover(ctx, arch)
	if err != nil {
		return failBeforeSwap("download_failed", err)
	}
	if !found || candidate.Version != version || !CanonicalStableVersion(candidate.Version) {
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
	archivePath := filepath.Join(stageDir, candidate.AssetName)
	checksumPath := filepath.Join(stageDir, o.ChecksumFilename)
	if err := o.Download(ctx, candidate.AssetURL, archivePath, candidate.ArchiveLimit); err != nil {
		return failBeforeSwap("download_failed", fmt.Errorf("downloading signed app: %w", err))
	}
	if err := o.Download(ctx, candidate.ChecksumURL, checksumPath, candidate.ChecksumLimit); err != nil {
		return failBeforeSwap("download_failed", fmt.Errorf("downloading app checksum: %w", err))
	}
	if err := o.VerifyArchive(archivePath, checksumPath, candidate.AssetName); err != nil {
		return failBeforeSwap("download_failed", err)
	}
	if err := o.Failpoint("after_download"); err != nil {
		return failBeforeSwap("failpoint", err)
	}
	extractDir := filepath.Join(stageDir, "expanded")
	if err := os.Mkdir(extractDir, 0o700); err != nil {
		return failBeforeSwap("staged_verify_failed", err)
	}
	stagedApp, err := o.Extract(ctx, archivePath, extractDir)
	if err != nil {
		return failBeforeSwap("staged_verify_failed", err)
	}
	if err := o.VerifyApp(ctx, stagedApp, candidate.Version, arch); err != nil {
		return failBeforeSwap("staged_verify_failed", fmt.Errorf("staged Mora.app verification failed: %w", err))
	}
	if err := o.Failpoint("after_staged_verify"); err != nil {
		return failBeforeSwap("failpoint", err)
	}
	// Plan/apply fence: identity and health are observations, never capabilities.
	// Re-run both immediately before the only mutating operation.
	if err := o.VerifyApp(ctx, appRoot, currentVersion, arch); err != nil {
		return failBeforeSwap("state_changed", fmt.Errorf("installed app changed before swap: %w", err))
	}
	if err := o.Health(ctx); err != nil {
		return deferApply("unsafe_health")
	}
	if err := o.Failpoint("before_swap"); err != nil {
		return failBeforeSwap("failpoint", err)
	}
	if err := o.Swap(appRoot, stagedApp); err != nil {
		return failBeforeSwap("swap_failed", err)
	}

	rollback := func(code string, cause error) error {
		receipt.ApplyErrorCode = code
		if receipt.RebuildOutcome == "" {
			receipt.RebuildOutcome = "not_run"
		}
		persistFailure := func(original error) error {
			if saveErr := o.Store.Save(*receipt); saveErr != nil {
				return errors.Join(original, fmt.Errorf("persisting rollback evidence: %w", saveErr))
			}
			return original
		}
		if rollbackErr := o.Swap(appRoot, stagedApp); rollbackErr != nil {
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
		if verifyErr := o.VerifyApp(ctx, appRoot, currentVersion, arch); verifyErr != nil {
			preserveStage = true
			receipt.ApplyOutcome = "rollback_failed"
			receipt.RollbackOutcome = "failed"
			return persistFailure(errors.Join(cause, fmt.Errorf("rollback restored an unverifiable app; recovery app preserved at %s: %w", stagedApp, verifyErr)))
		}
		if receipt.RebuildOutcome == "succeeded" {
			if _, repairErr := o.Rebuild(ctx, filepath.Join(appRoot, "Contents", "MacOS", "mora")); repairErr != nil {
				preserveStage = true
				receipt.ApplyOutcome = "rollback_failed"
				receipt.ApplyErrorCode = "rollback_rebuild_failed"
				receipt.RollbackOutcome = "failed"
				return persistFailure(errors.Join(cause, fmt.Errorf("app rollback succeeded but old index compatibility repair failed; recovery app preserved at %s: %w", stagedApp, repairErr)))
			}
		}
		return persistFailure(cause)
	}

	if err := o.Failpoint("after_swap"); err != nil {
		return rollback("failpoint", err)
	}
	if err := o.VerifyApp(ctx, appRoot, candidate.Version, arch); err != nil {
		return rollback("post_swap_verify_failed", err)
	}
	newExecutable := filepath.Join(appRoot, "Contents", "MacOS", "mora")
	rebuilt, err := o.Rebuild(ctx, newExecutable)
	if err != nil {
		receipt.RebuildOutcome = "failed"
		return rollback("rebuild_failed", err)
	}
	if rebuilt {
		receipt.RebuildOutcome = "succeeded"
	} else {
		receipt.RebuildOutcome = "not_needed"
	}
	if err := o.PostHealth(ctx, newExecutable); err != nil {
		return rollback("post_health_failed", err)
	}
	if err := o.Failpoint("after_post_health"); err != nil {
		return rollback("failpoint", err)
	}
	receipt.ApplyOutcome = "updated"
	receipt.ApplyErrorCode = ""
	receipt.RollbackOutcome = "not_needed"
	receipt.AppliedAt = o.Clock().UTC().Format(time.RFC3339)
	receipt.UpdateAvailable = false
	receipt.NotificationErrorCode = ""
	// A prior availability reminder is not post-apply evidence. Clear it before
	// persisting success; the checked completion notification below replaces it.
	receipt.LastNotifiedAt = ""
	receipt.LastNotifiedVersion = ""
	if err := o.Store.Save(*receipt); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "✓ automatically updated Mora.app to %s\n", candidate.Version)
	return o.NotifySuccess(receipt, candidate.Version, o.Clock().UTC())
}
func startApplyAttempt(receipt *Receipt, version string, now time.Time) {
	receipt.ApplyVersion = version
	receipt.ApplyAttemptAt = now.Format(time.RFC3339)
	receipt.AppliedAt = ""
	receipt.ApplyOutcome = "in_progress"
	receipt.ApplyErrorCode = ""
	receipt.RollbackOutcome = ""
	receipt.RebuildOutcome = ""
}
func HealthCheckWithNewBinary(ctx context.Context, executable string) error {
	cmd := exec.CommandContext(ctx, executable, "doctor", "--strict", "--json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("post-update health check failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
func RebuildIndexWithNewBinary(ctx context.Context, executable string) (bool, error) {
	cmd := exec.CommandContext(ctx, executable, "index", "rebuild", "--if-needed")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("post-update index check failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return !strings.Contains(string(output), "rebuild not needed"), nil
}
