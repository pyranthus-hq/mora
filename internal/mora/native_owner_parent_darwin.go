//go:build darwin

package mora

import (
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"time"
)

const nativeOwnerMacIdentity = "com.pyranthus.mora-status:VS8M5VJBZ5"
const nativeOwnerMacRequirement = `anchor apple generic and identifier "com.pyranthus.mora-status" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = "VS8M5VJBZ5"`

type darwinNativeOwnerPeer struct{}

func (darwinNativeOwnerPeer) identity() (string, bool) {
	pid := os.Getppid()
	if pid <= 1 {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--test-requirement", "="+nativeOwnerMacRequirement, strconv.Itoa(pid))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil || err != nil || len(out) != 0 || os.Getppid() != pid {
		return "", false
	}
	return nativeOwnerMacIdentity, true
}

func newNativeOwnerVerifier(cfg Config) desktopReviewVerifier {
	return newNativeOwnerVerifierForPeer(cfg, darwinNativeOwnerPeer{})
}

func serveNativeDesktopReview(ctx context.Context, cfg Config, stdin io.Reader, stdout io.Writer) error {
	return serveDesktopReviewStdio(ctx, cfg, newNativeOwnerVerifier(cfg), stdin, stdout)
}
