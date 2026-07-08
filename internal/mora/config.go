package mora

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/mattn/go-isatty"
)

// fusion returns the active RRF fusion params: the per-Config override when set,
// else the production default. Single source of truth for search + eval.
func (c Config) fusion() fusionParams {
	if c.fusionOv != nil {
		return *c.fusionOv
	}
	return defaultFusion
}

// mmr returns the active MMR params, or nil when MMR is off. The eval/test override
// (mmrOv) wins; else the durable user opt-in (MMR) yields default params; else nil.
// Single source of truth, mirroring fusion(). The returned params carry force only
// when set via mmrOv — the MMR bool path always has force=false.
func (c Config) mmr() *mmrParams {
	if c.mmrOv != nil {
		return c.mmrOv
	}
	if c.MMR {
		return &mmrParams{lambda: defaultLambda}
	}
	return nil
}
func defaultConfig() Config {
	// MORA_CONFIG_DIR points an entire invocation at an ISOLATED install
	// (scripts, launchd jobs, demos, tests): config, vault, derived index, and
	// watermark state ALL default under the override. Re-rooting only the
	// config dir was not enough — a scratch `init` then rebuilt (wiped) the
	// LIVE ~/.local/share index.db and shared the live watermark state, the
	// exact incident class this env var exists to prevent. A config.toml
	// inside the override still wins for any dir it names (loadConfig
	// overlays).
	if dir := os.Getenv("MORA_CONFIG_DIR"); dir != "" {
		return Config{
			VaultDir:  filepath.Join(dir, "vault"),
			ConfigDir: dir,
			DataDir:   filepath.Join(dir, "data"),
			StateDir:  filepath.Join(dir, "state"),
		}
	}
	home, _ := os.UserHomeDir()
	return Config{
		VaultDir:  filepath.Join(home, "vault", "mora"),
		ConfigDir: filepath.Join(home, ".config", "mora"),
		DataDir:   filepath.Join(home, ".local", "share", "mora"),
		StateDir:  filepath.Join(home, ".local", "state", "mora"),
	}
}

// parseConfigValue extracts a config value from the raw right-hand side of a
// `key = value` line. A quoted value parses via strconv.Unquote (escapes
// honored) and anything after the closing quote — an inline comment — is
// ignored; the old strip-outer-quotes approach loaded `"/x" # note` as the
// garbage path `/x" # note`, which the read-modify-write writeConfig then
// persisted back, orphaning the real vault. Hand-editing config.toml is a
// path our own refusal messages recommend, so it must parse exactly. An
// unquoted value cuts at the first '#'.
func parseConfigValue(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, `"`) {
		for i := 1; i < len(raw); i++ {
			switch raw[i] {
			case '\\':
				i++ // skip the escaped byte
			case '"':
				if v, err := strconv.Unquote(raw[:i+1]); err == nil {
					return v
				}
				return strings.Trim(raw[:i+1], `"`)
			}
		}
		return strings.Trim(raw, `"`) // unterminated quote: legacy lenient read
	}
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSpace(raw)
}
func loadConfig() (Config, error) {
	cfg := defaultConfig()
	path := filepath.Join(cfg.ConfigDir, "config.toml")
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := expandHome(parseConfigValue(parts[1]))
		switch key {
		case "vault_dir":
			cfg.VaultDir = val
		case "data_dir":
			cfg.DataDir = val
		case "state_dir":
			cfg.StateDir = val
		case "embedder":
			cfg.Embedder = val
		case "context":
			cfg.ContextProfile = val
		case "mmr":
			// Bool opt-in (`mmr = true`); only "true"/"1" enable. A bool can't be
			// mistyped into a silent wrong-mode the way a free-form string can.
			cfg.MMR = val == "true" || val == "1"
		}
	}
	return cfg, nil
}

