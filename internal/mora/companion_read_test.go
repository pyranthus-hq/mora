package mora

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/companion"
)

// decodeWire decodes one wire document and pins the WIRE envelope, which is
// companion.SchemaVersion, not the CLI receipt version decodeCompanion pins.
func decodeWire(t *testing.T, out, wantSchema string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output does not decode as one JSON document: %v\n%s", err, out)
	}
	if got, _ := doc["schema"].(string); got != wantSchema {
		t.Fatalf("schema = %q, want %q", got, wantSchema)
	}
	if got, _ := doc["schema_version"].(float64); got != float64(companion.SchemaVersion) {
		t.Fatalf("schema_version = %v, want %d", doc["schema_version"], companion.SchemaVersion)
	}
	return doc
}

func TestCompanionHealthEmitsTheWireDocument(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	doc := decodeWire(t, run(t, "companion", "health", "--json"), companion.SchemaHealth)

	if _, ok := doc["state"].(string); !ok {
		t.Fatalf("health document has no state: %v", doc)
	}
	if _, ok := doc["policy"].(string); !ok {
		t.Fatalf("health document has no policy: %v", doc)
	}
	index, ok := doc["index"].(map[string]any)
	if !ok {
		t.Fatalf("health document has no index object: %v", doc)
	}
	if _, ok := index["memories"].(float64); !ok {
		t.Fatalf("index has no memories count: %v", index)
	}
}

func TestCompanionHealthRefusesArgumentsAndRendersHumanText(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	_, _, err := runSplit(t, "companion", "health", "extra", "--json")
	if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("positional argument must be refused, got %v", err)
	}
	stdout, _, err := runSplit(t, "companion", "health")
	if err != nil {
		t.Fatalf("human rendering must succeed: %v", err)
	}
	if !strings.HasPrefix(stdout, "state\t") {
		t.Fatalf("human rendering must start with the state line, got %q", stdout)
	}
}
