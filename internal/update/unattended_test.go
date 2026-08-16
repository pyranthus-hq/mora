package update

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type applyFixture struct {
	opts                                 ApplyOptions
	receipt                              Receipt
	events                               *[]string
	appRoot, staged                      string
	verifyCalls, swapCalls, rebuildCalls int
}

func newApplyFixture(t *testing.T) *applyFixture {
	t.Helper()
	now := time.Date(2026, 8, 8, 1, 0, 0, 0, time.UTC)
	state := t.TempDir()
	root := t.TempDir()
	app := filepath.Join(root, "Mora.app")
	exe := filepath.Join(app, "Contents", "MacOS", "mora")
	if err := os.MkdirAll(filepath.Dir(exe), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("old"), 0700); err != nil {
		t.Fatal(err)
	}
	events := []string{}
	f := &applyFixture{events: &events, appRoot: app, staged: filepath.Join(root, "stage", "Mora.app")}
	f.receipt = Receipt{SchemaVersion: SchemaVersion, LastAttemptAt: now.Format(time.RFC3339), LastSuccessAt: now.Format(time.RFC3339), LatestVersion: "1.1.0", UpdateAvailable: true}
	f.opts = ApplyOptions{Store: Store{StateDir: state, Now: func() time.Time { return now }}, Receipt: &f.receipt, Now: now, CurrentVersion: "1.0.0", GOOS: "darwin", GOARCH: "arm64", Stdout: &bytes.Buffer{}, Executable: func() (string, error) { return exe, nil }, EvalSymlinks: func(p string) (string, error) { return p, nil }, AppRoot: func(string) (string, bool) { return app, true }, VerifyApp: func(context.Context, string, string, string) error {
		f.verifyCalls++
		events = append(events, "verify")
		return nil
	}, Health: func(context.Context) error { events = append(events, "health"); return nil }, PostHealth: func(context.Context, string) error { events = append(events, "post_health"); return nil }, Writable: func(string) bool { events = append(events, "writable"); return true }, Discover: func(context.Context, string) (Candidate, bool, error) {
		return Candidate{Version: "1.1.0", AssetName: "mora.zip", AssetURL: "archive", ChecksumURL: "checksum", ArchiveLimit: 100, ChecksumLimit: 100}, true, nil
	}, Download: func(_ context.Context, _ string, d string, _ int64) error {
		events = append(events, "download")
		return os.WriteFile(d, []byte("x"), 0600)
	}, VerifyArchive: func(string, string, string) error { events = append(events, "archive"); return nil }, Extract: func(context.Context, string, string) (string, error) {
		events = append(events, "extract")
		return f.staged, nil
	}, Swap: func(string, string) error { f.swapCalls++; events = append(events, "swap"); return nil }, Rebuild: func(context.Context, string) (bool, error) {
		f.rebuildCalls++
		events = append(events, "rebuild")
		return true, nil
	}, Failpoint: func(string) error { return nil }, NotifyAvailability: func(*Receipt, time.Time) error { events = append(events, "available"); return nil }, NotifySuccess: func(r *Receipt, v string, at time.Time) error {
		events = append(events, "notify")
		r.LastNotifiedAt = at.Format(time.RFC3339)
		r.LastNotifiedVersion = v
		return f.opts.Store.Save(*r)
	}, Clock: func() time.Time { return now }, ChecksumFilename: "checksums-app.txt"}
	return f
}
func TestUnattendedUpdateOrdersVerificationBeforeAtomicSwap(t *testing.T) {
	f := newApplyFixture(t)
	if err := Apply(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	want := "verify,health,writable,download,download,archive,extract,verify,verify,health,swap,verify,rebuild,post_health,notify"
	if got := strings.Join(*f.events, ","); got != want {
		t.Fatalf("events=%s want=%s", got, want)
	}
	if f.receipt.ApplyOutcome != "updated" || f.receipt.RollbackOutcome != "not_needed" || f.receipt.RebuildOutcome != "succeeded" || f.receipt.UpdateAvailable {
		t.Fatalf("receipt=%+v", f.receipt)
	}
}
func TestApplyPreSwapGuardsNeverMutate(t *testing.T) {
	cases := []struct {
		name, code string
		mutate     func(*applyFixture)
	}{{"platform", "not_verified_app", func(f *applyFixture) { f.opts.GOOS = "linux" }}, {"installed identity", "installed_verify_failed", func(f *applyFixture) {
		f.opts.VerifyApp = func(context.Context, string, string, string) error { return errors.New("signature") }
	}}, {"health", "unsafe_health", func(f *applyFixture) { f.opts.Health = func(context.Context) error { return errors.New("bad") } }}, {"writable", "app_unwritable", func(f *applyFixture) { f.opts.Writable = func(string) bool { return false } }}, {"release changed", "state_changed", func(f *applyFixture) {
		f.opts.Discover = func(context.Context, string) (Candidate, bool, error) { return Candidate{}, false, nil }
	}}, {"swap", "swap_failed", func(f *applyFixture) { f.opts.Swap = func(string, string) error { return errors.New("swap") } }}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newApplyFixture(t)
			tc.mutate(f)
			err := Apply(context.Background(), f.opts)
			if tc.name == "platform" || tc.name == "health" || tc.name == "writable" {
				if err != nil {
					t.Fatalf("defer err=%v", err)
				}
			} else if err == nil {
				t.Fatal("expected error")
			}
			if f.receipt.ApplyErrorCode != tc.code {
				t.Fatalf("receipt=%+v", f.receipt)
			}
			if tc.name != "swap" && f.swapCalls != 0 {
				t.Fatalf("swaps=%d", f.swapCalls)
			}
		})
	}
}
func TestApplyFailpointAndPostSwapFailuresRollback(t *testing.T) {
	for _, step := range []string{"after_swap", "after_post_health"} {
		t.Run(step, func(t *testing.T) {
			f := newApplyFixture(t)
			f.opts.Failpoint = func(got string) error {
				if got == step {
					return errors.New("stop")
				}
				return nil
			}
			if err := Apply(context.Background(), f.opts); err == nil {
				t.Fatal("expected error")
			}
			if f.receipt.ApplyOutcome != "rolled_back" || f.receipt.RollbackOutcome != "succeeded" || f.swapCalls != 2 {
				t.Fatalf("receipt=%+v swaps=%d", f.receipt, f.swapCalls)
			}
			if step == "after_post_health" && f.rebuildCalls != 2 {
				t.Fatalf("repair rebuilds=%d", f.rebuildCalls)
			}
		})
	}
}
func TestApplyRollbackFailureIsTyped(t *testing.T) {
	f := newApplyFixture(t)
	f.opts.Failpoint = func(s string) error {
		if s == "after_swap" {
			return errors.New("post")
		}
		return nil
	}
	f.opts.Swap = func(string, string) error {
		f.swapCalls++
		if f.swapCalls == 2 {
			return errors.New("rollback")
		}
		return nil
	}
	err := Apply(context.Background(), f.opts)
	if err == nil || f.receipt.ApplyOutcome != "rollback_failed" || f.receipt.RollbackOutcome != "failed" {
		t.Fatalf("err=%v receipt=%+v", err, f.receipt)
	}
}
func TestApplyUnwritableDefersOnlyOncePerVersion(t *testing.T) {
	f := newApplyFixture(t)
	f.opts.Writable = func(string) bool { return false }
	if err := Apply(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	first := f.receipt.ApplyAttemptAt
	if err := Apply(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	if f.receipt.ApplyAttemptAt != first {
		t.Fatalf("attempt changed %q -> %q", first, f.receipt.ApplyAttemptAt)
	}
}
func TestUpdateLeaseRejectsConcurrentHolderAndReapsStale(t *testing.T) {
	state := t.TempDir()
	now := time.Date(2026, 8, 8, 1, 0, 0, 0, time.UTC)
	release, err := AcquireLease(state, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireLease(state, now); err == nil {
		t.Fatal("concurrent lease acquired")
	}
	release()
	if err := os.WriteFile(LeasePath(state), []byte(`{"pid":123,"acquired_at":"2026-08-07T20:00:00Z"}`), 0600); err != nil {
		t.Fatal(err)
	}
	release, err = AcquireLease(state, now)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestAppParentWritable(t *testing.T) {
	root := t.TempDir()
	if !AppParentWritable(filepath.Join(root, "Mora.app")) {
		t.Fatal("writable parent rejected")
	}
	block := filepath.Join(root, "file")
	if err := os.WriteFile(block, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if AppParentWritable(filepath.Join(block, "Mora.app")) {
		t.Fatal("file parent accepted")
	}
}
