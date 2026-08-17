//go:build !windows

package atomicio

import (
	"errors"
	"syscall"
)

func claimLinkUnsupported(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, errors.ErrUnsupported)
}
