package sessions_test

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/corrections"
	"github.com/pyranthus-hq/mora/internal/sessions"
)

// perfMark is replaced in each copy of the chunk, so ids stay unique.
const perfMark = "@@@@@@@@"

// perfChunk is about 8 MiB of lines, with 34% of the bytes in blobs and one
// 2 MB tool result, repeated to fill each file.
func perfChunk() []byte {
	const stamp = `"timestamp":"2026-10-01T10:00:00.000Z","sessionId":"s` + perfMark + `"`
	result := func(id, extra string) string {
		return `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"text","is_error":true,"tool_use_id":"` +
			id + `"}]},` + stamp + extra + "}\n"
	}
	var b bytes.Buffer
	b.WriteString(result("t0", `,"pad":"`+strings.Repeat("x", 2_000_000)+`"`))
	stdout := `,"toolUseResult":{"stdout":"` + strings.Repeat("y", 4000) + `","interrupted":false}`
	for i := 0; b.Len() < 5_500_000; i++ {
		id := "t" + perfMark + "-" + strconv.Itoa(i)
		fmt.Fprintf(&b, `{"message":{"id":"m-%s","role":"assistant","content":[{"type":"tool_use","id":"%s","name":"Bash","input":{"command":"git status --short %d"}}]},"type":"assistant",%s}`+"\n",
			id, id, i, stamp)
		b.WriteString(result(id, stdout))
	}
	blob := `{"type":"api-request-blob","pad":"` + strings.Repeat("z", 94_000) + `"}` + "\n"
	for size := b.Len(); float64(b.Len()-size)/float64(b.Len()) < 0.34; {
		b.WriteString(blob)
	}
	return b.Bytes()
}

// TestScanPerf measures what mora check does, so it scans into a real Corpus.
// The per-call rows are where the memory goes.
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
	chunks := 0
	for i := 0; remaining > 0; i++ {
		path := filepath.Join(home, ".claude", "projects", "-fake-work", fmt.Sprintf("s%d.jsonl", i))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		w := bufio.NewWriter(f)
		for size := int64(0); size < fileBytes && remaining > 0; size += int64(len(chunk)) {
			chunks++
			if _, err := w.Write(bytes.ReplaceAll(chunk, []byte(perfMark), fmt.Appendf(nil, "%08d", chunks))); err != nil {
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
	corpus := corrections.NewCorpus(time.Time{})
	start := time.Now()
	st, err := sessions.Scan(t.Context(), sessions.Options{Home: home, Getenv: func(string) string { return "" }}, corpus)
	if err != nil {
		t.Fatal(err)
	}
	report := corpus.Report(st)
	elapsed := time.Since(start).Seconds()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	rate := float64(total) / 1e6 / elapsed
	sysMiB := float64(mem.Sys) / (1 << 20)
	t.Logf("%d bytes in %.1fs: %.0f MB/s, Sys %.0f MiB, %d calls, %d files", total, elapsed, rate, sysMiB, report.Harnesses[0].Calls, st.FilesRead)
	if rate < 85 {
		t.Errorf("%.0f MB/s, want at least 85", rate)
	}
	if sysMiB > 512 {
		t.Errorf("Sys %.0f MiB, want at most 512", sysMiB)
	}
}
