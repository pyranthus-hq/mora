package ingest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestEnsureLeaseRefusesLiveOtherHolder(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	if err := EnsureLease(cfg, "gmail", leaseSeams(99, map[int]bool{99: true, 42: true})); err != nil {
		t.Fatal(err)
	}
	err := EnsureLease(cfg, "gmail", leaseSeams(42, map[int]bool{99: true, 42: true}))
	var held *LeaseHeldError
	if !errors.As(err, &held) || held.PID != 99 {
		t.Fatalf("err = %v, want LeaseHeldError for pid 99", err)
	}
	if b, _ := os.ReadFile(LeasePath(cfg, "gmail")); string(b) != "99" {
		t.Fatalf("holder lease overwritten: %q", b)
	}
}

func TestEnsureLeaseTakesOverDeadMalformedAndAncient(t *testing.T) {
	for name, body := range map[string]string{"dead": "99", "malformed": "bad", "empty": ""} {
		cfg := config.Config{StateDir: t.TempDir()}
		path := LeasePath(cfg, "gmail")
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		_ = os.WriteFile(path, []byte(body), 0o644)
		if err := EnsureLease(cfg, "gmail", leaseSeams(42, map[int]bool{42: true})); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if b, _ := os.ReadFile(path); string(b) != "42" {
			t.Fatalf("%s: lease = %q, want 42", name, b)
		}
	}
	// A live PID with an ancient lease is PID reuse, not a running sync.
	cfg := config.Config{StateDir: t.TempDir()}
	path := LeasePath(cfg, "gmail")
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, []byte("99"), 0o644)
	old := time.Now().Add(-2 * maxLeaseAge)
	_ = os.Chtimes(path, old, old)
	if err := EnsureLease(cfg, "gmail", leaseSeams(42, map[int]bool{99: true, 42: true})); err != nil {
		t.Fatal(err)
	}
}
