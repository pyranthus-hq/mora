//go:build windows

package storage

import (
	"fmt"
	mrand "math/rand/v2"
	"os"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"

	"golang.org/x/sys/windows"
)

// fileIDKey identifies a file on Windows by (volume serial, file index).
type fileIDKey struct {
	volume uint32
	index  uint64
}

var fileIdentityCreateFile = windows.CreateFile

func fileIdentity(path string, fileInfo os.FileInfo) (fileIDKey, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fileIDKey{}, fmt.Errorf("storage accounting: file identity %s: %w", path, err)
	}
	flags := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if fileInfo.IsDir() {
		flags = windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	deadline := time.Now().Add(5 * time.Second)
	var h windows.Handle
	for attempt := 0; ; attempt++ {
		h, err = fileIdentityCreateFile(
			p,
			0,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil,
			windows.OPEN_EXISTING,
			flags,
			0,
		)
		if err == nil {
			break
		}
		// A SQLite WAL/SHM file can be delete-pending for a brief window while
		// another process checkpoints or closes it. Windows reports that as
		// ACCESS_DENIED/SHARING_VIOLATION even though the requested handle shares
		// read, write, and delete. Retry only that transient class; permanent
		// permission and identity failures still fail accounting closed.
		if !atomicio.SharingViolationRetryable(err) || !time.Now().Before(deadline) {
			return fileIDKey{}, fmt.Errorf("storage accounting: file identity %s: %w", path, err)
		}
		time.Sleep(acquireBackoff(attempt))
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return fileIDKey{}, fmt.Errorf("storage accounting: file identity %s: %w", path, err)
	}
	return fileIDKey{
		volume: info.VolumeSerialNumber,
		index:  uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
	}, nil
}

func acquireBackoff(attempt int) time.Duration {
	capMs := 1 << min(attempt, 5)
	return time.Duration(1+mrand.IntN(capMs)) * time.Millisecond
}
