//go:build !darwin

package mora

import "fmt"

func atomicSwapMoraAppDirectories(_, _ string) error {
	return fmt.Errorf("atomic Mora.app replacement is supported only on macOS")
}
