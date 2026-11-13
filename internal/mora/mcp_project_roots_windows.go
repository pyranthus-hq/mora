package mora

import (
	"path/filepath"
	"syscall"
	"unsafe"
)

var mcpRootGetDriveType = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDriveTypeW")

func mcpRootLocalVolume(dir string) bool {
	vol := filepath.VolumeName(dir)
	if len(vol) != 2 || !isASCIILetter(vol[0]) || vol[1] != ':' {
		return false
	}
	name, err := syscall.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return false
	}
	typ, _, _ := mcpRootGetDriveType.Call(uintptr(unsafe.Pointer(name)))
	const (
		driveRemovable = 2
		driveFixed     = 3
		driveCDROM     = 5
		driveRAMDisk   = 6
	)
	return typ == driveRemovable || typ == driveFixed || typ == driveCDROM || typ == driveRAMDisk
}

func mcpRootDirectory(dir string) bool {
	name, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return false
	}
	attr, err := syscall.GetFileAttributes(name)
	return err == nil && attr&syscall.FILE_ATTRIBUTE_DIRECTORY != 0 && attr&syscall.FILE_ATTRIBUTE_REPARSE_POINT == 0
}
