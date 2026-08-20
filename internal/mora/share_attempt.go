package mora

import (
	"os"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	sharingpkg "github.com/pyranthus-hq/mora/internal/sharing"
)

// Mora retains sharing-attempt orchestration; internal/sharing owns its durable store.
type shareAttempt = sharingpkg.Attempt

// These seams preserve the ordering witnesses around fetch/build dispatch.
var (
	shareAttemptStartFileSyncFn = (*os.File).Sync
	shareAttemptStartDirSyncFn  = atomicio.SyncDir
)

func shareAttemptStore(cfg Config) sharingpkg.AttemptStore {
	return sharingpkg.AttemptStore{
		DataDir:        cfg.DataDir,
		FileSync:       shareAttemptStartFileSyncFn,
		StartDirSync:   shareAttemptStartDirSyncFn,
		ClaimExclusive: claimExclusiveDurable,
	}
}
func shareAttemptClaimPaths(cfg Config, name string) ([]string, error) {
	return shareAttemptStore(cfg).ClaimPaths(name)
}
func loadShareAttempt(cfg Config, name string) (shareAttempt, bool, error) {
	return shareAttemptStore(cfg).Load(name)
}
func startShareAttempt(cfg Config, name, runID string, now time.Time) error {
	return shareAttemptStore(cfg).Start(name, runID, now)
}
func recoverShareAttemptClaims(cfg Config, name string) error {
	return shareAttemptStore(cfg).RecoverClaims(name)
}
func finishShareAttempt(cfg Config, name, runID string, terminal shareAttempt) error {
	return shareAttemptStore(cfg).Finish(name, runID, terminal)
}
