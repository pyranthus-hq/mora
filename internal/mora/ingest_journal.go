package mora

import (
	ingestpkg "github.com/pyranthus-hq/mora/internal/ingest"
	"os"
)

// removeIngestJournalFile is injected so committed-index cleanup failures remain testable.
var removeIngestJournalFile = os.Remove

func ingestLeaseSeams() ingestpkg.LeaseSeams {
	return ingestpkg.LeaseSeams{PID: os.Getpid, ProcessAlive: processAlive}
}
func ingestRecoverySeams() ingestpkg.RecoverySeams {
	return ingestpkg.RecoverySeams{CleanPathSet: cleanPathSet, CleanPath: cleanVaultPath, LeaseHeld: func(cfg Config, key string) bool { return ingestpkg.LeaseHeld(cfg, key, ingestLeaseSeams()) }, Remove: removeIngestJournalFile, ValidToken: validOperationToken}
}
func ingestPublishSeams() ingestpkg.PublishSeams {
	return ingestpkg.PublishSeams{ValidToken: validOperationToken, NewID: newID, Clock: indexClock, CleanPath: cleanVaultPath, Lease: ingestLeaseSeams()}
}
