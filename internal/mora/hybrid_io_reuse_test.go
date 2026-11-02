package mora

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

// Counting real HTTP requests catches duplicate probes and embeds without
// timing-sensitive sleeps or replacing production search logic with a stub.
func seedHybridIOFixture(t *testing.T, count int) (Config, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var probes, embeds atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		n := probes.Add(1)
		digest := "original"
		if n > 1 {
			digest = "changed-after-routing"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []any{map[string]string{"name": "nomic-embed-text", "digest": digest}}})
	})
	mux.HandleFunc("/api/embeddings", func(w http.ResponseWriter, r *http.Request) {
		embeds.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float64{1, 0, 0, 0}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("MORA_EMBEDDER", "ollama")
	t.Setenv("MORA_OLLAMA_URL", srv.URL)
	t.Setenv("MORA_OLLAMA_MODEL", "nomic-embed-text")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("memory-%03d", i)
		if err := writeMemory(cfg, Memory{ID: id, Scope: "global", Type: "note", Title: "needle " + id, Text: "needle evidence", CreatedAt: "2026-10-01T00:00:00Z"}); err != nil {
			t.Fatal(err)
		}
	}
	probes.Store(0)
	if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
		t.Fatal(err)
	}
	probes.Store(0)
	embeds.Store(0)
	return cfg, &probes, &embeds
}

func TestHybridRouterReusesResolvedEmbedder(t *testing.T) {
	cfg, probes, embeds := seedHybridIOFixture(t, 3)
	got, err := defaultSearchForMCPFiltered(testCtx(t), cfg, "needle", "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if probes.Load() != 1 || embeds.Load() != 1 {
		t.Fatalf("search made %d probes and %d embeddings; want one each", probes.Load(), embeds.Load())
	}
	if !got.SemanticPath || len(got.Trace.Vec) == 0 {
		t.Fatal("retrieval must use the router's original model digest, not a second probe's changed digest")
	}
}

func TestHybridDeepTraceReusesQueryEmbedding(t *testing.T) {
	cfg, probes, embeds := seedHybridIOFixture(t, 60)
	shallow, shallowTrace, err := hybridSearchTrace(testCtx(t), cfg, "needle", "", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	probes.Store(0)
	embeds.Store(0)
	deep, deepTrace, err := hybridSearchTrace(testCtx(t), cfg, "needle", "", 2, 100)
	if err != nil {
		t.Fatal(err)
	}
	if probes.Load() != 1 || embeds.Load() != 1 {
		t.Fatalf("deep trace made %d probes and %d embeddings; want one each", probes.Load(), embeds.Load())
	}
	if !reflect.DeepEqual(shallow, deep) || !reflect.DeepEqual(shallowTrace.Fused, deepTrace.Fused) {
		t.Fatal("deep attribution changed production results or fusion")
	}
	if deepTrace.PreTruncPool != 100 || len(deepTrace.Vec) != 60 || len(shallowTrace.Vec) != 50 {
		t.Fatalf("deep trace not populated: %+v", deepTrace)
	}
}

func TestHybridNoVectorsDoesNotResolveEmbedder(t *testing.T) {
	cfg, probes, embeds := seedHybridIOFixture(t, 3)
	actual, err := sql.Open("sqlite", rwIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := actual.Exec("DELETE FROM mem_vectors"); err != nil {
		actual.Close()
		t.Fatal(err)
	}
	actual.Close()
	_, trace, err := hybridSearchTrace(testCtx(t), cfg, "needle", "", 2, 100)
	if err != nil {
		t.Fatal(err)
	}
	if probes.Load() != 0 || embeds.Load() != 0 || len(trace.Vec) != 0 {
		t.Fatal("empty vector index must not resolve or call the embedder")
	}
}
