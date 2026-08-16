package mora

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	configstore "github.com/pyranthus-hq/mora/internal/config"
	updatepkg "github.com/pyranthus-hq/mora/internal/update"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/creativeprojects/go-selfupdate"
)

type updatePolicy string

const (
	updatePolicyAuto   updatePolicy = "auto"
	updatePolicyNotify updatePolicy = "notify"
	updatePolicyOff    updatePolicy = "off"

	updateReminderEvery = 72 * time.Hour
)

var canonicalStableTagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type updateReceipt = updatepkg.Receipt

const (
	updateReceiptSchema   = updatepkg.SchemaVersion
	updateReceiptMaxBytes = updatepkg.MaxReceiptBytes
	updateClockSkew       = updatepkg.ClockSkew
)

func updateReceiptStore(cfg Config) updatepkg.Store {
	return updatepkg.Store{StateDir: cfg.StateDir, Now: updatePolicyClock}
}
func updateReceiptPath(cfg Config) string                 { return updateReceiptStore(cfg).Path() }
func loadUpdateReceipt(cfg Config) (updateReceipt, error) { return updateReceiptStore(cfg).Load() }
func saveUpdateReceipt(cfg Config, r updateReceipt) error { return updateReceiptStore(cfg).Save(r) }
func canonicalStableVersion(v string) bool                { return updatepkg.CanonicalStableVersion(v) }

func parseUpdatePolicy(raw string) (updatePolicy, error) {
	p, err := configstore.ParseUpdatePolicy(raw)
	return updatePolicy(p), err
}

type resolvedUpdatePolicy struct {
	Policy updatePolicy
	Reason string
}

var (
	updatePolicyClock      = time.Now
	updatePolicyExecutable = os.Executable
	updatePolicyEvalLinks  = filepath.EvalSymlinks
	updateCheckLatest      = checkLatestStableRelease
	updateNotificationRun  = osascriptRunner
)

func resolveUpdatePolicy(cfg Config) resolvedUpdatePolicy {
	if cfg.UpdatePolicy != "" {
		p, err := parseUpdatePolicy(cfg.UpdatePolicy)
		if err == nil {
			return resolvedUpdatePolicy{Policy: p, Reason: "configured"}
		}
	}
	if BuildVersion == "" || BuildVersion == "dev" {
		return resolvedUpdatePolicy{Policy: updatePolicyOff, Reason: "source_build"}
	}
	if _, local := localBuildBase(BuildVersion); local {
		return resolvedUpdatePolicy{Policy: updatePolicyOff, Reason: "local_build"}
	}
	exe, err := updatePolicyExecutable()
	if err == nil {
		if resolved, resolveErr := updatePolicyEvalLinks(exe); resolveErr == nil {
			exe = resolved
		}
		if classifyUpgradeInstall(exe) == upgradeRouteApp {
			return resolvedUpdatePolicy{Policy: updatePolicyAuto, Reason: "mora_app_path"}
		}
	}
	return resolvedUpdatePolicy{Policy: updatePolicyNotify, Reason: "released_binary"}
}

type updateStatus struct {
	SchemaVersion         int    `json:"schema_version"`
	ConfiguredPolicy      string `json:"configured_policy"`
	ResolvedPolicy        string `json:"resolved_policy"`
	PolicyReason          string `json:"policy_reason"`
	InstalledVersion      string `json:"installed_version"`
	LatestVersion         string `json:"latest_version,omitempty"`
	UpdateAvailable       bool   `json:"update_available"`
	LastAttemptAt         string `json:"last_attempt_at,omitempty"`
	LastSuccessAt         string `json:"last_success_at,omitempty"`
	LastErrorCode         string `json:"last_error_code,omitempty"`
	LastNotifiedAt        string `json:"last_notified_at,omitempty"`
	NotificationErrorCode string `json:"notification_error_code,omitempty"`
	ApplyVersion          string `json:"apply_version,omitempty"`
	ApplyAttemptAt        string `json:"apply_attempt_at,omitempty"`
	AppliedAt             string `json:"applied_at,omitempty"`
	ApplyOutcome          string `json:"apply_outcome,omitempty"`
	ApplyErrorCode        string `json:"apply_error_code,omitempty"`
	RollbackOutcome       string `json:"rollback_outcome,omitempty"`
	RebuildOutcome        string `json:"rebuild_outcome,omitempty"`
	RecoveryCommand       string `json:"recovery_command,omitempty"`
}

