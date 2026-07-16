//go:build windows

package mora

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// fileIDKey identifies a file on Windows by (volume serial, file index).
type fileIDKey struct {
	volume uint32
	index  uint64
}

func fileIdentity(path string, fileInfo os.FileInfo) (fileIDKey, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fileIDKey{}, fmt.Errorf("storage accounting: file identity %s: %w", path, err)
	}
	flags := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if fileInfo.IsDir() {
		flags = windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	h, err := windows.CreateFile(
		p,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		flags,
		0,
	)
	if err != nil {
		return fileIDKey{}, fmt.Errorf("storage accounting: file identity %s: %w", path, err)
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
