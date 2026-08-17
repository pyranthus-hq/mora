package atomicio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestClaimExclusiveDurableCreateExclusiveAndFallback(t *testing.T) {
	dir := t.TempDir()
	temp := filepath.Join(dir, "temp")
	dest := filepath.Join(dir, "dest")
	if err := os.WriteFile(temp, []byte("winner"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ClaimExclusiveDurable(temp, dest); err != nil {
		t.Fatal(err)
	}
	if err := ClaimExclusiveDurable(temp, dest); !errors.Is(err, os.ErrExist) {
		t.Fatalf("collision=%v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "winner" {
		t.Fatalf("dest=%q", got)
	}
	fallbackTemp := filepath.Join(dir, "fallback-temp")
	fallbackDest := filepath.Join(dir, "fallback-dest")
	if err := os.WriteFile(fallbackTemp, []byte("fallback"), 0600); err != nil {
		t.Fatal(err)
	}
	unsupported := errors.New("unsupported")
	opts := ClaimOptions{Link: func(string, string) error { return unsupported }, Unsupported: func(err error) bool { return errors.Is(err, unsupported) }}
	if err := ClaimExclusiveDurable(fallbackTemp, fallbackDest, opts); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(fallbackDest)
	if string(got) != "fallback" {
		t.Fatalf("fallback=%q", got)
	}
	if err := ClaimExclusiveDurable(fallbackTemp, fallbackDest, opts); !errors.Is(err, os.ErrExist) {
		t.Fatalf("fallback collision=%v", err)
	}
}

func TestClaimExclusiveDurableSurfacesRealLinkErrorAndNilOptions(t *testing.T) {
	boom := errors.New("boom")
	err := ClaimExclusiveDurable("a", "b", ClaimOptions{Link: func(string, string) error { return boom }, Unsupported: func(error) bool { return false }})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	dir := t.TempDir()
	temp := filepath.Join(dir, "temp")
	dest := filepath.Join(dir, "dest")
	if err := os.WriteFile(temp, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ClaimExclusiveDurable(temp, dest, ClaimOptions{}); err != nil {
		t.Fatal(err)
	}
}
