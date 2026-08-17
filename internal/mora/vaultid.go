package mora

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
		// The marker is identity-critical: silently treating a corrupt one as
		// "absent" would disable the rebuild guard exactly when the vault's
		// identity is in question. Fail LOUD with an actionable message so the
		// rebuild surfaces it instead of clobbering the index. Do NOT advise
		// deleting it — a regenerated marker gets a fresh id that no longer
		// matches the index, blocking every future rebuild; restoring the
		// backed-up marker preserves the vault's real identity.
		return vaultMarker{}, true, fmt.Errorf("vault identity marker %s is unreadable (corrupt JSON) — restore it from your vault backup (e.g. `mora sync git`) rather than deleting it: %w", markerPath(cfg), err)
	}
	return m, true, nil
}

// createVaultMarkerIfAbsent writes the marker exactly once. If one already
// exists, it is left untouched and its id is returned — the marker must never
// become the next thing that gets clobbered. The write is atomic: the JSON is
// staged to a temp file in the vault dir, fsync'd, then rename(2)'d into place,
// so a crash mid-write can never leave a TORN marker (a half-written final file
// would silently disable the identity guard). The existence pre-check preserves
// write-once; the temp+fsync+rename gives atomicity.
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
	f, err := os.CreateTemp(cfg.VaultDir, ".mora-vault-*.json.tmp")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // clean up on any error path (no-op once renamed away)
	if _, err := f.Write(body); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, markerPath(cfg)); err != nil {
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

type rebuildBlock struct {
	At       string `json:"at"`
	Reason   string `json:"reason"`
	VaultDir string `json:"vault_dir"`
	OldCount int    `json:"old_count"`
	NewCount int    `json:"new_count"`
}

func blockRecordPath(cfg Config) string {
	return filepath.Join(cfg.DataDir, "last-rebuild-block.json")
}

func writeBlockRecord(cfg Config, d rebuildDecision, vaultDir string, oldCount, newCount int) error {
	reason := "vault looked empty"
	if d == decBlockIdentity {
		reason = "vault identity did not match the index"
	}
	rec := rebuildBlock{At: nowRFC3339(), Reason: reason, VaultDir: vaultDir, OldCount: oldCount, NewCount: newCount}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(blockRecordPath(cfg), b, 0o644)
}

func clearBlockRecord(cfg Config) error {
	err := os.Remove(blockRecordPath(cfg))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

func readBlockRecord(cfg Config) (rebuildBlock, bool, error) {
	b, err := os.ReadFile(blockRecordPath(cfg))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return rebuildBlock{}, false, nil
		}
		return rebuildBlock{}, false, err
	}
	var rec rebuildBlock
	if err := json.Unmarshal(b, &rec); err != nil {
		// The block record is an ADVISORY diagnostic, not identity-critical.
		// Degrade quietly on a garbage advisory (treat as absent) so a corrupt
		// last-rebuild-block.json never fails `mora doctor`.
		return rebuildBlock{}, false, nil
	}
	return rec, true, nil
}

// rebuildBlockMessage returns a human-readable explanation of why a rebuild was blocked.
func rebuildBlockMessage(d rebuildDecision, vaultDir string, oldCount int) string {
	if d == decBlockEmpty {
		return fmt.Sprintf("configured vault (%s) has no memory files, but the index holds %d — your vault may have moved. The existing index was left untouched. Fix vault_dir in config.toml then `mora index rebuild`; only use `mora index rebuild --force` if the empty vault is correct (it discards the %d indexed memories).", vaultDir, oldCount, oldCount)
	}
	return fmt.Sprintf("configured vault (%s) is a different vault than the index was built from. The existing index was left untouched. Re-point vault_dir to the original vault and `mora index rebuild`; only `mora index rebuild --force` if this new vault is correct (it discards the existing index).", vaultDir)
}

// readIndexVaultID opens the index read-only and returns the vault_id stored in
// index_meta, or "" if the table or row is absent (pre-feature index).
func readIndexVaultID(ctx context.Context, cfg Config) (string, error) {
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		return "", newCodedError(sqliteErrorCode(err), err, "%v", err)
	}
	defer db.Close()
	var v string
	err = db.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key='vault_id'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		// Missing table (older index) reads as "no id".
		if strings.Contains(err.Error(), "no such table") {
			return "", nil
		}
		// No index file at all (never built, or data_dir wiped) is also "no id" —
		// a missing db must not propagate as a hard error to identity callers.
		if errors.Is(err, fs.ErrNotExist) || strings.Contains(err.Error(), "unable to open database file") {
			return "", nil
		}
		// Anything left is a real read failure. The two checks above stay exactly
		// as they are — they decide which sqlite errors mean "no id" — while this
		// residual path gains the published code (sqliteErrorCode, index.go).
		return "", newCodedError(sqliteErrorCode(err), err, "%v", err)
	}
	return v, nil
}
