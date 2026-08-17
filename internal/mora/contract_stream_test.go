package mora

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
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

// TestContractMutationJSONReceipts covers the command paths whose --json
// contract landed in Plan 05: each emits one receipt under the schema name the
// command registry assigns it, and each keeps its progress prose off stdout.
func TestContractMutationJSONReceipts(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.md"), []byte("# note\n\nfilesystem receipt seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, schema, arrayKey string
		args                   []string
	}{
		{name: "connect_filesystem", schema: "mora.connect.filesystem", args: []string{"connect", "filesystem", dir, "--json"}},
		{name: "sync_filesystem", schema: "mora.sync.filesystem", args: []string{"sync", "filesystem", "--json"}},
		{name: "ingest_run", schema: "mora.ingest.run", args: []string{"ingest", "run", "--all", "--json"}},
		{name: "tasks_sync", schema: "mora.tasks.sync", args: []string{"tasks", "sync", "--json"}},
		{name: "tasks_add", schema: "mora.tasks.add", args: []string{"tasks", "add", "receipt task", "--json"}},
		{name: "tasks_done", schema: "mora.tasks.done", args: []string{"tasks", "done", "receipt task", "--json"}},
		{name: "loop_register", schema: "mora.loop.register", args: []string{"loop", "register", "receiptloop", "--json"}},
		{name: "teach_history", schema: "mora.teach.history", arrayKey: "entries", args: []string{"teach", "history", "--json"}},
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

// TestMergeDecisionJSONReceipts proves the shared merge-decision implementation
// publishes the schema name the registry assigns to the path that invoked it:
// `merge confirm` and its `teach identity confirm` alias are one code path with
// two contracts.
func TestMergeDecisionJSONReceipts(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	for _, tc := range []struct {
		name, schema  string
		args          []string
		wantDecision  string
		wantUndoStart string
	}{
		{
			name: "merge_confirm", schema: "mora.merge.confirm", wantDecision: "confirm",
			args: []string{"merge", "confirm", "--handle", "+14155550111", "--email", "one@example.com", "--yes", "--json"},
		},
		{
			name: "merge_reject", schema: "mora.merge.reject", wantDecision: "reject",
			args: []string{"merge", "reject", "--handle", "+14155550222", "--email", "two@example.com", "--json"},
		},
		{
			name: "teach_identity_confirm", schema: "mora.teach.identity.confirm", wantDecision: "confirm",
			args: []string{"teach", "identity", "confirm", "--handle", "+14155550333", "--email", "three@example.com", "--yes", "--json"},
		},
		{
			name: "teach_identity_reject", schema: "mora.teach.identity.reject", wantDecision: "reject",
			args: []string{"teach", "identity", "reject", "--handle", "+14155550444", "--email", "four@example.com", "--json"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, _, err := runSplit(t, tc.args...)
			if err != nil {
				t.Fatalf("%s returned error: %v", tc.name, err)
			}
			var receipt struct {
				Schema        string   `json:"schema"`
				SchemaVersion int      `json:"schema_version"`
				Decision      string   `json:"decision"`
				EntryID       string   `json:"entry_id"`
				Affected      []string `json:"affected_items"`
			}
			if err := json.Unmarshal([]byte(stdout), &receipt); err != nil {
				t.Fatalf("%s stdout is not one JSON receipt: %q: %v", tc.name, stdout, err)
			}
			if receipt.Schema != tc.schema || receipt.SchemaVersion != 1 {
				t.Fatalf("%s envelope = %q v%d; want %q v1", tc.name, receipt.Schema, receipt.SchemaVersion, tc.schema)
			}
			if receipt.Decision != tc.wantDecision || receipt.EntryID == "" {
				t.Fatalf("%s receipt = %+v", tc.name, receipt)
			}
			if receipt.Affected == nil {
				t.Fatalf("%s affected_items must be [] rather than null", tc.name)
			}
			if strings.Contains(stdout, "Review before confirming") {
				t.Fatalf("%s leaked the human review block into the machine stream: %q", tc.name, stdout)
			}
		})
	}
}

// TestBriefCorrectJSONReceipt checks that a citation correction reports its
// source-native atoms, the same key the governance ledger stores.
func TestBriefCorrectJSONReceipt(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	written := run(t, "write", "--title", "cited line", "--text", "attendee attribution seed", "--json")
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(written), &created); err != nil || created.ID == "" {
		t.Fatalf("seed write receipt = %q, %v", written, err)
	}
	stdout, _, err := runSplit(t, "brief", "correct", "--memory-id", created.ID,
		"--attendee", "sam@example.com", "--confirm", "--json")
	if err != nil {
		t.Fatalf("brief correct --json returned error: %v", err)
	}
	var receipt struct {
		Schema        string `json:"schema"`
		SchemaVersion int    `json:"schema_version"`
		Decision      string `json:"decision"`
		MemoryID      string `json:"memory_id"`
		AttendeeAtom  string `json:"attendee_atom"`
		EntryID       string `json:"entry_id"`
	}
	if err := json.Unmarshal([]byte(stdout), &receipt); err != nil {
		t.Fatalf("brief correct stdout is not one JSON receipt: %q: %v", stdout, err)
	}
	if receipt.Schema != "mora.brief.correct" || receipt.SchemaVersion != 1 {
		t.Fatalf("envelope = %q v%d; want mora.brief.correct v1", receipt.Schema, receipt.SchemaVersion)
	}
	if receipt.Decision != "confirm" || receipt.MemoryID != created.ID ||
		receipt.AttendeeAtom != "sam@example.com" || receipt.EntryID == "" {
		t.Fatalf("brief correct receipt = %+v", receipt)
	}
}

