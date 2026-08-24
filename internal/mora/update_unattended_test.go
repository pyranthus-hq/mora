package mora

import (
	"bytes"
	"context"
	"errors"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/creativeprojects/go-selfupdate"
)

func isolateUnattendedTest(t *testing.T) Config {
	t.Helper()
	cfg := isolateUpdatePolicyTest(t)
	oldApply := unattendedUpdateApply
	oldVerify := unattendedVerifyApp
	oldHealth := unattendedHealthCheck
	oldPostHealth := unattendedPostHealthCheck
	oldWritable := unattendedAppWritable
	oldSwap := unattendedSwapApps
	oldVerifyArchive := unattendedVerifyArchive
	oldExtract := unattendedExtractApp
	oldRebuild := unattendedRebuild
	oldFailpoint := unattendedFailpoint
	oldSource := newAppReleaseSource
	oldDownload := downloadAppReleaseFile
	t.Cleanup(func() {
		unattendedUpdateApply = oldApply
		unattendedVerifyApp = oldVerify
		unattendedHealthCheck = oldHealth
		unattendedPostHealthCheck = oldPostHealth
		unattendedAppWritable = oldWritable
		unattendedSwapApps = oldSwap
		unattendedVerifyArchive = oldVerifyArchive
		unattendedExtractApp = oldExtract
		unattendedRebuild = oldRebuild
		unattendedFailpoint = oldFailpoint
		newAppReleaseSource = oldSource
		downloadAppReleaseFile = oldDownload
	})
	return cfg
}

