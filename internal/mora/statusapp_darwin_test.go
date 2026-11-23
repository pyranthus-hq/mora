//go:build darwin

package mora

import "testing"

// A test binary is started by go test, not by the signed Mora Status collector.
func TestStatusAppCallerRefusesAnUnsignedParent(t *testing.T) {
	if err := statusAppCaller(); err == nil {
		t.Fatal("go test passed the Mora Status caller check")
	}
}
