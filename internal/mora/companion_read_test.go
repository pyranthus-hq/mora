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
func TestCompanionTodayEmitsTheWireDocument(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--title", "Pilot scope reply", "--text", "Reply to the pilot scope question by Friday.", "--json")

	doc := decodeWire(t, run(t, "companion", "today", "--json"), companion.SchemaToday)

	items, ok := doc["items"].([]any)
	if !ok {
		t.Fatalf("today document has no items array: %v", doc)
	}
	for _, raw := range items {
		item := raw.(map[string]any)
		for _, key := range []string{"id", "kind", "title"} {
			if _, ok := item[key].(string); !ok {
				t.Fatalf("item lacks %s: %v", key, item)
			}
		}
	}
	if _, ok := doc["truncated"].(bool); !ok {
		t.Fatalf("today document has no truncated flag: %v", doc)
	}
	health, ok := doc["health"].(map[string]any)
	if !ok || health["policy"] == nil {
		t.Fatalf("today document has no health summary: %v", doc)
	}
}

func TestCompanionTodayMatchesTheLoopbackRoute(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--title", "Pilot scope reply", "--text", "Reply to the pilot scope question by Friday.", "--json")

	cli := decodeWire(t, run(t, "companion", "today", "--json"), companion.SchemaToday)

	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	direct, err := newCompanionReader(cfg).Today(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	directJSON, _ := json.Marshal(direct)
	var route map[string]any
	if err := json.Unmarshal(directJSON, &route); err != nil {
		t.Fatal(err)
	}
	delete(cli, "generated_at")
	delete(route, "generated_at")
	cliJSON, _ := json.Marshal(cli)
	routeJSON, _ := json.Marshal(route)
	if string(cliJSON) != string(routeJSON) {
		t.Fatalf("CLI and reader documents differ\ncli:   %s\nroute: %s", cliJSON, routeJSON)
	}
}
func TestCompanionTodayRefusesArgumentsAndAcceptsEmptyHumanRead(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	_, _, err := runSplit(t, "companion", "today", "extra", "--json")
	if err == nil || !strings.Contains(err.Error(), "unexpected argument") {
		t.Fatalf("positional argument must be refused, got %v", err)
	}
	stdout, _, err := runSplit(t, "companion", "today")
	if err != nil {
		t.Fatalf("human rendering must succeed: %v", err)
	}
	_ = stdout
}

func TestCompanionContextEmitsTheWireDocument(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--title", "Pilot scope", "--text", "Keep the pilot to the platform team until the board meets.", "--json")

	doc := decodeWire(t,
		run(t, "companion", "context", "--mode", "search", "--query", "pilot scope", "--json"),
		companion.SchemaContext)

	if doc["mode"] != "search" || doc["query"] != "pilot scope" {
		t.Fatalf("context document does not echo the request: %v", doc)
	}
	evidence, ok := doc["evidence"].([]any)
	if !ok || len(evidence) == 0 {
		t.Fatalf("context document has no evidence for a matching memory: %v", doc)
	}
	first := evidence[0].(map[string]any)
	if _, ok := first["memory_id"].(string); !ok {
		t.Fatalf("evidence row lacks memory_id: %v", first)
	}
	if _, ok := doc["synthesis_prompt"].(string); !ok {
		t.Fatalf("context document lacks synthesis_prompt: %v", doc)
	}
}

func TestCompanionContextRefusesBadModeAndMissingQuery(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	_, _, err := runSplit(t, "companion", "context", "--mode", "chat", "--query", "x", "--json")
	if err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("unknown mode must be refused with a message naming mode, got %v", err)
	}
	_, _, err = runSplit(t, "companion", "context", "--mode", "search", "--json")
	if err == nil || !strings.Contains(err.Error(), "query") {
		t.Fatalf("missing query must be refused with a message naming query, got %v", err)
	}
	_, _, err = runSplit(t, "companion", "context", "--mode", "search", "--query", "x")
	if err == nil || !strings.Contains(err.Error(), "--json") {
		t.Fatalf("context has no human rendering; omitting --json must be a usage error, got %v", err)
	}
}
