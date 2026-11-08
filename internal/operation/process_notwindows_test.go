//go:build !windows

package operation

import "testing"

// Signalling pid 1 fails with a permission error for a normal user. That proves
// the process exists, so calling it dead could clear a live owner's marker.
func TestProcessAliveTreatsPermissionErrorAsAlive(t *testing.T) {
	if !processAlive(1) {
		t.Fatal("pid 1 read as dead")
	}
}
