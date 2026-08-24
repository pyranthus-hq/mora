package mora

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"testing"
	"time"
)

func TestSourceProcessHelper(t *testing.T) {
	if os.Getenv("MORA_SOURCE_PROCESS_HELPER") == "" {
		return
	}
	mode := os.Getenv("MORA_SOURCE_PROCESS_MODE")
	if mode == "cooperative" {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt)
		if ready := os.Getenv("MORA_SOURCE_PROCESS_READY"); ready != "" {
			_ = os.WriteFile(ready, []byte("ready"), 0o600)
		}
		<-ch
		os.Exit(0)
	}
	signal.Ignore(os.Interrupt)
	for {
		time.Sleep(time.Hour)
	}
}

func helperProcessRunner(t *testing.T, mode string) (sourceProcessRunner, **exec.Cmd, string) {
	t.Helper()
	var captured *exec.Cmd
	ready := t.TempDir() + "/ready"
	grace := 25 * time.Millisecond
	if mode == "cooperative" {
		grace = 2 * time.Second
	}
	r := sourceProcessRunner{
		grace: grace,
		after: time.After,
		command: func(string, ...string) *exec.Cmd {
			captured = exec.Command(os.Args[0], "-test.run=TestSourceProcessHelper")
			captured.Env = append(os.Environ(), "MORA_SOURCE_PROCESS_HELPER=1", "MORA_SOURCE_PROCESS_MODE="+mode, "MORA_SOURCE_PROCESS_READY="+ready)
			return captured
		},
	}
	return r, &captured, ready
}

func TestSourceProcessCancellationReapsCooperativeChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("protected-source launcher is macOS-only")
	}
	runner, captured, ready := helperProcessRunner(t, "cooperative")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for i := 0; i < 200; i++ {
			if _, err := os.Stat(ready); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	err := runSourceProcess(ctx, runner, "helper")
	var processErr *sourceProcessError
	if !errors.As(err, &processErr) || processErr.Forced || !errors.Is(err, context.Canceled) {
		t.Fatalf("runSourceProcess error = %#v, want cooperative cancellation", err)
	}
	if (*captured).ProcessState == nil {
		t.Fatal("runner returned before cooperative child was reaped")
	}
}

func TestSourceProcessCancellationForcesUncooperativeChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("protected-source launcher is macOS-only")
	}
	runner, captured, _ := helperProcessRunner(t, "uncooperative")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	err := runSourceProcess(ctx, runner, "helper")
	var processErr *sourceProcessError
	if !errors.As(err, &processErr) || !processErr.Forced || !errors.Is(err, context.Canceled) {
		t.Fatalf("runSourceProcess error = %#v, want forced cancellation", err)
	}
	if (*captured).ProcessState == nil {
		t.Fatal("runner returned before forced child was reaped")
	}
}
