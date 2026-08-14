package embed

import (
	"bytes"
	"context"

	"encoding/json"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/config"
	"io"
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
// zero-egress thesis holds (no data leaves the machine). If an opted-in daemon is
// unreachable, resolution FAILS CLOSED (errEmbedderUnavailable) — it never silently
// substitutes the static embedder (HEALTH-12, D2); callers propagate the error.

// ollamaEmbedder calls a local Ollama daemon's /api/embeddings endpoint.
type OllamaEmbedder struct {
	baseURL string
	model   string
	digest  string // resolved model digest from /api/tags; "" when the daemon doesn't list it
	dim     int
	client  *http.Client
}

func (e OllamaEmbedder) Dim() int { return e.dim }

// ModelID is stamped on every stored vector and used as the query-time match key.
// It carries the resolved model digest when known: an `ollama pull` that re-resolves
// the same model NAME to new weights produces a new digest → a new ModelID → the
// already-stored vectors no longer match the query model, so the vector arm cleanly
// empties (FTS + graph still answer) instead of silently comparing vectors from two
// different embedding spaces. With no digest it degrades to the bare "ollama:<model>"
// form, which keeps indexes built by older binaries (or daemons that don't list a
// digest) compatible.
func (e OllamaEmbedder) ModelID() string {
	if e.digest == "" {
		return "ollama:" + e.model
	}
	return "ollama:" + e.model + "@" + e.digest
}

// digestForModel extracts the content digest of `model` from an Ollama /api/tags
// response body. Ollama resolves a bare name (e.g. "nomic-embed-text") to its
// ":latest" tag, so a bare name matches both an exact entry and the ":latest"-tagged
// one. Returns "" if the model is absent or the body is unparseable — the caller then
// falls back to the bare model id.
func digestForModel(tagsBody []byte, model string) string {
	var out struct {
		Models []struct {
			Name   string `json:"name"`
			Model  string `json:"model"`
			Digest string `json:"digest"`
		} `json:"models"`
	}
	if err := json.Unmarshal(tagsBody, &out); err != nil {
		return ""
	}
	for _, m := range out.Models {
		for _, name := range []string{m.Name, m.Model} {
			if name == model || name == model+":latest" {
				return m.Digest
			}
		}
	}
	return ""
}

// Digest returns the resolved Ollama model digest ("" when the daemon didn't list
// one). It is stamped as index provenance (embedder_digest) so a later `ollama pull`
// that re-resolves the same NAME to new weights is detectable (HEALTH-12, D3).
func (e OllamaEmbedder) Digest() string { return e.digest }

// Embed calls the daemon and returns an error on ANY transport/decode failure — it
// NEVER fabricates a zero vector. The pre-D1 code returned a dim-length zero slice on
// failure "to never crash indexing"; that is exactly how a daemon that died
// mid-rebuild committed zero vectors stamped with the real Ollama model id and the
// rebuild still exited 0 (the recorded incident, HEALTH-12). A real error lets the
// rebuild tx roll back to the last good index (index.go's deferred tx.Rollback).
func (e OllamaEmbedder) Embed(text string) ([]float32, error) {
	reqBody, _ := json.Marshal(map[string]string{"model": e.model, "prompt": text})
	resp, err := e.client.Post(e.baseURL+"/api/embeddings", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errEmbedderUnavailable, err)
	}
	defer resp.Body.Close()
	var out struct {
		Embedding []float64 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: decoding /api/embeddings: %v", errEmbedderUnavailable, err)
	}
	if len(out.Embedding) == 0 {
		return nil, fmt.Errorf("%w: /api/embeddings returned an empty vector", errEmbedderUnavailable)
	}
	vec := make([]float32, len(out.Embedding))
	for i, v := range out.Embedding {
		vec[i] = float32(v)
	}
	normalize(vec) // RRF/cosine assume unit vectors; Ollama returns unnormalized
	return vec, nil
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

// probe does one GET /api/tags: it reports whether the daemon answers quickly and,
// on success, the resolved digest of e.model ("" if the daemon doesn't list it).
// Reachability and digest resolution share the single request — no extra round-trip.
func (e OllamaEmbedder) probe() (digest string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.baseURL+"/api/tags", nil)
	if err != nil {
		return "", false
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", true // reachable but digest unreadable → fall back to the bare model id
	}
	return digestForModel(body, e.model), true
}

