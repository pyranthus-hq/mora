package index

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/config"
)

func TestListPendingRetriesContentionWithoutLosingMarkers(t *testing.T) {
	for _, transition := range []string{"none", "rename", "retire"} {
		t.Run(transition, func(t *testing.T) {
			cfg := config.Config{StateDir: t.TempDir()}
			if err := os.MkdirAll(PendingDir(cfg), 0o700); err != nil {
				t.Fatal(err)
			}
			path := PendingPath(cfg, "d")
			body := []byte(`{"op_id":"d","kind":"delete","memory_id":"m"}`)
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			oldRead, oldRetryable := readPendingFile, pendingReadRetryable
			t.Cleanup(func() { readPendingFile, pendingReadRetryable = oldRead, oldRetryable })
			contention := errors.New("transient marker contention")
			pendingReadRetryable = func(err error) bool { return errors.Is(err, contention) }
			calls := map[string]int{}
			readPendingFile = func(name string) ([]byte, error) {
				calls[name]++
				if calls[name] == 1 {
					if name == path {
						switch transition {
						case "rename":
							if err := os.Rename(path, FTSCommittedPath(cfg, "d")); err != nil {
								t.Fatal(err)
							}
						case "retire":
							if err := os.Remove(path); err != nil {
								t.Fatal(err)
							}
						}
					}
					return nil, &os.PathError{Op: "open", Path: name, Err: contention}
				}
				return os.ReadFile(name)
			}
			ops, err := ListPending(cfg)
			if err != nil {
				t.Fatalf("transient contention aborted listing: %v", err)
			}
			if transition == "retire" {
				if len(ops) != 0 {
					t.Fatalf("retired marker survived: %+v", ops)
				}
				return
			}
			if len(ops) != 1 || ops[0].MemoryID != "m" || ops[0].Unlinked != (transition == "rename") {
				t.Fatalf("delete evidence lost or misclassified: %+v", ops)
			}
			if !PendingDeleteIDs(cfg)["m"] {
				t.Fatal("delete suppression lost")
			}
		})
	}
}

func TestListPendingPermanentReadErrorStillFails(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	if err := os.MkdirAll(PendingDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(PendingDir(cfg), "bad.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := readPendingFile
	t.Cleanup(func() { readPendingFile = old })
	boom := errors.New("permanent I/O failure")
	calls := 0
	readPendingFile = func(string) ([]byte, error) { calls++; return nil, boom }
	if ops, err := ListPending(cfg); !errors.Is(err, boom) || ops != nil || calls != 1 {
		t.Fatalf("ops=%v err=%v calls=%d", ops, err, calls)
	}
}

func TestPendingReadExpiredBudgetIsNotRestarted(t *testing.T) {
	oldRead, oldRetryable := readPendingFile, pendingReadRetryable
	t.Cleanup(func() { readPendingFile, pendingReadRetryable = oldRead, oldRetryable })
	contention := errors.New("contention")
	pendingReadRetryable = func(err error) bool { return errors.Is(err, contention) }
	calls := 0
	readPendingFile = func(string) ([]byte, error) { calls++; return nil, contention }
	deadline := time.Now().Add(-time.Second)
	original := deadline
	for _, path := range []string{"first.json", "second.fts.json"} {
		if b, err := readPendingWithRetry(path, &deadline); b != nil || !errors.Is(err, contention) {
			t.Fatalf("b=%v err=%v", b, err)
		}
	}
	if calls != 2 || deadline != original {
		t.Fatalf("expired shared budget restarted: calls=%d deadline=%v", calls, deadline)
	}
}
