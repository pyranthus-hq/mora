package mora

import (
	"bytes"
	"strings"
	"testing"
)

// TestReingestNoSources proves `mora reingest` runs cleanly (rebuilds the graph)
// when there are no enabled connector sources, and reports zero items.
func TestReingestNoSources(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	var out bytes.Buffer
	if err := cmdReingest(testCtx(t), nil, &out, testStderr); err != nil {
		t.Fatalf("reingest: %v", err)
	}
	if !strings.Contains(out.String(), "reingested 0 item(s)") {
		t.Fatalf("unexpected output: %q", out.String())
	}
	// The graph index must exist after reingest (it rebuilds).
	if !graphReady(cfg) {
		t.Fatal("reingest did not build the graph index")
	}
}

func TestReingestFullFlagAndHelp(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	var out bytes.Buffer
	if err := cmdReingest(testCtx(t), []string{"--full"}, &out, testStderr); err != nil {
		t.Fatalf("reingest --full: %v", err)
	}
	if !strings.Contains(out.String(), "full lookback") {
		t.Fatalf("--full not reflected in output: %q", out.String())
	}

	var help bytes.Buffer
	if err := cmdReingest(testCtx(t), []string{"--help"}, &help, testStderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(help.String(), "usage: mora reingest") {
		t.Fatalf("help missing: %q", help.String())
	}

	var bad bytes.Buffer
	if err := cmdReingest(testCtx(t), []string{"--bogus"}, &bad, testStderr); err == nil {
		t.Fatal("expected an error for an unknown flag")
	}
}
