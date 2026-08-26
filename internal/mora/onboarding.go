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
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	doctorpkg "github.com/pyranthus-hq/mora/internal/doctor"
	"github.com/pyranthus-hq/mora/internal/genericutil"
)

// The first onboarding slice deliberately reconciles only the local foundation.
// The receipt records completed work, but every invocation re-observes the
// underlying state before it trusts a completed step.
const setupReceiptSchemaVersion = 1

type setupStep struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Evidence string `json:"evidence"`
	Next     string `json:"next,omitempty"`
}

type setupStatus struct {
	Complete        bool        `json:"complete"`
	Steps           []setupStep `json:"steps"`
	RemainingChecks []string    `json:"remaining_checks"`
	ReceiptPresent  bool        `json:"receipt_present"`
}

type setupProgressReceipt struct {
	SchemaVersion int         `json:"schema_version"`
	RecordedAt    string      `json:"recorded_at"`
	Steps         []setupStep `json:"steps"`
}

var (
	setupRebuildIndex  = rebuildIndex
	setupIsInteractive = genericutil.IsInteractive
)

func setupReceiptPath(cfg Config) string {
	return filepath.Join(cfg.StateDir, "setup", "foundation-receipt.json")
}

func readSetupReceipt(cfg Config) (setupProgressReceipt, bool, error) {
	body, err := os.ReadFile(setupReceiptPath(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return setupProgressReceipt{}, false, nil
	}
	if err != nil {
		return setupProgressReceipt{}, false, err
	}
	var receipt setupProgressReceipt
	if err := json.Unmarshal(body, &receipt); err != nil {
		return setupProgressReceipt{}, false, fmt.Errorf("read setup receipt: %w", err)
	}
	if receipt.SchemaVersion != setupReceiptSchemaVersion {
		return setupProgressReceipt{}, false, fmt.Errorf("read setup receipt: unsupported schema version %d", receipt.SchemaVersion)
	}
	return receipt, true, nil
}

func writeSetupReceipt(cfg Config, steps []setupStep) error {
	body, err := json.MarshalIndent(setupProgressReceipt{
		SchemaVersion: setupReceiptSchemaVersion,
		RecordedAt:    time.Now().UTC().Format(time.RFC3339),
		Steps:         steps,
	}, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.WriteDurable(setupReceiptPath(cfg), body, 0o600)
}

// indexIdentityMatchesVault verifies the vault marker's identity is bound to the
// committed index. Index freshness + the content manifest alone are a lie: a
// vault that LOST .mora-vault.json and had local_layout recreate a fresh random
// marker carries a different vault_id than the one the index was built from.
// This reuses the exact identity machinery the rebuild guard trusts
// (readVaultMarker + readIndexVaultID), so setup and rebuild cannot disagree
// about which vault a healthy index belongs to.
func indexIdentityMatchesVault(cfg Config) (ok bool, critical bool) {
	marker, markerPresent, err := readVaultMarker(cfg)
	if err != nil {
		return false, true
	}
	indexID, err := readIndexVaultID(context.Background(), cfg)
	if err != nil {
		return false, true
	}
	if !markerPresent || marker.VaultID == "" {
		return false, true
	}
	if indexID == "" {
		// An unbound index proves nothing about identity: a legacy or foreign
		// index must stay pending until a rebuild establishes the binding.
		return false, false
	}
	if marker.VaultID == indexID {
		return true, false
	}
	return false, true
}

func setupFoundationStatus(cfg Config) []setupStep {
	layoutOK := configFileExists(cfg)
	if _, present, err := readVaultMarker(cfg); err != nil || !present {
		layoutOK = false
	}
	for _, dir := range []string{cfg.VaultDir, cfg.ConfigDir, cfg.DataDir, cfg.StateDir, memoriesRoot(cfg), sourcesRoot(cfg)} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			layoutOK = false
			break
		}
	}
	layout := setupStep{ID: "local_layout", State: "pending", Evidence: "config.toml, vault marker, and local directories are missing or unreadable", Next: "mora setup"}
	if layoutOK {
		layout = setupStep{ID: "local_layout", State: "verified", Evidence: "config.toml, vault marker, and local directories are present"}
	}

	indexOK := indexHealthOf(cfg, doctorClock()).State == idxFresh
	if manifestOK, manifestCritical := indexMatchesVault(cfg); manifestCritical && !manifestOK {
		indexOK = false
	}
	// Identity check: the marker's vault_id must match the index's bound vault_id.
	// A fresh random marker created by layout reconciliation after a lost marker
	// must NOT make the committed_index appear verified.
	// An unbound index (no vault_id) is non-critical but still unproven, so any
	// !identityOK outcome keeps the step pending.
	if identityOK, _ := indexIdentityMatchesVault(cfg); !identityOK {
		indexOK = false
	}
	index := setupStep{ID: "committed_index", State: "pending", Evidence: "the committed index is missing, dirty, degraded, or unreadable", Next: "mora setup"}
	if indexOK {
		index = setupStep{ID: "committed_index", State: "verified", Evidence: "doctor index freshness, manifest, and vault identity checks pass"}
	}

	tokenDir := filepath.Join(cfg.ConfigDir, "tokens")
	info, err := os.Stat(tokenDir)
	tokensOK := err == nil && info.IsDir() && doctorpkg.PathsDisjoint(cfg.VaultDir, tokenDir)
	tokens := setupStep{ID: "credential_storage", State: "pending", Evidence: "the token directory is missing or overlaps the vault", Next: "mora setup"}
	if tokensOK {
		tokens = setupStep{ID: "credential_storage", State: "verified", Evidence: "token directory exists and is disjoint from the vault"}
	}
	return []setupStep{layout, index, tokens}
}

