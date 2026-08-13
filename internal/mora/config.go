package mora

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	configstore "github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/genericutil"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/mattn/go-isatty"
)

// fusion returns the active RRF fusion params: the per-Config override when set,
// else the production default. Single source of truth for search + eval.
func configFusion(c Config) fusionParams {
	if override, ok := c.FusionOverride().(*fusionParams); ok && override != nil {
		return *override
	}
	return defaultFusion
}

// mmr returns the active MMR params, or nil when MMR is off. The eval/test override
// (mmrOv) wins; else the durable user opt-in (MMR) yields default params; else nil.
// Single source of truth, mirroring fusion(). The returned params carry force only
// when set via mmrOv — the MMR bool path always has force=false.
func configMMR(c Config) *mmrParams {
	if override, ok := c.MMROverride().(*mmrParams); ok && override != nil {
		return override
	}
	if c.MMR {
		return &mmrParams{lambda: defaultLambda}
	}
	return nil
}
func defaultConfig() Config           { return configstore.Default() }
func persistVaultDir(c Config) string { return c.PersistVaultDir() }
func loadConfig() (Config, error)     { return configstore.Load() }
func writeConfig(cfg Config) error    { return configstore.Write(cfg) }

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
		// D3: report the RESOLVED embedder (probing Ollama), not the raw cfg.Embedder
		// — an opted-in daemon that is down must read "UNREACHABLE — index built with
		// static-hash-v1", never a confident "ollama".
		embedder := resolvedEmbedderLine(cfg)
		mmr := "off"
		if cfg.MMR {
			mmr = "on"
		}
		fmt.Fprintf(stdout, "vault_dir = %s   ← your memories (back this up)\n", cfg.VaultDir)
		fmt.Fprintf(stdout, "data_dir  = %s   ← search index (rebuildable)\n", cfg.DataDir)
		fmt.Fprintf(stdout, "state_dir = %s   ← sync watermarks (rebuildable)\n", cfg.StateDir)
		fmt.Fprintf(stdout, "config    = %s   ← settings + tokens\n", cfg.ConfigDir)
		update := resolveUpdatePolicy(cfg)
		fmt.Fprintf(stdout, "embedder  = %s\ncontext   = %s  (default budget %d tokens, digest snippets %d chars; ceiling %d)\nmmr       = %s\nmcp_write_policy = %s\nupdate_policy = %s (%s)\n",
			embedder, profile,
			contextDefaultTokens(cfg), digestSnippetChars(cfg), contextMaxTokens(cfg), mmr, configMCPWritePolicy(cfg), update.Policy, update.Reason)
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
		return errors.New("usage: mora config [context <small|default|large> | embedder <ollama|static> | mmr <on|off> | mcp-write-policy <open|propose|readonly>]")
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
	case "mcp-write-policy":
		policy, err := parseMCPWritePolicy(val)
		if err != nil {
			return err
		}
		cfg.MCPWritePolicy = policy
		key = "mcp-write-policy"
	default:
		return fmt.Errorf("unknown config key %q (want context, embedder, mmr, or mcp-write-policy)", key)
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
			contextDefaultTokens(cfg), digestSnippetChars(cfg), contextMaxTokens(cfg))
	}
	return nil
}

// writeConfig persists the keys this binary owns by READ-MODIFY-WRITE:
// every line it does not own (comments, blank lines, keys written by hand or
// by a newer mora) is preserved byte-for-byte. The old regenerate-from-struct
// behavior silently ate those lines on every rewrite — loadConfig skips
// unknowns, so they survived the load only to vanish on the next save. An
// empty Embedder/ContextProfile DROPS its line (reset-to-default semantics,
// keeping config.toml minimal); an empty DIR value is broken either way but
// is preserved verbatim — dropping it would silently repoint the install to
// the defaults via an unrelated rewrite.
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
		want := genericutil.ExpandHome(*vault)
		// Repointing an EXISTING install's vault orphans the current one from
		// Mora's view — it must never happen as a side effect of a scripted
		// init (two live incidents). Same-dir re-init stays idempotent, and
		// the comparison cleans both sides so a trailing slash (shell tab
		// completion, install.sh's MORA_VAULT) is not misread as a repoint.
		// Compare against the PERSISTED vault (persistVaultDir), not the
		// MORA_VAULT-effective one: the durable config.toml location is what a
		// repoint orphans, and an exported MORA_VAULT equal to --vault must not
		// let the confirmation gate be skipped.
		if filepath.Clean(persistVaultDir(cfg)) != filepath.Clean(want) && configFileExists(cfg) {
			if err := confirmVaultRepointFn(stdin, stdout, persistVaultDir(cfg), want); err != nil {
				return err
			}
			repointed = true
		}
		cfg.VaultDir = want
		// An explicit --vault is the one sanctioned repoint path: drop the env
		// stash so writeConfig persists the flag value even under MORA_VAULT.
		cfg.ClearVaultOverride()
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
		if err := atomicio.Write(path, []byte(body), 0o644); err != nil {
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
