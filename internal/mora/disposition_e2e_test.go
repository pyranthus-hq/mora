package mora

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDispositionCLIFourValuesPersistAndReadImmediately(t *testing.T) {
	for _, value := range []string{"not-context", "keep", "done", "outdated"} {
		t.Run(value, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			cfg := mustConfig(t)
			target := Memory{ID: "target", Scope: "global", Type: "task", Source: "manual", Title: "target", Text: "dispositionprobe", CreatedAt: time.Now().Add(-time.Hour).Format(time.RFC3339)}
			if err := writeMemory(cfg, target); err != nil {
				t.Fatal(err)
			}
			mustRebuild(t, cfg)
			var out bytes.Buffer
			if err := cmdWrite(testCtx(t), []string{"--title", "explicit correction", "--text", "owner choice", "--target", "target", "--disposition", value, "--json"}, &out, &out); err != nil {
				t.Fatal(err)
			}
			rows, err := dispositionListedRows(t)
			if err != nil {
				t.Fatal(err)
			}
			correctionFound, targetFound := false, false
			for _, m := range rows {
				if m.Type == "correction" {
					persisted, err := findMemory(cfg, m.ID)
					if err != nil {
						t.Fatal(err)
					}
					correctionFound = persisted.Meta["target"] == "target" && persisted.Meta["disposition"] == value
				}
				if m.ID == "target" {
					targetFound = true
					if m.Disposition == nil || m.Disposition.Value != value {
						t.Fatalf("read-after-write annotation %+v", m)
					}
				}
			}
			if !correctionFound || !targetFound {
				t.Fatal("correction missing or target hidden")
			}
			result := filtersStructured(t, "search_memory", `{"query":"dispositionprobe","limit":10}`)
			ids := filterResultIDs(t, result["results"].([]any))
			if !containsID(ids, "target") {
				t.Fatal("disposition silently hid target", ids)
			}
		})
	}
}

func TestDispositionCLIInvalidFieldsAreTypedUsageErrors(t *testing.T) {
	for _, extra := range [][]string{{"--disposition", "bogus", "--target", "target"}, {"--disposition", "keep"}, {"--target", ""}, {"--target", "target", "--disposition", ""}, {"--target", "target", "--type", "insight"}} {
		var out bytes.Buffer
		err := cmdWrite(testCtx(t), append([]string{"--title", "test", "--text", "test"}, extra...), &out, &out)
		var typed moraError
		if !errors.As(err, &typed) || typed.Code != errCodeUsageUnknownValue {
			t.Fatalf("%v => %#v, expected typed usage", extra, err)
		}
	}
}

func TestDispositionMCPProposalAuthorityAndApprovalRevalidation(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "approve", true: "deleted-target"}[deleted], func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			cfg := mustConfig(t)
			target := Memory{ID: "target", Scope: "global", Type: "insight", Source: "manual", Title: "target", Text: "target body", CreatedAt: time.Now().Add(-time.Hour).Format(time.RFC3339)}
			if err := writeMemory(cfg, target); err != nil {
				t.Fatal(err)
			}
			mustRebuild(t, cfg)
			cfg.MCPWritePolicy = mcpWritePolicyPropose
			if err := writeConfig(cfg); err != nil {
				t.Fatal(err)
			}
			got, err := callMCPTool(testCtx(t), "write_memory", map[string]any{"title": "correction", "text": "owner verdict", "target": "target", "disposition": "not-context"})
			if err != nil {
				t.Fatal(err)
			}
			proposal := got.(map[string]any)["proposal"].(map[string]any)
			id := proposal["id"].(string)
			rows, err := dispositionListedRows(t)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Disposition != nil {
				t.Fatal("pending correction applied", rows)
			}
			if deleted {
				target.DeletedAt = time.Now().Format(time.RFC3339)
				if err := writeMemory(cfg, target); err != nil {
					t.Fatal(err)
				}
			}
			var out bytes.Buffer
			err = cmdMCP(testCtx(t), []string{"proposals", "approve", id}, &out, &out, strings.NewReader(""))
			if deleted {
				if err == nil {
					t.Fatal("approved against deleted target")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			rows, err = dispositionListedRows(t)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, m := range rows {
				if m.ID == "target" && m.Disposition != nil && m.Disposition.Value == "not-context" {
					found = true
				}
			}
			if !found {
				t.Fatal("approved correction not immediately projected", rows)
			}
			cfg.MCPWritePolicy = mcpWritePolicyReadonly
			if err := writeConfig(cfg); err != nil {
				t.Fatal(err)
			}
			if _, err := callMCPTool(testCtx(t), "write_memory", map[string]any{"title": "correction", "text": "owner verdict", "target": "target", "disposition": "keep"}); err == nil {
				t.Fatal("readonly allowed typed correction")
			}
		})
	}
}

func dispositionListedRows(t *testing.T) ([]Memory, error) {
	t.Helper()
	result := filtersStructured(t, "list_memory", `{"scope":"global","limit":20}`)
	encoded, err := json.Marshal(result["memories"])
	if err != nil {
		return nil, err
	}
	var rows []Memory
	err = json.Unmarshal(encoded, &rows)
	return rows, err
}

func TestDispositionListVariantContract(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	seedRecencyVault(t,
		Memory{ID: "target", Scope: "global", Type: "insight", Source: "manual", Title: "target", Text: "body", CreatedAt: "2026-09-01T00:00:00Z"},
		Memory{ID: "correction", Scope: "global", Type: "correction", Source: "manual", Title: "correction", Text: "explicit owner choice", CreatedAt: "2026-09-02T00:00:00Z", Meta: map[string]any{"target": "target", "disposition": "not-context"}})
	old := briefClock
	briefClock = func() time.Time { return now }
	t.Cleanup(func() { briefClock = old })
	raw := run(t, "list", "--limit", "20", "--json")
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	for _, row := range got["memories"].([]any) {
		row.(map[string]any)["path"] = "<path>"
	}
	path := filepath.Join("testdata", "contracts", "variants", "mora.list.disposition.json")
	if os.Getenv("MORA_UPDATE_ACTIVITY_GOLDENS") == "1" {
		data, _ := json.MarshalIndent(got, "", "  ")
		if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("disposition contract drift; regeneration requires Golden change reason: %s", raw)
	}
}
