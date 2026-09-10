package mora

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"
)

func TestActivitySearchDispositionPrePoolRefillAndExactPageCount(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	old := briefClock
	briefClock = func() time.Time { return now }
	t.Cleanup(func() { briefClock = old })
	for i := 0; i < 60; i++ {
		id := fmt.Sprintf("excluded-%02d", i)
		target := Memory{ID: id, Scope: "global", Type: "insight", Source: "manual", Title: id, Text: "throlbex throlbex throlbex strong", CreatedAt: "2026-09-01T00:00:00Z"}
		correction := Memory{ID: "correction-" + id, Scope: "global", Type: "correction", Source: "manual", Title: "explicit correction", Text: "set aside", CreatedAt: "2026-09-02T00:00:00Z", Meta: map[string]any{"target": id, "disposition": "not-context"}}
		if err := writeMemory(cfg, target); err != nil {
			t.Fatal(err)
		}
		if err := writeMemory(cfg, correction); err != nil {
			t.Fatal(err)
		}
	}
	target := Memory{ID: "kept", Scope: "global", Type: "insight", Source: "manual", Title: "kept", Text: "passing throlbex mention with extra unrelated words", CreatedAt: "2026-09-01T00:00:00Z"}
	correction := Memory{ID: "keep-correction", Scope: "global", Type: "correction", Source: "manual", Title: "keep correction", Text: "retain", CreatedAt: "2026-09-02T00:00:00Z", Meta: map[string]any{"target": "kept", "disposition": "keep"}}
	if err := writeMemory(cfg, target); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, correction); err != nil {
		t.Fatal(err)
	}
	mustRebuild(t, cfg)
	baseline := filtersStructured(t, "search_memory", `{"query":"throlbex","limit":1}`)
	ids := filterResultIDs(t, baseline["results"].([]any))
	if len(ids) != 1 || ids[0] == "kept" {
		t.Fatalf("fixture did not place excluded hits above kept: %v", ids)
	}
	if _, ok := baseline["excluded_by_disposition"]; ok {
		t.Fatal("default exclusion receipt appeared")
	}
	filtered := filtersStructured(t, "search_memory", `{"query":"throlbex","limit":1,"exclude_dispositions":["not-context","not-context"]}`)
	ids = filterResultIDs(t, filtered["results"].([]any))
	if len(ids) != 1 || ids[0] != "kept" {
		t.Fatalf("excluded pool crowded eligible row: %v", ids)
	}
	if filtered["excluded_by_disposition"] != float64(1) || filtered["exclusion_count_basis"] != "unexcluded-ranked-page" {
		t.Fatalf("wrong count receipt: %+v", filtered)
	}
	values := filtered["filters"].(map[string]any)["exclude_dispositions"].([]any)
	if len(values) != 1 || values[0] != "not-context" {
		t.Fatal(values)
	}
	row := filtered["results"].([]any)[0].(map[string]any)
	if row["disposition"].(map[string]any)["value"] != "keep" {
		t.Fatal("kept annotation lost")
	}
}

