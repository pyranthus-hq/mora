package backupjob

import (
	"github.com/pyranthus-hq/mora/internal/leasefile"
	"os"
)

func lockRun(f *os.File) error   { return leasefile.TryLock(f) }
func unlockRun(f *os.File) error { return leasefile.Unlock(f) }
