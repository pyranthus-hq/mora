package mora

import (
	"encoding/json"
	"strings"
	"testing"
)

// Plain `mora read <id>` used to print only the one-line list header, so
// agents re-ran every read with --json (pilot F12: 6 calls for 3 reads). The
// plain form now prints the header line unchanged, then the full body.
func TestPlainReadPrintsTheBody(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	var m Memory
	if err := json.Unmarshal([]byte(run(t, "write", "--scope", "project:acme", "--type", "decision",
		"--title", "Ship it", "--text", "we decided to ship\n\nsecond paragraph", "--json")), &m); err != nil {
		t.Fatal(err)
	}
	out := run(t, "read", m.ID)
	header := m.ID + "\tproject:acme\tShip it\n"
	if !strings.Contains(out, header) {
		t.Fatalf("plain read lost its header line:\n%s", out)
	}
	if !strings.Contains(out, header+"\nwe decided to ship\n\nsecond paragraph\n") {
		t.Fatalf("plain read must print the body after the header:\n%s", out)
	}
}

func TestPlainReadPrintsASharedMemoryBody(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	setupSubscription(t, cfg, "neil", []Memory{
		fixtureMemory("mem_20260601_000000_aaaaaaaa", "Neil sqlite decision", "neil standardized on sqlite too"),
	})
	out := run(t, "read", "mem_20260601_000000_aaaaaaaa")
	if !strings.Contains(out, "[neil] Neil sqlite decision\n\nneil standardized on sqlite too\n") {
		t.Fatalf("plain read of a shared memory must print its owner-labelled header and body:\n%s", out)
	}
}