func TestActivitySearchExclusionDoesNotUseForeignScopeOrFutureCorrection(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	cfg := seedRecencyVault(t,
		Memory{ID: "target", Scope: "global", Type: "insight", Source: "manual", Title: "target", Text: "fixture", CreatedAt: "2026-09-01T00:00:00Z"},
		Memory{ID: "foreign", Scope: "work", Type: "correction", Source: "manual", Title: "foreign", Text: "fixture", CreatedAt: "2026-09-02T00:00:00Z", Meta: map[string]any{"target": "target", "disposition": "not-context"}},
		Memory{ID: "future", Scope: "global", Type: "correction", Source: "manual", Title: "future", Text: "fixture", CreatedAt: "2026-09-11T00:00:00Z", Meta: map[string]any{"target": "target", "disposition": "not-context"}})
	f, err := prepareDispositionFilter(cfg, searchFilters{Now: now, ExcludeDispositions: []string{"not-context"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.ExcludedMemoryIDs) != 0 {
		t.Fatal("unapplied correction changed eligibility", f.ExcludedMemoryIDs)
	}
}

func TestActivitySearchEventWindowAcrossRankingArms(t *testing.T) {
	for _, arm := range []string{"fts", "vector", "graph"} {
		t.Run(arm, func(t *testing.T) {
			if arm == "vector" {
				srv := fakeOllama(t, []float64{1, 0, 0, 0})
				defer srv.Close()
				t.Setenv("MORA_EMBEDDER", "ollama")
				t.Setenv("MORA_OLLAMA_URL", srv.URL)
				t.Setenv("MORA_OLLAMA_MODEL", "nomic-embed-text")
			}
			withTempHome(t)
			run(t, "init")
			cfg := mustConfig(t)
			now := time.Date(2026, 9, 10, 12, 0, 0, 123456789, time.UTC)
			old := briefClock
			briefClock = func() time.Time { return now }
			t.Cleanup(func() { briefClock = old })
			query := "throlbex"
			if arm == "vector" {
				query = "qzxvecarmprobe"
			}
			if arm == "graph" {
				query = "Zqvethran Bolgo"
			}
			for i := 0; i < 61; i++ {
				at := now.Add(-48 * time.Hour)
				id := fmt.Sprintf("aaa-decoy-%02d", i)
				body := "unrelated arm filler"
				created := now
				if arm == "fts" {
					body = "throlbex throlbex throlbex strong"
				}
				if i == 60 {
					id = "zzz-target"
					at = now.Add(-time.Hour)
					created = now.Add(-72 * time.Hour)
					body = "weak throlbex mention and unrelated words"
				}
				meta := map[string]any{"occurred_at": at.Format(time.RFC3339Nano)}
				if arm == "graph" {
					meta["from"] = []string{"zqvethran@example.com"}
					meta["to"] = []string{"adit@x.com"}
					meta["names"] = map[string]string{"zqvethran@example.com": "Zqvethran Bolgo"}
				}
				m := Memory{ID: id, Scope: "global", Type: "email", Provider: "gmail", Title: id, Text: body, CreatedAt: created.Format(time.RFC3339Nano), Meta: meta}
				if err := writeMemory(cfg, m); err != nil {
					t.Fatal(err)
				}
			}
			mustRebuild(t, cfg)
			if arm == "graph" {
				db, err := sql.Open("sqlite", roIndexDSN(cfg))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				ids, err := graphExpandIDs(context.Background(), db, query, "", 1, searchFilters{Now: now, EventSinceHours: 24})
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(ids, []string{"zzz-target"}) {
					t.Fatalf("graph prepool failed: %v", ids)
				}
				ids, err = graphExpandIDs(context.Background(), db, query, "", 1, searchFilters{Now: now, EventSinceHours: 24, SinceHours: 24})
				if err != nil {
					t.Fatal(err)
				}
				if len(ids) != 0 {
					t.Fatal("graph event/write windows did not intersect", ids)
				}
				return
			}
			result, err := defaultSearchForMCP(context.Background(), cfg, query, "", 1, searchFilters{Now: now, EventSinceHours: 24})
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Results) != 1 || result.Results[0].ID != "zzz-target" {
				t.Fatalf("%s event prepool filter failed: %+v", arm, result.Results)
			}
			if result.Results[0].EventAt != now.Add(-time.Hour).Format(time.RFC3339Nano) {
				t.Fatalf("event annotation lost: %+v", result.Results[0])
			}
			both, err := defaultSearchForMCP(context.Background(), cfg, query, "", 1, searchFilters{Now: now, EventSinceHours: 24, SinceHours: 24})
			if err != nil {
				t.Fatal(err)
			}
			if len(both.Results) != 0 {
				t.Fatal("event/write windows did not intersect", both.Results)
			}
		})
	}
}

func TestActivitySearchEventWindowUnknownFutureAndNanosecondBounds(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	now := time.Date(2026, 9, 10, 12, 0, 0, 123456789, time.UTC)
	cutoff := now.Add(-24 * time.Hour)
	old := briefClock
	briefClock = func() time.Time { return now }
	t.Cleanup(func() { briefClock = old })
	for id, at := range map[string]string{"lower": cutoff.Format(time.RFC3339Nano), "upper": now.Format(time.RFC3339Nano), "before": cutoff.Add(-time.Nanosecond).Format(time.RFC3339Nano), "future": now.Add(time.Nanosecond).Format(time.RFC3339Nano), "unknown": "", "malformed": "not-a-date"} {
		m := Memory{ID: id, Scope: "global", Type: "event", Provider: "calendar", Title: id, Text: "boundaryprobe", CreatedAt: now.Format(time.RFC3339Nano), Meta: map[string]any{"occurred_at": at}}
		if err := writeMemory(cfg, m); err != nil {
			t.Fatal(err)
		}
	}
	mustRebuild(t, cfg)
	result := filtersStructured(t, "search_memory", `{"query":"boundaryprobe","event_since_hours":24,"limit":20}`)
	ids := filterResultIDs(t, result["results"].([]any))
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, []string{"lower", "upper"}) {
		t.Fatalf("wrong exact inclusive window: %v", ids)
	}
	if result["filters"].(map[string]any)["event_since_hours"] != float64(24) {
		t.Fatal("missing window receipt")
	}
	// A persisted future stamp becomes eligible as the query clock advances, with
	// no rebuild or Markdown mutation between queries.
	later := now.Add(time.Nanosecond)
	out, err := defaultSearchForMCP(context.Background(), cfg, "boundaryprobe", "", 20, searchFilters{Now: later, EventSinceHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range out.Results {
		if m.ID == "future" {
			found = true
		}
	}
	if !found {
		t.Fatal("valid future time was discarded at index construction")
	}
}
