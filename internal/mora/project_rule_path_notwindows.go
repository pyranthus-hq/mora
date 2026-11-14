//go:build !windows

package mora

import "os"

func projectRuleRegularFile(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	return info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular(), nil
}
