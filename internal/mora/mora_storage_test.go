package mora

import (
	"bytes"
	"strings"
	"testing"
)

// TestDoctorReportsStorage verifies the composition root exposes the storage report.
func TestDoctorReportsStorage(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	var out bytes.Buffer
	if err := Run(testCtx(t), []string{"doctor"}, &out, &out, nil); err != nil {
		t.Fatalf("doctor: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "storage") {
		t.Fatalf("doctor output should include a storage line; got:\n%s", out.String())
	}
}
