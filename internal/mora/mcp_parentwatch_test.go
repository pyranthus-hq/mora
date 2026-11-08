package mora

import (
	"bytes"
	"sync/atomic"
	"testing"
	"time"
)

func stubParentWatch(t *testing.T, ppid *atomic.Int64, exited chan int) {
	og, oe, oi, oo := getppid, exitFn, parentWatchInterval, parentWatchGOOS
	t.Cleanup(func() { getppid, exitFn, parentWatchInterval, parentWatchGOOS = og, oe, oi, oo })
	getppid = func() int { return int(ppid.Load()) }
	exitFn = func(c int) { exited <- c }
	parentWatchInterval = 5 * time.Millisecond
	parentWatchGOOS = "linux"
}

func TestWatchParentExitsWhenReparented(t *testing.T) {
	var ppid atomic.Int64
	ppid.Store(4242)
	exited := make(chan int, 1)
	stubParentWatch(t, &ppid, exited)
	var stderr bytes.Buffer
	stop := watchParent(&stderr)
	defer stop()
	ppid.Store(1)
	select {
	case c := <-exited:
		if c != 0 {
			t.Fatalf("exit code %d", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("exitFn not called after parent change")
	}
}

func TestWatchParentSkippedWhenOrphanedOrWindows(t *testing.T) {
	var ppid atomic.Int64
	exited := make(chan int, 1)
	stubParentWatch(t, &ppid, exited)
	ppid.Store(1)
	watchParent(&bytes.Buffer{})()
	ppid.Store(4242)
	parentWatchGOOS = "windows"
	stop := watchParent(&bytes.Buffer{})
	defer stop()
	ppid.Store(7)
	select {
	case <-exited:
		t.Fatal("watchdog should not run")
	case <-time.After(100 * time.Millisecond):
	}
}