// TestContractDashLedPositionalsAreRefused pins the mutation guard: a bare
// --json used to land in the positional slot, so `tasks add --json` created a
// task called "--json" and `loop begin --json` started a run for a loop of that
// name. A machine caller asking for JSON must never mutate state instead.
func TestContractDashLedPositionalsAreRefused(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	for _, args := range [][]string{
		{"tasks", "add", "--json"},
		{"tasks", "done", "--json"},
		{"loop", "begin", "--json"},
		{"loop", "heartbeat", "--json"},
		{"loop", "done", "--json"},
		{"loop", "status", "--json"},
		{"loop", "register", "--json"},
	} {
		stdout, _, err := runSplit(t, args...)
		if err == nil {
			t.Fatalf("%q accepted a flag as its positional argument", args)
		}
		if !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("%q error = %v; want a usage error", args, err)
		}
		if strings.TrimSpace(stdout) != "" {
			t.Fatalf("%q wrote to stdout on a usage error: %q", args, stdout)
		}
	}
	stdout, _, err := runSplit(t, "tasks", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "--json") {
		t.Fatalf("a flag was stored as a task name: %q", stdout)
	}
	stdout, _, err = runSplit(t, "loop", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "--json") {
		t.Fatalf("a flag was stored as a loop id: %q", stdout)
	}
}

// TestContractTrailingArgumentsAreRefused pins the other half of the dash-led
// guard. Go's flag package stops at the first non-flag argument and parks the
// rest in Args(); left unchecked, `tasks done a b --json junk` closed "a b" and
// discarded "junk" without a word — mutating on a name the caller never
// finished spelling.
func TestContractTrailingArgumentsAreRefused(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "tasks", "add", "alpha beta")

	for _, args := range [][]string{
		{"tasks", "done", "alpha", "beta", "--json", "junk"},
		{"tasks", "done", "alpha beta", "--", "gamma delta"},
		{"tasks", "add", "gamma", "delta"},
	} {
		stdout, _, err := runSplit(t, args...)
		if err == nil {
			t.Fatalf("%q silently discarded a trailing argument", args)
		}
		if !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("%q error = %v; want a usage error", args, err)
		}
		if strings.TrimSpace(stdout) != "" {
			t.Fatalf("%q wrote to stdout on a usage error: %q", args, stdout)
		}
	}
}

