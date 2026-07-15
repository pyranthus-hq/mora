package mora

import (
	"encoding/binary"
	"hash/fnv"
	"math"
)

// Embedder turns text into a unit-length dense vector for semantic retrieval. The
// default is a pure-Go, zero-egress, single-binary static embedder (no model
// download, no CGO); an Ollama-backed embedder is an opt-in upgrade (embed_ollama.go).
type Embedder interface {
	// Embed returns a unit-length dense vector, or an error the caller MUST
	// propagate. An embedder that cannot produce a real vector (e.g. Ollama down)
	// NEVER fabricates a zero/substitute vector — HEALTH-12: the recorded incident
	// was a rebuild that committed 3,400 static-fallback vectors and exited 0
	// because Embed could not signal failure. Write paths fail closed; read paths
	// degrade visibly (embed.go / embed_ollama.go).
	Embed(text string) ([]float32, error)
	Dim() int
	ModelID() string // stored per-vector so a model change triggers re-embed
}

// staticEmbedder is a deterministic feature-hashing embedder: it hashes word
// tokens and character n-grams into a fixed-dim space (the "hashing trick", signed
// to keep buckets unbiased), TF-weighted and L2-normalized. Cosine similarity then
// tracks shared lexical + subword features — weaker than a transformer, but $0,
// pure-Go, single-binary, and the correctness anchor (BM25/FTS5) carries exact
// matching. It is the deterministic floor; Ollama is the prose-grade upgrade.
type staticEmbedder struct{ dim int }

// defaultEmbedder is the embedder used during indexing and query unless an Ollama
// daemon is detected and opted into.
func defaultEmbedder() Embedder { return staticEmbedder{dim: 256} }

func (e staticEmbedder) Dim() int        { return e.dim }
func (e staticEmbedder) ModelID() string { return "static-hash-v1" }

// charNGrams adds character trigrams of a token (boundary-padded) to give the
// embedder subword signal, so "launching" and "launch" share features.
func charNGrams(tok string, emit func(string)) {
	r := "^" + tok + "$"
	rs := []rune(r)
	for i := 0; i+3 <= len(rs); i++ {
		emit(string(rs[i : i+3]))
	}
}

// Embed for the static embedder never errors: it is the deterministic, zero-egress
// floor. The error in the return is the interface contract every Embedder shares.
func (e staticEmbedder) Embed(text string) ([]float32, error) {
	vec := make([]float32, e.dim)
	add := func(feature string) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(feature))
		sum := h.Sum64()
		idx := int(sum % uint64(e.dim))
		// Sign bit from a high bit keeps the contribution unbiased across buckets.
		if sum&(1<<63) != 0 {
			vec[idx]++
		} else {
			vec[idx]--
		}
	}
	for _, tok := range tokenizeWords(text) {
		add(tok)
		charNGrams(tok, add)
	}
	normalize(vec)
	return vec, nil
}

// normalize scales vec to unit L2 length in place (zero vector stays zero, so a
// memory with no tokens has a defined — all-zero — embedding).
func normalize(vec []float32) {
	var ss float64
	for _, v := range vec {
		ss += float64(v) * float64(v)
	}
	if ss == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(ss))
	for i := range vec {
		vec[i] *= inv
	}
}

// cosine is the dot product of two vectors; both are L2-normalized at creation, so
// the dot product is the cosine similarity. Mismatched dims return 0 (defensive:
// a stored vector from a different model never silently corrupts a ranking).
func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot
}

// encodeVec serializes a vector as little-endian float32 bytes for the SQLite BLOB.
func encodeVec(vec []float32) []byte {
	b := make([]byte, 4*len(vec))
	for i, v := range vec {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(v))
	}
	return b
}

// decodeVec is the inverse of encodeVec.
func decodeVec(b []byte) []float32 {
	n := len(b) / 4
	vec := make([]float32, n)
	for i := 0; i < n; i++ {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return vec
}
