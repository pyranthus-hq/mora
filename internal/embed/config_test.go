package embed

import (
	"github.com/pyranthus-hq/mora/internal/config"
	"os"
	"testing"
)

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

	// ident collapses a resolution to a comparable identity: the model id on success,
	// or a stable "ERR" marker when the ollama branch fails closed (D2 — an unreachable
	// daemon now returns errEmbedderUnavailable, never a silent static substitute).
	ident := func(e Embedder, err error) string {
		if err != nil {
			return "ERR"
		}
		return e.ModelID()
	}

	// 1. env="" (the eval/CI knob) forces static even when config opts into ollama.
	os.Setenv("MORA_EMBEDDER", "")
	if got := ident(chooseEmbedderFor(config.Config{Embedder: "ollama"})); got != static {
		t.Fatalf("env=%q must override the config opt-in → static; got %q", "", got)
	}

	// 2. no env + no config ⇒ the static floor.
	os.Unsetenv("MORA_EMBEDDER")
	if got := ident(chooseEmbedderFor(config.Config{})); got != static {
		t.Fatalf("no env + no config ⇒ static floor; got %q", got)
	}

	// 3. The config opt-in must resolve IDENTICALLY to the env opt-in (so a
	// config-driven deploy and an env-driven one index/query the same model). Both go
	// through the same ollama branch, so they agree whether the daemon is up or down
	// (here: down ⇒ both fail closed to "ERR").
	os.Unsetenv("MORA_EMBEDDER")
	viaCfg := ident(chooseEmbedderFor(config.Config{Embedder: "ollama"}))
	os.Setenv("MORA_EMBEDDER", "ollama")
	viaEnv := ident(chooseEmbedderFor(config.Config{}))
	if viaCfg != viaEnv {
		t.Fatalf("config opt-in must resolve identically to env opt-in: cfg=%q env=%q", viaCfg, viaEnv)
	}
}