// TestContractDashLedNamesStayAddressable checks the escape hatch the dash-led
// guard needs to stay honest: refusing a flag in the name slot must not make a
// legitimately dash-led task name unreachable.
func TestContractDashLedNamesStayAddressable(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "tasks", "add", "--", "-urgent")

	if listed := run(t, "tasks", "list"); !strings.Contains(listed, "-urgent") {
		t.Fatalf("`tasks add -- -urgent` did not store the dash-led name:\n%s", listed)
	}

	stdout, _, err := runSplit(t, "tasks", "done", "--json", "--", "-urgent")
	if err != nil {
		t.Fatalf("`tasks done --json -- -urgent`: %v", err)
	}
	var receipt struct {
		Schema        string `json:"schema"`
		SchemaVersion int    `json:"schema_version"`
		Task          string `json:"task"`
		RowsUpdated   int    `json:"rows_updated"`
	}
	if uerr := json.Unmarshal([]byte(stdout), &receipt); uerr != nil {
		t.Fatalf("stdout is not exactly one JSON document: %v\nstdout: %q", uerr, stdout)
	}
	if receipt.Schema != "mora.tasks.done" || receipt.SchemaVersion != 1 {
		t.Fatalf("envelope = %q v%d; want mora.tasks.done v1", receipt.Schema, receipt.SchemaVersion)
	}
	if receipt.Task != "-urgent" || receipt.RowsUpdated != 1 {
		t.Fatalf("receipt = %+v; want one row closed for -urgent", receipt)
	}
}

// TestContractIngestRunReceiptSurvivesFailure pins CON-02 on the path that ends
// in an error after work already landed. A partial named-source ingest has
// written memories to the vault, so returning only the error handed an agent
// exit 1 and an empty stdout — the one outcome it cannot act on.
func TestContractIngestRunReceiptSurvivesFailure(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	cfg := mustConfig(t)
	origFn := ingestSourceFn
	t.Cleanup(func() { ingestSourceFn = origFn })
	// Partial run: three items land, then the connector fails.
	ingestSourceFn = func(_ Config, _ Source, _ io.Writer) (int, error) {
		return 3, errString("kaboom")
	}
	if err := saveSources(cfg, []Source{
		{Name: "boom", Type: "filesystem", Scope: "personal", Enabled: ptr(true), CreatedAt: nowRFC3339()},
	}); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := runSplit(t, "ingest", "run", "--source", "boom", "--json")
	if err == nil {
		t.Fatal("a failing named source must still surface its error")
	}
	if !strings.Contains(err.Error(), "kaboom") {
		t.Fatalf("error = %v; want the connector failure to survive", err)
	}
	var receipt struct {
		Schema        string `json:"schema"`
		SchemaVersion int    `json:"schema_version"`
		Source        string `json:"source"`
		Items         int    `json:"items"`
		FailedSources int    `json:"failed_sources"`
	}
	if uerr := json.Unmarshal([]byte(stdout), &receipt); uerr != nil {
		t.Fatalf("stdout is not exactly one JSON document: %v\nstdout: %q", uerr, stdout)
	}
	if receipt.Schema != "mora.ingest.run" || receipt.SchemaVersion != 1 {
		t.Fatalf("envelope = %q v%d; want mora.ingest.run v1", receipt.Schema, receipt.SchemaVersion)
	}
	if receipt.Source != "boom" || receipt.Items != 3 || receipt.FailedSources != 1 {
		t.Fatalf("receipt = %+v; want the 3 landed items and one failed source", receipt)
	}

	// Human mode is unchanged: the success-only prose line still does not print
	// on a failing run.
	humanOut, _, herr := runSplit(t, "ingest", "run", "--source", "boom")
	if herr == nil {
		t.Fatal("human-mode failing ingest must error too")
	}
	if strings.Contains(humanOut, "ingested ") {
		t.Fatalf("human stdout gained a success line on a failing run: %q", humanOut)
	}
}

