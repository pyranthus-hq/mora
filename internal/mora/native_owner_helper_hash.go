package mora

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
)

func nativeOwnerHelperDigestMatches(r io.Reader, expected string) error {
	if len(expected) != sha256.Size*2 {
		return errors.New("missing helper digest")
	}
	want, err := hex.DecodeString(expected)
	if err != nil {
		return err
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, r); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(sum.Sum(nil), want) != 1 {
		return errors.New("helper digest mismatch")
	}
	return nil
}
