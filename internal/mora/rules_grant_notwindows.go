//go:build !windows

package mora

import (
	"os"
	"path/filepath"
	"syscall"
)

// rulesSignersProtected reports whether only root can change the signers file:
// the file and every folder above it must be owned by root, with no write
// access for group or others.
func rulesSignersProtected(path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return false
	}
	for p := resolved; ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err != nil {
			return false
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid != 0 || fi.Mode().Perm()&0o022 != 0 {
			return false
		}
		if p == filepath.Dir(p) {
			return true
		}
	}
}
