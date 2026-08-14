package mora

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	embedpkg "github.com/pyranthus-hq/mora/internal/embed"
)

func TestRRFFusion(t *testing.T) {
	// id "b" is rank-1 in list A and rank-2 in list B; id "a" is rank-2 in A only.
	score := rrf([][]string{{"x", "b", "a"}, {"y", "b"}}, rrfK)
	if score["b"] <= score["a"] {
		t.Fatalf("b (in both lists) should outscore a: b=%.4f a=%.4f", score["b"], score["a"])
	}
	// Rank-1 of a single list beats rank-3 of a single list.
	if score["x"] <= score["a"] {
		t.Fatalf("rank-1 x should beat rank-3 a: x=%.4f a=%.4f", score["x"], score["a"])
	}
}

func fakeOllama(t *testing.T, vec []float64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"models":[]}`)) })
	mux.HandleFunc("/api/embeddings", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": vec})
	})
	return httptest.NewServer(mux)
}

type ollamaEmbedder struct {
	baseURL, model, digest string
	dim                    int
	client                 *http.Client
}

func (e ollamaEmbedder) inner() embedpkg.OllamaEmbedder {
	return embedpkg.NewOllamaEmbedder(e.baseURL, e.model, e.digest, e.dim, e.client)
}
func (e ollamaEmbedder) Embed(s string) ([]float32, error) { return e.inner().Embed(s) }
func (e ollamaEmbedder) Dim() int                          { return e.inner().Dim() }
func (e ollamaEmbedder) ModelID() string                   { return e.inner().ModelID() }
func (e ollamaEmbedder) Digest() string                    { return e.inner().Digest() }
func (e ollamaEmbedder) probe() (string, bool)             { return e.inner().Probe() }
func (e ollamaEmbedder) reachable() bool                   { return e.inner().Reachable() }
