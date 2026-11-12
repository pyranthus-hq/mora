//go:build !windows

package mora

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// givenBlocked runs fn and fails if it does not return at once.
func givenBlocked(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("call blocked on the given log")
	}
}

func TestGivenFIFOLogNeverBlocks(t *testing.T) {
	cfg := projectBriefSetup(t)
	pbSeed(t, cfg, "mem_a", pbScope, "decision", "Rule", "Pnpm body", 1)
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	enableUnboundSessionBrief(t)
	defer stubHookBrief(t, "daily brief body")()
	if err := syscall.Mkfifo(givenLogPath(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	givenBlocked(t, func() {
		var out bytes.Buffer
		in := fmt.Sprintf(`{"source":"startup","cwd":%q}`, t.TempDir())
		_ = cmdHook(testCtx(t), []string{"session-start"}, &out, testStderr, strings.NewReader(in))
		if !strings.Contains(out.String(), "daily brief body") {
			t.Errorf("hook output changed: %q", out.String())
		}
		inv := invokeMCPTool(testCtx(t), "list_memory", map[string]any{"scope": pbScope})
		if ids, _, _ := givenFromMCP("list_memory", nil, inv); inv.err != nil || len(ids) != 1 {
			t.Errorf("tool result changed: %v %v", ids, inv.err)
		}
	})
}

func TestGivenSymlinkLogIsNotFollowed(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	target := cfg.StateDir + "/elsewhere"
	if err := os.WriteFile(target, []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, givenLogPath(cfg)); err != nil {
		t.Fatal(err)
	}
	_ = appendGiven(cfg, givenLine{Surface: "hook-recall", Kind: "recall", Caller: "claude-code"}, []string{"a"}, 1)
	if b, _ := os.ReadFile(target); string(b) != "keep\n" {
		t.Fatalf("symlink target was written: %q", b)
	}
}

func TestGivenExistingWideLogIsNarrowed(t *testing.T) {
	cfg := Config{StateDir: t.TempDir()}
	if err := os.WriteFile(givenLogPath(cfg), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(givenLogPath(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendGiven(cfg, givenLine{Surface: "hook-recall", Kind: "recall", Caller: "claude-code"}, []string{"a"}, 1); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(givenLogPath(cfg)); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, err %v", info.Mode().Perm(), err)
	}
}