func TestScheduledNotifyHoldsLeaseAndNeverApplies(t *testing.T) {
	cfg := isolateUnattendedTest(t)
	BuildVersion = "1.0.0"
	cfg.UpdatePolicy = "notify"
	updatePolicyClock = func() time.Time { return time.Date(2026, 8, 8, 1, 0, 0, 0, time.UTC) }
	checked, applied := false, false
	updateCheckLatest = func(context.Context) (stableReleaseResult, error) {
		checked = true
		if _, err := os.Stat(updateLeasePath(cfg)); err != nil {
			t.Fatalf("checker ran without lease: %v", err)
		}
		return stableReleaseResult{Version: "1.1.0", Found: true}, nil
	}
	unattendedUpdateApply = func(context.Context, Config, *updateReceipt, time.Time, io.Writer) error { applied = true; return nil }
	runtimeGOOS = func() string { return "linux" }
	if err := runUpdateCheck(context.Background(), cfg, true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !checked || applied {
		t.Fatalf("checked=%v applied=%v", checked, applied)
	}
	if _, err := os.Stat(updateLeasePath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lease remained: %v", err)
	}
}

func TestScheduledAutoCallsApplyUnderLease(t *testing.T) {
	cfg := isolateUnattendedTest(t)
	BuildVersion = "1.0.0"
	cfg.UpdatePolicy = "auto"
	updatePolicyClock = func() time.Time { return time.Date(2026, 8, 8, 1, 0, 0, 0, time.UTC) }
	updateCheckLatest = func(context.Context) (stableReleaseResult, error) {
		return stableReleaseResult{Version: "1.1.0", Found: true}, nil
	}
	calls := 0
	unattendedUpdateApply = func(_ context.Context, got Config, receipt *updateReceipt, _ time.Time, _ io.Writer) error {
		calls++
		if got.StateDir != cfg.StateDir || receipt.LatestVersion != "1.1.0" {
			t.Fatalf("bad apply input: %+v %+v", got, receipt)
		}
		if _, err := os.Stat(updateLeasePath(cfg)); err != nil {
			t.Fatalf("apply ran without lease: %v", err)
		}
		return nil
	}
	if err := runUpdateCheck(context.Background(), cfg, true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("apply calls=%d", calls)
	}
}

func TestOffScheduledCheckDoesNotCreateLease(t *testing.T) {
	cfg := isolateUnattendedTest(t)
	BuildVersion = "1.0.0"
	cfg.UpdatePolicy = "off"
	updateCheckLatest = func(context.Context) (stableReleaseResult, error) {
		t.Fatal("network called")
		return stableReleaseResult{}, nil
	}
	if err := runUpdateCheck(context.Background(), cfg, true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(updateLeasePath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("off wrote lease: %v", err)
	}
	if _, err := os.Stat(updateReceiptPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("off wrote receipt: %v", err)
	}
}

type unattendedFixture struct {
	cfg                Config
	appRoot, stagedApp string
	events             *[]string
	now                time.Time
}

func setupUnattendedFixture(t *testing.T) unattendedFixture {
	t.Helper()
	cfg := isolateUnattendedTest(t)
	BuildVersion = "1.0.0"
	cfg.UpdatePolicy = "auto"
	now := time.Date(2026, 8, 8, 1, 0, 0, 0, time.UTC)
	updatePolicyClock = func() time.Time { return now }
	runtimeGOOS = func() string { return "darwin" }
	root := t.TempDir()
	appRoot := filepath.Join(root, "Mora.app")
	executable := filepath.Join(appRoot, "Contents", "MacOS", "mora")
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	updatePolicyExecutable = func() (string, error) { return executable, nil }
	updatePolicyEvalLinks = func(path string) (string, error) { return path, nil }
	events := []string{}
	unattendedVerifyApp = func(_ context.Context, path, version, arch string) error {
		events = append(events, "verify:"+version)
		return nil
	}
	unattendedHealthCheck = func(context.Context, Config) error { events = append(events, "health"); return nil }
	unattendedPostHealthCheck = func(context.Context, string) error { events = append(events, "post_health"); return nil }
	unattendedAppWritable = func(string) bool { events = append(events, "writable"); return true }
	unattendedSwapApps = func(_, _ string) error { events = append(events, "swap"); return nil }
	unattendedVerifyArchive = func(_, _, _ string) error { events = append(events, "archive"); return nil }
	stagedApp := filepath.Join(root, "staged", "Mora.app")
	unattendedExtractApp = func(_ context.Context, _, _ string) (string, error) {
		events = append(events, "extract")
		return stagedApp, nil
	}
	unattendedRebuild = func(context.Context, string) (bool, error) { events = append(events, "rebuild"); return true, nil }
	updateNotificationRun = func(...string) error { events = append(events, "notify"); return nil }
	newAppReleaseSource = func(string) (selfupdate.Source, error) {
		asset, _ := moraAppAssetName("1.1.0", runtime.GOARCH)
		return &fakeAppSource{releases: []selfupdate.SourceRelease{fakeAppRelease{
			tag: "v1.1.0",
			assets: []selfupdate.SourceAsset{
				fakeAppAsset{id: 1, name: asset, size: 10, url: "https://github.com/pyranthus-hq/mora/releases/download/v1.1.0/" + asset},
				fakeAppAsset{id: 2, name: moraAppChecksumFilename, size: 10, url: "https://github.com/pyranthus-hq/mora/releases/download/v1.1.0/checksums-app.txt"},
			},
		}}}, nil
	}
	downloadAppReleaseFile = func(_ context.Context, _, _ string, destination string, _ int64) error {
		events = append(events, "download")
		return os.WriteFile(destination, []byte("fixture"), 0o600)
	}
	return unattendedFixture{cfg: cfg, appRoot: appRoot, stagedApp: stagedApp, events: &events, now: now}
}

func availableReceipt(f unattendedFixture) updateReceipt {
	ts := f.now.Format(time.RFC3339)
	return updateReceipt{SchemaVersion: updateReceiptSchema, LastAttemptAt: ts, LastSuccessAt: ts, LatestVersion: "1.1.0", UpdateAvailable: true}
}

func TestUnattendedPreSwapFailureJoinsReceiptPersistenceFailure(t *testing.T) {
	f := setupUnattendedFixture(t)
	unattendedVerifyApp = func(context.Context, string, string, string) error { return errors.New("bad signature") }
	originalSync := atomicio.MarkerSyncFn
	syncs := 0
	atomicio.MarkerSyncFn = func(file *os.File) error {
		syncs++
		if syncs == 2 {
			return errors.New("receipt disk unavailable")
		}
		return originalSync(file)
	}
	receipt := availableReceipt(f)
	err := runUnattendedAppUpdate(context.Background(), f.cfg, &receipt, f.now, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "bad signature") || !strings.Contains(err.Error(), "receipt disk unavailable") {
		t.Fatalf("joined error=%v", err)
	}
}

func TestUnattendedRollbackFailureJoinsReceiptPersistenceFailure(t *testing.T) {
	f := setupUnattendedFixture(t)
	unattendedFailpoint = func(step string) error {
		if step == "after_swap" {
			return errors.New("post swap failure")
		}
		return nil
	}
	originalSync := atomicio.MarkerSyncFn
	syncs := 0
	atomicio.MarkerSyncFn = func(file *os.File) error {
		syncs++
		if syncs == 2 {
			return errors.New("rollback receipt disk unavailable")
		}
		return originalSync(file)
	}
	receipt := availableReceipt(f)
	err := runUnattendedAppUpdate(context.Background(), f.cfg, &receipt, f.now, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "post swap failure") || !strings.Contains(err.Error(), "rollback receipt disk unavailable") {
		t.Fatalf("joined error=%v", err)
	}
}

func TestUnattendedPreSwapFailpointsNeverMutate(t *testing.T) {
	for _, step := range []string{"after_installed_verify", "after_download", "after_staged_verify", "before_swap"} {
		t.Run(step, func(t *testing.T) {
			f := setupUnattendedFixture(t)
			unattendedFailpoint = func(got string) error {
				if got == step {
					return errors.New("stop")
				}
				return nil
			}
			receipt := availableReceipt(f)
			if err := runUnattendedAppUpdate(context.Background(), f.cfg, &receipt, f.now, &bytes.Buffer{}); err == nil {
				t.Fatal("failpoint returned nil")
			}
			if strings.Contains(strings.Join(*f.events, ","), "swap") {
				t.Fatalf("pre-swap failpoint mutated app: %v", *f.events)
			}
			if receipt.ApplyOutcome != "failed_before_swap" || receipt.RollbackOutcome != "not_needed" {
				t.Fatalf("receipt=%+v", receipt)
			}
		})
	}
}

func TestUnattendedFailpointAfterSwapRollsBack(t *testing.T) {
	f := setupUnattendedFixture(t)
	unattendedFailpoint = func(step string) error {
		if step == "after_swap" {
			return errors.New("kill")
		}
		return nil
	}
	receipt := availableReceipt(f)
	if err := runUnattendedAppUpdate(context.Background(), f.cfg, &receipt, f.now, &bytes.Buffer{}); err == nil {
		t.Fatal("failpoint returned nil")
	}
	if receipt.ApplyOutcome != "rolled_back" || receipt.RollbackOutcome != "succeeded" {
		t.Fatalf("receipt=%+v", receipt)
	}
	if strings.Count(strings.Join(*f.events, ","), "swap") != 2 {
		t.Fatalf("events=%v", *f.events)
	}
}

func TestUnattendedSuccessReplacesPriorAvailabilityNotification(t *testing.T) {
	f := setupUnattendedFixture(t)
	updatePolicyClock = func() time.Time { return f.now }
	receipt := availableReceipt(f)
	receipt.LastNotifiedAt = f.now.Add(-time.Minute).Format(time.RFC3339)
	receipt.LastNotifiedVersion = receipt.LatestVersion
	if err := runUnattendedAppUpdate(context.Background(), f.cfg, &receipt, f.now, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if receipt.ApplyOutcome != "updated" || receipt.LastNotifiedAt != f.now.Format(time.RFC3339) {
		t.Fatalf("receipt=%+v", receipt)
	}
}

func TestAutomaticSuccessNotificationMayFollowApplyAttempt(t *testing.T) {
	f := setupUnattendedFixture(t)
	applied := f.now.Add(10 * time.Second)
	notified := applied.Add(20 * time.Second)
	updatePolicyClock = func() time.Time { return notified }
	receipt := availableReceipt(f)
	receipt.UpdateAvailable = false
	receipt.ApplyVersion = receipt.LatestVersion
	receipt.ApplyAttemptAt = f.now.Format(time.RFC3339)
	receipt.AppliedAt = applied.Format(time.RFC3339)
	receipt.ApplyOutcome = "updated"
	receipt.RollbackOutcome = "not_needed"
	receipt.RebuildOutcome = "not_needed"
	if err := notifyAutomaticUpdateSuccess(f.cfg, &receipt, receipt.ApplyVersion, notified); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadUpdateReceipt(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LastNotifiedAt != notified.Format(time.RFC3339) || loaded.LastNotifiedAt <= loaded.LastAttemptAt || loaded.LastNotifiedAt <= loaded.AppliedAt {
		t.Fatalf("receipt=%+v", loaded)
	}
}

func TestUnattendedSuccessNotificationFailureKeepsUpdatedEvidence(t *testing.T) {
	f := setupUnattendedFixture(t)
	updateNotificationRun = func(...string) error { return errors.New("osascript failed /private/path") }
	receipt := availableReceipt(f)
	if err := runUnattendedAppUpdate(context.Background(), f.cfg, &receipt, f.now, &bytes.Buffer{}); err == nil {
		t.Fatal("notification partial failure returned nil")
	}
	if receipt.ApplyOutcome != "updated" || receipt.NotificationErrorCode != "notification_failed" {
		t.Fatalf("receipt=%+v", receipt)
	}
	body, _ := os.ReadFile(updateReceiptPath(f.cfg))
	if bytes.Contains(body, []byte("/private/path")) {
		t.Fatalf("receipt leaked notification error: %s", body)
	}
}

func TestAutomaticNotificationFailureJoinsReceiptPersistenceFailure(t *testing.T) {
	f := setupUnattendedFixture(t)
	updateNotificationRun = func(...string) error { return errors.New("notification transport failed") }
	atomicio.MarkerSyncFn = func(*os.File) error { return errors.New("notification receipt disk unavailable") }
	receipt := availableReceipt(f)
	receipt.UpdateAvailable = false
	receipt.ApplyVersion = receipt.LatestVersion
	receipt.ApplyAttemptAt = f.now.Format(time.RFC3339)
	receipt.AppliedAt = f.now.Format(time.RFC3339)
	receipt.ApplyOutcome = "updated"
	receipt.RollbackOutcome = "not_needed"
	receipt.RebuildOutcome = "not_needed"
	err := notifyAutomaticUpdateSuccess(f.cfg, &receipt, receipt.ApplyVersion, f.now)
	if err == nil || !strings.Contains(err.Error(), "notification transport failed") || !strings.Contains(err.Error(), "notification receipt disk unavailable") {
		t.Fatalf("joined error=%v", err)
	}
}

func TestUnattendedPlanApplyFenceDetectsIdentityChange(t *testing.T) {
	f := setupUnattendedFixture(t)
	oldVerifies := 0
	unattendedVerifyApp = func(_ context.Context, _ string, version, _ string) error {
		*f.events = append(*f.events, "verify:"+version)
		if version == BuildVersion {
			oldVerifies++
			if oldVerifies == 2 {
				return errors.New("installed app changed")
			}
		}
		return nil
	}
	receipt := availableReceipt(f)
	if err := runUnattendedAppUpdate(context.Background(), f.cfg, &receipt, f.now, &bytes.Buffer{}); err == nil {
		t.Fatal("state change returned nil")
	}
	if receipt.ApplyErrorCode != "state_changed" || strings.Contains(strings.Join(*f.events, ","), "swap") {
		t.Fatalf("receipt=%+v events=%v", receipt, *f.events)
	}
}

func TestUnattendedRollbackFailureIsTypedAndSanitized(t *testing.T) {
	f := setupUnattendedFixture(t)
	swaps := 0
	unattendedSwapApps = func(_, _ string) error {
		swaps++
		*f.events = append(*f.events, "swap")
		if swaps == 2 {
			return errors.New("rollback disk failure /private/recovery")
		}
		return nil
	}
	unattendedFailpoint = func(step string) error {
		if step == "after_swap" {
			return errors.New("post swap launch failure")
		}
		return nil
	}
	receipt := availableReceipt(f)
	if err := runUnattendedAppUpdate(context.Background(), f.cfg, &receipt, f.now, &bytes.Buffer{}); err == nil {
		t.Fatal("rollback failure returned nil")
	}
	if receipt.ApplyOutcome != "rollback_failed" || receipt.RollbackOutcome != "failed" {
		t.Fatalf("receipt=%+v", receipt)
	}
	body, _ := os.ReadFile(updateReceiptPath(f.cfg))
	if bytes.Contains(body, []byte("/private/recovery")) {
		t.Fatalf("receipt leaked rollback error: %s", body)
	}
}

func TestUnattendedSchemaCurrentPostHealthFailurePersistsRollback(t *testing.T) {
	f := setupUnattendedFixture(t)
	unattendedRebuild = func(context.Context, string) (bool, error) { return false, nil }
	unattendedPostHealthCheck = func(context.Context, string) error { return errors.New("new binary unhealthy") }
	receipt := availableReceipt(f)
	if err := runUnattendedAppUpdate(context.Background(), f.cfg, &receipt, f.now, &bytes.Buffer{}); err == nil {
		t.Fatal("post health failure returned nil")
	}
	if receipt.ApplyOutcome != "rolled_back" || receipt.RollbackOutcome != "succeeded" || receipt.RebuildOutcome != "not_needed" {
		t.Fatalf("receipt=%+v", receipt)
	}
	loaded, err := loadUpdateReceipt(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ApplyOutcome != "rolled_back" || loaded.RebuildOutcome != "not_needed" {
		t.Fatalf("persisted receipt=%+v", loaded)
	}
}

func TestUnattendedPostHealthFailureRollsBack(t *testing.T) {
	f := setupUnattendedFixture(t)
	unattendedPostHealthCheck = func(context.Context, string) error { return errors.New("new binary unhealthy") }
	rebuilds := 0
	unattendedRebuild = func(context.Context, string) (bool, error) {
		rebuilds++
		return true, nil
	}
	receipt := availableReceipt(f)
	if err := runUnattendedAppUpdate(context.Background(), f.cfg, &receipt, f.now, &bytes.Buffer{}); err == nil {
		t.Fatal("post health failure returned nil")
	}
	if receipt.ApplyOutcome != "rolled_back" || receipt.ApplyErrorCode != "post_health_failed" || receipt.RollbackOutcome != "succeeded" {
		t.Fatalf("receipt=%+v", receipt)
	}
	if rebuilds != 2 { // forward schema decision, then rollback compatibility repair
		t.Fatalf("rebuild calls=%d, want forward + rollback repair", rebuilds)
	}
}

func TestUnattendedRollbackIndexRepairFailureIsNotSwallowed(t *testing.T) {
	f := setupUnattendedFixture(t)
	unattendedPostHealthCheck = func(context.Context, string) error { return errors.New("new binary unhealthy") }
	rebuilds := 0
	unattendedRebuild = func(context.Context, string) (bool, error) {
		rebuilds++
		if rebuilds == 2 {
			return false, errors.New("old schema repair failed /private/path")
		}
		return true, nil
	}
	receipt := availableReceipt(f)
	if err := runUnattendedAppUpdate(context.Background(), f.cfg, &receipt, f.now, &bytes.Buffer{}); err == nil {
		t.Fatal("rollback repair failure returned nil")
	}
	if receipt.ApplyOutcome != "rollback_failed" || receipt.ApplyErrorCode != "rollback_rebuild_failed" || receipt.RollbackOutcome != "failed" {
		t.Fatalf("receipt=%+v", receipt)
	}
	body, _ := os.ReadFile(updateReceiptPath(f.cfg))
	if bytes.Contains(body, []byte("/private/path")) {
		t.Fatalf("receipt leaked repair error: %s", body)
	}
}

func TestUnattendedRebuildFailureRollsBack(t *testing.T) {
	f := setupUnattendedFixture(t)
	unattendedRebuild = func(context.Context, string) (bool, error) {
		*f.events = append(*f.events, "rebuild")
		return false, errors.New("schema rebuild failed /private/path")
	}
	receipt := availableReceipt(f)
	if err := runUnattendedAppUpdate(context.Background(), f.cfg, &receipt, f.now, &bytes.Buffer{}); err == nil {
		t.Fatal("rebuild failure returned nil")
	}
	if receipt.ApplyOutcome != "rolled_back" || receipt.RebuildOutcome != "failed" {
		t.Fatalf("receipt=%+v", receipt)
	}
	body, _ := os.ReadFile(updateReceiptPath(f.cfg))
	if bytes.Contains(body, []byte("/private/path")) {
		t.Fatalf("receipt leaked error: %s", body)
	}
}

func TestUnattendedHealthAndIdentityFailuresNeverSwap(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		breakFn    func()
	}{
		{"identity", "installed_verify_failed", func() {
			unattendedVerifyApp = func(context.Context, string, string, string) error { return errors.New("bad signature") }
		}},
		{"health", "unsafe_health", func() {
			unattendedHealthCheck = func(context.Context, Config) error { return errors.New("dirty index") }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupUnattendedFixture(t)
			tc.breakFn()
			receipt := availableReceipt(f)
			_ = runUnattendedAppUpdate(context.Background(), f.cfg, &receipt, f.now, &bytes.Buffer{})
			if strings.Contains(strings.Join(*f.events, ","), "swap") {
				t.Fatalf("unsafe state swapped: %v", *f.events)
			}
			if receipt.ApplyErrorCode != tc.code {
				t.Fatalf("receipt=%+v", receipt)
			}
		})
	}
}

func TestUnwritableAppFallsBackOnceForSameVersion(t *testing.T) {
	f := setupUnattendedFixture(t)
	unattendedAppWritable = func(string) bool { *f.events = append(*f.events, "writable"); return false }
	// Keep the app path apply gate Darwin while disabling notification via env.
	runtimeGOOS = func() string { return "darwin" }
	t.Setenv("MORA_NO_NOTIFY", "1")
	receipt := availableReceipt(f)
	if err := runUnattendedAppUpdate(context.Background(), f.cfg, &receipt, f.now, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	firstEvents := len(*f.events)
	if receipt.ApplyOutcome != "deferred" || receipt.ApplyErrorCode != "app_unwritable" {
		t.Fatalf("receipt=%+v", receipt)
	}
	status, err := currentUpdateStatus(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if status.RecoveryCommand != "brew upgrade --cask --greedy pyranthus-hq/tap/mora" {
		t.Fatalf("recovery=%q", status.RecoveryCommand)
	}
	if err := runUnattendedAppUpdate(context.Background(), f.cfg, &receipt, f.now.Add(time.Hour), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(*f.events) != firstEvents {
		t.Fatalf("same unwritable version retried apply: %v", *f.events)
	}
}

func TestIndexRebuildIfNeededNoOpsOnCurrentSchema(t *testing.T) {
	cfg := isolateUnattendedTest(t)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := cmdIndex(context.Background(), []string{"rebuild", "--if-needed"}, &out, testStderr, bytes.NewBuffer(nil)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "rebuild not needed") {
		t.Fatalf("out=%q", out.String())
	}
}

func TestUnattendedUpdateOrdersVerificationBeforeAtomicSwap(t *testing.T) {
	f := setupUnattendedFixture(t)
	receipt := availableReceipt(f)
	var out bytes.Buffer
	if err := runUnattendedAppUpdate(context.Background(), f.cfg, &receipt, f.now, &out); err != nil {
		t.Fatal(err)
	}
	if receipt.ApplyOutcome != "updated" || receipt.RollbackOutcome != "not_needed" || receipt.RebuildOutcome != "succeeded" || receipt.UpdateAvailable {
		t.Fatalf("receipt=%+v", receipt)
	}
	joined := strings.Join(*f.events, ",")
	want := "verify:1.0.0,health,writable,download,download,archive,extract,verify:1.1.0,verify:1.0.0,health,swap,verify:1.1.0,rebuild,post_health,notify"
	if joined != want {
		t.Fatalf("events=%s\nwant=%s", joined, want)
	}
	if strings.Count(joined, "rebuild") != 1 {
		t.Fatalf("forward rebuild ran more than once: %s", joined)
	}
	stored, err := loadUpdateReceipt(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ApplyOutcome != "updated" || stored.AppliedAt == "" {
		t.Fatalf("stored=%+v", stored)
	}
}

func TestUpdateLeaseRejectsConcurrentHolderAndReapsStale(t *testing.T) {
	cfg := isolateUnattendedTest(t)
	now := time.Date(2026, 8, 8, 1, 0, 0, 0, time.UTC)
	release, err := acquireUpdateLease(cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireUpdateLease(cfg, now); err == nil {
		t.Fatal("concurrent lease acquired")
	}
	release()
	staleBody := []byte(`{"pid":123,"acquired_at":"2026-08-07T20:00:00Z"}`)
	if err := os.WriteFile(updateLeasePath(cfg), staleBody, 0o600); err != nil {
		t.Fatal(err)
	}
	release, err = acquireUpdateLease(cfg, now)
	if err != nil {
		t.Fatalf("stale lease not reaped: %v", err)
	}
	release()
}
