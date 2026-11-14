//go:build !windows

package gitsync

import (
	"path/filepath"
	"runtime"
)

func backupReparse(string) (bool, error) { return false, nil }
func backupSystemAlias(path string) bool {
	if runtime.GOOS != "darwin" || (path != "/tmp" && path != "/var") {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	return err == nil && resolved == "/private"+path
}
