package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSyncFailureIsDurableAndCannotReportSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	root := t.TempDir()
	t.Setenv("MORA_TENTACLES_STATE_DIR", root)
	bin := filepath.Join(t.TempDir(), "mora")
	script := `#!/bin/sh
if [ "$2" = list ]; then
 printf '%s\n' '{"payload":{"publishes":[{"name":"devbox-test"}],"subscriptions":[{"name":"from-devbox-test"}]}}'
 exit 0
fi
if [ "$2" = push ]; then exit 42; fi
exit 0
`
	if e := os.WriteFile(bin, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("MORA_BIN", bin)
	previous := "2026-11-01T00:00:00Z"
	path := filepath.Join(root, "mac-devbox.json")
	if e := atomicJSON(path, Receipt{Schema: 1, LastSuccess: previous}); e != nil {
		t.Fatal(e)
	}
	if e := syncShares("mac-devbox"); e == nil {
		t.Fatal("failed push swallowed")
	}
	b, _ := os.ReadFile(path)
	var r Receipt
	_ = json.Unmarshal(b, &r)
	if r.State != "failed" || r.Operations != 1 || r.LastSuccess != previous || !strings.Contains(r.Detail, "push devbox-test") {
		t.Fatalf("bad receipt: %+v", r)
	}
	if c := readReceipt(path, "test", time.Hour); c.State != "failed" {
		t.Fatalf("bad channel: %+v", c)
	}

	script = strings.ReplaceAll(script, "exit 42", "exit 0")
	_ = os.WriteFile(bin, []byte(script), 0700)
	if e := syncShares("mac-devbox"); e != nil {
		t.Fatal(e)
	}
	if c := readReceipt(path, "test", time.Hour); c.State != "healthy" {
		t.Fatalf("successful run not confirmed: %+v", c)
	}
}
func TestEmptyWrongAndUnsafeShareListsFailClosed(t *testing.T) {
	for _, s := range []string{`{}`, `{"publishes":[],"subscriptions":[]}`, `{"publishes":[{"name":"gw-test"}]}`, `{"publishes":[{"name":"devbox-;evil"}],"subscriptions":[{"name":"from-devbox-test"}]}`, `{"publishes":{},"subscriptions":[]}`} {
		if _, e := planOperations("mac-devbox", []byte(s)); e == nil {
			t.Fatalf("accepted %s", s)
		}
	}
}
func TestReceiptAgesAndInterruptedRun(t *testing.T) {
	oldClock := clock
	defer func() { clock = oldClock }()
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	clock = func() time.Time { return now }
	path := filepath.Join(t.TempDir(), "test.json")
	for _, tt := range []struct {
		r    Receipt
		want string
	}{
		{Receipt{Schema: 1, State: "running", StartedAt: now.Add(-time.Hour).Format(time.RFC3339)}, "failed"},
		{Receipt{Schema: 1, State: "running", StartedAt: now.Format(time.RFC3339)}, "running"},
		{Receipt{Schema: 1, State: "succeeded", Operations: 1, LastSuccess: "2026-11-08T07:00:00Z", FinishedAt: "2026-11-08T07:00:00Z"}, "stale"},
		{Receipt{Schema: 1, State: "succeeded", Operations: 1, LastSuccess: "2026-11-09T07:00:00Z", FinishedAt: "2026-11-09T07:00:00Z"}, "unknown"},
		{Receipt{Schema: 1, State: "succeeded", Operations: 0}, "unknown"},
	} {
		_ = atomicJSON(path, tt.r)
		if c := readReceipt(path, "test", time.Hour); c.State != tt.want {
			t.Fatalf("%+v: got %+v want %s", tt.r, c, tt.want)
		}
	}
	_ = os.WriteFile(path, []byte("broken"), 0600)
	if c := readReceipt(path, "test", time.Hour); c.State != "unknown" {
		t.Fatal(c)
	}
}
func TestSSHConfigRejectsOptionsAndInjection(t *testing.T) {
	for _, s := range []string{"-oProxyCommand=evil", "host;echo bad", "host\nname", "$(evil)"} {
		if e := validateConfig(Config{Peers: []Peer{{ID: "test", Name: "Test", SSH: s, Profile: "devbox"}}}); e == nil {
			t.Fatalf("accepted %q", s)
		}
	}
	if e := validateConfig(Config{Peers: []Peer{{ID: "windows", Name: "Windows", SSH: "white-nova-linux", HostName: "100.127.143.81", HostKeyAlias: "[white-nova]:2222", Profile: "windows"}}}); e != nil {
		t.Fatal(e)
	}
}
func TestCommandOutputBoundAndTimeout(t *testing.T) {
	var b bounded
	_, _ = b.Write(make([]byte, 2*1024*1024))
	if !b.overflow || b.Len() != 1024*1024 {
		t.Fatal("output not bounded")
	}
	if runtime.GOOS == "windows" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, e := command(ctx, "/bin/sleep", "5"); e == nil {
		t.Fatal("deadline ignored")
	}
}

func TestJobLockRecoversAfterProcessDeath(t *testing.T) {
	if path := os.Getenv("MORA_LOCK_TEST_PATH"); path != "" {
		release, err := acquireJobLock(path)
		if err != nil {
			os.Exit(2)
		}
		defer release()
		_ = os.WriteFile(path+".ready", []byte("ready"), 0600)
		time.Sleep(30 * time.Second)
		return
	}
	path := filepath.Join(t.TempDir(), "job.lock")
	child := exec.Command(os.Args[0], "-test.run=^TestJobLockRecoversAfterProcessDeath$")
	child.Env = append(os.Environ(), "MORA_LOCK_TEST_PATH="+path)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	until := time.Now().Add(5 * time.Second)
	for {
		if _, e := os.Stat(path + ".ready"); e == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("child not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if r, e := acquireJobLock(path); e == nil {
		r()
		t.Fatal("concurrent lock acquired")
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	r, e := acquireJobLock(path)
	if e != nil {
		t.Fatal("dead process wedged lock", e)
	}
	r()
}
