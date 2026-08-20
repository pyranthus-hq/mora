//go:build !darwin

package appupdate

import "fmt"

func atomicSwapMoraAppDirectories(_, _ string) error {
	return fmt.Errorf("atomic Mora.app replacement is supported only on macOS")
}