// reachable reports whether the daemon answers a cheap request quickly.
func (e OllamaEmbedder) reachable() bool { _, ok := e.probe(); return ok }

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
func chooseEmbedderFor(cfg config.Config) (Embedder, error) {
	pref, ok := os.LookupEnv("MORA_EMBEDDER")
	if !ok {
		pref = cfg.Embedder // env unset ⇒ fall back to the durable config opt-in
	}
	return embedderForPref(pref)
}

// chooseEmbedder is the env-only resolver, retained for call sites and tests that
// have no Config in hand. It is exactly chooseEmbedderFor with an empty config.
func chooseEmbedder() (Embedder, error) { return embedderForPref(os.Getenv("MORA_EMBEDDER")) }

// errEmbedderUnavailable is the fail-closed sentinel (HEALTH-12): an EXPLICIT
// `ollama` preference whose daemon is unreachable — or whose URL is non-loopback —
// resolves to this error, NEVER a silent static substitute. It is the ONE
// load-bearing gate (Packet D2): every caller propagates it. Write paths hard-fail
// (a rebuild refuses rather than re-embedding the whole vault with the static
// fallback); read paths degrade visibly to FTS and redden the health banner.
var errEmbedderUnavailable = errors.New("configured embedder (ollama) is unavailable; refusing to substitute the static embedder — start the Ollama daemon, fix MORA_OLLAMA_URL, or set embedder=static")

// embedderForPref maps a resolved preference string to an Embedder. "ollama" yields
// the local Ollama embedder ONLY when the daemon is reachable and the URL is
// loopback; an unreachable daemon or a non-loopback URL returns errEmbedderUnavailable
// (it NEVER substitutes the static embedder — that silent swap is the recorded
// incident). Anything else (incl. "") is the deterministic static floor, which
// cannot fail.
func embedderForPref(pref string) (Embedder, error) {
	if pref != "ollama" {
		return defaultEmbedder(), nil
	}
	base := os.Getenv("MORA_OLLAMA_URL")
	if base == "" {
		base = "http://localhost:11434"
	}
	// Zero-egress guard: Embed POSTs memory text to this host, so refuse any
	// non-loopback URL — memory bytes must never leave the machine (codex I2). This
	// is a fail-closed refusal, NOT a silent fall back to static: an explicit ollama
	// opt-in pointed at a bad URL must be visible, never quietly downgraded.
	if !isLoopbackURL(base) {
		return nil, fmt.Errorf("%w: MORA_OLLAMA_URL %q is not loopback (memory text must never leave the machine)", errEmbedderUnavailable, base)
	}
	model := os.Getenv("MORA_OLLAMA_MODEL")
	if model == "" {
		model = "nomic-embed-text"
	}
	e := OllamaEmbedder{baseURL: base, model: model, dim: 768, client: &http.Client{Timeout: 30 * time.Second}}
	digest, ok := e.probe()
	if !ok {
		return nil, fmt.Errorf("%w: the Ollama daemon at %s is unreachable", errEmbedderUnavailable, base)
	}
	e.digest = digest // stamp the resolved digest into ModelID() (see ollamaEmbedder.ModelID)
	return e, nil
}

// ErrUnavailable is returned when an explicit Ollama preference cannot be honored safely.
var ErrUnavailable = errEmbedderUnavailable

func ChooseFor(cfg config.Config) (Embedder, error) { return chooseEmbedderFor(cfg) }
func Choose() (Embedder, error)                     { return chooseEmbedder() }
func NewOllamaEmbedder(baseURL, model, digest string, dim int, client *http.Client) OllamaEmbedder {
	return OllamaEmbedder{baseURL: baseURL, model: model, digest: digest, dim: dim, client: client}
}
func (e OllamaEmbedder) Probe() (string, bool) { return e.probe() }
func (e OllamaEmbedder) Reachable() bool       { return e.reachable() }

type ollamaEmbedder = OllamaEmbedder

func embedderDigestOf(e Embedder) string {
	if d, ok := e.(interface{ Digest() string }); ok {
		return d.Digest()
	}
	return ""
}
func DigestOf(e Embedder) string { return embedderDigestOf(e) }
