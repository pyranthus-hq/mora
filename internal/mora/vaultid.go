package mora

import (
	"context"
	"time"

	indexpkg "github.com/pyranthus-hq/mora/internal/index"
)

const vaultMarkerSchema = indexpkg.VaultMarkerSchema

type vaultMarker = indexpkg.VaultMarker
type rebuildDecision = indexpkg.RebuildDecision

const (
	decProceed       = indexpkg.Proceed
	decAdopt         = indexpkg.Adopt
	decBlockEmpty    = indexpkg.BlockEmpty
	decBlockIdentity = indexpkg.BlockIdentity
)

type rebuildBlock = indexpkg.RebuildBlock

func markerPath(cfg Config) string                          { return indexpkg.MarkerPath(cfg) }
func readVaultMarker(cfg Config) (vaultMarker, bool, error) { return indexpkg.ReadVaultMarker(cfg) }
func createVaultMarkerIfAbsent(cfg Config, id string) (string, error) {
	return indexpkg.CreateVaultMarkerIfAbsent(cfg, id, nowRFC3339(), "mora "+BuildVersion)
}
func nowRFC3339() string { return time.Now().Format(time.RFC3339) }
func assessRebuild(oldCount, newCount int, markerID string, markerPresent bool, indexID string) rebuildDecision {
	return indexpkg.AssessRebuild(oldCount, newCount, markerID, markerPresent, indexID)
}
func blockRecordPath(cfg Config) string { return indexpkg.BlockRecordPath(cfg) }
func writeBlockRecord(cfg Config, d rebuildDecision, vaultDir string, oldCount, newCount int) error {
	return indexpkg.WriteBlockRecord(cfg, d, vaultDir, oldCount, newCount, nowRFC3339())
}
func clearBlockRecord(cfg Config) error                      { return indexpkg.ClearBlockRecord(cfg) }
func readBlockRecord(cfg Config) (rebuildBlock, bool, error) { return indexpkg.ReadBlockRecord(cfg) }
func rebuildBlockMessage(d rebuildDecision, vaultDir string, oldCount int) string {
	return indexpkg.RebuildBlockMessage(d, vaultDir, oldCount)
}
func readIndexVaultID(ctx context.Context, cfg Config) (string, error) {
	v, err := indexpkg.ReadVaultID(ctx, cfg)
	if err != nil {
		// The "no id" outcomes (missing db, missing table, no row) come back as
		// err == nil from indexpkg. Anything left is a real read failure, which
		// gains the published code here (sqliteErrorCode, index.go) — the CLI
		// error-code contract lives on this side of the extraction seam.
		return "", newCodedError(sqliteErrorCode(err), err, "%v", err)
	}
	return v, nil
}
