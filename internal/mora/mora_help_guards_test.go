package mora

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// `mora sync --help` must print usage and must NOT trigger a real backfill. It
// previously fell through the cmdSync dispatcher (neither "status" nor "imessage")
// straight into backfillEnabledGoogle, running a live Gmail+Calendar sync from a
// help query — a state+network footgun.
func TestSyncHelpPrintsUsageNoBackfill(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	out := run(t, "sync", "--help")
	if strings.Contains(out, "synced") {
		t.Fatalf("`sync --help` performed a sync; want usage only:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "usage") {
		t.Fatalf("`sync --help` did not print usage:\n%s", out)
	}
}

// `mora search --help` must degrade gracefully (usage / zero results), never leak a
// raw fts5 syntax error from the leading dash being treated as a MATCH operator.
func TestSearchHelpNoRawFTSError(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	var out bytes.Buffer
	err := Run(context.Background(), []string{"search", "--help"}, &out, &out, strings.NewReader(""))
	if err != nil {
		t.Fatalf("`search --help` returned an error (want graceful): %v\n%s", err, out.String())
	}
	low := strings.ToLower(out.String())
	if strings.Contains(low, "fts5") || strings.Contains(low, "syntax error") {
		t.Fatalf("`search --help` leaked a raw fts5 error:\n%s", out.String())
	}
}

// A leading-dash query token must be sanitised so FTS5 never sees a bare operator.
// Tokens are now emitted as quoted FTS5 strings: edge dashes are trimmed, internal
// hyphens survive inside the quotes, and a bare '-' operator can never reach FTS5.