// cmdConfig is the durable-settings surface: `mora config` shows the resolved
// configuration; `mora config context <small|default|large>` sets the context
// profile (the quality/size knob — small for lean agent windows, large for
// denser briefs/digests whose conversation tails survive the snippet clip);
// `mora config embedder <ollama|static>` is the same durable seam the retrieval
// docs point at. "default"/"static" reset by DROPPING the key rather than
// persisting a redundant value, so config.toml stays minimal.
func cmdConfig(args []string, stdout io.Writer) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if len(args) == 0 {
		profile := cfg.ContextProfile
		if profile == "" {
			profile = "default"
		}
		embedder := cfg.Embedder
		if embedder == "" {
			embedder = "static"
		}
		mmr := "off"
		if cfg.MMR {
			mmr = "on"
		}
		fmt.Fprintf(stdout, "vault_dir = %s   ← your memories (back this up)\n", cfg.VaultDir)
		fmt.Fprintf(stdout, "data_dir  = %s   ← search index (rebuildable)\n", cfg.DataDir)
		fmt.Fprintf(stdout, "state_dir = %s   ← sync watermarks (rebuildable)\n", cfg.StateDir)
		fmt.Fprintf(stdout, "config    = %s   ← settings + tokens\n", cfg.ConfigDir)
		fmt.Fprintf(stdout, "embedder  = %s\ncontext   = %s  (default budget %d tokens, digest snippets %d chars; ceiling %d)\nmmr       = %s\n",
			embedder, profile,
			cfg.contextDefaultTokens(), cfg.digestSnippetChars(), cfg.contextMaxTokens(), mmr)
		return nil
	}
	// Machine-readable path dump for tooling (uninstall.ps1 -Purge consumes this
	// instead of scraping the human output, which truncated paths with double
	// spaces and mojibake'd non-ASCII paths under PowerShell 5.1's OEM decoding).
	if len(args) == 1 && args[0] == "--json" {
		b, err := json.MarshalIndent(map[string]string{
			"vault_dir":  cfg.VaultDir,
			"data_dir":   cfg.DataDir,
			"state_dir":  cfg.StateDir,
			"config_dir": cfg.ConfigDir,
		}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, string(b))
		return nil
	}
	if len(args) != 2 {
		return errors.New("usage: mora config [context <small|default|large> | embedder <ollama|static> | mmr <on|off>]")
	}
	key, val := args[0], strings.ToLower(strings.TrimSpace(args[1]))
	switch key {
	case "context":
		switch val {
		case "small", "large":
			cfg.ContextProfile = val
		case "default":
			cfg.ContextProfile = ""
		default:
			return fmt.Errorf("unknown context profile %q (want small, default, or large)", val)
		}
	case "embedder":
		switch val {
		case "ollama":
			cfg.Embedder = val
		case "static", "default":
			cfg.Embedder = ""
		default:
			return fmt.Errorf("unknown embedder %q (want ollama or static)", val)
		}
	case "mmr":
		switch val {
		case "on", "true", "1":
			cfg.MMR = true
		case "off", "false", "0", "default":
			cfg.MMR = false
		default:
			return fmt.Errorf("unknown mmr setting %q (want on or off)", val)
		}
	default:
		return fmt.Errorf("unknown config key %q (want context, embedder, or mmr)", key)
	}
	if err := writeConfig(cfg); err != nil {
		return err
	}
	shown := val
	if key == "mmr" {
		shown = "off"
		if cfg.MMR {
			shown = "on"
		}
	}
	fmt.Fprintf(stdout, "%s = %s\n", key, shown)
	if key == "mmr" && cfg.MMR && cfg.Embedder != "ollama" {
		fmt.Fprintln(stdout, "note: MMR reranks on vector similarity, so it only takes effect under a semantic embedder — run `mora config embedder ollama`.")
	}
	if key == "context" {
		fmt.Fprintf(stdout, "(default budget %d tokens, digest snippets %d chars; per-call max_tokens still wins, ceiling %d)\n",
			cfg.contextDefaultTokens(), cfg.digestSnippetChars(), cfg.contextMaxTokens())
	}
	return nil
}

