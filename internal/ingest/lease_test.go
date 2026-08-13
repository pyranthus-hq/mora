package ingest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pyranthus-hq/mora/internal/config"
)

func leaseSeams(pid int, alive map[int]bool) LeaseSeams {
	return LeaseSeams{PID: func() int { return pid }, ProcessAlive: func(p int) bool { return alive[p] }}
}
func TestEnsureLeaseIdempotentAndOwnedRelease(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	seams := leaseSeams(42, map[int]bool{42: true})
	if err := EnsureLease(cfg, "gmail", seams); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(LeasePath(cfg, "gmail"))
	if err != nil || string(body) != "42" {
		t.Fatalf("lease=(%q,%v)", body, err)
	}
	if err := EnsureLease(cfg, "gmail", seams); err != nil {
		t.Fatal(err)
	}
	if !LeaseHeld(cfg, "gmail", seams) {
		t.Fatal("live lease not held")
	}
	ReleaseLeaseOwnedHere(cfg, "gmail", seams)
	if _, err = os.Stat(LeasePath(cfg, "gmail")); !os.IsNotExist(err) {
		t.Fatalf("owned lease remains: %v", err)
	}
}
func TestLeaseHeldReclaimsDeadAndMalformed(t *testing.T) {
	for _, body := range []string{"99", "bad"} {
		cfg := config.Config{StateDir: t.TempDir()}
		path := LeasePath(cfg, "gmail")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if LeaseHeld(cfg, "gmail", leaseSeams(42, map[int]bool{})) {
			t.Fatalf("%q lease reported held", body)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale lease remains: %v", err)
		}
	}
}
func TestReleaseLeaseDoesNotStealOtherPID(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	path := LeasePath(cfg, "gmail")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("99"), 0o600); err != nil {
		t.Fatal(err)
	}
	ReleaseLeaseOwnedHere(cfg, "gmail", leaseSeams(42, map[int]bool{99: true}))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("foreign lease removed: %v", err)
	}
}
func TestReleaseAllOnlyOwned(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	for key, body := range map[string]string{"gmail": "42", "calendar": "99"} {
		path := LeasePath(cfg, key)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ReleaseLeasesOwnedHere(cfg, leaseSeams(42, map[int]bool{42: true, 99: true}))
	if _, err := os.Stat(LeasePath(cfg, "gmail")); !os.IsNotExist(err) {
		t.Fatalf("owned remains: %v", err)
	}
	if _, err := os.Stat(LeasePath(cfg, "calendar")); err != nil {
		t.Fatalf("foreign removed: %v", err)
	}
}
