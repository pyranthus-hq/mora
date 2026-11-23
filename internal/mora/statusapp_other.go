//go:build !darwin

package mora

import "errors"

func statusAppCaller() error { return errors.New("Mora Status runs only on macOS") }
