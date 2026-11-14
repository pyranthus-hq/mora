//go:build !darwin

package mora

type unavailableNativeOwnerPeer struct{}

func (unavailableNativeOwnerPeer) identity() (string, bool) { return "", false }
func newNativeOwnerVerifier(cfg Config) desktopReviewVerifier {
	return newNativeOwnerVerifierForPeer(cfg, unavailableNativeOwnerPeer{})
}
