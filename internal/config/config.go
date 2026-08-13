// Package config owns Mora's process configuration contract.
package config

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
