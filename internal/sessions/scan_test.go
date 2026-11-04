package sessions

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFixture(t *testing.T) {
	home := t.TempDir()
	copyFixture(t, home)
	rec, st := scanHome(t, home)
	want := Stats{FilesRead: 7, EmptyFiles: 1, Lines: 72, UnknownRecords: 1, MalformedLines: 1, PartialLastLines: 1,
		CodexCommandsUnreadable: 1, ClaudeSessions: 5, ClaudeSubagentFiles: 1, CodexSessions: 1}
	if st != want {
		t.Fatalf("stats\n got %+v\nwant %+v", st, want)
	}
	byHarness := map[string]int{}
	ids := map[string]bool{}
	for _, c := range rec.calls {
		byHarness[c.Harness]++
		ids[c.ToolUseID] = true
	}
	// 18 surviving Bash calls plus the 4 copies in the fork.
	if byHarness[HarnessClaude] != 22 || byHarness[HarnessCodex] != 2 || len(ids) != 20 {
		t.Fatalf("calls %v, %d distinct ids", byHarness, len(ids))
	}
	kinds := map[string]int{}
	for _, d := range rec.denials {
		kinds[d.Tool+" "+d.Kind]++
	}
	wantKinds := map[string]int{"Bash user-rejected": 5, "Bash interrupted": 3, "Edit user-rejected": 2, "Edit interrupted": 1,
		"Edit cancelled": 1, "Bash permission-rule": 1, "Bash automode-blocked": 1}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("denials %v", kinds)
	}
}

// keepOriginal applies the copy rule of spec section 3.2: of several records
// with one id, keep the lowest (FileFirst, TS, SessionID).
func keepOriginal(calls []Call) map[string]Call {
	keep := map[string]Call{}
	for _, c := range calls {
		key := c.Harness + "/" + c.ToolUseID
		old, seen := keep[key]
		switch {
		case !seen, c.FileFirst.Before(old.FileFirst):
		case c.FileFirst.Equal(old.FileFirst) && c.TS.Before(old.TS):
		case c.FileFirst.Equal(old.FileFirst) && c.TS.Equal(old.TS) && c.SessionID < old.SessionID:
		default:
			continue
		}
		keep[key] = c
	}
	return keep
}

func TestCopiedBashCallsKeepOriginalWhenForkPathSortsFirst(t *testing.T) {
	home := t.TempDir()
	later := "2026-01-01T00:01:00Z"
	claudeCall := func(session string) string {
		return `{"type":"assistant","timestamp":"` + later + `","sessionId":"` + session +
			`","message":{"id":"m","content":[{"type":"tool_use","id":"shared-claude","name":"Bash","input":{"command":"echo fake"}}]}}` + "\n"
	}
	// The first line gives each file its first timestamp, with no type.
	writeFile(t, claudeFile(home, "a-fork.jsonl"), `{"timestamp":"2026-01-02T00:00:00Z"}`+"\n"+claudeCall("fork-session"))
	writeFile(t, claudeFile(home, "z-original.jsonl"), `{"timestamp":"2026-01-01T00:00:00Z"}`+"\n"+claudeCall("original-session"))
	meta := func(id, ts string) string {
		return `{"timestamp":"` + ts + `","type":"session_meta","payload":{"id":"` + id + `","timestamp":"` + ts + `"}}` + "\n"
	}
	shell := `{"timestamp":"` + later + `","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"shared-codex","arguments":"{\"cmd\":\"echo fake\"}"}}` + "\n"
	writeFile(t, codexFile(home, "rollout-a-fork.jsonl"), meta("fork-thread", "2026-01-02T00:00:00Z")+shell)
	writeFile(t, codexFile(home, "rollout-z-original.jsonl"), meta("original-thread", "2026-01-01T00:00:00Z")+shell)
	rec, _ := scanHome(t, home)
	if len(rec.calls) != 4 {
		t.Fatalf("calls %+v", rec.calls)
	}
	keep := keepOriginal(rec.calls)
	if keep["claude/shared-claude"].SessionID != "original-session" || keep["codex/shared-codex"].SessionID != "original-thread" {
		t.Fatalf("kept %+v", keep)
	}
}

