package google

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRecordAuthAppendsAndLastAuthReturnsLatest writes several auth events at
// increasing times for one account and asserts LastAuth returns the latest.
func TestRecordAuthAppendsAndLastAuthReturnsLatest(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	times := []time.Time{
		base,
		base.Add(24 * time.Hour),
		base.Add(72 * time.Hour),
	}
	for _, at := range times {
		if err := RecordAuth(dir, "google", at); err != nil {
			t.Fatalf("RecordAuth(%v): %v", at, err)
		}
	}

	got, ok, err := LastAuth(dir, "google")
	if err != nil {
		t.Fatalf("LastAuth: %v", err)
	}
	if !ok {
		t.Fatal("LastAuth: want ok=true, got false")
	}
	want := times[len(times)-1]
	if !got.Equal(want) {
		t.Fatalf("LastAuth = %v, want latest %v", got, want)
	}

	// One JSON line per event (JSONL).
	b, err := os.ReadFile(filepath.Join(dir, "auth-history.jsonl"))
	if err != nil {
		t.Fatalf("read history: %v", err)
	}
	if lines := countLines(string(b)); lines != len(times) {
		t.Fatalf("history has %d lines, want %d", lines, len(times))
	}
}

// TestLastAuthIsolatesAccounts asserts two accounts in the same history file do
// not cross-contaminate.
func TestLastAuthIsolatesAccounts(t *testing.T) {
	dir := t.TempDir()
	aTime := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	bTime := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	if err := RecordAuth(dir, "alpha", aTime); err != nil {
		t.Fatal(err)
	}
	if err := RecordAuth(dir, "beta", bTime); err != nil {
		t.Fatal(err)
	}

	gotA, okA, err := LastAuth(dir, "alpha")
	if err != nil || !okA {
		t.Fatalf("LastAuth(alpha): ok=%v err=%v", okA, err)
	}
	if !gotA.Equal(aTime) {
		t.Fatalf("LastAuth(alpha) = %v, want %v", gotA, aTime)
	}

	gotB, okB, err := LastAuth(dir, "beta")
	if err != nil || !okB {
		t.Fatalf("LastAuth(beta): ok=%v err=%v", okB, err)
	}
	if !gotB.Equal(bTime) {
		t.Fatalf("LastAuth(beta) = %v, want %v", gotB, bTime)
	}

	// Empty account => latest across ALL accounts.
	gotAll, okAll, err := LastAuth(dir, "")
	if err != nil || !okAll {
		t.Fatalf("LastAuth(all): ok=%v err=%v", okAll, err)
	}
	if !gotAll.Equal(bTime) {
		t.Fatalf("LastAuth(all) = %v, want latest %v", gotAll, bTime)
	}
}

// TestLastAuthMissingFile asserts a missing history file is not an error: it
// returns (zero, false, nil).
func TestLastAuthMissingFile(t *testing.T) {
	dir := t.TempDir() // no auth-history.jsonl written
	got, ok, err := LastAuth(dir, "google")
	if err != nil {
		t.Fatalf("missing file must NOT error, got: %v", err)
	}
	if ok {
		t.Fatalf("missing file must yield ok=false, got true (%v)", got)
	}
	if !got.IsZero() {
		t.Fatalf("missing file must yield zero time, got %v", got)
	}
}

// TestLastAuthUnknownAccount asserts an account with no events yields ok=false.
func TestLastAuthUnknownAccount(t *testing.T) {
	dir := t.TempDir()
	if err := RecordAuth(dir, "alpha", time.Now()); err != nil {
		t.Fatal(err)
	}
	_, ok, err := LastAuth(dir, "nonexistent")
	if err != nil {
		t.Fatalf("unknown account must NOT error, got: %v", err)
	}
	if ok {
		t.Fatal("unknown account must yield ok=false")
	}
}

func countLines(s string) int {
	n := 0
	for _, r := range s {
		if r == '\n' {
			n++
		}
	}
	return n
}
