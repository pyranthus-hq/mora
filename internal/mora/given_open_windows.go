//go:build windows

package mora

import (
	"errors"
	"syscall"
	"time"
)

const givenOpenFlags = 0

// givenRetry retries an open or rename that lost a race with another process
// (errno 32 is ERROR_SHARING_VIOLATION; Go opens files without FILE_SHARE_DELETE). shortcut: five tries, 10 ms apart;
// after that the caller skips the line, as the log must never block a hook.
func givenRetry(op func() error) error {
	err := op()
	for i := 0; i < 4 && errors.Is(err, syscall.Errno(32)); i++ {
		time.Sleep(10 * time.Millisecond)
		err = op()
	}
	return err
}
