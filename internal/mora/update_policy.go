package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	updateReceiptSchema   = 1
	updateReminderEvery   = 72 * time.Hour
	updateReceiptMaxBytes = 64 << 10
	updateClockSkew       = 5 * time.Minute
)

var canonicalStableTagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func parseUpdatePolicy(raw string) (updatePolicy, error) {
	p := updatePolicy(strings.ToLower(strings.TrimSpace(raw)))
	switch p {
	case updatePolicyAuto, updatePolicyNotify, updatePolicyOff:
		return p, nil
	default:
		return "", fmt.Errorf("unknown update policy %q (want auto, notify, or off)", raw)
	}
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
		if _, app := moraAppRoot(exe); app {
			return resolvedUpdatePolicy{Policy: updatePolicyAuto, Reason: "mora_app_path"}
		}
	}
	return resolvedUpdatePolicy{Policy: updatePolicyNotify, Reason: "released_binary"}
}

type updateReceipt struct {
	SchemaVersion         int    `json:"schema_version"`
	LastAttemptAt         string `json:"last_attempt_at,omitempty"`
	LastSuccessAt         string `json:"last_success_at,omitempty"`
	LastErrorCode         string `json:"last_error_code,omitempty"`
	LatestVersion         string `json:"latest_version,omitempty"`
	UpdateAvailable       bool   `json:"update_available"`
	LastNotifiedAt        string `json:"last_notified_at,omitempty"`
	LastNotifiedVersion   string `json:"last_notified_version,omitempty"`
	NotificationErrorCode string `json:"notification_error_code,omitempty"`
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
	RecoveryCommand       string `json:"recovery_command,omitempty"`
}

func updateReceiptPath(cfg Config) string {
	return filepath.Join(cfg.StateDir, "update", "status.json")
}

func loadUpdateReceipt(cfg Config) (updateReceipt, error) {
	var receipt updateReceipt
	body, err := os.ReadFile(updateReceiptPath(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return updateReceipt{SchemaVersion: updateReceiptSchema}, nil
	}
	if err != nil {
		return receipt, err
	}
	if len(body) > updateReceiptMaxBytes {
		return receipt, fmt.Errorf("update status exceeds %d bytes", updateReceiptMaxBytes)
	}
	if err := rejectDuplicateReceiptKeys(body); err != nil {
		return receipt, fmt.Errorf("reading update status: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		return receipt, fmt.Errorf("reading update status: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return receipt, fmt.Errorf("reading update status: multiple JSON values")
		}
		return receipt, fmt.Errorf("reading update status: %w", err)
	}
	if err := validateUpdateReceipt(receipt, updatePolicyClock().UTC()); err != nil {
		return receipt, fmt.Errorf("reading update status: %w", err)
	}
	return receipt, nil
}

func rejectDuplicateReceiptKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	opening, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := opening.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("update status must be one JSON object")
	}
	seen := map[string]struct{}{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("update status contains a non-string key")
		}
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("update status contains duplicate field %q", key)
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := closing.(json.Delim); !ok || delim != '}' {
		return fmt.Errorf("update status has an invalid object terminator")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("update status contains multiple JSON values")
		}
		return err
	}
	return nil
}

func saveUpdateReceipt(cfg Config, receipt updateReceipt) error {
	receipt.SchemaVersion = updateReceiptSchema
	if err := validateUpdateReceipt(receipt, updatePolicyClock().UTC()); err != nil {
		return fmt.Errorf("refusing invalid update status: %w", err)
	}
	body, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if len(body) > updateReceiptMaxBytes {
		return fmt.Errorf("update status exceeds %d bytes", updateReceiptMaxBytes)
	}
	return atomicWriteDurable(updateReceiptPath(cfg), body, 0o600)
}