func TestOnlySession(t *testing.T) {
	home := t.TempDir()
	copyFixture(t, home)
	rec := &recorder{}
	if _, err := Scan(t.Context(), Options{Home: home, Getenv: noEnv, OnlySession: "sess-c"}, rec); err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 6 || len(rec.denials) != 3 {
		t.Fatalf("calls=%d denials=%d", len(rec.calls), len(rec.denials))
	}
	for _, c := range rec.calls {
		if c.SessionID != "sess-c" {
			t.Fatalf("call from %s", c.SessionID)
		}
	}
}

func TestScanResultDoesNotDependOnWorkers(t *testing.T) {
	home := t.TempDir()
	copyFixture(t, home)
	var want Stats
	for i, workers := range []int{1, 2, 16} {
		rec := &recorder{}
		st, err := Scan(t.Context(), Options{Home: home, Getenv: noEnv, Workers: workers}, rec)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			want = st
		}
		if st != want || len(rec.calls) != 24 {
			t.Fatalf("workers=%d calls=%d stats=%+v", workers, len(rec.calls), st)
		}
	}
}

func TestScanStopsOnCancel(t *testing.T) {
	home := t.TempDir()
	copyFixture(t, home)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rec := &recorder{}
	st, err := Scan(ctx, Options{Home: home, Getenv: noEnv}, rec)
	if !errors.Is(err, context.Canceled) || st.FilesRead != 0 || len(rec.calls) != 0 {
		t.Fatalf("err=%v stats=%+v", err, st)
	}
}

func TestScanRefusesRelativeHome(t *testing.T) {
	for _, home := range []string{"", "relative/home"} {
		if _, err := Scan(t.Context(), Options{Home: home, Getenv: noEnv}, &recorder{}); err == nil {
			t.Errorf("home %q: no error", home)
		}
	}
}

func TestStatsAddSumsEveryField(t *testing.T) {
	var one Stats
	v := reflect.ValueOf(&one).Elem()
	for i := range v.NumField() {
		v.Field(i).SetInt(int64(i + 1))
	}
	var total Stats
	total.add(one)
	total.add(one)
	got := reflect.ValueOf(total)
	for i := range got.NumField() {
		if got.Field(i).Int() != int64(2*(i+1)) {
			t.Fatalf("field %d: %d", i, got.Field(i).Int())
		}
	}
}

func TestUnreadableFileIsACount(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs file modes and a non-root user")
	}
	home := t.TempDir()
	locked := writeFile(t, claudeFile(home, "locked.jsonl"), bash("t1", "ls"))
	writeFile(t, claudeFile(home, "open.jsonl"), bash("t2", "pwd"))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	rec, st := scanHome(t, home)
	if !reflect.DeepEqual(rec.commands(), []string{"pwd"}) || st.UnreadableFiles != 1 || st.FilesRead != 1 {
		t.Fatalf("commands=%q stats=%+v", rec.commands(), st)
	}
}

