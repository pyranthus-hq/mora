package mora

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const vaultMarkerSchema = 1

type vaultMarker struct {
	Schema    int    `json:"schema"`
	VaultID   string `json:"vault_id"`
	CreatedAt string `json:"created_at"`
	CreatedBy string `json:"created_by"`
}

func markerPath(cfg Config) string { return filepath.Join(cfg.VaultDir, ".mora-vault.json") }

func readVaultMarker(cfg Config) (vaultMarker, bool, error) {
	b, err := os.ReadFile(markerPath(cfg))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return vaultMarker{}, false, nil
		}
		return vaultMarker{}, false, err
	}
	var m vaultMarker
	if err := json.Unmarshal(b, &m); err != nil {
		// A corrupt marker must not crash a rebuild; treat as "present but unusable".
		return vaultMarker{}, true, nil
	}
	return m, true, nil
}

// createVaultMarkerIfAbsent writes the marker exactly once (O_EXCL). If one
// already exists, it is left untouched and its id is returned — the marker must
// never become the next thing that gets clobbered.
func createVaultMarkerIfAbsent(cfg Config, id string) (string, error) {
	if m, present, err := readVaultMarker(cfg); err != nil {
		return "", err
	} else if present && m.VaultID != "" {
		return m.VaultID, nil
	}
	if err := os.MkdirAll(cfg.VaultDir, 0o700); err != nil {
		return "", err
	}
	m := vaultMarker{Schema: vaultMarkerSchema, VaultID: id, CreatedAt: nowRFC3339(), CreatedBy: "mora " + BuildVersion}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(markerPath(cfg), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			// Lost a race; read back the winner's id.
			if m2, present, rerr := readVaultMarker(cfg); rerr == nil && present {
				return m2.VaultID, nil
			}
		}
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(body); err != nil {
		return "", err
	}
	return id, nil
}

func nowRFC3339() string { return time.Now().Format(time.RFC3339) }

type rebuildDecision int

const (
	decProceed       rebuildDecision = iota // commit the new index
	decAdopt                                // commit + bind a (new or marker) vault id
	decBlockEmpty                           // populated index would become empty
	decBlockIdentity                        // new index is from a different/unknown vault
)

// assessRebuild decides what to do with a freshly built index given the prior
// state. Pure: no I/O. indexID=="" means the existing index never recorded a
// vault id (a genuinely pre-feature index).
func assessRebuild(oldCount, newCount int, markerID string, markerPresent bool, indexID string) rebuildDecision {
	if oldCount == 0 {
		return decProceed // first build / already-empty index: nothing to protect
	}
	if newCount == 0 {
		return decBlockEmpty // populated index would be wiped by an empty vault
	}
	if indexID == "" {
		return decAdopt // legacy index with no bound id -> adopt the vault's identity
	}
	if markerPresent && markerID == indexID {
		return decProceed // same vault, ordinary rebuild after edits
	}
	return decBlockIdentity // marker missing or from a different vault
}
