package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// contractBaselineNonExecutable records paths that must never be driven by the
// baseline probe. The reasons make the safety boundary reviewable: this test
// may run in CI and must not block, open a browser, mutate the host, or replace
// the binary that is executing it.
var contractBaselineNonExecutable = map[string]string{
	"mcp":                "long-running JSON-RPC server",
	"mcp serve":          "long-running JSON-RPC server",
	"serve":              "long-running loopback HTTP server entrypoint",
	"serve http":         "long-running loopback HTTP server",
	"upgrade":            "self-replacing binary",
	"upgrade app":        "self-replacing binary",
	"connect":            "interactive browser OAuth handoff",
	"connect google":     "interactive browser OAuth handoff",
	"connectors setup":   "interactive connector setup menu",
	"init":               "interactive first-run wizard",
	"hook install":       "mutates host hook configuration",
	"hook uninstall":     "mutates host hook configuration",
	"schedule install":   "mutates host scheduler configuration",
	"schedule uninstall": "mutates host scheduler configuration",
	"delete":             "destructive memory mutation",
	"forget":             "destructive governance mutation",
	"unforget":           "governance mutation",
	"disconnect":         "mutates connector configuration",
	"--help":             "human help alias",
	"-h":                 "human help alias",
	"help":               "human help text",
	"--version":          "version alias intentionally measured statically",
	"-v":                 "version alias intentionally measured statically",
}

type contractJSONBaselineRow struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

func TestContractJSONBaseline(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "write", "--title", "baseline seed one", "--text", "alpha contract baseline seed")
	run(t, "write", "--title", "baseline seed two", "--text", "beta contract baseline seed")

	registry := loadCLIRegistry(t)
	rows := make([]contractJSONBaselineRow, 0, len(registry.Commands))
	for _, command := range registry.Commands {
		status := "static_only"
		if command.Platform == "all" {
			if _, denied := contractBaselineNonExecutable[command.Path]; !denied {
				status = classifyContractJSONProbe(t, strings.Fields(command.Path))
			}
		}
		rows = append(rows, contractJSONBaselineRow{Path: command.Path, Status: status})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })

	got, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", "contracts", "json-baseline.json")
	if os.Getenv("MORA_UPDATE_CONTRACT_BASELINE") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write JSON baseline: %v", err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read JSON baseline: %v (regenerate with MORA_UPDATE_CONTRACT_BASELINE=1)", err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("JSON baseline drifted; regenerate with MORA_UPDATE_CONTRACT_BASELINE=1\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func classifyContractJSONProbe(t *testing.T, path []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(testCtx(t), 30*time.Second)
	defer cancel()
	args := append(append([]string(nil), path...), "--json")
	var stdout, stderr bytes.Buffer
	err := Run(ctx, args, &stdout, &stderr, strings.NewReader(""))
	if err != nil && strings.Contains(err.Error(), "flag provided but not defined") {
		return "flag_error"
	}
	out := stdout.Bytes()
	if len(out) == 0 {
		return "empty"
	}
	var value any
	if err := json.Unmarshal(out, &value); err == nil {
		if value == nil {
			return "parses_null"
		}
		return "parses"
	}
	if hasJSONDocumentSuffix(out) {
		return "mixed"
	}
	return "prose"
}

func hasJSONDocumentSuffix(out []byte) bool {
	for i, b := range out {
		if b != '{' && b != '[' && b != '"' && b != 'n' && b != 't' && b != 'f' && b != '-' && (b < '0' || b > '9') {
			continue
		}
		if i > 0 && json.Valid(out[i:]) {
			return true
		}
	}
	return false
}