func TestReadWhileAppending(t *testing.T) {
	home := t.TempDir()
	path := writeFile(t, claudeFile(home, "s1.jsonl"), "")
	const total = 3000
	var written bytes.Buffer
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		defer close(stop)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		put := func(s string) {
			if _, err := f.WriteString(s); err != nil {
				t.Error(err)
			}
			written.WriteString(s)
		}
		for i := range total {
			line := bash("t"+strconv.Itoa(i), "echo "+strconv.Itoa(i))
			// Two writes, so the reader can land on a torn line.
			put(line[:len(line)/2])
			put(line[len(line)/2:])
		}
		put(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"torn"`)
	})
	check := func(rec *recorder) {
		t.Helper()
		for i, command := range rec.commands() {
			if command != "echo "+strconv.Itoa(i) {
				t.Fatalf("call %d saw %q", i, command)
			}
		}
	}
	for running := true; running; {
		select {
		case <-stop:
			running = false
		default:
		}
		rec, st := scanHome(t, home)
		check(rec)
		if st.MalformedLines != 0 {
			t.Fatalf("stats %+v", st)
		}
	}
	wg.Wait()
	rec, st := scanHome(t, home)
	check(rec)
	if len(rec.calls) != total || st.PartialLastLines != 1 {
		t.Fatalf("calls=%d stats=%+v", len(rec.calls), st)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, written.Bytes()) {
		t.Fatal("Scan changed the bytes on disk")
	}
}

type fileState struct {
	body  string
	mtime time.Time
}

func snapshot(t *testing.T, root string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	err := filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		out[path] = fileState{string(body), info.ModTime()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("0400 is not enforced on NTFS")
	}
	home := t.TempDir()
	copyFixture(t, home)
	before := snapshot(t, home)
	for path := range before {
		if err := os.Chmod(path, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	rec, st := scanHome(t, home)
	if st.UnreadableFiles != 0 || st.FilesRead != 7 || len(rec.calls) != 24 {
		t.Fatalf("calls=%d stats=%+v", len(rec.calls), st)
	}
	if after := snapshot(t, home); !reflect.DeepEqual(before, after) {
		t.Fatal("Scan changed a file or its mtime")
	}
}

type countVisitor struct{ calls, denials int }

func (c *countVisitor) Call(Call)     { c.calls++ }
func (c *countVisitor) Denial(Denial) { c.denials++ }

// perfChunk is about 8 MiB of lines, with 34% of the bytes in blobs and one
// 2 MB tool result, repeated to fill each file.
func perfChunk() []byte {
	var b bytes.Buffer
	b.WriteString(toolResult("t1", `,"pad":"`+strings.Repeat("x", 2_000_000)+`"`))
	result := toolResult("t1", `,"toolUseResult":{"stdout":"`+strings.Repeat("y", 4000)+`","interrupted":false}`)
	for i := 0; b.Len() < 5_500_000; i++ {
		b.WriteString(bash("t"+strconv.Itoa(i), "git status --short "+strconv.Itoa(i)))
		b.WriteString(result)
	}
	blob := `{"type":"api-request-blob","pad":"` + strings.Repeat("z", 94_000) + `"}` + "\n"
	for size := b.Len(); float64(b.Len()-size)/float64(b.Len()) < 0.34; {
		b.WriteString(blob)
	}
	return b.Bytes()
}

func TestScanPerf(t *testing.T) {
	gb, err := strconv.ParseFloat(os.Getenv("MORA_PERF_GB"), 64)
	if err != nil || gb <= 0 {
		t.Skip("set MORA_PERF_GB to run")
	}
	home := t.TempDir()
	chunk := perfChunk()
	const fileBytes = 256 << 20
	remaining := int64(gb * 1e9)
	var total int64
	for i := 0; remaining > 0; i++ {
		path := claudeFile(home, fmt.Sprintf("s%d.jsonl", i))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		w := bufio.NewWriter(f)
		for size := int64(0); size < fileBytes && remaining > 0; size += int64(len(chunk)) {
			if _, err := w.Write(chunk); err != nil {
				t.Fatal(err)
			}
			remaining -= int64(len(chunk))
			total += int64(len(chunk))
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	v := &countVisitor{}
	start := time.Now()
	st, err := Scan(t.Context(), Options{Home: home, Getenv: noEnv}, v)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start).Seconds()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	rate := float64(total) / 1e6 / elapsed
	sysMiB := float64(mem.Sys) / (1 << 20)
	t.Logf("%d bytes in %.1fs: %.0f MB/s, Sys %.0f MiB, %d calls, %d files", total, elapsed, rate, sysMiB, v.calls, st.FilesRead)
	if rate < 85 {
		t.Errorf("%.0f MB/s, want at least 85", rate)
	}
	if sysMiB > 512 {
		t.Errorf("Sys %.0f MiB, want at most 512", sysMiB)
	}
}