func currentUpdateStatus(cfg Config) (updateStatus, error) {
	receipt, err := loadUpdateReceipt(cfg)
	if err != nil {
		return updateStatus{}, err
	}
	resolved := resolveUpdatePolicy(cfg)
	configured := cfg.UpdatePolicy
	if configured == "" {
		configured = "default"
	}
	return updateStatus{
		SchemaVersion:         updateReceiptSchema,
		ConfiguredPolicy:      configured,
		ResolvedPolicy:        string(resolved.Policy),
		PolicyReason:          resolved.Reason,
		InstalledVersion:      BuildVersion,
		LatestVersion:         receipt.LatestVersion,
		UpdateAvailable:       receipt.UpdateAvailable,
		LastAttemptAt:         receipt.LastAttemptAt,
		LastSuccessAt:         receipt.LastSuccessAt,
		LastErrorCode:         receipt.LastErrorCode,
		LastNotifiedAt:        receipt.LastNotifiedAt,
		NotificationErrorCode: receipt.NotificationErrorCode,
		ApplyVersion:          receipt.ApplyVersion,
		ApplyAttemptAt:        receipt.ApplyAttemptAt,
		AppliedAt:             receipt.AppliedAt,
		ApplyOutcome:          receipt.ApplyOutcome,
		ApplyErrorCode:        receipt.ApplyErrorCode,
		RollbackOutcome:       receipt.RollbackOutcome,
		RebuildOutcome:        receipt.RebuildOutcome,
		RecoveryCommand:       updateRecoveryCommandForReceipt(receipt),
	}, nil
}

func updateRecoveryCommandForReceipt(receipt updateReceipt) string {
	if receipt.ApplyOutcome == "deferred" && receipt.ApplyErrorCode == "app_unwritable" {
		return "brew upgrade --cask --greedy pyranthus-hq/tap/mora"
	}
	return updateRecoveryCommand()
}

func updateRecoveryCommand() string {
	if BuildVersion == "" || BuildVersion == "dev" {
		return "git pull && go build ./cmd/mora"
	}
	exe, err := updatePolicyExecutable()
	if err == nil {
		if resolved, resolveErr := updatePolicyEvalLinks(exe); resolveErr == nil {
			exe = resolved
		}
		if isHomebrewManaged(exe) {
			return "brew upgrade pyranthus-hq/tap/mora"
		}
	}
	return "mora upgrade"
}

func cmdUpgradeStatus(cfg Config, jsonOut bool, stdout io.Writer) error {
	status, err := currentUpdateStatus(cfg)
	if err != nil {
		return err
	}
	if jsonOut {
		body, err := json.MarshalIndent(status, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, string(body))
		return nil
	}
	fmt.Fprintf(stdout, "update policy: %s (%s)\n", status.ResolvedPolicy, status.PolicyReason)
	if status.LastAttemptAt == "" {
		fmt.Fprintln(stdout, "last check: never")
	} else {
		fmt.Fprintf(stdout, "last check: %s", status.LastAttemptAt)
		if status.LastErrorCode != "" {
			fmt.Fprintf(stdout, " (%s)", status.LastErrorCode)
		}
		fmt.Fprintln(stdout)
	}
	if status.ApplyOutcome != "" {
		fmt.Fprintf(stdout, "last automatic apply: %s", status.ApplyOutcome)
		if status.ApplyVersion != "" {
			fmt.Fprintf(stdout, " (%s)", status.ApplyVersion)
		}
		if status.ApplyErrorCode != "" {
			fmt.Fprintf(stdout, " [%s]", status.ApplyErrorCode)
		}
		fmt.Fprintln(stdout)
	}
	if status.UpdateAvailable {
		fmt.Fprintf(stdout, "update available: %s → %s\n", status.InstalledVersion, status.LatestVersion)
		fmt.Fprintf(stdout, "recovery: %s\n", status.RecoveryCommand)
	} else if status.LatestVersion != "" {
		fmt.Fprintf(stdout, "latest checked release: %s\n", status.LatestVersion)
	}
	return nil
}

type stableReleaseResult struct {
	Version string
	Found   bool
}

func checkLatestStableRelease(ctx context.Context) (stableReleaseResult, error) {
	token := firstNonEmpty(os.Getenv("MORA_GITHUB_TOKEN"), os.Getenv("GITHUB_TOKEN"), os.Getenv("GH_TOKEN"))
	source, err := selfupdate.NewGitHubSource(selfupdate.GitHubConfig{APIToken: token})
	if err != nil {
		return stableReleaseResult{}, err
	}
	releases, err := source.ListReleases(ctx, selfupdate.NewRepositorySlug(upgradeRepoOwner, upgradeRepoName))
	if err != nil {
		return stableReleaseResult{}, err
	}
	return selectLatestStableRelease(releases)
}

