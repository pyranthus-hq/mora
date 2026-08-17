package mora

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContractStdoutIsPure(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--title", "stream seed one", "--text", "alpha stream seed")
	run(t, "write", "--title", "stream seed two", "--text", "beta stream seed")

	body, err := os.ReadFile(filepath.Join("testdata", "contracts", "json-baseline.json"))
	if err != nil {
		t.Fatal(err)
	}
	var baseline []contractJSONBaselineRow
	if err := json.Unmarshal(body, &baseline); err != nil {
		t.Fatalf("parse JSON baseline: %v", err)
	}
	statusByPath := make(map[string]string, len(baseline))
	for _, row := range baseline {
		statusByPath[row.Path] = row.Status
	}

	expectedSkips := map[string]bool{}
	seenSkips := map[string]bool{}
	for _, row := range loadCLIRegistry(t).Commands {
		if row.Platform != "all" || row.JSONContract == "exempt" || contractBaselineNonExecutable[row.Path] != "" {
			continue
		}
		status, ok := statusByPath[row.Path]
		if !ok {
			t.Fatalf("%s has no committed baseline classification", row.Path)
		}
		// The measured baseline also has two mixed rows (version and loop begin).
		// They are not pure JSON yet, so they stay in the same temporary Plan 04/05
		// exclusion set until their command contracts land.
		if status == "prose" || status == "flag_error" || status == "mixed" {
			expectedSkips[row.Path] = true
		}
	}

	for _, row := range loadCLIRegistry(t).Commands {
		row := row
		if row.Platform != "all" || row.JSONContract == "exempt" || contractBaselineNonExecutable[row.Path] != "" {
			continue
		}
		t.Run(strings.ReplaceAll(row.Path, " ", "/"), func(t *testing.T) {
			if expectedSkips[row.Path] {
				seenSkips[row.Path] = true
				t.Skipf("%s is a committed %s baseline; Plans 04 and 05 own its JSON contract", row.Path, statusByPath[row.Path])
			}
			args := append(strings.Fields(row.Path), "--json")
			stdout, _, _ := runSplit(t, args...)
			if trimmed := strings.TrimSpace(stdout); trimmed != "" && !json.Valid([]byte(trimmed)) {
				t.Fatalf("stdout is not one JSON document: %q", stdout)
			}
		})
	}

	if diff := setDiff(expectedSkips, seenSkips); diff != "" {
		t.Fatalf("stdout-purity skip set drift%s", diff)
	}
}

func TestContractWriteJSONDegradedIndexKeepsStdoutPure(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--title", "indexed stream seed", "--text", "seed the initial index")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	idxUpsertStampVaultID(t, cfg, "v_stream_contract_mismatch")

	stdout, stderr, err := runSplit(t, "write", "--title", "degraded stream", "--text", "warning must leave stdout", "--json")
	if err != nil {
		t.Fatalf("degraded write returned error: %v", err)
	}
	if !json.Valid([]byte(stdout)) {
		t.Fatalf("degraded write stdout is not valid JSON: %q", stdout)
	}
	const warning = "warning: memory saved but the search index was not updated"
	if strings.Contains(stdout, warning) {
		t.Fatalf("degraded-write warning leaked to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, warning) {
		t.Fatalf("degraded-write warning missing from stderr: %q", stderr)
	}
}
