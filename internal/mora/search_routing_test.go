package mora

import "testing"

// embedderIsSemantic is the routing gate behind search_memory and `mora search`.
// The T2 recall eval proved hybrid beats FTS-only ONLY with a real semantic
// embedder, so the default search path must stay FTS-only under the static-hash
// floor and switch to hybrid only when a semantic embedder is actually active —
// otherwise recall regresses (0.591 -> 0.394 @5).
func TestEmbedderIsSemantic(t *testing.T) {
	if embedderIsSemantic(defaultEmbedder()) {
		t.Fatal("static-hash floor must be classified non-semantic (default search stays FTS-only)")
	}
	// ollamaEmbedder.ModelID() is "ollama:<model>" and does not probe the daemon,
	// so this is deterministic and CI-safe without a running Ollama.
	if !embedderIsSemantic(ollamaEmbedder{model: "nomic-embed-text"}) {
		t.Fatal("an Ollama embedder must be classified semantic (default search uses hybrid)")
	}
}
