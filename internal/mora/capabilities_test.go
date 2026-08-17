package mora

import (
	"encoding/json"
	"testing"
)

func TestCapabilitiesContract(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	var payload map[string]any
	if err := json.Unmarshal([]byte(run(t, "capabilities", "--json")), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["schema"] != "mora.capabilities" || payload["schema_version"] != float64(1) {
		t.Fatalf("receipt envelope = %#v", payload)
	}
	for _, key := range []string{"commands", "connectors", "schemas"} {
		if values, ok := payload[key].([]any); !ok || values == nil {
			t.Fatalf("%s = %#v, want non-nil array", key, payload[key])
		}
	}
	features, ok := payload["features"].(map[string]any)
	if !ok || features["repair"] != featureUnsupported || features["deep_link"] != featureUnsupported {
		t.Fatalf("features = %#v", payload["features"])
	}
	mcp, ok := payload["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("mcp = %#v", payload["mcp"])
	}
	if tools, ok := mcp["tools"].([]any); !ok || len(tools) != 12 {
		t.Fatalf("mcp.tools = %#v, want 12 entries", mcp["tools"])
	}
	for _, connector := range payload["connectors"].([]any) {
		if connector.(map[string]any)["type"] == "gdrive" {
			t.Fatal("gdrive must stay outside the capabilities payload")
		}
	}
}
