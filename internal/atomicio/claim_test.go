package atomicio

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

func TestClaimExclusiveDurableFallbackConcurrentSingleWinner(t *testing.T) {
	unsupported := errors.New("forced unsupported link")
	opts := ClaimOptions{Link: func(string, string) error { return unsupported }, Unsupported: func(err error) bool { return errors.Is(err, unsupported) }}
	const writers = 16
	for iter := 0; iter < 100; iter++ {
		dir := t.TempDir()
		dest := filepath.Join(dir, "dest")
		temps := make([]string, writers)
		bodies := make([][]byte, writers)
		for i := range temps {
			temps[i] = filepath.Join(dir, fmt.Sprintf("temp-%d", i))
			bodies[i] = bytes.Repeat([]byte{byte('A' + i)}, 4096)
			if err := WriteDurable(temps[i], bodies[i], 0600); err != nil {
				t.Fatal(err)
			}
		}
		errs := make([]error, writers)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range temps {
			wg.Add(1)
			go func(i int) { defer wg.Done(); <-start; errs[i] = ClaimExclusiveDurable(temps[i], dest, opts) }(i)
		}
		close(start)
		wg.Wait()
		winner, successes, exists := -1, 0, 0
		for i, err := range errs {
			switch {
			case err == nil:
				winner = i
				successes++
			case errors.Is(err, os.ErrExist):
				exists++
			default:
				t.Fatalf("iter %d writer %d: %v", iter, i, err)
			}
		}
		if successes != 1 || exists != writers-1 {
			t.Fatalf("iter %d: want 1 winner and %d EEXIST losers, got %d and %d", iter, writers-1, successes, exists)
		}
		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, bodies[winner]) {
			t.Fatalf("iter %d: body does not match winner %d", iter, winner)
		}
	}
}

func TestClaimExclusiveDurableFallbackPreservesMode(t *testing.T) {
	for _, mode := range []os.FileMode{0600, 0666, 0444} {
		t.Run(fmt.Sprintf("%o", mode), func(t *testing.T) {
			dir := t.TempDir()
			temp, dest := filepath.Join(dir, "temp"), filepath.Join(dir, "dest")
			if err := WriteDurable(temp, []byte("body"), mode); err != nil {
				t.Fatal(err)
			}
			want, err := os.Stat(temp)
			if err != nil {
				t.Fatal(err)
			}
			unsupported := errors.New("forced unsupported link")
			opts := ClaimOptions{Link: func(string, string) error { return unsupported }, Unsupported: func(err error) bool { return errors.Is(err, unsupported) }}
			if err := ClaimExclusiveDurable(temp, dest, opts); err != nil {
				t.Fatal(err)
			}
			got, err := os.Stat(dest)
			if err != nil {
				t.Fatal(err)
			}
			if got.Mode().Perm() != want.Mode().Perm() {
				t.Fatalf("mode = %o, want %o", got.Mode().Perm(), want.Mode().Perm())
			}
		})
	}
}

func TestClaimExclusiveDurableFallbackSourceFailureCleansClaim(t *testing.T) {
	unsupported := errors.New("forced unsupported link")
	opts := ClaimOptions{Link: func(string, string) error { return unsupported }, Unsupported: func(err error) bool { return errors.Is(err, unsupported) }}
	dir := t.TempDir()
	dest := filepath.Join(dir, "dest")
	if err := ClaimExclusiveDurable(filepath.Join(dir, "missing"), dest, opts); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing source: %v", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed claim left destination: %v", err)
	}
	if err := os.WriteFile(dest, []byte("winner"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ClaimExclusiveDurable(filepath.Join(dir, "missing"), dest, opts); !errors.Is(err, os.ErrExist) {
		t.Fatalf("collision: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "winner" {
		t.Fatalf("existing destination changed: %q", got)
	}
}
