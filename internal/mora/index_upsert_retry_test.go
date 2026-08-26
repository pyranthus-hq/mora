package mora

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestRetryWhileIndexBusy pins the three behaviours the concurrency contract
// depends on: contention is absorbed, a real error is not, and the retry is
// bounded rather than open-ended.
func TestRetryWhileIndexBusy(t *testing.T) {
	busy := errors.New("database is locked (5) (SQLITE_BUSY)")

	subRun(t, "busy then success is absorbed", func(t *testing.T) {
		calls := 0
		err := retryWhileIndexBusy(context.Background(), func() error {
			calls++
			if calls < 3 {
				return busy
			}
			return nil
		})
		if err != nil {
			t.Fatalf("retryWhileIndexBusy = %v, want nil", err)
		}
		if calls != 3 {
			t.Errorf("op called %d times, want 3", calls)
		}
	})

	subRun(t, "a non-busy error surfaces on the first attempt", func(t *testing.T) {
		want := errors.New("disk full")
		calls := 0
		err := retryWhileIndexBusy(context.Background(), func() error {
			calls++
			return want
		})
		if !errors.Is(err, want) {
			t.Fatalf("retryWhileIndexBusy = %v, want %v", err, want)
		}
		if calls != 1 {
			t.Errorf("op called %d times, want 1 — a permanent error must not pay the backoff", calls)
		}
	})

	subRun(t, "sustained contention surfaces the last error at the deadline", func(t *testing.T) {
		calls := 0
		start := time.Now()
		err := retryWhileIndexBusy(context.Background(), func() error {
			calls++
			return busy
		})
		if !errors.Is(err, busy) {
			t.Fatalf("retryWhileIndexBusy = %v, want the busy error", err)
		}
		if elapsed := time.Since(start); elapsed < indexUpsertBusyDeadline {
			t.Errorf("gave up after %v, want at least the %v deadline", elapsed, indexUpsertBusyDeadline)
		}
		if calls < 2 {
			t.Errorf("op called %d times, want more than one attempt", calls)
		}
	})

	subRun(t, "a cancelled context stops the retry", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		start := time.Now()
		if err := retryWhileIndexBusy(ctx, func() error {
			calls++
			return busy
		}); !errors.Is(err, busy) {
			t.Fatalf("retryWhileIndexBusy = %v, want the busy error", err)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("took %v after cancellation, want an immediate return", elapsed)
		}
		if calls != 1 {
			t.Errorf("op called %d times, want 1", calls)
		}
	})
}

// TestIsIndexBusyErr keeps the classifier honest about the spellings the driver
// produces and about what must NOT be treated as contention.
func TestIsIndexBusyErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"driver busy with extended code", errors.New("database is locked (5) (SQLITE_BUSY)"), true},
		{"table locked", errors.New("database table is locked"), true},
		{"wrapped busy", errors.New("upsert memory: database is locked"), true},
		{"unrelated failure", errors.New("no such table: memories"), false},
		{"read-only vault", errors.New("attempt to write a readonly database"), false},
	}
	for _, c := range cases {
		subRun(t, c.name, func(t *testing.T) {
			if got := isIndexBusyErr(c.err); got != c.want {
				t.Errorf("isIndexBusyErr(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
