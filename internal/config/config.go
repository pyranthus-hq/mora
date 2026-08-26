// Package config owns Mora's process configuration contract.
package config

import (
	"context"
	"os"
	"time"
)

// Config is the resolved durable configuration plus process-local orchestration overrides.
type Config struct {
	VaultDir       string
	ConfigDir      string
	DataDir        string
	StateDir       string
	MCPWritePolicy string
	UpdatePolicy   string
	Embedder       string
	ContextProfile string
	SelfEmails     []string
	MMR            bool

	operationRunID     string
	fusionOverride     any
	mmrOverride        any
	vaultDirConfigured *string
	operationClock     func() time.Time
	authoredReconciler func(context.Context, Config) error
	embedderPref       *string
	homeDirOverride    *string
}

// HomeDir resolves the user home every home-derived side lookup (LaunchAgents,
// app discovery) must use: the injected sandbox home when one was carried in
// (test isolation), else the real process home. Fail-safe: with neither, "".
func (c Config) HomeDir() string {
	if c.homeDirOverride != nil {
		return *c.homeDirOverride
	}
	home, _ := os.UserHomeDir()
	return home
}

// SetHomeDir pins the home directory on this config (test seam).
func (c *Config) SetHomeDir(home string) { c.homeDirOverride = &home }

// EmbedderPref resolves the embedder preference with the production precedence:
// an explicitly pinned/env-set MORA_EMBEDDER wins (including "" → static), else
// the durable cfg.Embedder opt-in applies at the caller.
func (c Config) EmbedderPref() (string, bool) {
	if c.embedderPref != nil {
		return *c.embedderPref, true
	}
	return os.LookupEnv("MORA_EMBEDDER")
}

// SetEmbedderPref pins the embedder preference on this config (test seam).
func (c *Config) SetEmbedderPref(pref string) { c.embedderPref = &pref }

// OperationClock returns the pinned time source for operation-activity records,
// defaulting to time.Now when none was injected.
func (c Config) OperationClock() time.Time {
	if c.operationClock != nil {
		return c.operationClock()
	}
	return time.Now()
}

// SetOperationClock pins the time source on this config (test seam).
func (c *Config) SetOperationClock(now func() time.Time) { c.operationClock = now }

// AuthoredReconciler returns the configured reconciler launcher, or nil for the
// production default.
func (c Config) AuthoredReconciler() func(context.Context, Config) error {
	return c.authoredReconciler
}

// SetAuthoredReconciler overrides the reconciler launcher on this config.
func (c *Config) SetAuthoredReconciler(fn func(context.Context, Config) error) {
	c.authoredReconciler = fn
}

// OperationRunID returns the process-local ingest run identity.
func (c Config) OperationRunID() string { return c.operationRunID }

// SetOperationRunID binds an ingest to its activity receipt.
func (c *Config) SetOperationRunID(id string) { c.operationRunID = id }

// FusionOverride returns the process-local retrieval tuning override.
func (c Config) FusionOverride() any { return c.fusionOverride }

// SetFusionOverride sets the process-local retrieval tuning override.
func (c *Config) SetFusionOverride(value any) { c.fusionOverride = value }

// MMROverride returns the process-local MMR evaluation override.
func (c Config) MMROverride() any { return c.mmrOverride }

// SetMMROverride sets the process-local MMR evaluation override.
func (c *Config) SetMMROverride(value any) { c.mmrOverride = value }

// PersistVaultDir returns the durable vault directory, excluding a runtime override.
func (c Config) PersistVaultDir() string {
	if c.vaultDirConfigured != nil {
		return *c.vaultDirConfigured
	}
	return c.VaultDir
}

// ApplyVaultOverride replaces the runtime vault while retaining its durable value.
func (c *Config) ApplyVaultOverride(path string) {
	persisted := c.VaultDir
	c.vaultDirConfigured = &persisted
	c.VaultDir = path
}

// ClearVaultOverride makes the current runtime vault the durable value.
func (c *Config) ClearVaultOverride() { c.vaultDirConfigured = nil }
