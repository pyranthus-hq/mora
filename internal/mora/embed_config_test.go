package mora

import "testing"

func TestConfigEmbedderRoundTrips(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	cfg.Embedder = "ollama"
	if err := writeConfig(cfg); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	got, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if got.Embedder != "ollama" {
		t.Fatalf("embedder did not round-trip through config.toml: got %q, want %q", got.Embedder, "ollama")
	}
}
