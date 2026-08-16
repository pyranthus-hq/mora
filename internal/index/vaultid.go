package index

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

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/config"
)

const VaultMarkerSchema = 1

// VaultMarker is the write-once identity record stored with the vault.
type VaultMarker struct {
	Schema    int    `json:"schema"`
	VaultID   string `json:"vault_id"`
	CreatedAt string `json:"created_at"`
	CreatedBy string `json:"created_by"`
}

// MarkerPath returns the vault-resident identity marker path.
func MarkerPath(cfg config.Config) string { return filepath.Join(cfg.VaultDir, ".mora-vault.json") }

// ReadVaultMarker loads the identity marker and fails loudly on corruption.
func ReadVaultMarker(cfg config.Config) (VaultMarker, bool, error) {
	b, err := os.ReadFile(MarkerPath(cfg))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return VaultMarker{}, false, nil
		}
		return VaultMarker{}, false, err
	}
	var m VaultMarker
	if err := json.Unmarshal(b, &m); err != nil {
		// The marker is identity-critical: silently treating a corrupt one as
		// "absent" would disable the rebuild guard exactly when the vault's
		// identity is in question. Fail LOUD with an actionable message so the
		// rebuild surfaces it instead of clobbering the index. Do NOT advise
		// deleting it — a regenerated marker gets a fresh id that no longer
		// matches the index, blocking every future rebuild; restoring the
		// backed-up marker preserves the vault's real identity.
		return VaultMarker{}, true, fmt.Errorf("vault identity marker %s is unreadable (corrupt JSON) — restore it from your vault backup (e.g. `mora sync git`) rather than deleting it: %w", MarkerPath(cfg), err)
	}
	return m, true, nil
}

// CreateVaultMarkerIfAbsent writes the marker exactly once. If one already
// exists, it is left untouched and its id is returned — the marker must never
// become the next thing that gets clobbered. The write is atomic: the JSON is
// staged to a temp file in the vault dir, fsync'd, then rename(2)'d into place,
// so a crash mid-write can never leave a TORN marker (a half-written final file
// would silently disable the identity guard). The existence pre-check preserves
// write-once; the temp+fsync+rename gives atomicity.
// CreateVaultMarkerIfAbsent atomically creates the marker without replacing a bound identity.
func CreateVaultMarkerIfAbsent(cfg config.Config, id, createdAt, createdBy string) (string, error) {
	if m, present, err := ReadVaultMarker(cfg); err != nil {
		return "", err
	} else if present && m.VaultID != "" {
		return m.VaultID, nil
	}
	if err := os.MkdirAll(cfg.VaultDir, 0o700); err != nil {
		return "", err
	}
	m := VaultMarker{Schema: VaultMarkerSchema, VaultID: id, CreatedAt: createdAt, CreatedBy: createdBy}
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
	if err := os.Rename(tmp, MarkerPath(cfg)); err != nil {
		return "", err
	}
	return id, nil
}

// RebuildDecision is the pure identity-safety verdict for a staged rebuild.
type RebuildDecision int

const (
	Proceed       RebuildDecision = iota // commit the new index
	Adopt                                // commit + bind a (new or marker) vault id
	BlockEmpty                           // populated index would become empty
	BlockIdentity                        // new index is from a different/unknown vault
)

// AssessRebuild decides what to do with a freshly built index given the prior
// state. Pure: no I/O. indexID=="" means the existing index never recorded a
// vault id (a genuinely pre-feature index).
// AssessRebuild blocks empty or foreign replacement of a populated index.
func AssessRebuild(oldCount, newCount int, markerID string, markerPresent bool, indexID string) RebuildDecision {
	if oldCount == 0 {
		return Proceed // first build / already-empty index: nothing to protect
	}
	if newCount == 0 {
		return BlockEmpty // populated index would be wiped by an empty vault
	}
	if indexID == "" {
		return Adopt // legacy index with no bound id -> adopt the vault's identity
	}
	if markerPresent && markerID == indexID {
		return Proceed // same vault, ordinary rebuild after edits
	}
	return BlockIdentity // marker missing or from a different vault
}

// RebuildBlock is the advisory diagnostic for the last refused rebuild.
type RebuildBlock struct {
	At       string `json:"at"`
	Reason   string `json:"reason"`
	VaultDir string `json:"vault_dir"`
	OldCount int    `json:"old_count"`
	NewCount int    `json:"new_count"`
}

// BlockRecordPath returns the state-directory diagnostic path.
func BlockRecordPath(cfg config.Config) string {
	return filepath.Join(cfg.DataDir, "last-rebuild-block.json")
}

// WriteBlockRecord atomically persists an advisory rebuild diagnostic.
func WriteBlockRecord(cfg config.Config, d RebuildDecision, vaultDir string, oldCount, newCount int, at string) error {
	reason := "vault looked empty"
	if d == BlockIdentity {
		reason = "vault identity did not match the index"
	}
	rec := RebuildBlock{At: at, Reason: reason, VaultDir: vaultDir, OldCount: oldCount, NewCount: newCount}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.Write(BlockRecordPath(cfg), b, 0o644)
}

// ClearBlockRecord removes an advisory diagnostic and tolerates absence.
func ClearBlockRecord(cfg config.Config) error {
	err := os.Remove(BlockRecordPath(cfg))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// ReadBlockRecord loads the advisory diagnostic and ignores corrupt advisory bytes.
func ReadBlockRecord(cfg config.Config) (RebuildBlock, bool, error) {
	b, err := os.ReadFile(BlockRecordPath(cfg))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return RebuildBlock{}, false, nil
		}
		return RebuildBlock{}, false, err
	}
	var rec RebuildBlock
	if err := json.Unmarshal(b, &rec); err != nil {
		// The block record is an ADVISORY diagnostic, not identity-critical.
		// Degrade quietly on a garbage advisory (treat as absent) so a corrupt
		// last-rebuild-block.json never fails `mora doctor`.
		return RebuildBlock{}, false, nil
	}
	return rec, true, nil
}

// RebuildBlockMessage returns a human-readable explanation of why a rebuild was blocked.
// RebuildBlockMessage returns the exact actionable refusal text.
func RebuildBlockMessage(d RebuildDecision, vaultDir string, oldCount int) string {
	if d == BlockEmpty {
		return fmt.Sprintf("configured vault (%s) has no memory files, but the index holds %d — your vault may have moved. The existing index was left untouched. Fix vault_dir in config.toml then `mora index rebuild`; only use `mora index rebuild --force` if the empty vault is correct (it discards the %d indexed memories).", vaultDir, oldCount, oldCount)
	}
	return fmt.Sprintf("configured vault (%s) is a different vault than the index was built from. The existing index was left untouched. Re-point vault_dir to the original vault and `mora index rebuild`; only `mora index rebuild --force` if this new vault is correct (it discards the existing index).", vaultDir)
}

// ReadVaultID opens the index read-only and returns the vault_id stored in
// index_meta, or "" if the table or row is absent (pre-feature index).
// ReadVaultID reads the bound identity from a read-only index, tolerating legacy absence.
func ReadVaultID(ctx context.Context, cfg config.Config) (string, error) {
	db, err := sql.Open("sqlite", ReadOnlyDSN(cfg))
	if err != nil {
		return "", err
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
		return "", err
	}
	return v, nil
}
