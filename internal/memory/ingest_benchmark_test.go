package memory

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
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
	// Median Apple M1 Pro baselines captured 2026-08-24. CI fails above +20%.
	baselines := []struct {
		records  int
		duration time.Duration
	}{
		{10_000, 9_272_833 * time.Nanosecond},
		{100_000, 53_075_959 * time.Nanosecond},
		{1_000_000, 530_479_750 * time.Nanosecond},
	}
	for _, baseline := range baselines {
		var samples []time.Duration
		for i := 0; i < 3; i++ {
			started := time.Now()
			if err := runBenchmarkCorpus(baseline.records); err != nil {
				t.Fatal(err)
			}
			samples = append(samples, time.Since(started))
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		limit := baseline.duration + baseline.duration/5
		if samples[1] > limit {
			t.Fatalf("%d-memory median %s exceeds baseline %s by more than 20%%", baseline.records, samples[1], baseline.duration)
		}
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
