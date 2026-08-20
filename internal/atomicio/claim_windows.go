//go:build windows

package atomicio

import (
	"errors"
	"syscall"
)

const errClaimNotSupported syscall.Errno = 50

func claimLinkUnsupported(err error) bool {
	return errors.Is(err, errClaimNotSupported) || errors.Is(err, errors.ErrUnsupported)
}
