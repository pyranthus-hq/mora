package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/pyranthus-hq/mora/internal/imessage"
	"github.com/pyranthus-hq/mora/internal/memory"
	"io"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestConnectProgressTickerWritesProgressFile(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	defer stubIMessageReadiness(t, true)()
	defer stubIMessageFetcherPages(t, 3, 4)()
	out := run(t, "connect", "imessage", "--json", "--progress")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	p := connectProgressPath(cfg, "imessage")
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("progress file must be removed at exit: %v", err)
	}
	if _, err := os.Stat(strings.TrimSuffix(p, ".progress.json") + ".receipt.json"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"schema":"mora.connect.imessage"`) {
		t.Fatal(out)
	}
}

func TestDetachArgs(t *testing.T) {
	got := detachArgs([]string{"connect", "imessage", "--json", "--progress", "--detach", "--since-days", "7"})
	want := []string{"connect", "imessage", "--json", "--progress", "--since-days", "7", "--mora-detached-child"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestConnectDetachedHelper(t *testing.T) {
	if os.Getenv("MORA_K16_CHILD") != "1" {
		return
	}
	defer stubIMessageReadiness(t, true)()
	defer stubIMessageFetcherPages(t, 3, 4)()
	// Leave enough reading time to inspect the child's progress and signal it.
	old := imessageReadinessFn
	imessageReadinessFn = func(cfg Config, w io.Writer, b bool) bool {
		time.Sleep(time.Second)
		return old(cfg, w, b)
	}
	if err := Run(context.Background(), []string{"connect", "imessage", "--json", "--progress", "--mora-detached-child"}, os.Stdout, os.Stderr, nil); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestConnectDetachReturnsStartedReceiptAndChildFinishes(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MORA_K16_CHILD", "1")
	t.Setenv("MORA_CONFIG_DIR", cfg.ConfigDir)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := spawnDetached(context.Background(), cfg, "imessage", exe, []string{"-test.run=^TestConnectDetachedHelper$"})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.PID == os.Getpid() || receipt.ProgressPath != connectProgressPath(cfg, "imessage") {
		t.Fatalf("%+v", receipt)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(strings.TrimSuffix(receipt.ProgressPath, ".progress.json") + ".receipt.json")
		if err == nil {
			var r connectReceipt
			if err := json.Unmarshal(b, &r); err != nil {
				t.Fatal(err)
			}
			if r.MessagesRead != 12 || r.Chats != 3 {
				t.Fatalf("%s", b)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("child did not finish")
}

func TestConnectProgressVisibleMidRun(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	defer stubIMessageReadiness(t, true)()
	original := newIMessageFetcher
	defer func() { newIMessageFetcher = original }()
	blocked, release := make(chan struct{}), make(chan struct{})
	newIMessageFetcher = func(string, imessage.DenyList) (iMessageFetcher, error) {
		f := imessage.NewSyntheticFetcher(3, 4)
		calls := 0
		return connectTestFetcher{f, func(ctx context.Context, k memory.ItemKind, w memory.FetchWindow, c string) (memory.Page, error) {
			calls++
			if calls == 2 {
				close(blocked)
				<-release
			}
			return f.FetchPage(k, w, c)
		}}, nil
	}
	done := make(chan error, 1)
	ctx := testCtx(t)
	go func() {
		done <- Run(ctx, []string{"connect", "imessage", "--json", "--progress"}, io.Discard, io.Discard, nil)
	}()
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("reader never blocked")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p, err := readConnectProgress(connectProgressPath(cfg, "imessage"))
		if err == nil && p.MessagesRead > 0 {
			if p.PID != os.Getpid() || p.Source != "imessage" || p.Chats != 1 || p.ElapsedMs < 500 {
				t.Fatalf("%+v", p)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("no nonzero progress during read")
}

func TestConnectHealthReadsInFlight(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	p := connectProgressFile{Schema: schemaConnectProgress, SchemaVersion: 1, Source: "imessage", PID: os.Getpid(),
		StartedAt: cfg.OperationClock().Add(-time.Minute).UTC().Format(time.RFC3339Nano), UpdatedAt: cfg.OperationClock().UTC().Format(time.RFC3339Nano),
		Phase: "reading", MessagesRead: 4, Chats: 1, ElapsedMs: 1000}
	path := connectProgressPath(cfg, "imessage")
	if err := writeConnectFile(path, p); err != nil {
		t.Fatal(err)
	}
	health := func() []connectReadInFlight {
		out := run(t, "companion", "health", "--json")
		var doc struct {
			Reads []connectReadInFlight `json:"reads_in_flight"`
		}
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.Reads == nil {
			t.Fatal("health must include reads_in_flight array")
		}
		return doc.Reads
	}
	if got := health(); len(got) != 1 || got[0].PID != p.PID || got[0].MessagesRead != 4 {
		t.Fatalf("%+v", got)
	}
	p.UpdatedAt = cfg.OperationClock().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	if err := writeConnectFile(path, p); err != nil {
		t.Fatal(err)
	}
	if len(health()) != 1 {
		t.Fatal("old heartbeat with live PID must stay visible")
	}
	cmd := exec.Command("true")
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "exit", "0")
	}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	p.PID = cmd.Process.Pid
	if err := writeConnectFile(path, p); err != nil {
		t.Fatal(err)
	}
	if len(health()) != 0 {
		t.Fatal("stale dead reader shown")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("health must not delete stale file")
	}
	defer stubIMessageReadiness(t, false)()
	run(t, "connect", "imessage", "--json")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("next connect left stale file: %v", err)
	}
}

func TestConnectStartedGolden(t *testing.T) {
	var out bytes.Buffer
	r := connectStartedReceipt{"imessage", 1234, "<home>/state/sync/imessage-imessage.progress.json", "2026-09-11T00:00:00Z"}
	if err := emitReceipt(&out, "mora.connect.started", 1, r); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/contracts/v1/mora.connect.started.json")
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if err := json.Unmarshal(out.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("started golden mismatch: %s", out.String())
	}
}

func TestConnectDetachRejectsInvalidFlags(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	for _, args := range [][]string{
		{"imessage", "--detach"}, {"imessage", "--json", "--detach"},
		{"imessage", "--json", "--progress", "--detach", "--unknown"},
		{"imessage", "--json", "--progress", "--detach", "--since-days", "bad"},
		{"github", "--json", "--detach"},
	} {
		if _, err := runErr(t, append([]string{"connect"}, args...)...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestConnectProgressDoesNotReplaceLiveReader(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	sink := newConnectProgressSink(io.Discard, time.Now)
	if err := sink.reserveFile(cfg, "imessage"); err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	before, err := os.ReadFile(sink.filePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runErr(t, "connect", "imessage", "--json", "--progress"); err == nil {
		t.Fatal("accepted concurrent read")
	}
	after, err := os.ReadFile(sink.filePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("replaced live reader progress")
	}
}

func TestConnectDetachedChildSIGTERMCancels(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGTERM is Unix-only")
	}
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MORA_K16_CHILD", "1")
	t.Setenv("MORA_CONFIG_DIR", cfg.ConfigDir)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, err := spawnDetached(context.Background(), cfg, "imessage", exe, []string{"-test.run=^TestConnectDetachedHelper$"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := os.FindProcess(r.PID)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := terminateDetached(p); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	path := strings.TrimSuffix(r.ProgressPath, ".progress.json") + ".receipt.json"
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			var receipt connectReceipt
			if err := json.Unmarshal(b, &receipt); err != nil {
				t.Fatal(err)
			}
			if !receipt.Cancelled {
				t.Fatalf("SIGTERM did not cancel: %s", b)
			}
			// The receipt is published before progress removal.
			for time.Now().Before(deadline) {
				if _, err := os.Stat(r.ProgressPath); errors.Is(err, os.ErrNotExist) {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
			t.Fatal("cancelled progress not removed")
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("cancelled child did not save receipt")
}
