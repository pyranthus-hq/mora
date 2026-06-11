package mora

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// One JSON-RPC request is one stdio line; a big write_memory or think payload
// easily exceeds bufio.Scanner's 64KB default. Without an explicit buffer the
// scanner dies with ErrTooLong and the MCP server exits mid-session — the agent
// sees every subsequent tool call fail. Pin: an oversized line is served.
func TestServeMCPHandlesOversizedRequestLine(t *testing.T) {
	pad := strings.Repeat("a", 80*1024) // > 64KB default MaxScanTokenSize
	line := `{"jsonrpc":"2.0","id":7,"method":"initialize","params":{"_pad":"` + pad + `"}}` + "\n"
	var out bytes.Buffer
	if err := serveMCP(context.Background(), &out, strings.NewReader(line)); err != nil {
		t.Fatalf("serveMCP returned error on oversized line: %v", err)
	}
	if !strings.Contains(out.String(), `"id":7`) {
		t.Fatalf("no response for the oversized request; stdout=%q", out.String())
	}
}

// And a line beyond the new hard cap must fail LOUDLY (actionable error), not
// silently — honesty rule.
func TestServeMCPOverHardCapErrorsLoudly(t *testing.T) {
	pad := strings.Repeat("a", mcpMaxRequestBytes+1024)
	line := `{"jsonrpc":"2.0","id":8,"method":"initialize","params":{"_pad":"` + pad + `"}}` + "\n"
	var out bytes.Buffer
	err := serveMCP(context.Background(), &out, strings.NewReader(line))
	if err == nil {
		t.Fatal("expected an error for a request beyond the hard cap")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("error should say the request exceeded the cap, got: %v", err)
	}
}
