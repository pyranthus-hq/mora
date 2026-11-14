//go:build !darwin && !windows

package mora

import (
	"context"
	"io"
)

type unavailableNativeOwnerPeer struct{}

func (unavailableNativeOwnerPeer) identity() (string, bool) { return "", false }
func newNativeOwnerVerifier(cfg Config) desktopReviewVerifier {
	return newNativeOwnerVerifierForPeer(cfg, unavailableNativeOwnerPeer{})
}

func serveNativeDesktopReview(ctx context.Context, cfg Config, stdin io.Reader, stdout io.Writer) error {
	return serveDesktopReviewStdio(ctx, cfg, newNativeOwnerVerifier(cfg), stdin, stdout)
}
