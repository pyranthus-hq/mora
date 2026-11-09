package mora

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestDeleteConnectorRefusal(t *testing.T) {
	for _, via := range []string{"CLI", "MCP"} {
		t.Run(via, func(t *testing.T) {
			coreBMcpInit(t)
			cfg, err := loadConfigFor(testCtx(t))
			if err != nil {
				t.Fatal(err)
			}
			id := "gmail_thread/delete-refusal"
			if err := writeMappedMemory(cfg, memory.MappedMemory{
				StableID: id, Provider: "gmail", ProviderID: "delete-refusal",
				Scope: "global", Type: "email", Title: "Connector evidence",
				Body: "Keep this message", CreatedAt: nowRFC3339(),
			}); err != nil {
				t.Fatal(err)
			}
			m, err := findMemory(cfg, id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rebuildIndex(testCtx(t), cfg); err != nil {
				t.Fatal(err)
			}
			if via == "CLI" {
				err = Run(testCtx(t), []string{"delete", id, "--yes"}, io.Discard, io.Discard, strings.NewReader(""))
			} else {
				_, err = callMCPTool(testCtx(t), "delete_memory", map[string]any{"id": id})
			}
			// AC-001.1: CLI refuses connector evidence.
			// AC-002.1: MCP refuses and points to the lasting suppression command.
			if err == nil {
				t.Fatal("connector delete succeeded")
			}
			// AC-001.2: Explain the connector origin, restoration risk and remedy.
			for _, want := range []string{"came from a connector", "later sync can restore it", "mora forget --chat " + id + " --yes"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q missing %q", err, want)
				}
			}
			// AC-001.3: Refused CLI delete preserves the file.
			// AC-002.2: Refused MCP delete preserves the file and stages no operation.
			if _, err := os.Stat(m.Path); err != nil {
				t.Fatalf("connector file: %v", err)
			}
			ops, err := listPendingOps(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(ops) != 0 {
				t.Fatalf("pending operations after refusal: %v", ops)
			}
		})
	}
}

func TestDeleteAuthoredStillSucceeds(t *testing.T) {
	for _, via := range []string{"CLI", "MCP"} {
		t.Run(via, func(t *testing.T) {
			coreBMcpInit(t)
			written, err := callMCPTool(testCtx(t), "write_memory", map[string]any{"title": "Authored", "text": "delete me"})
			if err != nil {
				t.Fatal(err)
			}
			m := written.(map[string]any)["memory"].(Memory)
			cfg, err := loadConfigFor(testCtx(t))
			if err != nil {
				t.Fatal(err)
			}
			m, err = findMemory(cfg, m.ID)
			if err != nil {
				t.Fatal(err)
			}
			if m.Provider != "" {
				t.Fatalf("authored provider = %q", m.Provider)
			}
			// AC-001.4: CLI still deletes authored memories.
			// AC-002.3: MCP retains its authored-memory happy path.
			if via == "CLI" {
				err = Run(testCtx(t), []string{"delete", m.ID, "--yes"}, io.Discard, io.Discard, strings.NewReader(""))
			} else {
				var got any
				got, err = callMCPTool(testCtx(t), "delete_memory", map[string]any{"id": m.ID})
				if err == nil && got.(map[string]any)["deleted"] != m.ID {
					t.Fatalf("delete result: %v", got)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(m.Path); !os.IsNotExist(err) {
				t.Fatalf("authored file remains: %v", err)
			}
		})
	}
}