// writeConfig persists the five keys this binary owns by READ-MODIFY-WRITE:
// every line it does not own (comments, blank lines, keys written by hand or
// by a newer mora) is preserved byte-for-byte. The old regenerate-from-struct
// behavior silently ate those lines on every rewrite — loadConfig skips
// unknowns, so they survived the load only to vanish on the next save. An
// empty Embedder/ContextProfile DROPS its line (reset-to-default semantics,
// keeping config.toml minimal); an empty DIR value is broken either way but
// is preserved verbatim — dropping it would silently repoint the install to
// the defaults via an unrelated rewrite.
func writeConfig(cfg Config) error {
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(cfg.ConfigDir, "config.toml")
	mmrVal := ""
	if cfg.MMR {
		mmrVal = "true"
	}
	owned := []struct{ key, val string }{
		{"vault_dir", cfg.VaultDir},
		{"data_dir", cfg.DataDir},
		{"state_dir", cfg.StateDir},
		{"embedder", cfg.Embedder},
		{"context", cfg.ContextProfile},
		{"mmr", mmrVal}, // "" ⇒ off ⇒ line dropped (reset-to-default), like embedder/context
	}
	ownedVal := func(key string) (string, bool) {
		for _, kv := range owned {
			if kv.key == key {
				return kv.val, true
			}
		}
		return "", false
	}

	var existing []string
	if b, err := os.ReadFile(path); err == nil {
		existing = strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		if len(existing) == 1 && existing[0] == "" {
			existing = nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	written := map[string]bool{}
	var out []string
	for _, line := range existing {
		trimmed := strings.TrimSpace(line)
		key := ""
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			if parts := strings.SplitN(trimmed, "=", 2); len(parts) == 2 {
				key = strings.TrimSpace(parts[0])
			}
		}
		val, owns := ownedVal(key)
		if !owns {
			out = append(out, line) // not ours: preserve verbatim
			continue
		}
		if written[key] {
			continue // collapse duplicate owned keys onto the first occurrence
		}
		written[key] = true
		if val == "" {
			if key == "embedder" || key == "context" || key == "mmr" {
				continue // reset-to-default: drop the line
			}
			out = append(out, line) // empty dir value: preserve, never silently repoint
			continue
		}
		out = append(out, fmt.Sprintf("%s = %q", key, val))
	}
	for _, kv := range owned {
		if kv.val == "" || written[kv.key] {
			continue
		}
		out = append(out, fmt.Sprintf("%s = %q", kv.key, kv.val))
	}
	return atomicWrite(path, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}
func cmdInit(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	vault := fs.String("vault", "", "vault directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Preserve an existing install's config (vault_dir/data_dir/state_dir) so a
	// re-run of `init` never repoints Mora away from a custom vault and orphans
	// it. loadConfig returns defaults when no config.toml exists (first-time
	// init), so brand-new setups still scaffold at the default location.
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	repointed := false
	if *vault != "" {
		want := expandHome(*vault)
		// Repointing an EXISTING install's vault orphans the current one from
		// Mora's view — it must never happen as a side effect of a scripted
		// init (two live incidents). Same-dir re-init stays idempotent, and
		// the comparison cleans both sides so a trailing slash (shell tab
		// completion, install.sh's MORA_VAULT) is not misread as a repoint.
		if filepath.Clean(cfg.VaultDir) != filepath.Clean(want) && configFileExists(cfg) {
			if err := confirmVaultRepointFn(stdin, stdout, cfg.VaultDir, want); err != nil {
				return err
			}
			repointed = true
		}
		cfg.VaultDir = want
	}
	for _, dir := range []string{cfg.VaultDir, cfg.ConfigDir, cfg.DataDir, cfg.StateDir, memoriesRoot(cfg), sourcesRoot(cfg), filepath.Join(cfg.ConfigDir, "tokens")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if err := writeConfig(cfg); err != nil {
		return err
	}
	if _, err := createVaultMarkerIfAbsent(cfg, "v_"+newID()); err != nil {
		return err
	}
	if err := scaffoldControlFiles(cfg); err != nil {
		return err
	}
	// On a CONFIRMED repoint, config + marker now point at the NEW vault but
	// data_dir still holds the OLD vault's index (oldCount>0, bound to the old
	// id). A hard-wired Enforce rebuild would self-block (empty NEW → decBlockEmpty;
	// populated NEW → decBlockIdentity) and leave config=NEW / index=OLD. Discard
	// the stale index so the rebuild is a clean first-build for the new vault
	// (oldCount=0 → decProceed → adopt the new marker's id).
	if repointed {
		for _, p := range []string{dbPath(cfg), dbPath(cfg) + "-wal", dbPath(cfg) + "-shm"} {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		return err
	}
	files, _ := allMemoryFiles(cfg)
	status := "empty — nothing indexed yet"
	if len(files) > 0 {
		status = fmt.Sprintf("%d memories indexed", len(files))
	}
	fmt.Fprintf(stdout, "\n✓ Mora initialized.\n")
	fmt.Fprintf(stdout, "  Vault:  %s   (your memories live here — back this up)\n", cfg.VaultDir)
	fmt.Fprintf(stdout, "  Status: %s\n", status)
	fmt.Fprintf(stdout, "  Next:   mora connectors setup     # connect Gmail / iMessage / files\n")
	fmt.Fprintf(stdout, "  or:     mora write --title \"...\" --text \"...\"\n")
	fmt.Fprintf(stdout, "  Layout: mora config\n\n")
	// D-08: launch the interactive connector setup menu on a real TTY; on a
	// non-TTY (scripts, CI, tests) runSetupMenu prints a hint and returns.
	return runSetupMenu(ctx, cfg, stdin, stdout)
}

// configFileExists reports whether a config.toml is already on disk —
// loadConfig alone can't distinguish "defaults because no file" from a real
// install, and the repoint guard must only fire for the latter.
func configFileExists(cfg Config) bool {
	_, err := os.Stat(filepath.Join(cfg.ConfigDir, "config.toml"))
	return err == nil
}

// confirmVaultRepoint gates `init --vault <new>` when config.toml already
// points elsewhere. Non-interactive callers are refused with the exact manual
// alternative (a script must never silently repoint a live install); a TTY
// gets an explicit default-NO confirm, mirroring runSetupMenu's gate.
func confirmVaultRepoint(stdin io.Reader, stdout io.Writer, from, to string) error {
	f, ok := stdin.(*os.File)
	if !ok || !isatty.IsTerminal(f.Fd()) {
		return fmt.Errorf("refusing to repoint the vault non-interactively: config.toml already points at %s (requested: %s) — re-run `mora init --vault` in a terminal to confirm, or edit config.toml yourself", from, to)
	}
	var yes bool
	confirm := huh.NewConfirm().
		Title(fmt.Sprintf("Repoint vault from %s to %s?", from, to)).
		Description("The current vault stays on disk, but Mora stops reading it.").
		Affirmative("Repoint").
		Negative("Keep current vault").
		Value(&yes)
	if err := confirm.Run(); err != nil {
		return err
	}
	if !yes {
		fmt.Fprintln(stdout, "init cancelled — vault unchanged.")
		return errors.New("init cancelled — vault unchanged")
	}
	return nil
}
func scaffoldControlFiles(cfg Config) error {
	files := map[string]string{
		"index.md":           "# Mora Index\n\n> Generated by `mora index rebuild`.\n",
		"priority-map.md":    defaultPriorityMap(),
		"live-tasks.md":      defaultLiveTasks(),
		"heartbeat.md":       defaultHeartbeat(),
		"auto-resolver.md":   defaultAutoResolver(),
		"log.md":             "# Mora Log\n\n",
		"meetings/ledger.md": "# Meeting Ledger\n\n> Append-only decisions and action items.\n\n",
	}
	for rel, body := range files {
		path := filepath.Join(cfg.VaultDir, rel)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := atomicWrite(path, []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}
func defaultPriorityMap() string {
	return `# Priority Map

> P0 = this week, P1 = this month, P2 = backlog.

## P0 — Active This Week

1. **Set up Mora** — connect a source and run your first recall.
   - Outcome: daily use across CLI/MCP memory recall.

## P1 — This Month

- **Connector hardening** — filesystem + Google read-only ingestion.

## P2 — Backlog

- **Embeddings** — defer until FTS5 is proven insufficient.
`
}
func defaultLiveTasks() string {
	return `# Live Tasks

| Task | Domain | Owner | Pri | Status | Blocker | Horizon | Last touched |
|------|--------|-------|-----|--------|---------|---------|--------------|
`
}
func defaultHeartbeat() string {
	return `# HEARTBEAT

Read in order: index.md, priority-map.md, live-tasks.md, auto-resolver.md, meetings/ledger.md, log.md.
Run ` + "`mora pulse --write --digest`" + ` to reconcile tasks and stale work.
`
}
func defaultAutoResolver() string {
	return `# Auto Resolver

- P0 without live task: create a live task.
- Owner-flagged blocker: surface in digest, do not auto-action.
- Routine successful cron: log only.
- Failed cron twice: report exact blocker.
`
}
