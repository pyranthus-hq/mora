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

// TestDigestForModel locks the pure /api/tags → digest parser: it resolves a bare
// model name to its (":latest"-tagged) digest, matches an explicit tag exactly, and
// returns "" (caller falls back to the bare model id) for an absent model or garbage.
func TestDigestForModel(t *testing.T) {
	body := []byte(`{"models":[
		{"name":"nomic-embed-text:latest","model":"nomic-embed-text:latest","digest":"sha256:0a109f"},
		{"name":"llama3:8b","model":"llama3:8b","digest":"sha256:beef"}
	]}`)
	cases := []struct {
		model, want string
	}{
		{"nomic-embed-text", "sha256:0a109f"},        // bare name resolves via :latest
		{"nomic-embed-text:latest", "sha256:0a109f"}, // explicit :latest exact
		{"llama3:8b", "sha256:beef"},                 // explicit non-latest tag exact
		{"llama3", ""},                               // bare name with no :latest entry → no match
		{"missing-model", ""},                        // absent
	}
	for _, c := range cases {
		if got := digestForModel(body, c.model); got != c.want {
			t.Errorf("digestForModel(%q) = %q, want %q", c.model, got, c.want)
		}
	}
	if got := digestForModel([]byte("not json"), "nomic-embed-text"); got != "" {
		t.Errorf("garbage body should yield \"\", got %q", got)
	}
	if got := digestForModel([]byte(`{"models":[]}`), "nomic-embed-text"); got != "" {
		t.Errorf("empty models should yield \"\", got %q", got)
	}
}

// TestOllamaModelIDIncludesDigest: the per-vector model id carries the resolved
// digest so an `ollama pull` that re-resolves the same NAME to a new digest changes
// the id (→ stored vectors no longer match the query model → vec arm cleanly empties
// instead of silently mixing two embedding spaces). Empty digest falls back to the
// bare id so existing indexes / daemons that don't list a digest stay compatible.
func TestOllamaModelIDIncludesDigest(t *testing.T) {
	with := ollamaEmbedder{model: "nomic-embed-text", digest: "sha256:0a109f"}
	if got, want := with.ModelID(), "ollama:nomic-embed-text@sha256:0a109f"; got != want {
		t.Errorf("ModelID() with digest = %q, want %q", got, want)
	}
	without := ollamaEmbedder{model: "nomic-embed-text"}
	if got, want := without.ModelID(), "ollama:nomic-embed-text"; got != want {
		t.Errorf("ModelID() without digest = %q, want %q (must preserve old format)", got, want)
	}
}

// TestChooseEmbedderStampsDigest: end-to-end, a reachable daemon that lists the model
// with a digest yields a digest-stamped model id through the real resolver.
func TestChooseEmbedderStampsDigest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"nomic-embed-text:latest","digest":"sha256:deadbeef"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("MORA_EMBEDDER", "ollama")
	t.Setenv("MORA_OLLAMA_URL", srv.URL)
	t.Setenv("MORA_OLLAMA_MODEL", "nomic-embed-text")
	if id, want := chooseEmbedder().ModelID(), "ollama:nomic-embed-text@sha256:deadbeef"; id != want {
		t.Fatalf("digest-stamped embedder id = %q, want %q", id, want)
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