// contractJSONDefectStatus names every baseline classification that means a
// command path has no usable --json contract: prose instead of a document, an
// undefined-flag error, a JSON null, or prose with a document appended.
var contractJSONDefectStatus = map[string]bool{
	"prose":       true,
	"flag_error":  true,
	"parses_null": true,
	"mixed":       true,
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

	// The escape hatch closes here. Plans 03 and 04 skipped whatever the
	// committed baseline classified as an unfinished contract; every such path
	// now has one, so a defect classification is a failure rather than a skip.
	// A new one can only appear by regressing a shipped contract.
	var defects []string
	for _, row := range loadCLIRegistry(t).Commands {
		if row.Platform != "all" || row.JSONContract == "exempt" || contractBaselineNonExecutable[row.Path] != "" {
			continue
		}
		status, ok := statusByPath[row.Path]
		if !ok {
			t.Fatalf("%s has no committed baseline classification", row.Path)
		}
		if contractJSONDefectStatus[status] {
			defects = append(defects, row.Path+" ("+status+")")
		}
	}
	if len(defects) > 0 {
		sort.Strings(defects)
		t.Fatalf("committed JSON baseline still classifies %d executable path(s) as an unfinished contract: %s",
			len(defects), strings.Join(defects, ", "))
	}

	for _, row := range loadCLIRegistry(t).Commands {
		row := row
		if row.Platform != "all" || row.JSONContract == "exempt" || contractBaselineNonExecutable[row.Path] != "" {
			continue
		}
		t.Run(strings.ReplaceAll(row.Path, " ", "/"), func(t *testing.T) {
			args := append(strings.Fields(row.Path), "--json")
			stdout, _, _ := runSplit(t, args...)
			if trimmed := strings.TrimSpace(stdout); trimmed != "" && !json.Valid([]byte(trimmed)) {
				t.Fatalf("stdout is not one JSON document: %q", stdout)
			}
		})
	}
}

func TestContractWriteJSONDegradedIndexKeepsStdoutPure(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	healthyStdout, healthyStderr, err := runSplit(t, "write", "--title", "indexed stream seed", "--text", "seed the initial index", "--json")
	if err != nil {
		t.Fatalf("healthy write returned error: %v", err)
	}
	if healthyStderr != "" {
		t.Fatalf("healthy write emitted stderr: %q", healthyStderr)
	}
	var healthy struct {
		Schema        string `json:"schema"`
		SchemaVersion int    `json:"schema_version"`
		ID            string `json:"id"`
		Path          string `json:"path"`
		Scope         string `json:"scope"`
		Type          string `json:"type"`
		Title         string `json:"title"`
		IndexUpdated  bool   `json:"index_updated"`
	}
	if err := json.Unmarshal([]byte(healthyStdout), &healthy); err != nil {
		t.Fatalf("healthy write stdout is not a receipt: %v", err)
	}
	if healthy.Schema != "mora.write" || healthy.SchemaVersion != 1 || healthy.ID == "" || healthy.Path == "" || healthy.Scope == "" || healthy.Type == "" || healthy.Title == "" || !healthy.IndexUpdated {
		t.Fatalf("healthy write receipt = %+v", healthy)
	}
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
	var degraded struct {
		Schema       string `json:"schema"`
		IndexUpdated bool   `json:"index_updated"`
	}
	if err := json.Unmarshal([]byte(stdout), &degraded); err != nil {
		t.Fatalf("degraded write stdout is not a receipt: %v", err)
	}
	if degraded.Schema != "mora.write" || degraded.IndexUpdated {
		t.Fatalf("degraded write receipt = %+v, want index_updated=false", degraded)
	}
	const warning = "warning: memory saved but the search index was not updated"
	if strings.Contains(stdout, warning) {
		t.Fatalf("degraded-write warning leaked to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, warning) {
		t.Fatalf("degraded-write warning missing from stderr: %q", stderr)
	}
}
