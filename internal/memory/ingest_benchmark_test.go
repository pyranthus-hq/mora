package memory

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func runBenchmarkCorpus(records int) error {
	_, err := Ingest(IngestParams{
		Fetcher: benchmarkCorpusFetcher{total: records, page: 500}, Kind: "benchmark",
		Status: &SyncStatus{}, Write: func(MappedMemory) error { return nil },
		Limits: IngestLimits{MaxRecords: records, MaxRuntime: 10 * time.Minute},
	})
	return err
}

type benchmarkCorpusFetcher struct {
	total int
	page  int
}

func (f benchmarkCorpusFetcher) FetchPage(_ ItemKind, _ FetchWindow, cursor string) (Page, error) {
	start := 0
	if cursor != "" {
		var err error
		start, err = strconv.Atoi(cursor)
		if err != nil {
			return Page{}, err
		}
	}
	if start >= f.total {
		return Page{}, nil
	}
	end := start + f.page
	if end > f.total {
		end = f.total
	}
	items := make([]Item, 0, end-start)
	for i := start; i < end; i++ {
		items = append(items, Item{Kind: "benchmark", ProviderID: fmt.Sprintf("record-%d", i), Title: "Benchmark record", Body: "bounded representative memory body"})
	}
	next := ""
	if end < f.total {
		next = strconv.Itoa(end)
	}
	return Page{Items: items, NextCursor: next}, nil
}

func benchmarkIngestCorpus(b *testing.B, records int) {
	b.Helper()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		err := runBenchmarkCorpus(records)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func TestIngestReferenceBenchmarkRegression(t *testing.T) {
	if os.Getenv("MORA_ENFORCE_INGEST_BENCH") != "1" {
		t.Skip("reference benchmark gate is opt-in")
	}
	reference := os.Getenv("MORA_INGEST_REFERENCE_BINARY")
	if reference == "" {
		t.Fatal("same-run reference required: run bash scripts/regress/ingest-performance.sh")
	}
	candidate, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, corpus := range []string{"10K", "100K", "1M"} {
		t.Run(corpus, func(t *testing.T) {
			var ratios []float64
			for i := 0; i < 5; i++ {
				// Separate processes give both binaries the same GC startup state.
				// Alternate order so warming/load drift does not favor one side.
				binaries := []string{reference, candidate}
				if i%2 != 0 {
					binaries[0], binaries[1] = binaries[1], binaries[0]
				}
				var samples [2]float64
				for j, binary := range binaries {
					cmd := exec.Command(binary, "-test.run=^$", "-test.bench=^BenchmarkIngestCorpus"+corpus+"$", "-test.benchtime=3x")
					out, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("%s benchmark: %v\n%s", binary, err, out)
					}
					samples[j], err = ingestBenchmarkNS(string(out), "BenchmarkIngestCorpus"+corpus)
					if err != nil {
						t.Fatal(err)
					}
				}
				if i%2 != 0 {
					samples[0], samples[1] = samples[1], samples[0]
				}
				ratio := samples[1] / samples[0]
				t.Logf("pair %d: reference %.0f ns/op, candidate %.0f ns/op, ratio %.3f", i+1, samples[0], samples[1], ratio)
				ratios = append(ratios, ratio)
			}
			sort.Float64s(ratios)
			median := ratios[len(ratios)/2]
			t.Logf("median candidate/reference ratio: %.3f (limit 1.200)", median)
			if median > 1.2 {
				t.Errorf("%s-memory median candidate/reference ratio %.3f exceeds 1.200 (20%% regression)", corpus, median)
			}
		})
	}
}

// Reject missing/malformed measurements instead of silently passing a broken
// reference binary or a renamed benchmark. Values come from Go's benchmark runner.
func ingestBenchmarkNS(output, name string) (float64, error) {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || strings.Split(fields[0], "-")[0] != name {
			continue
		}
		if fields[3] != "ns/op" {
			break
		}
		ns, err := strconv.ParseFloat(fields[2], 64)
		if err == nil && ns > 0 && ns < float64(time.Hour) {
			return ns, nil
		}
		break
	}
	return 0, fmt.Errorf("missing or invalid %s measurement: %s", name, output)
}

func TestIngestBenchmarkMeasurement(t *testing.T) {
	for _, output := range []string{"PASS", "BenchmarkOther-2 3 100 ns/op", "BenchmarkIngestCorpus10K-2 3 0 ns/op", "BenchmarkIngestCorpus10K-2 3 NaN ns/op", "BenchmarkIngestCorpus10K-2 3 +Inf ns/op", "BenchmarkIngestCorpus10K-2 3 100 ms/op"} {
		if _, err := ingestBenchmarkNS(output, "BenchmarkIngestCorpus10K"); err == nil {
			t.Fatalf("accepted invalid measurement %q", output)
		}
	}
	if ns, err := ingestBenchmarkNS("BenchmarkIngestCorpus10K-2 3 12345 ns/op 10 B/op", "BenchmarkIngestCorpus10K"); err != nil || ns != 12345 {
		t.Fatalf("measurement = %v, %v", ns, err)
	}
}

func TestIncremental1000RecordsUnder500MB(t *testing.T) {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := runBenchmarkCorpus(1_000); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated >= 500<<20 {
		t.Fatalf("1,000-record ingest allocated %d bytes, budget <500 MiB", allocated)
	}
}

func BenchmarkIngestCorpus10K(b *testing.B)  { benchmarkIngestCorpus(b, 10_000) }
func BenchmarkIngestCorpus100K(b *testing.B) { benchmarkIngestCorpus(b, 100_000) }
func BenchmarkIngestCorpus1M(b *testing.B)   { benchmarkIngestCorpus(b, 1_000_000) }

func TestIngestNoOpUnderTwoSeconds(t *testing.T) {
	started := time.Now()
	res, err := Ingest(IngestParams{Fetcher: benchmarkCorpusFetcher{page: 500}, Status: &SyncStatus{}, Write: func(MappedMemory) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("no-op sync took %s, budget <2s", elapsed)
	}
	if res.Examined != 0 || res.Stages.Pages != 1 {
		t.Fatalf("no-op receipt = %+v", res)
	}
}
