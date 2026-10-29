package mora

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

func TestMCPProposalShowFullContentReadOnly(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	run(t, "config", "mcp-write-policy", "propose")
	cfg := mustConfig(t)
	marker := filepath.Join(t.TempDir(), "must-not-execute")
	text := strings.Repeat("Full correction content. ", 40) + "\n$(touch " + marker + ")\n\x1b[2J\u202e"
	args := map[string]any{"scope": "project:demo", "type": "decision", "title": "Use pnpm", "source": "fixture", "text": text}
	result, err := callMCPTool(testCtx(t), "write_memory", args)
	if err != nil {
		t.Fatal(err)
	}
	id := result.(map[string]any)["proposal"].(map[string]any)["id"].(string)
	before, err := os.ReadFile(filepath.Join(mcpProposalDir(cfg), id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := proposalInspectionSnapshot(t, cfg)
	for _, jsonOut := range []bool{false, true} {
		var out bytes.Buffer
		command := []string{"mcp", "proposals", "show", id}
		if jsonOut {
			command = append(command, "--json")
		}
		if err := Run(testCtx(t), command, &out, testStderr, strings.NewReader("")); err != nil {
			t.Fatal(err)
		}
		var receipt struct {
			Schema     string `json:"schema"`
			Version    int    `json:"schema_version"`
			Inspection struct {
				ID        string         `json:"id"`
				Scope     string         `json:"effective_scope"`
				Type      string         `json:"effective_type"`
				Source    string         `json:"effective_source"`
				Status    string         `json:"status"`
				Arguments map[string]any `json:"arguments"`
				Redacted  bool           `json:"redacted"`
			} `json:"inspection"`
		}
		if err := json.Unmarshal(out.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.Schema != "mora.mcp.proposals" || receipt.Version != 1 || receipt.Inspection.ID != id || receipt.Inspection.Scope != "project:demo" || receipt.Inspection.Type != "decision" || receipt.Inspection.Source != "fixture" || receipt.Inspection.Status != "pending" || receipt.Inspection.Redacted || !reflect.DeepEqual(receipt.Inspection.Arguments, args) {
			t.Fatalf("wrong inspection: %+v", receipt)
		}
		for _, r := range out.String() {
			if r == '\x1b' || r == '\u202e' {
				t.Fatalf("active terminal control in output: %q", out.String())
			}
		}
	}
	if after := proposalInspectionSnapshot(t, cfg); !reflect.DeepEqual(snapshot, after) {
		t.Fatal("inspection changed config, state, data, or vault files")
	}
	after, err := os.ReadFile(filepath.Join(mcpProposalDir(cfg), id+".json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("inspection changed proposal: %v", err)
	}
	files, err := allMemoryFiles(cfg)
	if err != nil || len(files) != 0 {
		t.Fatalf("inspection published memory: %v, %v", files, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("proposal content executed: %v", err)
	}
}

func TestMCPProposalShowRejectsInvalidOrMissingInput(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	for _, id := range []string{"p_missing", "../../private", "p_a/b", "p_a\\b", "--help"} {
		var out bytes.Buffer
		if err := cmdMCP(testCtx(t), []string{"proposals", "show", id}, &out, testStderr, strings.NewReader("")); err == nil || out.Len() != 0 {
			t.Fatalf("accepted %q: %s, %v", id, &out, err)
		}
	}
	if _, err := os.Stat(mcpProposalDir(cfg)); !os.IsNotExist(err) {
		t.Fatalf("inspection created proposal directory: %v", err)
	}
	if err := os.MkdirAll(mcpProposalDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	for id, body := range map[string]string{"p_bad": "{", "p_wrongid": `{"id":"p_other","proposed_at":"2026-10-29T00:00:00Z","arguments":{"title":"x","text":"y"}}`, "p_invalid": `{"id":"p_invalid","proposed_at":"2026-10-29T00:00:00Z","arguments":{"title":"x"}}`} {
		path := filepath.Join(mcpProposalDir(cfg), id+".json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := cmdMCP(testCtx(t), []string{"proposals", "show", id}, &out, testStderr, strings.NewReader("")); err == nil || out.Len() != 0 {
			t.Fatalf("accepted corrupt %q: %s, %v", id, &out, err)
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != body {
			t.Fatalf("changed corrupt proposal: %v", err)
		}
	}
}

func TestMCPProposalShowMasksCredentialPatterns(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	secret := "sk-" + strings.Repeat("X", 32)
	body := "api_key=fixture-sensitive-value\nAuthorization: Bearer fixture-sensitive-bearer\n" + secret + "\nhttps://user:fixture-password@example.invalid/path\n-----BEGIN PRIVATE KEY-----\nfixture-private-key\n-----END PRIVATE KEY-----"
	args := map[string]any{"title": "Fixture " + secret, "scope": "project:" + secret, "source": "Bearer fixture-source-sensitive", "text": body, "password": "fixture-password-field", "nested": map[string]any{"token": "fixture-token-field"}}
	p := mcpWriteProposal{ID: "p_redacted", ProposedAt: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC).Format(time.RFC3339), Arguments: args}
	path, err := mcppkg.SaveProposal(cfg, p)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := cmdMCP(testCtx(t), []string{"proposals", "show", p.ID, "--json"}, &out, testStderr, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Inspection struct {
			Redacted  bool           `json:"redacted"`
			Notice    string         `json:"notice"`
			Scope     string         `json:"effective_scope"`
			Source    string         `json:"effective_source"`
			Arguments map[string]any `json:"arguments"`
		} `json:"inspection"`
	}
	if err := json.Unmarshal(out.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if !receipt.Inspection.Redacted || receipt.Inspection.Notice == "" {
		t.Fatal("missing redaction indicator")
	}
	rendered, err := json.Marshal(receipt.Inspection)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{secret, "fixture-sensitive-value", "fixture-sensitive-bearer", "fixture-password", "fixture-private-key", "fixture-token-field", "fixture-source-sensitive"} {
		if bytes.Contains(rendered, []byte(needle)) {
			t.Fatalf("credential pattern leaked: %q", needle)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("redaction changed stored proposal: %v", err)
	}
}

func proposalInspectionSnapshot(t *testing.T, cfg Config) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	for _, root := range []string{cfg.ConfigDir, cfg.DataDir, cfg.StateDir, cfg.VaultDir} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if entry.IsDir() {
				snapshot[path] = info.Mode().String()
				return nil
			}
			if info.Mode()&os.ModeSymlink != 0 {
				link, err := os.Readlink(path)
				if err != nil {
					return err
				}
				snapshot[path] = "link:" + link
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			snapshot[path] = fmt.Sprintf("%s:%x", info.Mode(), sha256.Sum256(body))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return snapshot
}

func TestMCPProposalShowRefusesUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"directory-symlink", "file-symlink", "file-directory", "oversize"} {
		subRun(t, kind, func(t *testing.T) {
			withTempHome(t)
			cfg := mustConfig(t)
			root := mcpProposalDir(cfg)
			path := filepath.Join(root, "p_fixture.json")
			foreign := t.TempDir()
			body := []byte(`{"id":"p_fixture","proposed_at":"2026-10-29T00:00:00Z","arguments":{"title":"PRIVATE_FOREIGN","text":"PRIVATE_FOREIGN"}}`)
			foreignFile := filepath.Join(foreign, "p_fixture.json")
			if err := os.WriteFile(foreignFile, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if kind == "directory-symlink" {
				if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(foreign, root); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			} else {
				if err := os.MkdirAll(root, 0o700); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "file-symlink":
					if err := os.Symlink(foreignFile, path); err != nil {
						t.Skipf("symlinks unavailable: %v", err)
					}
				case "file-directory":
					if err := os.Mkdir(path, 0o700); err != nil {
						t.Fatal(err)
					}
				case "oversize":
					f, err := os.Create(path)
					if err != nil {
						t.Fatal(err)
					}
					if err := f.Truncate(maxProposalInspectionBytes + 1); err != nil {
						_ = f.Close()
						t.Fatal(err)
					}
					if err := f.Close(); err != nil {
						t.Fatal(err)
					}
				}
			}
			var out bytes.Buffer
			if err := cmdMCP(testCtx(t), []string{"proposals", "show", "p_fixture"}, &out, testStderr, strings.NewReader("")); err == nil || out.Len() != 0 {
				t.Fatalf("unsafe file accepted: %s, %v", &out, err)
			}
			after, err := os.ReadFile(foreignFile)
			if err != nil || !bytes.Equal(after, body) {
				t.Fatalf("foreign file changed: %v", err)
			}
		})
	}
}

func TestMCPProposalShowEffectiveDefaults(t *testing.T) {
	withTempHome(t)
	cfg := mustConfig(t)
	p := mcpWriteProposal{ID: "p_defaults", ProposedAt: "2026-10-29T00:00:00Z", Arguments: map[string]any{"title": "Fixture", "text": "A plain correction"}}
	if _, err := mcppkg.SaveProposal(cfg, p); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := cmdMCP(testCtx(t), []string{"proposals", "show", p.ID, "--json"}, &out, testStderr, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Inspection struct {
			Scope  string `json:"effective_scope"`
			Type   string `json:"effective_type"`
			Source string `json:"effective_source"`
		} `json:"inspection"`
	}
	if err := json.Unmarshal(out.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Inspection.Scope != "global" || receipt.Inspection.Type != "insight" || receipt.Inspection.Source != "mcp" {
		t.Fatalf("missing effective defaults: %+v", receipt)
	}
}

func TestProposalCredentialMaskingConsumesEscapedQuotes(t *testing.T) {
	for _, text := range []string{
		`password="prefix\"SENSITIVE_SUFFIX"`,
		`api_key='prefix\'SENSITIVE_SUFFIX'`,
		`{"password":"prefix\\\"SENSITIVE_SUFFIX"}`,
		"password=\"prefix\\\nSENSITIVE_SUFFIX\"",
	} {
		masked, changed := redactProposalText(text)
		if !changed || strings.Contains(masked, "SENSITIVE_SUFFIX") {
			t.Fatalf("escaped credential suffix survived masking: %q", masked)
		}
	}
}
