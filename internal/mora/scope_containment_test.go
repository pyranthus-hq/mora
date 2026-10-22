package mora

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

// Existing fixtures need a string path; publication code must handle the error.
func mustMemoryPath(t testing.TB, cfg Config, m Memory) string {
	t.Helper()
	path, err := memoryPath(cfg, m)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func assertNoOutsideFiles(t *testing.T, root string, allowed ...string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		for _, dir := range allowed {
			rel, err := filepath.Rel(dir, path)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel) {
				return nil
			}
		}
		t.Errorf("unexpected file outside allowed trees: %s", path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWriteScopeEntryPoints(t *testing.T) {
	for _, scope := range []string{"", "../../outside", "..:..:outside", "project:..:..:..:outside", `..\..\outside`} {
		for _, entry := range []string{"cli", "mcp-open", "mcp-propose", "http-open", "http-propose", "replace"} {
			if scope == "" && strings.HasPrefix(entry, "http-") {
				continue // HTTP convenience translation treats empty fields as omitted.
			}
			t.Run(entry+"/"+scope, func(t *testing.T) {
				// All filesystem destinations, even on vulnerable code, fit within this root.
				withTempHome(t)
				run(t, "init")
				cfg := mustConfig(t)
				root := lookupTestEnv(t).home
				before := scopeVaultTree(t, cfg.VaultDir)
				policy := mcpWritePolicyOpen
				if strings.HasSuffix(entry, "propose") {
					policy = mcpWritePolicyPropose
				}
				cfg.MCPWritePolicy = policy
				if err := writeConfig(cfg); err != nil {
					t.Fatal(err)
				}
				args := map[string]any{"scope": scope, "title": "Synthetic", "text": "Synthetic"}
				var err error
				switch entry {
				case "cli":
					var out bytes.Buffer
					err = Run(testCtx(t), []string{"write", "--scope", scope, "--title", "Synthetic", "--text", "Synthetic"}, &out, &out, strings.NewReader(""))
				case "mcp-open", "mcp-propose":
					_, err = callMCPTool(testCtx(t), "write_memory", args)
				case "http-open", "http-propose":
					body, marshalErr := json.Marshal(args)
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					s := &httpServer{token: "synthetic", port: 7777}
					req := httptest.NewRequest(http.MethodPost, "/write", bytes.NewReader(body))
					req.Host = "127.0.0.1:7777"
					req.Header.Set("Authorization", "Bearer synthetic")
					rec := httptest.NewRecorder()
					s.handler(testCtx(t)).ServeHTTP(rec, req)
					if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid write scope") {
						t.Fatalf("HTTP %d: %s", rec.Code, rec.Body)
					}
					err = errors.New(rec.Body.String())
				case "replace":
					err = writeMemory(cfg, Memory{ID: "synthetic", Scope: scope, Type: "insight", Title: "Synthetic", Text: "Synthetic"})
				}
				if err == nil {
					t.Fatal("unsafe scope accepted")
				}
				if !strings.Contains(err.Error(), "invalid write scope") {
					t.Fatalf("wrong error: %v", err)
				}
				if !sameTree(before, scopeVaultTree(t, cfg.VaultDir)) {
					t.Fatal("rejected write changed vault")
				}
				if proposalCount(t, cfg) != 0 {
					t.Fatal("unsafe proposal staged")
				}
				assertNoOutsideFiles(t, root, cfg.VaultDir, cfg.ConfigDir, cfg.StateDir, cfg.DataDir)
			})
		}
	}
}

func TestApprovalRevalidatesStoredScope(t *testing.T) {
	cfg := coreBIngestInitCfg(t)
	p := mcpWriteProposal{ID: "p_synthetic", ProposedAt: time.Now().Format(time.RFC3339), Arguments: map[string]any{"scope": "../../outside", "title": "Synthetic", "text": "Synthetic"}}
	if _, err := mcppkg.SaveProposal(cfg, p); err != nil {
		t.Fatal(err)
	}
	before := scopeVaultTree(t, cfg.VaultDir)
	var out bytes.Buffer
	err := cmdMCPProposals(testCtx(t), []string{"approve", p.ID}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "invalid write scope") {
		t.Fatalf("approval: %v", err)
	}
	if !sameTree(before, scopeVaultTree(t, cfg.VaultDir)) {
		t.Fatal("approval changed vault")
	}
	assertNoOutsideFiles(t, lookupTestEnv(t).home, cfg.VaultDir, cfg.ConfigDir, cfg.StateDir, cfg.DataDir)
}

func TestRetentionCompactionScopeContainment(t *testing.T) {
	for _, scope := range []string{"project:my-app.v2", "team/demo", "../../outside", "project:..:..:..:outside"} {
		t.Run(scope, func(t *testing.T) {
			cfg := coreBIngestInitCfg(t)
			// Seed malformed legacy frontmatter directly, without invoking a writer.
			path := filepath.Join(memoriesRoot(cfg), "synthetic.md")
			raw := "---\nid: synthetic\nscope: " + scope + "\ntype: note\ntitle: Synthetic\ncreated_at: 2024-01-01T00:00:00Z\n---\n\nSynthetic evidence\n"
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
			report, err := buildRetentionReport(cfg, now, 365, 30)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decideRetentionCandidate(cfg, report.ReportID, "synthetic", retentionDecision{Action: "compact", Summary: "Synthetic summary"}); err != nil {
				t.Fatal(err)
			}
			_, err = executeRetentionReport(testCtx(t), cfg, report.ReportID, now)
			if strings.Contains(scope, "..") {
				if err == nil || !strings.Contains(err.Error(), "invalid write scope") {
					t.Fatalf("compaction: %v", err)
				}
				got, readErr := os.ReadFile(path)
				if readErr != nil || string(got) != raw {
					t.Fatalf("original was lost: %v", readErr)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				memories := vaultMemories(t, cfg)
				if len(memories) != 1 || memories[0].Scope != scope || memories[0].Text != "Synthetic summary" {
					t.Fatalf("compaction: %+v", memories)
				}
			}
			assertNoOutsideFiles(t, lookupTestEnv(t).home, cfg.VaultDir, cfg.ConfigDir, cfg.StateDir, cfg.DataDir)
		})
	}
}

func scopeVaultTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = string(body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
