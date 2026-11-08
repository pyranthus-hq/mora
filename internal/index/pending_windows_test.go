//go:build windows

package index

import (
	"errors"
	"os"
	"testing"

	"github.com/pyranthus-hq/mora/internal/config"
	"golang.org/x/sys/windows"
)

// Force the actual Windows sharing violation, then release the conflicting
// handle only after the reader observes it. No scheduler timing or retry of the
// whole test can make the original single-read implementation pass.
func TestListPendingWindowsSharingViolation(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "dirty"
		if committed {
			name = "committed"
		}
		t.Run(name, func(t *testing.T) {
			cfg := config.Config{StateDir: t.TempDir()}
			if err := os.MkdirAll(PendingDir(cfg), 0o700); err != nil {
				t.Fatal(err)
			}
			path := PendingPath(cfg, "d")
			if committed {
				path = FTSCommittedPath(cfg, "d")
			}
			if err := os.WriteFile(path, []byte(`{"op_id":"d","kind":"delete","memory_id":"m"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			ptr, err := windows.UTF16PtrFromString(path)
			if err != nil {
				t.Fatal(err)
			}
			handle, err := windows.CreateFile(ptr, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
			if err != nil {
				t.Fatal(err)
			}
			held := true
			t.Cleanup(func() {
				if held {
					_ = windows.CloseHandle(handle)
				}
			})
			oldRead := readPendingFile
			t.Cleanup(func() { readPendingFile = oldRead })
			calls := 0
			readPendingFile = func(name string) ([]byte, error) {
				calls++
				b, readErr := os.ReadFile(name)
				if held {
					if !errors.Is(readErr, windows.ERROR_SHARING_VIOLATION) {
						t.Fatalf("expected native sharing violation, got %v", readErr)
					}
					if err := windows.CloseHandle(handle); err != nil {
						t.Fatal(err)
					}
					held = false
				}
				return b, readErr
			}
			ops, err := ListPending(cfg)
			if err != nil || len(ops) != 1 || ops[0].MemoryID != "m" || ops[0].Unlinked != committed || calls != 2 {
				t.Fatalf("ops=%+v err=%v attempts=%d", ops, err, calls)
			}
			if !PendingDeleteIDs(cfg)["m"] {
				t.Fatal("delete suppression lost")
			}
		})
	}
}
