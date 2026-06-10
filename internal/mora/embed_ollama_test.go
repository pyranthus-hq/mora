package mora

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeOllama serves /api/tags (reachability) and /api/embeddings (a fixed vector).
func fakeOllama(t *testing.T, vec []float64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[]}`))
	})
	mux.HandleFunc("/api/embeddings", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": vec})
	})
	return httptest.NewServer(mux)
}

func TestOllamaEmbedderNormalizes(t *testing.T) {
	srv := fakeOllama(t, []float64{3, 4}) // |v| = 5, should normalize to 0.6,0.8
	defer srv.Close()
	e := ollamaEmbedder{baseURL: srv.URL, model: "nomic-embed-text", dim: 2, client: &http.Client{Timeout: 5 * time.Second}}

	if !e.reachable() {
		t.Fatal("fake daemon should be reachable")
	}
	v := e.Embed("hello")
	var ss float64
	for _, x := range v {
		ss += float64(x) * float64(x)
	}
	if math.Abs(ss-1) > 1e-5 {
		t.Fatalf("ollama embedding not normalized: |v|^2 = %v", ss)
	}
	if e.ModelID() != "ollama:nomic-embed-text" {
		t.Fatalf("model id = %q", e.ModelID())
	}
}

func TestOllamaEmbedderDegradesWhenDown(t *testing.T) {
	// Unreachable daemon: Embed returns a defined zero vector, never panics.
	e := ollamaEmbedder{baseURL: "http://127.0.0.1:1", model: "m", dim: 4, client: &http.Client{Timeout: 200 * time.Millisecond}}
	if e.reachable() {
		t.Fatal("nothing should be listening on port 1")
	}
	v := e.Embed("anything")
	if len(v) != 4 {
		t.Fatalf("degraded embed should return a dim-length zero vector, got %d", len(v))
	}
	for _, x := range v {
		if x != 0 {
			t.Fatal("degraded embed should be the zero vector")
		}
	}
}

func TestChooseEmbedderDefaultsToStatic(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "")
	if id := chooseEmbedder().ModelID(); id != "static-hash-v1" {
		t.Fatalf("default embedder = %q, want static", id)
	}
}

func TestChooseEmbedderOllamaWhenOptedInAndReachable(t *testing.T) {
	srv := fakeOllama(t, []float64{1, 0})
	defer srv.Close()
	t.Setenv("MORA_EMBEDDER", "ollama")
	t.Setenv("MORA_OLLAMA_URL", srv.URL)
	t.Setenv("MORA_OLLAMA_MODEL", "nomic-embed-text")
	if id := chooseEmbedder().ModelID(); id != "ollama:nomic-embed-text" {
		t.Fatalf("opted-in embedder = %q, want ollama", id)
	}
}

func TestChooseEmbedderFallsBackWhenUnreachable(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "ollama")
	t.Setenv("MORA_OLLAMA_URL", "http://127.0.0.1:1")
	if id := chooseEmbedder().ModelID(); id != "static-hash-v1" {
		t.Fatalf("unreachable ollama should fall back to static, got %q", id)
	}
}

// TestChooseEmbedderRefusesNonLoopback is the zero-egress guard (codex I2): a
// remote MORA_OLLAMA_URL must never receive memory text — fall back to static.
func TestChooseEmbedderRefusesNonLoopback(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "ollama")
	for _, bad := range []string{"http://example.com:11434", "http://10.0.0.5:11434", "https://api.openai.com"} {
		t.Setenv("MORA_OLLAMA_URL", bad)
		if id := chooseEmbedder().ModelID(); id != "static-hash-v1" {
			t.Fatalf("non-loopback %q must fall back to static, got %q", bad, id)
		}
	}
}

func TestIsLoopbackURL(t *testing.T) {
	for _, ok := range []string{"http://localhost:11434", "http://127.0.0.1:11434", "http://[::1]:11434"} {
		if !isLoopbackURL(ok) {
			t.Fatalf("%q should be loopback", ok)
		}
	}
	for _, bad := range []string{"http://example.com", "http://10.0.0.5", "http://169.254.1.1", "garbage"} {
		if isLoopbackURL(bad) {
			t.Fatalf("%q should NOT be loopback", bad)
		}
	}
}
