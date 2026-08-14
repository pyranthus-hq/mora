package mora

import embedpkg "github.com/pyranthus-hq/mora/internal/embed"

type Embedder = embedpkg.Embedder

func defaultEmbedder() Embedder      { return embedpkg.Default() }
func cosine(a, b []float32) float64  { return embedpkg.Cosine(a, b) }
func encodeVec(vec []float32) []byte { return embedpkg.EncodeVec(vec) }
func decodeVec(b []byte) []float32   { return embedpkg.DecodeVec(b) }
