package mora

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContractLeafJSONReceipts(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cases := []struct {
		name, schema, arrayKey string
		args                   []string
	}{
		{name: "schedule", schema: "mora.schedule.list", args: []string{"schedule", "list", "--json"}},
		{name: "usage", schema: "mora.usage.report", args: []string{"usage", "report", "--json"}},
		{name: "backup", schema: "mora.backup", args: []string{"backup", "--json"}},
		{name: "forget", schema: "mora.forget.list", arrayKey: "entries", args: []string{"forget", "list", "--json"}},
		{name: "hook", schema: "mora.hook.status", arrayKey: "harnesses", args: []string{"hook", "status", "--json"}},
		{name: "entities", schema: "mora.entities", arrayKey: "entities", args: []string{"entities", "--json"}},
		{name: "graph", schema: "mora.graph", arrayKey: "entities", args: []string{"graph", "--json"}},
		{name: "sources", schema: "mora.sources.list", arrayKey: "sources", args: []string{"sources", "list", "--json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, _, err := runSplit(t, tc.args...)
			if err != nil {
				t.Fatalf("%s returned error: %v", tc.name, err)
			}
			var receipt map[string]json.RawMessage
			if err := json.Unmarshal([]byte(stdout), &receipt); err != nil {
				t.Fatalf("%s stdout is not one JSON receipt: %q: %v", tc.name, stdout, err)
			}
			var schema string
			if err := json.Unmarshal(receipt["schema"], &schema); err != nil || schema != tc.schema {
				t.Fatalf("%s schema = %q, %v; want %q", tc.name, schema, err, tc.schema)
			}
			var version int
			if err := json.Unmarshal(receipt["schema_version"], &version); err != nil || version != 1 {
				t.Fatalf("%s schema_version = %d, %v; want 1", tc.name, version, err)
			}
			if tc.arrayKey != "" && string(receipt[tc.arrayKey]) == "null" {
				t.Fatalf("%s %s must be [] rather than null", tc.name, tc.arrayKey)
			}
		})
	}
}

func TestContractLeafCommandsRejectUnknownFlags(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	for _, args := range [][]string{
		{"schedule", "list", "--bogusflag"},
		{"usage", "report", "--bogusflag"},
		{"backup", "--bogusflag"},
		{"forget", "list", "--bogusflag"},
		{"hook", "status", "--bogusflag"},
		{"entities", "--bogusflag"},
		{"graph", "--bogusflag"},
		{"sources", "list", "--bogusflag"},
	} {
		_, _, err := runSplit(t, args...)
		if err == nil {
			t.Fatalf("%q accepted an unknown flag", args)
		}
		var typed moraError
		if !errors.As(err, &typed) || typed.Code != errCodeUsageUnknownFlag {
			t.Fatalf("%q error = %v; want usage.unknown_flag", args, err)
		}
	}
}

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
