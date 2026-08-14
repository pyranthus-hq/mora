package mcp

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/config"
)

func TestProposalPathValidation(t *testing.T) {
	cfg := config.Config{ConfigDir: t.TempDir()}
	got, err := ProposalPath(cfg, "p_abc")
	if err != nil || got != filepath.Join(cfg.ConfigDir, "mcp-proposals", "p_abc.json") {
		t.Fatalf("path=(%q,%v)", got, err)
	}
	for _, id := range []string{"", "abc", "p_a/b", "p_a\\b", "../p_a"} {
		if _, err := ProposalPath(cfg, id); err == nil {
			t.Errorf("invalid %q accepted", id)
		}
	}
}
func TestSaveReadProposalExactBytesAndMode(t *testing.T) {
	cfg := config.Config{ConfigDir: t.TempDir()}
	proposal := Proposal{ID: "p_abc", ProposedAt: "2026-08-13T01:02:03Z", Arguments: map[string]any{"title": "Decision", "count": float64(2)}}
	path, err := SaveProposal(cfg, proposal)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n  \"id\": \"p_abc\",\n  \"proposed_at\": \"2026-08-13T01:02:03Z\",\n  \"arguments\": {\n    \"count\": 2,\n    \"title\": \"Decision\"\n  }\n}\n"
	if string(body) != want {
		t.Fatalf("body=%q", body)
	}
	st, _ := os.Stat(path)
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%#o", st.Mode().Perm())
	}
	got, gotPath, err := ReadProposal(cfg, "p_abc")
	if err != nil || gotPath != path || !reflect.DeepEqual(got, proposal) {
		t.Fatalf("read=(%+v,%q,%v)", got, gotPath, err)
	}
}
func TestReadProposalErrors(t *testing.T) {
	cfg := config.Config{ConfigDir: t.TempDir()}
	if _, _, err := ReadProposal(cfg, "p_missing"); err == nil || !strings.Contains(err.Error(), `MCP proposal "p_missing" not found`) {
		t.Fatalf("missing=%v", err)
	}
	dir := ProposalDir(cfg)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "p_bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadProposal(cfg, "p_bad"); err == nil || !strings.Contains(err.Error(), "parse MCP proposal") {
		t.Fatalf("parse=%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "p_file.json"), []byte(`{"id":"p_other"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadProposal(cfg, "p_file"); err == nil || !strings.Contains(err.Error(), "id mismatch") {
		t.Fatalf("mismatch=%v", err)
	}
}
func TestListProposalsDeterministicAndStrict(t *testing.T) {
	cfg := config.Config{ConfigDir: t.TempDir()}
	got, err := ListProposals(cfg)
	if err != nil || got != nil {
		t.Fatalf("missing=(%v,%v)", got, err)
	}
	for _, p := range []Proposal{{ID: "p_z", ProposedAt: "2026-02-01T00:00:00Z"}, {ID: "p_b", ProposedAt: "2026-01-01T00:00:00Z"}, {ID: "p_a", ProposedAt: "2026-01-01T00:00:00Z"}} {
		if _, err := SaveProposal(cfg, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ProposalDir(cfg), "ignore.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = ListProposals(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{got[0].ID, got[1].ID, got[2].ID}
	if !reflect.DeepEqual(ids, []string{"p_a", "p_b", "p_z"}) {
		t.Fatalf("ids=%v", ids)
	}
}
