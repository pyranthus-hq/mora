package mora

import (
	"context"
	"strings"
	"testing"
)

func protectedSyncTestToken() string { return "0123456789abcdef" + "0123456789abcdef" }

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestProtectedSyncReceiptRejectsUnprotectedSource(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	err := Run(context.Background(), []string{"sync", "google", protectedSyncReceiptFlag, protectedSyncTestToken()}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "only valid") {
		t.Fatalf("unprotected receipt error = %v", err)
	}
}
