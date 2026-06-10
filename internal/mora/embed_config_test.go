package mora

import (
	"os"
	"testing"
)

// TestChooseEmbedderForPrecedence pins the resolution order behind the durable
// `embedder = "ollama"` config opt-in: a SET MORA_EMBEDDER env always wins (incl.
// "" → static, the CI-determinism knob), and the config value is consulted only when
// the env is unset. The ollama branch's daemon-reachability is irrelevant here — the
// test asserts the *selection*, not the network — so it is deterministic on any host.
func TestChooseEmbedderForPrecedence(t *testing.T) {
	orig, had := os.LookupEnv("MORA_EMBEDDER")
	t.Cleanup(func() {
		if had {
			os.Setenv("MORA_EMBEDDER", orig)
		} else {
			os.Unsetenv("MORA_EMBEDDER")
		}
	})
	static := defaultEmbedder().ModelID()

	// 1. env="" (the eval/CI knob) forces static even when config opts into ollama.
	os.Setenv("MORA_EMBEDDER", "")
	if got := chooseEmbedderFor(Config{Embedder: "ollama"}).ModelID(); got != static {
		t.Fatalf("env=%q must override the config opt-in → static; got %q", "", got)
	}

	// 2. no env + no config ⇒ the static floor.
	os.Unsetenv("MORA_EMBEDDER")
	if got := chooseEmbedderFor(Config{}).ModelID(); got != static {
		t.Fatalf("no env + no config ⇒ static floor; got %q", got)
	}

	// 3. The config opt-in must resolve IDENTICALLY to the env opt-in (so a
	// config-driven deploy and an env-driven one index/query the same model). Both go
	// through the same ollama branch, so they agree whether the daemon is up or down.
	os.Unsetenv("MORA_EMBEDDER")
	viaCfg := chooseEmbedderFor(Config{Embedder: "ollama"}).ModelID()
	os.Setenv("MORA_EMBEDDER", "ollama")
	viaEnv := chooseEmbedderFor(Config{}).ModelID()
	if viaCfg != viaEnv {
		t.Fatalf("config opt-in must resolve identically to env opt-in: cfg=%q env=%q", viaCfg, viaEnv)
	}
}

// TestConfigEmbedderRoundTrips verifies the durable opt-in survives the
// loadConfig→writeConfig round-trip a re-`init` performs (otherwise the key would be
// silently dropped back to the static floor).
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
