package health

import (
	"github.com/pyranthus-hq/mora/internal/operation"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestFreshSourceCannotMaskDirtyIndex(t *testing.T) {
	h := Health{Sources: []Source{{Key: "gmail", State: Fresh}}, Index: Index{State: IndexDirty, PendingOps: 3, DirtySince: "2026-01-01T12:00:00Z"}}
	if got := AggregateState(h); got != Unhealthy {
		t.Fatalf("aggregate=%q", got)
	}
	if got := BannerAll(h); !strings.Contains(got, "search index is DIRTY") {
		t.Fatalf("banner=%q", got)
	}
}
func TestAggregateStateWorstOfAllArms(t *testing.T) {
	cases := []struct {
		name string
		h    Health
		want string
	}{
		{"healthy", Health{Sources: []Source{{State: Fresh}}, Index: Index{State: IndexFresh}, Producers: []Producer{{State: ProducerFresh}}}, Healthy},
		{"source stale", Health{Sources: []Source{{State: Stale}}}, Degraded}, {"source never", Health{Sources: []Source{{State: Never}}}, Unhealthy}, {"source failed", Health{Sources: []Source{{State: Failed}}}, Unhealthy},
		{"index degraded", Health{Index: Index{State: IndexDegraded}}, Degraded}, {"index never", Health{Index: Index{State: IndexNever}}, Unhealthy}, {"index failed", Health{Index: Index{State: IndexFailed}}, Unhealthy}, {"index dirty", Health{Index: Index{State: IndexDirty}}, Unhealthy},
		{"share degraded", Health{Index: Index{State: IndexFresh, Shares: []Index{{State: IndexDegraded}}}}, Degraded}, {"share never", Health{Index: Index{State: IndexFresh, Shares: []Index{{State: IndexNever}}}}, Unhealthy}, {"share failed", Health{Index: Index{State: IndexFresh, Shares: []Index{{State: IndexFailed}}}}, Unhealthy}, {"share dirty", Health{Index: Index{State: IndexFresh, Shares: []Index{{State: IndexDirty}}}}, Unhealthy},
		{"producer stale", Health{Producers: []Producer{{State: ProducerStale}}}, Degraded}, {"producer never", Health{Producers: []Producer{{State: ProducerNever}}}, Degraded}, {"producer failed", Health{Producers: []Producer{{State: ProducerFailed}}}, Degraded}, {"ledger", Health{Producers: []Producer{{Subject: ProducerSubjectLedger, State: ProducerFresh}}}, Unhealthy},
		{"activity stalled", Health{Activities: []operation.Activity{{State: operation.Stalled}}}, Unhealthy}, {"activity failed", Health{Activities: []operation.Activity{{State: operation.Failed}}}, Unhealthy}, {"activity running", Health{Activities: []operation.Activity{{State: operation.Running}}}, Healthy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := AggregateState(tc.h); got != tc.want {
				t.Fatalf("AggregateState=%q want %q", got, tc.want)
			}
		})
	}
}
func TestProjectionLagHours(t *testing.T) {
	base := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	if got := ProjectionLagHours(Projection{FTSIndexedAt: base.Format(time.RFC3339), GraphIndexedAt: base.Add(-7 * time.Hour).Format(time.RFC3339)}); got != 7 {
		t.Fatal(got)
	}
	for _, p := range []Projection{{}, {FTSIndexedAt: "bad", GraphIndexedAt: base.Format(time.RFC3339)}, {FTSIndexedAt: base.Add(-time.Hour).Format(time.RFC3339), GraphIndexedAt: base.Format(time.RFC3339)}} {
		if got := ProjectionLagHours(p); got != 0 {
			t.Fatalf("got %d", got)
		}
	}
}
func TestAggregateBannerIndexAndShareStates(t *testing.T) {
	now := "2026-01-02T12:34:00Z"
	cases := []struct {
		name     string
		h        Health
		contains string
	}{
		{"fresh", Health{Index: Index{State: IndexFresh}}, ""}, {"never", Health{Index: Index{State: IndexNever}}, "never been built"}, {"blocked", Health{Index: Index{State: IndexFailed, Blocked: true}}, "BLOCKED"}, {"failed", Health{Index: Index{State: IndexFailed, LastError: "db\nboom"}}, "db boom"}, {"degraded", Health{Index: Index{State: IndexDegraded, Embedder: Embedder{Model: "old", Configured: "new"}}}, "built with old, config requests new"},
		{"dirty running", Health{Index: Index{State: IndexDirty, IndexedAt: now}, Activities: []operation.Activity{{State: operation.Running, Kind: operation.KindIndexRebuild, Phase: "writing\nrows"}, {State: operation.Completed}}}, "serving the last committed snapshot from " + now}, {"dirty running no snapshot", Health{Index: Index{State: IndexDirty}, Activities: []operation.Activity{{State: operation.Running, Kind: operation.KindIngest, Phase: "fetching"}}}, "serving the last committed snapshot"}, {"dirty pending one", Health{Index: Index{State: IndexDirty, PendingOps: 1, DirtySince: now}}, "1 vault write not indexed since 12:34"}, {"dirty pending many bad clock", Health{Index: Index{State: IndexDirty, PendingOps: 2, DirtySince: "bad"}}, "2 vault writes not indexed"}, {"dirty graph", Health{Index: Index{State: IndexDirty}}, "graph projection is lagging"}, {"unknown", Health{Index: Index{State: "mystery"}}, "search index is mystery"},
		{"share never", Health{Index: Index{State: IndexFresh, Shares: []Index{{State: IndexNever}}}}, "subscription index has never"}, {"share failed", Health{Index: Index{State: IndexFresh, Shares: []Index{{State: IndexFailed, LastError: "bad"}}}}, "subscription index FAILED — bad"}, {"share degraded", Health{Index: Index{State: IndexFresh, Shares: []Index{{State: IndexDegraded}}}}, "subscription index is DEGRADED"}, {"share dirty", Health{Index: Index{State: IndexFresh, Shares: []Index{{State: IndexDirty}}}}, "subscription index is DIRTY"}, {"share unknown", Health{Index: Index{State: IndexFresh, Shares: []Index{{State: "odd"}}}}, "subscription index is odd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BannerAll(tc.h)
			if tc.contains == "" {
				if got != "" {
					t.Fatal(got)
				}
				return
			}
			if !strings.Contains(got, tc.contains) {
				t.Fatalf("BannerAll=%q want %q", got, tc.contains)
			}
		})
	}
}
func TestAggregateBannerProducerActivityAndPriority(t *testing.T) {
	cases := []struct {
		name     string
		h        Health
		contains string
	}{
		{"ledger", Health{Producers: []Producer{{Subject: ProducerSubjectLedger, LastError: "bad\nledger"}}}, "producer ledger unreadable — bad ledger"}, {"producer never", Health{Producers: []Producer{{Name: "digest", State: ProducerNever}}}, "digest has never been produced"}, {"producer stale", Health{Producers: []Producer{{Name: "brief", State: ProducerStale, AgeHours: 9}}}, "brief has not been produced for 9h"}, {"older producer tie", Health{Producers: []Producer{{Name: "young", State: ProducerStale, AgeHours: 2}, {Name: "old", State: ProducerStale, AgeHours: 8}}}, "old has not been produced"},
		{"failed operation", Health{Activities: []operation.Activity{{State: operation.Failed, Kind: operation.KindIndexRebuild, FailureCode: "disk_full"}}}, "index rebuild operation FAILED (disk_full)"}, {"stalled operation", Health{Activities: []operation.Activity{{State: operation.Stalled, Kind: operation.KindIngest, Phase: "fetching\nmail"}}}, "ingest operation STALLED (phase unknown)"}, {"source never", Health{Sources: []Source{{Key: "calendar", State: Never}}}, "calendar"},
		{"source stale", Health{Sources: []Source{{Key: "mail", State: Stale, AgeHours: 9}}}, "mail"},
		{"source unknown", Health{Sources: []Source{{Key: "oddsource", State: "odd"}}}, "oddsource"},
		{"producer failed", Health{Producers: []Producer{{Name: "digest", State: ProducerFailed, AgeHours: 4}}}, "digest has not been produced"},
		{"producer unknown", Health{Producers: []Producer{{Name: "odd", State: "odd", AgeHours: 1}}}, "odd has not been produced"},
		{"source wins producer", Health{Sources: []Source{{Key: "gmail", State: Failed, LastError: "boom"}}, Producers: []Producer{{Name: "brief", State: ProducerStale, AgeHours: 999}}}, "gmail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BannerAll(tc.h); !strings.Contains(got, tc.contains) {
				t.Fatalf("BannerAll=%q want %q", got, tc.contains)
			}
		})
	}
}
func TestCapBannerLine(t *testing.T) {
	if got := CapBannerLine("one\ntwo\tthree"); got != "one two three" {
		t.Fatal(got)
	}
	long := strings.Repeat("界", 100)
	got := CapBannerLine(long)
	if len(got) > BannerLineCap || !utf8.ValidString(got) || !strings.HasSuffix(got, "…") {
		t.Fatalf("len=%d valid=%v got=%q", len(got), utf8.ValidString(got), got)
	}
	bad := string([]byte{'x', 0xff, 'y'})
	if got := CapBannerLine(bad); !utf8.ValidString(got) {
		t.Fatal("invalid utf8")
	}
}

func TestBannerPrivateEmptyAndSkipBranches(t *testing.T) {
	if got := shareIndexBannerLine(Index{State: IndexFresh}); got != "" {
		t.Fatal(got)
	}
	if got := shareIndexBannerLine(Index{}); got != "" {
		t.Fatal(got)
	}
	if got := indexBannerLineWithActivity(Index{State: IndexFresh}, nil); got != "" {
		t.Fatal(got)
	}
	if got := indexBannerLineWithActivity(Index{}, nil); got != "" {
		t.Fatal(got)
	}
	if got := bannerClockOf(""); got != "" {
		t.Fatal(got)
	}
	if got := BannerAll(Health{Index: Index{State: IndexFresh, Shares: []Index{{State: IndexFresh}, {State: IndexDirty}}}, Producers: []Producer{{State: ProducerFresh}, {Name: "x", State: ProducerStale}}}); !strings.Contains(got, "subscription index is DIRTY") {
		t.Fatal(got)
	}
}
