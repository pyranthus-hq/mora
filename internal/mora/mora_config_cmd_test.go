package mora

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestContextProfileScalesBudgets locks the small/default/large knob: the
// profile scales the DEFAULT token budget every budget-bounded surface
// resolves when the caller passes no max_tokens (context_memory, digest,
// brief, the persisted brief artifact) and the digest per-item snippet length.
// The 20k ceiling is profile-INVARIANT — "large" buys a denser default, never
// a bigger one-call maximum.
func TestContextProfileScalesBudgets(t *testing.T) {
	cases := []struct {
		profile     string
		wantTokens  int
		wantSnip    int
		wantCeiling int
	}{
		{"", 6000, 200, 20000},       // default
		{"small", 3000, 120, 20000},  // lean: smaller agent windows / cheap models
		{"large", 12000, 400, 50000}, // dense: big windows, fuller tails, raised one-call ceiling
		{"bogus", 6000, 200, 20000},  // unknown profile falls back to default, never zero
	}
	for _, c := range cases {
		cfg := Config{ContextProfile: c.profile}
		if got := resolveContextBudget(cfg, 0); got != c.wantTokens*charsPerToken {
			t.Errorf("profile %q: resolveContextBudget(Config{}, 0) = %d, want %d", c.profile, got, c.wantTokens*charsPerToken)
		}
		// Explicit caller max_tokens always wins over the profile default…
		if got := resolveContextBudget(cfg, 8000); got != 8000*charsPerToken {
			t.Errorf("profile %q: explicit max_tokens must win, got %d", c.profile, got)
		}
		// …clamped to the PROFILE's ceiling: large opts into 50k per call,
		// small/default keep the 20k guardrail.
		if got := resolveContextBudget(cfg, 999999); got != c.wantCeiling*charsPerToken {
			t.Errorf("profile %q: ceiling = %d tokens, want %d", c.profile, got/charsPerToken, c.wantCeiling)
		}
		if got := cfg.digestSnippetChars(); got != c.wantSnip {
			t.Errorf("profile %q: digestSnippetChars = %d, want %d", c.profile, got, c.wantSnip)
		}
	}
}

// TestCmdConfigContextRoundTrip locks the user-facing knob: `mora config
// context small` persists to config.toml, survives loadConfig, and `mora
// config` (no args) prints the resolved profile. Invalid values error and
// change nothing.
func TestCmdConfigContextRoundTrip(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	out := run(t, "config", "context", "small")
	if !strings.Contains(out, "small") {
		t.Fatalf("set should confirm the new value, got: %s", out)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.ContextProfile != "small" {
		t.Fatalf("ContextProfile = %q after set, want small", cfg.ContextProfile)
	}

	show := run(t, "config")
	if !strings.Contains(show, "context") || !strings.Contains(show, "small") {
		t.Fatalf("`mora config` should show the profile, got: %s", show)
	}

	// default resets (drops the key rather than persisting a redundant value)
	run(t, "config", "context", "default")
	cfg, _ = loadConfig()
	if cfg.ContextProfile != "" {
		t.Fatalf("ContextProfile = %q after reset, want empty", cfg.ContextProfile)
	}

	if err := cmdConfig([]string{"context", "huge"}, io.Discard, testStderr); err == nil {
		t.Fatalf("invalid profile must error")
	}
}

// TestCmdConfigMMRRoundTrip verifies `mora config mmr on|off` persists to
// config.toml, survives loadConfig, shows in `mora config`, drops the line on
// off (reset-to-default), coexists with other owned keys, and rejects garbage.
func TestCmdConfigMMRRoundTrip(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg0 := mustConfig(t)
	tomlPath := filepath.Join(cfg0.ConfigDir, "config.toml")

	// Set an unrelated owned key first, so the round-trip also proves coexistence.
	run(t, "config", "embedder", "ollama")

	out := run(t, "config", "mmr", "on")
	if !strings.Contains(out, "on") {
		t.Fatalf("set should confirm on, got: %s", out)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if !cfg.MMR {
		t.Fatalf("MMR = false after `mmr on`, want true")
	}
	if cfg.Embedder != "ollama" {
		t.Fatalf("setting mmr clobbered embedder: %q", cfg.Embedder)
	}

	show := run(t, "config")
	if !strings.Contains(show, "mmr") || !strings.Contains(show, "on") {
		t.Fatalf("`mora config` should show mmr = on, got: %s", show)
	}

	// off drops the line entirely (reset-to-default), like embedder/context.
	run(t, "config", "mmr", "off")
	cfg, _ = loadConfig()
	if cfg.MMR {
		t.Fatalf("MMR = true after `mmr off`, want false")
	}
	if b, err := os.ReadFile(tomlPath); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(b), "mmr") {
		t.Fatalf("mmr off must drop the line, config.toml still has it:\n%s", b)
	}

	if err := cmdConfig([]string{"mmr", "maybe"}, io.Discard, testStderr); err == nil {
		t.Fatalf("invalid mmr setting must error")
	}
}
