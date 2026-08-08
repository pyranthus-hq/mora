package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/creativeprojects/go-selfupdate"
)

func isolateUpdatePolicyTest(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MORA_CONFIG_DIR", dir)
	t.Setenv("MORA_NO_NOTIFY", "")
	oldVersion := BuildVersion
	oldClock := updatePolicyClock
	oldExecutable := updatePolicyExecutable
	oldEval := updatePolicyEvalLinks
	oldCheck := updateCheckLatest
	oldNotify := updateNotificationRun
	oldGOOS := runtimeGOOS
	oldMarkerSync := markerSyncFn
	t.Cleanup(func() {
		BuildVersion = oldVersion
		updatePolicyClock = oldClock
		updatePolicyExecutable = oldExecutable
		updatePolicyEvalLinks = oldEval
		updateCheckLatest = oldCheck
		updateNotificationRun = oldNotify
		runtimeGOOS = oldGOOS
		markerSyncFn = oldMarkerSync
	})
	cfg := defaultConfig()
	for _, path := range []string{cfg.ConfigDir, cfg.StateDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func TestParseUpdatePolicy(t *testing.T) {
	for _, value := range []string{"auto", "notify", "off"} {
		got, err := parseUpdatePolicy(value)
		if err != nil || string(got) != value {
			t.Fatalf("parse(%q)=%q,%v", value, got, err)
		}
	}
	if _, err := parseUpdatePolicy("always"); err == nil {
		t.Fatal("invalid policy accepted")
	}
}

func TestResolveUpdatePolicyContextDefaults(t *testing.T) {
	isolateUpdatePolicyTest(t)
	updatePolicyEvalLinks = func(path string) (string, error) { return path, nil }
	tests := []struct{ name, version, exe, policy, reason string }{
		{"source", "dev", "/tmp/mora", "off", "source_build"},
		{"local", "v1.2.3-4-gabcdef", "/tmp/mora", "off", "local_build"},
		{"signed app", "1.2.3", "/Applications/Mora.app/Contents/MacOS/mora", "auto", "mora_app_path"},
		{"release", "1.2.3", "/usr/local/bin/mora", "notify", "released_binary"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			BuildVersion = tt.version
			updatePolicyExecutable = func() (string, error) { return tt.exe, nil }
			got := resolveUpdatePolicy(Config{})
			if string(got.Policy) != tt.policy || got.Reason != tt.reason {
				t.Fatalf("got %+v", got)
			}
		})
	}
	got := resolveUpdatePolicy(Config{UpdatePolicy: "off"})
	if got.Policy != updatePolicyOff || got.Reason != "configured" {
		t.Fatalf("configured = %+v", got)
	}
}

func TestUpgradePolicyPersistsWithoutDroppingUnknownConfig(t *testing.T) {
	cfg := isolateUpdatePolicyTest(t)
	configPath := filepath.Join(cfg.ConfigDir, "config.toml")
	if err := os.WriteFile(configPath, []byte("future_key = \"keep\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := cmdUpgrade(context.Background(), []string{"--policy", "off"}, &out); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("future_key = \"keep\"")) || !bytes.Contains(body, []byte("update_policy = \"off\"")) {
		t.Fatalf("config = %s", body)
	}
	loaded, err := loadConfig()
	if err != nil || loaded.UpdatePolicy != "off" {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
}

func TestOffScheduledCheckMakesZeroNetworkAndNotifierCalls(t *testing.T) {
	cfg := isolateUpdatePolicyTest(t)
	BuildVersion = "1.0.0"
	cfg.UpdatePolicy = "off"
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	checks, notifications := 0, 0
	updateCheckLatest = func(context.Context) (stableReleaseResult, error) { checks++; return stableReleaseResult{}, nil }
	updateNotificationRun = func(...string) error { notifications++; return nil }
	runtimeGOOS = func() string { return "darwin" }
	var out bytes.Buffer
	if err := cmdUpgrade(context.Background(), []string{"--scheduled-check"}, &out); err != nil {
		t.Fatal(err)
	}
	if err := cmdUpgrade(context.Background(), []string{"--check"}, &out); err != nil {
		t.Fatal(err)
	}
	if checks != 0 || notifications != 0 {
		t.Fatalf("checks=%d notifications=%d", checks, notifications)
	}
	if _, err := os.Stat(updateReceiptPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("off wrote receipt: %v", err)
	}
}

func TestUpdateCheckCachesAvailabilityAcrossErrorsWithoutSecrets(t *testing.T) {
	cfg := isolateUpdatePolicyTest(t)
	BuildVersion = "1.0.0"
	cfg.UpdatePolicy = "notify"
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	updatePolicyClock = func() time.Time { return now }
	updateCheckLatest = func(context.Context) (stableReleaseResult, error) {
		return stableReleaseResult{Version: "1.2.0", Found: true}, nil
	}
	if err := runUpdateCheck(context.Background(), cfg, false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	secret := "token-secret@example.com/private/path"
	now = now.Add(time.Hour)
	updateCheckLatest = func(context.Context) (stableReleaseResult, error) { return stableReleaseResult{}, errors.New(secret) }
	if err := runUpdateCheck(context.Background(), cfg, false, &bytes.Buffer{}); err == nil {
		t.Fatal("failed check returned nil")
	}
	receipt, err := loadUpdateReceipt(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.UpdateAvailable || receipt.LatestVersion != "1.2.0" || receipt.LastErrorCode != "check_failed" {
		t.Fatalf("receipt=%+v", receipt)
	}
	body, err := os.ReadFile(updateReceiptPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(secret)) {
		t.Fatalf("receipt leaked error: %s", body)
	}
	if info, err := os.Stat(updateReceiptPath(cfg)); err != nil || (runtimeGOOS() != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("mode=%v err=%v", info.Mode().Perm(), err)
	}
}

func TestScheduledNotificationIsCheckedAndRestrained(t *testing.T) {
	cfg := isolateUpdatePolicyTest(t)
	BuildVersion = "1.0.0"
	cfg.UpdatePolicy = "notify"
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	updatePolicyClock = func() time.Time { return now }
	updateCheckLatest = func(context.Context) (stableReleaseResult, error) {
		return stableReleaseResult{Version: "1.1.0", Found: true}, nil
	}
	runtimeGOOS = func() string { return "darwin" }
	calls := 0
	updateNotificationRun = func(args ...string) error { calls++; return nil }
	if err := runUpdateCheck(context.Background(), cfg, true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := runUpdateCheck(context.Background(), cfg, true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("notification calls=%d, want 1", calls)
	}
	now = now.Add(updateReminderEvery)
	if err := runUpdateCheck(context.Background(), cfg, true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("notification calls=%d, want restrained reminder", calls)
	}
}

func TestNotificationFailureKeepsCachedWarning(t *testing.T) {
	cfg := isolateUpdatePolicyTest(t)
	BuildVersion = "1.0.0"
	cfg.UpdatePolicy = "auto"
	updatePolicyClock = func() time.Time { return time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC) }
	updateCheckLatest = func(context.Context) (stableReleaseResult, error) {
		return stableReleaseResult{Version: "1.1.0", Found: true}, nil
	}
	runtimeGOOS = func() string { return "darwin" }
	updateNotificationRun = func(...string) error { return errors.New("osascript unavailable /private/user") }
	if err := runUpdateCheck(context.Background(), cfg, true, &bytes.Buffer{}); err == nil {
		t.Fatal("notification failure returned nil")
	}
	status, err := currentUpdateStatus(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !status.UpdateAvailable || status.LatestVersion != "1.1.0" || status.NotificationErrorCode != "notification_failed" {
		t.Fatalf("status=%+v", status)
	}
	body, _ := os.ReadFile(updateReceiptPath(cfg))
	if bytes.Contains(body, []byte("/private/user")) {
		t.Fatalf("receipt leaked notification error: %s", body)
	}
}

func TestUpgradeStatusJSONIsCachedAndMakesNoCalls(t *testing.T) {
	cfg := isolateUpdatePolicyTest(t)
	BuildVersion = "1.0.0"
	cfg.UpdatePolicy = "notify"
	if err := writeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	now := updatePolicyClock().UTC().Format(time.RFC3339)
	if err := saveUpdateReceipt(cfg, updateReceipt{LastAttemptAt: now, LastSuccessAt: now, LatestVersion: "1.1.0", UpdateAvailable: true}); err != nil {
		t.Fatal(err)
	}
	checks, notifications := 0, 0
	updateCheckLatest = func(context.Context) (stableReleaseResult, error) { checks++; return stableReleaseResult{}, nil }
	updateNotificationRun = func(...string) error { notifications++; return nil }
	var out bytes.Buffer
	if err := cmdUpgrade(context.Background(), []string{"--status", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	if checks != 0 || notifications != 0 {
		t.Fatalf("status caused calls: %d/%d", checks, notifications)
	}
	var status updateStatus
	if err := json.Unmarshal(out.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.SchemaVersion != 1 || status.ResolvedPolicy != "notify" || !status.UpdateAvailable || status.LatestVersion != "1.1.0" {
		t.Fatalf("status=%+v", status)
	}
}

func TestSourceDefaultScheduledCheckDoesNothing(t *testing.T) {
	cfg := isolateUpdatePolicyTest(t)
	BuildVersion = "dev"
	checks, notifications := 0, 0
	updateCheckLatest = func(context.Context) (stableReleaseResult, error) { checks++; return stableReleaseResult{}, nil }
	updateNotificationRun = func(...string) error { notifications++; return nil }
	if err := runUpdateCheck(context.Background(), cfg, true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if checks != 0 || notifications != 0 {
		t.Fatalf("source build called check/notifier: %d/%d", checks, notifications)
	}
}

func TestLoadUpdateReceiptRejectsCorruptOrFutureData(t *testing.T) {
	cfg := isolateUpdatePolicyTest(t)
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	updatePolicyClock = func() time.Time { return now }
	validTime := now.Format(time.RFC3339)
	earlier := now.Add(-time.Minute).Format(time.RFC3339)
	future := now.Add(updateClockSkew + time.Second).Format(time.RFC3339)
	tests := []struct {
		name string
		body string
	}{
		{"unknown field", `{"schema_version":1,"mystery":true}`},
		{"duplicate field", `{"schema_version":2,"schema_version":1}`},
		{"trailing value", `{"schema_version":1} {"schema_version":1}`},
		{"future schema", `{"schema_version":2}`},
		{"coercive version", `{"schema_version":1,"last_attempt_at":"` + validTime + `","last_success_at":"` + validTime + `","latest_version":"1.2","update_available":true}`},
		{"version with v", `{"schema_version":1,"last_attempt_at":"` + validTime + `","last_success_at":"` + validTime + `","latest_version":"v1.2.3","update_available":true}`},
		{"available without version", `{"schema_version":1,"update_available":true}`},
		{"latest without success", `{"schema_version":1,"latest_version":"1.2.3"}`},
		{"bad timestamp", `{"schema_version":1,"last_attempt_at":"yesterday"}`},
		{"future success", `{"schema_version":1,"last_attempt_at":"` + future + `","last_success_at":"` + future + `"}`},
		{"unknown error token", `{"schema_version":1,"last_attempt_at":"` + validTime + `","last_error_code":"token=/private/path"}`},
		{"half notification", `{"schema_version":1,"last_notified_at":"` + validTime + `"}`},
		{"notification error without availability", `{"schema_version":1,"notification_error_code":"notification_failed"}`},
		{"unknown apply outcome", `{"schema_version":1,"apply_version":"1.2.3","apply_attempt_at":"` + validTime + `","apply_outcome":"destroyed"}`},
		{"apply private error", `{"schema_version":1,"apply_version":"1.2.3","apply_attempt_at":"` + validTime + `","apply_outcome":"failed_before_swap","apply_error_code":"/private/path"}`},
		{"updated without rebuild decision", `{"schema_version":1,"apply_version":"1.2.3","apply_attempt_at":"` + validTime + `","applied_at":"` + validTime + `","apply_outcome":"updated","rollback_outcome":"not_needed"}`},
		{"post-apply notification before apply", `{"schema_version":1,"last_attempt_at":"` + earlier + `","last_success_at":"` + earlier + `","latest_version":"1.2.3","last_notified_at":"` + earlier + `","last_notified_version":"1.2.3","apply_version":"1.2.3","apply_attempt_at":"` + earlier + `","applied_at":"` + validTime + `","apply_outcome":"updated","rollback_outcome":"not_needed","rebuild_outcome":"not_needed"}`},
		{"deferred claimed rollback", `{"schema_version":1,"apply_version":"1.2.3","apply_attempt_at":"` + validTime + `","apply_outcome":"deferred","apply_error_code":"app_unwritable","rollback_outcome":"succeeded","rebuild_outcome":"not_run"}`},
		{"deferred claimed rebuild", `{"schema_version":1,"apply_version":"1.2.3","apply_attempt_at":"` + validTime + `","apply_outcome":"deferred","apply_error_code":"app_unwritable","rollback_outcome":"not_needed","rebuild_outcome":"succeeded"}`},
		{"failed before swap claimed applied", `{"schema_version":1,"apply_version":"1.2.3","apply_attempt_at":"` + validTime + `","applied_at":"` + validTime + `","apply_outcome":"failed_before_swap","apply_error_code":"download_failed","rollback_outcome":"not_needed","rebuild_outcome":"not_run"}`},
		{"failed before swap missing not-run rebuild", `{"schema_version":1,"apply_version":"1.2.3","apply_attempt_at":"` + validTime + `","apply_outcome":"failed_before_swap","apply_error_code":"download_failed","rollback_outcome":"not_needed"}`},
		{"rollback repair code without failed rollback", `{"schema_version":1,"apply_version":"1.2.3","apply_attempt_at":"` + validTime + `","apply_outcome":"rolled_back","apply_error_code":"rollback_rebuild_failed","rollback_outcome":"succeeded","rebuild_outcome":"succeeded"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := updateReceiptPath(cfg)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadUpdateReceipt(cfg); err == nil {
				t.Fatalf("corrupt receipt accepted: %s", tt.body)
			}
		})
	}
}

func TestLoadUpdateReceiptRejectsOversizedJSON(t *testing.T) {
	cfg := isolateUpdatePolicyTest(t)
	path := updateReceiptPath(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := append([]byte(`{"schema_version":1}`), bytes.Repeat([]byte(" "), updateReceiptMaxBytes)...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadUpdateReceipt(cfg); err == nil {
		t.Fatal("oversized receipt accepted")
	}
}

func TestSaveUpdateReceiptValidatesBeforeMutation(t *testing.T) {
	cfg := isolateUpdatePolicyTest(t)
	if err := saveUpdateReceipt(cfg, updateReceipt{LatestVersion: "01.2.3"}); err == nil {
		t.Fatal("invalid receipt saved")
	}
	if _, err := os.Stat(updateReceiptPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid save mutated receipt: %v", err)
	}
}

func TestFutureNotificationTimestampNeverSuppressesReminder(t *testing.T) {
	cfg := isolateUpdatePolicyTest(t)
	BuildVersion = "1.0.0"
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	updatePolicyClock = func() time.Time { return now }
	runtimeGOOS = func() string { return "darwin" }
	calls := 0
	updateNotificationRun = func(...string) error { calls++; return nil }
	receipt := updateReceipt{
		SchemaVersion:       updateReceiptSchema,
		LastAttemptAt:       now.Format(time.RFC3339),
		LastSuccessAt:       now.Format(time.RFC3339),
		LatestVersion:       "1.1.0",
		UpdateAvailable:     true,
		LastNotifiedAt:      now.Add(time.Minute).Format(time.RFC3339),
		LastNotifiedVersion: "1.1.0",
	}
	if err := maybeNotifyUpdate(cfg, &receipt, now); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("future notification timestamp suppressed reminder: calls=%d", calls)
	}
}

type updatePolicyRelease struct {
	tag               string
	draft, prerelease bool
}

func (r updatePolicyRelease) GetID() int64              { return 1 }
func (r updatePolicyRelease) GetTagName() string        { return r.tag }
func (r updatePolicyRelease) GetDraft() bool            { return r.draft }
func (r updatePolicyRelease) GetPrerelease() bool       { return r.prerelease }
func (r updatePolicyRelease) GetPublishedAt() time.Time { return time.Time{} }
func (r updatePolicyRelease) GetReleaseNotes() string   { return "" }
func (r updatePolicyRelease) GetName() string           { return r.tag }
func (r updatePolicyRelease) GetURL() string {
	return "https://github.com/pyranthus-hq/mora/releases/tag/" + r.tag
}
func (r updatePolicyRelease) GetAssets() []selfupdate.SourceAsset { return nil }

func TestSelectLatestStableRelease(t *testing.T) {
	releases := []selfupdate.SourceRelease{
		updatePolicyRelease{tag: "v1.2.0"},
		updatePolicyRelease{tag: "v9.0.0", draft: true},
		updatePolicyRelease{tag: "v8.0.0", prerelease: true},
		updatePolicyRelease{tag: "v2.0.0-rc.1"},
		updatePolicyRelease{tag: "not-semver"},
		updatePolicyRelease{tag: "v7.1"},
		updatePolicyRelease{tag: "7.1.0"},
		updatePolicyRelease{tag: "v07.1.0"},
		updatePolicyRelease{tag: "v7.1.0+metadata"},
		updatePolicyRelease{tag: "v1.10.0"},
	}
	got, err := selectLatestStableRelease(releases)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found || got.Version != "1.10.0" {
		t.Fatalf("got=%+v", got)
	}
}