func validateUpdateReceipt(receipt updateReceipt, now time.Time) error {
	if receipt.SchemaVersion != updateReceiptSchema {
		return fmt.Errorf("unsupported schema %d", receipt.SchemaVersion)
	}
	codes := []struct {
		field, value, allowed string
	}{
		{"last_error_code", receipt.LastErrorCode, "check_failed"},
		{"notification_error_code", receipt.NotificationErrorCode, "notification_failed"},
	}
	for _, code := range codes {
		if code.value != "" && code.value != code.allowed {
			return fmt.Errorf("%s contains unknown code %q", code.field, code.value)
		}
	}
	versions := []struct{ field, value string }{
		{"latest_version", receipt.LatestVersion},
		{"last_notified_version", receipt.LastNotifiedVersion},
	}
	for _, version := range versions {
		if version.value != "" && !canonicalStableVersion(version.value) {
			return fmt.Errorf("%s %q is not canonical stable semver", version.field, version.value)
		}
	}
	if receipt.UpdateAvailable && receipt.LatestVersion == "" {
		return fmt.Errorf("update_available requires latest_version")
	}
	if receipt.LatestVersion != "" && receipt.LastSuccessAt == "" {
		return fmt.Errorf("latest_version requires last_success_at")
	}
	if receipt.LastSuccessAt != "" && receipt.LastAttemptAt == "" {
		return fmt.Errorf("last_success_at requires last_attempt_at")
	}
	if receipt.LastErrorCode != "" && receipt.LastAttemptAt == "" {
		return fmt.Errorf("last_error_code requires last_attempt_at")
	}
	if (receipt.LastNotifiedAt == "") != (receipt.LastNotifiedVersion == "") {
		return fmt.Errorf("last_notified_at and last_notified_version must appear together")
	}
	if receipt.NotificationErrorCode != "" && !receipt.UpdateAvailable {
		return fmt.Errorf("notification_error_code requires an available update")
	}

	timestamps := []struct{ field, value string }{
		{"last_attempt_at", receipt.LastAttemptAt},
		{"last_success_at", receipt.LastSuccessAt},
		{"last_notified_at", receipt.LastNotifiedAt},
	}
	parsed := make(map[string]time.Time, len(timestamps))
	for _, timestamp := range timestamps {
		if timestamp.value == "" {
			continue
		}
		value, err := time.Parse(time.RFC3339, timestamp.value)
		if err != nil {
			return fmt.Errorf("%s is not RFC3339", timestamp.field)
		}
		if value.After(now.Add(updateClockSkew)) {
			return fmt.Errorf("%s is too far in the future", timestamp.field)
		}
		parsed[timestamp.field] = value
	}
	if success, ok := parsed["last_success_at"]; ok {
		if attempt := parsed["last_attempt_at"]; success.After(attempt) {
			return fmt.Errorf("last_success_at is after last_attempt_at")
		}
	}
	if notified, ok := parsed["last_notified_at"]; ok {
		if attempt, present := parsed["last_attempt_at"]; !present || notified.After(attempt) {
			return fmt.Errorf("last_notified_at requires an equal or later last_attempt_at")
		}
	}
	return nil
}

func canonicalStableVersion(version string) bool {
	parsed, err := semver.StrictNewVersion(version)
	return err == nil && parsed.Prerelease() == "" && parsed.Metadata() == "" && parsed.String() == version
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
		RecoveryCommand:       updateRecoveryCommand(),
	}, nil
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
	receipt, err := loadUpdateReceipt(cfg)
	if err != nil {
		return err
	}
	receipt.LastAttemptAt = now.Format(time.RFC3339)
	result, err := updateCheckLatest(ctx)
	if err != nil {
		receipt.LastErrorCode = "check_failed"
		if saveErr := saveUpdateReceipt(cfg, receipt); saveErr != nil {
			return saveErr
		}
		return fmt.Errorf("checking for updates failed; cached availability was preserved: %w", err)
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
		fmt.Fprintf(stdout, "run `%s` to install it\n", updateRecoveryCommand())
	} else if result.Found {
		fmt.Fprintf(stdout, "mora is up to date (%s)\n", BuildVersion)
	} else {
		fmt.Fprintln(stdout, "no published stable releases found")
	}
	if scheduled && receipt.UpdateAvailable {
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
	script := `display notification "Mora ` + escapeAppleScriptString(receipt.LatestVersion) + ` is available; run ` + escapeAppleScriptString(updateRecoveryCommand()) + `" with title "Mora · Update available"`
	if err := updateNotificationRun("-e", script); err != nil {
		receipt.NotificationErrorCode = "notification_failed"
		if saveErr := saveUpdateReceipt(cfg, *receipt); saveErr != nil {
			return saveErr
		}
		return fmt.Errorf("update is available but notification failed: %w", err)
	}
	receipt.LastNotifiedAt = now.Format(time.RFC3339)
	receipt.LastNotifiedVersion = receipt.LatestVersion
	receipt.NotificationErrorCode = ""
	return saveUpdateReceipt(cfg, *receipt)
}
