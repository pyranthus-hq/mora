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
