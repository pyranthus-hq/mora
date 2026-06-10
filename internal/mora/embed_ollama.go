package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

// Ollama opt-in: when MORA_EMBEDDER=ollama and a local daemon answers, embeddings
// come from a transformer model (e.g. nomic-embed-text) instead of the built-in
// static embedder. This is the ONLY path that touches a network socket, it is
// strictly opt-in (never the default), and it only talks to localhost — the
// zero-egress thesis holds (no data leaves the machine). If the daemon is
// unreachable, it degrades to the static embedder with a warning, never an error.

// ollamaEmbedder calls a local Ollama daemon's /api/embeddings endpoint.
type ollamaEmbedder struct {
	baseURL string
	model   string
	dim     int
	client  *http.Client
}

func (e ollamaEmbedder) Dim() int        { return e.dim }
func (e ollamaEmbedder) ModelID() string { return "ollama:" + e.model }

func (e ollamaEmbedder) Embed(text string) []float32 {
	reqBody, _ := json.Marshal(map[string]string{"model": e.model, "prompt": text})
	resp, err := e.client.Post(e.baseURL+"/api/embeddings", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return make([]float32, e.dim) // degrade to a zero vector; never crash indexing
	}
	defer resp.Body.Close()
	var out struct {
		Embedding []float64 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Embedding) == 0 {
		return make([]float32, e.dim)
	}
	vec := make([]float32, len(out.Embedding))
	for i, v := range out.Embedding {
		vec[i] = float32(v)
	}
	normalize(vec) // RRF/cosine assume unit vectors; Ollama returns unnormalized
	return vec
}

// isLoopbackURL reports whether the URL's host is localhost or a loopback IP, so
// the opt-in Ollama path can never send memory text off the machine.
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Hostname()
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// reachable reports whether the daemon answers a cheap request quickly.
func (e ollamaEmbedder) reachable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.baseURL+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// chooseEmbedderFor resolves the active embedder for a config, honoring (in order):
//  1. the MORA_EMBEDDER env var when SET — incl. "" → static. This is the
//     CI-determinism knob (the eval forces "") and the power-user/per-host override.
//  2. cfg.Embedder from config.toml — the DURABLE opt-in that makes both the CLI and
//     the MCP server use semantic retrieval without fragile per-host env wiring.
//  3. the static-hash floor.
//
// Index-time and query-time both call this with the SAME cfg, so the model id stored
// per vector matches the query model (a mismatch just empties the vector arm — the
// FTS + graph arms still answer).
func chooseEmbedderFor(cfg Config) Embedder {
	pref, ok := os.LookupEnv("MORA_EMBEDDER")
	if !ok {
		pref = cfg.Embedder // env unset ⇒ fall back to the durable config opt-in
	}
	return embedderForPref(pref)
}

// chooseEmbedder is the env-only resolver, retained for call sites and tests that
// have no Config in hand. It is exactly chooseEmbedderFor with an empty config.
func chooseEmbedder() Embedder { return embedderForPref(os.Getenv("MORA_EMBEDDER")) }

// embedderForPref maps a resolved preference string to an Embedder: "ollama" yields
// the local Ollama embedder when the daemon is reachable and the URL is loopback;
// anything else (incl. "") yields the deterministic static embedder.
func embedderForPref(pref string) Embedder {
	if pref != "ollama" {
		return defaultEmbedder()
	}
	base := os.Getenv("MORA_OLLAMA_URL")
	if base == "" {
		base = "http://localhost:11434"
	}
	// Zero-egress guard: Embed POSTs memory text to this host, so refuse any
	// non-loopback URL — memory bytes must never leave the machine (codex I2). A
	// misconfigured remote URL falls back to the local static embedder.
	if !isLoopbackURL(base) {
		fmt.Fprintf(os.Stderr, "warn: MORA_OLLAMA_URL %q is not loopback; refusing to send memory text off-machine, using the static embedder\n", base)
		return defaultEmbedder()
	}
	model := os.Getenv("MORA_OLLAMA_MODEL")
	if model == "" {
		model = "nomic-embed-text"
	}
	e := ollamaEmbedder{baseURL: base, model: model, dim: 768, client: &http.Client{Timeout: 30 * time.Second}}
	if !e.reachable() {
		fmt.Fprintln(os.Stderr, "warn: MORA_EMBEDDER=ollama but the Ollama daemon is unreachable; using the built-in static embedder")
		return defaultEmbedder()
	}
	return e
}
