package mora

import (
	"encoding/json"
	"testing"
)

// The CLI returns the same derived origin as MCP on every JSON read path.
// The stored memory stays free of that presentation field.
func TestCLIJSONReadListSearchCarryDerivedProvenance(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	written := run(t, "write", "--title", "provenance parity fixture", "--text", "provenance parity fixture body", "--json")
	var receipt struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(written), &receipt); err != nil || receipt.ID == "" {
		t.Fatalf("write receipt: %v: %s", err, written)
	}

	for _, tc := range []struct {
		name string
		args []string
		key  string
	}{
		{"read", []string{"read", receipt.ID, "--json"}, ""},
		{"list", []string{"list", "--json"}, "memories"},
		{"search", []string{"search", "provenance parity fixture", "--json"}, "memories"},
	} {
		out := run(t, tc.args...)
		var doc map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("decode %s: %v", tc.name, err)
		}
		var rows []map[string]any
		if tc.key == "" {
			var row map[string]any
			if err := json.Unmarshal([]byte(out), &row); err != nil {
				t.Fatal(err)
			}
			rows = []map[string]any{row}
		} else if err := json.Unmarshal(doc[tc.key], &rows); err != nil {
			t.Fatalf("decode %s rows: %v", tc.name, err)
		}
		found := false
		for _, row := range rows {
			if row["id"] == receipt.ID {
				found = true
				if row["provenance"] != "authored" {
					t.Fatalf("%s provenance = %#v", tc.name, row["provenance"])
				}
			}
		}
		if !found {
			t.Fatalf("%s omitted fixture %s", tc.name, receipt.ID)
		}
	}
}
