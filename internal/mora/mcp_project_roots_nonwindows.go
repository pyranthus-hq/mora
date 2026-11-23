//go:build !windows

package mora

import "os"

func mcpRootLocalVolume(string) bool { return true }

func mcpRootDirectory(dir string) bool {
	info, err := os.Lstat(dir)
	return err == nil && info.IsDir()
}