func allSetupStepsVerified(steps []setupStep) bool {
	for _, step := range steps {
		if step.State != "verified" {
			return false
		}
	}
	return true
}

func setupRemainingChecks() []string {
	return []string{
		"installed signed-app identity",
		"protected-source readability and connector enablement",
		"initial connector ingest",
		"MCP registration and protocol smoke test",
		"scheduled refresh and update jobs",
		"update policy and latest update check",
		"bounded retrieval or truthful empty-index result",
	}
}

func cmdSetup(ctx context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader) error {
	if len(args) > 0 && args[0] == "status" {
		fs := flag.NewFlagSet("setup status", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		jsonOut := fs.Bool("json", false, "emit machine-readable setup status")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || !*jsonOut {
			return errors.New("usage: mora setup status --json")
		}
		cfg, err := loadConfigFor(ctx)
		if err != nil {
			return err
		}
		_, receiptPresent, err := readSetupReceipt(cfg)
		if err != nil {
			return err
		}
		return emitReceipt(stdout, "mora.setup.status", 1, setupStatus{
			Complete:        false, // Later #293 checks are intentionally not inferred.
			Steps:           setupFoundationStatus(cfg),
			RemainingChecks: setupRemainingChecks(),
			ReceiptPresent:  receiptPresent,
		})
	}

	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	plan := fs.Bool("plan", false, "show the read-only foundation reconciliation plan")
	applyLayout := fs.Bool("local-layout", false, "approve local layout reconciliation in a non-interactive run")
	applyIndex := fs.Bool("committed-index", false, "approve committed index reconciliation in a non-interactive run")
	applyTokens := fs.Bool("credential-storage", false, "approve credential-storage reconciliation in a non-interactive run")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: mora setup [--plan] [--local-layout] [--committed-index] [--credential-storage] | mora setup status --json")
	}
	if *plan && (*applyLayout || *applyIndex || *applyTokens) {
		return errors.New("--plan cannot be combined with setup mutation flags")
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	_, receiptPresent, err := readSetupReceipt(cfg)
	if err != nil {
		return err
	}
	steps := setupFoundationStatus(cfg)
	interactive := setupIsInteractive(stdin)
	if *plan || (!interactive && !*applyLayout && !*applyIndex && !*applyTokens) {
		fmt.Fprintln(stdout, "Mora setup foundation plan (read-only):")
		for _, step := range steps {
			fmt.Fprintf(stdout, "  %s: %s — %s\n", step.ID, step.State, step.Evidence)
		}
		if !*plan {
			fmt.Fprintln(stdout, "Non-interactive runs are read-only unless each needed step is explicitly approved with --local-layout, --committed-index, or --credential-storage.")
		}
		fmt.Fprintln(stdout, "This first slice does not yet reconcile connector, MCP, schedule, update, or retrieval checks.")
		return nil
	}
	allowLayout := interactive || *applyLayout
	allowIndex := interactive || *applyIndex
	allowTokens := interactive || *applyTokens

	// Data-safety gate: before ANY mutation, fail closed if any operational root
	// overlaps the vault (either direction, symlink-resolved). The receipt path is
	// checked too — a receipt written inside the vault would be a lossy ghost on vault moves.
	for _, root := range []string{cfg.StateDir, cfg.DataDir, cfg.ConfigDir, setupReceiptPath(cfg)} {
		if !doctorpkg.PathsDisjoint(cfg.VaultDir, root) || !doctorpkg.PathsDisjoint(root, cfg.VaultDir) {
			return fmt.Errorf("mora setup: %s overlaps the vault directory (%s); move it outside the vault before rerunning", root, cfg.VaultDir)
		}
	}

	// Re-observe before every action. A receipt is evidence of prior work, never
	// authority to skip a check whose underlying state changed.
	if steps[0].State != "verified" {
		if !allowLayout {
			return errors.New("setup local layout is pending; rerun with --local-layout")
		}
		if err := ensureMoraLayout(cfg, false); err != nil {
			return fmt.Errorf("setup local layout: %w", err)
		}
		steps = setupFoundationStatus(cfg)
		if steps[0].State != "verified" {
			return errors.New("setup local layout did not verify after reconciliation")
		}
		if err := writeSetupReceipt(cfg, steps); err != nil {
			return fmt.Errorf("record setup progress: %w", err)
		}
		receiptPresent = true
	}
	if steps[1].State != "verified" {
		if !allowIndex {
			fmt.Fprintln(stdout, "Committed index remains pending; rerun with --committed-index.")
			return nil
		}
		if _, err := setupRebuildIndex(ctx, cfg); err != nil {
			return fmt.Errorf("setup committed index: %w", err)
		}
		steps = setupFoundationStatus(cfg)
		if steps[1].State != "verified" {
			return errors.New("setup committed index did not verify after reconciliation")
		}
		if err := writeSetupReceipt(cfg, steps); err != nil {
			return fmt.Errorf("record setup progress: %w", err)
		}
		receiptPresent = true
	}
	if steps[2].State != "verified" {
		if !allowTokens {
			fmt.Fprintln(stdout, "Credential storage remains pending; rerun with --credential-storage.")
			return nil
		}
		tokenDir := filepath.Join(cfg.ConfigDir, "tokens")
		if !doctorpkg.PathsDisjoint(cfg.VaultDir, tokenDir) {
			return fmt.Errorf("setup credential storage: %s overlaps the vault; move tokens outside the vault before rerunning", tokenDir)
		}
		if err := os.MkdirAll(tokenDir, 0o700); err != nil {
			return fmt.Errorf("setup credential storage: %w", err)
		}
		steps = setupFoundationStatus(cfg)
		if steps[2].State != "verified" {
			return errors.New("setup credential storage did not verify after reconciliation")
		}
		if err := writeSetupReceipt(cfg, steps); err != nil {
			return fmt.Errorf("record setup progress: %w", err)
		}
		receiptPresent = true
	}

	if !allSetupStepsVerified(steps) {
		return errors.New("setup foundation remains incomplete")
	}
	if !receiptPresent {
		if err := writeSetupReceipt(cfg, steps); err != nil {
			return fmt.Errorf("record setup progress: %w", err)
		}
		receiptPresent = true
	}
	fmt.Fprintln(stdout, "Foundation setup verified.")
	fmt.Fprintln(stdout, "Full verified onboarding is not complete yet: connector, MCP, schedule, update, and retrieval checks remain deliberately unimplemented in this first slice.")
	return nil
}