func selectLatestStableRelease(releases []selfupdate.SourceRelease) (stableReleaseResult, error) {
	type candidate struct {
		version *semver.Version
		text    string
	}
	var candidates []candidate
	for _, release := range releases {
		if release.GetDraft() || release.GetPrerelease() {
			continue
		}
		match := canonicalStableTagPattern.FindStringSubmatch(release.GetTagName())
		if match == nil {
			continue
		}
		versionText := strings.TrimPrefix(release.GetTagName(), "v")
		version, err := semver.StrictNewVersion(versionText)
		if err != nil || version.Prerelease() != "" || version.Metadata() != "" {
			continue
		}
		candidates = append(candidates, candidate{version: version, text: versionText})
	}
	if len(candidates) == 0 {
		return stableReleaseResult{}, nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].version.GreaterThan(candidates[j].version) })
	return stableReleaseResult{Version: candidates[0].text, Found: true}, nil
}

func runUpdateCheck(ctx context.Context, cfg Config, scheduled bool, stdout io.Writer) error {
	resolved := resolveUpdatePolicy(cfg)
	if resolved.Policy == updatePolicyOff {
		fmt.Fprintln(stdout, "automatic update checks are off; run `mora upgrade --policy notify` or `mora upgrade --policy auto` to enable them")
		return nil
	}
	now := updatePolicyClock().UTC()
	if scheduled {
		release, err := acquireUpdateLease(cfg, now)
		if err != nil {
			return err
		}
		defer release()
	}
	receipt, err := loadUpdateReceipt(cfg)
	if err != nil {
		return err
	}
	receipt.LastAttemptAt = now.Format(time.RFC3339)
	result, err := updateCheckLatest(ctx)
	if err != nil {
		receipt.LastErrorCode = "check_failed"
		checkErr := fmt.Errorf("checking for updates failed; cached availability was preserved: %w", err)
		if saveErr := saveUpdateReceipt(cfg, receipt); saveErr != nil {
			return errors.Join(checkErr, fmt.Errorf("persisting check failure receipt: %w", saveErr))
		}
		return checkErr
	}
	receipt.LastSuccessAt = now.Format(time.RFC3339)
	receipt.LastErrorCode = ""
	if !result.Found {
		receipt.LatestVersion = ""
		receipt.UpdateAvailable = false
	} else {
		receipt.LatestVersion = result.Version
		receipt.UpdateAvailable = updateIsAvailable(BuildVersion, result.Version)
	}
	if !receipt.UpdateAvailable {
		receipt.NotificationErrorCode = ""
	}
	if err := saveUpdateReceipt(cfg, receipt); err != nil {
		return err
	}
	if receipt.UpdateAvailable {
		fmt.Fprintf(stdout, "update available: %s → %s\n", BuildVersion, receipt.LatestVersion)
		if scheduled && resolved.Policy == updatePolicyAuto {
			fmt.Fprintln(stdout, "verifying safe automatic Mora.app apply …")
		} else {
			fmt.Fprintf(stdout, "run `%s` to install it\n", updateRecoveryCommand())
		}
	} else if result.Found {
		fmt.Fprintf(stdout, "mora is up to date (%s)\n", BuildVersion)
	} else {
		fmt.Fprintln(stdout, "no published stable releases found")
	}
	if scheduled && receipt.UpdateAvailable {
		if resolved.Policy == updatePolicyAuto {
			return unattendedUpdateApply(ctx, cfg, &receipt, now, stdout)
		}
		return maybeNotifyUpdate(cfg, &receipt, now)
	}
	return nil
}

func updateIsAvailable(current, latest string) bool {
	if current == "" || current == "dev" {
		return true
	}
	verdict, _, err := decideUpgrade(current, latest)
	return err == nil && verdict == verdictUpgrade
}

func maybeNotifyUpdate(cfg Config, receipt *updateReceipt, now time.Time) error {
	if !shouldNotify(runtimeGOOS()) {
		return nil
	}
	if receipt.LastNotifiedVersion == receipt.LatestVersion && receipt.LastNotifiedAt != "" {
		if last, err := time.Parse(time.RFC3339, receipt.LastNotifiedAt); err == nil && !last.After(now) && now.Sub(last) < updateReminderEvery {
			return nil
		}
	}
	script := `display notification "Mora ` + escapeAppleScriptString(receipt.LatestVersion) + ` is available; run ` + escapeAppleScriptString(updateRecoveryCommandForReceipt(*receipt)) + `" with title "Mora · Update available"`
	if err := updateNotificationRun("-e", script); err != nil {
		receipt.NotificationErrorCode = "notification_failed"
		notifyErr := fmt.Errorf("update is available but notification failed: %w", err)
		if saveErr := saveUpdateReceipt(cfg, *receipt); saveErr != nil {
			return errors.Join(notifyErr, fmt.Errorf("persisting notification failure receipt: %w", saveErr))
		}
		return notifyErr
	}
	receipt.LastNotifiedAt = now.Format(time.RFC3339)
	receipt.LastNotifiedVersion = receipt.LatestVersion
	receipt.NotificationErrorCode = ""
	return saveUpdateReceipt(cfg, *receipt)
}
