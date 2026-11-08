package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/mcp"
)

func fixture(t *testing.T) (config.Config, batch) {
	t.Helper()
	d := t.TempDir()
	cfg := config.Config{ConfigDir: filepath.Join(d, "config"), StateDir: filepath.Join(d, "state"), VaultDir: filepath.Join(d, "vault"), MCPWritePolicy: "propose"}
	p := remoteProposal{ID: "p_test_1", ProposedAt: "2026-11-08T00:00:00Z", ProposedBy: "grok", Arguments: map[string]any{"scope": "project:mora", "source": "agent:grok", "type": "insight", "title": "fixture", "text": "Synthetic transport check"}}
	return cfg, batch{Version: 1, Proposals: []remoteProposal{p}}
}
func TestRoundtripPendingOnlyAndReplay(t *testing.T) {
	cfg, b := fixture(t)
	p := b.Proposals[0]
	body, _ := json.Marshal(p)
	os.MkdirAll(mcp.ProposalDir(cfg), 0700)
	os.WriteFile(filepath.Join(mcp.ProposalDir(cfg), p.ID+".json"), body, 0600)
	got, err := export(cfg)
	if err != nil || len(got.Proposals) != 1 {
		t.Fatalf("export: %+v %v", got, err)
	}
	local, _ := fixture(t)
	r, err := intake(local, got)
	if err != nil || len(r.Imported) != 1 {
		t.Fatalf("intake: %+v %v", r, err)
	}
	id := r.Imported[0]
	pending, path, err := mcp.ReadProposal(local, id)
	if err != nil || pending.Arguments["source"] != "agent:grok" {
		t.Fatalf("proposal: %+v %v", pending, err)
	}
	if _, err := os.Stat(local.VaultDir); !os.IsNotExist(err) {
		t.Fatal("intake touched vault")
	}
	if r, err = intake(local, got); err != nil || r.Seen != 1 {
		t.Fatalf("replay: %+v %v", r, err)
	}
	// Legacy CLI approval/rejection removes the file without a decision ledger.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if r, err = intake(local, got); err != nil || r.Seen != 1 {
		t.Fatalf("decided replay: %+v %v", r, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("restaged a decided proposal")
	}
	got.Proposals[0].Arguments["text"] = "changed"
	if _, err = intake(local, got); err == nil {
		t.Fatal("accepted changed replay")
	}
}
func TestBoundaryRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*config.Config, *batch)
	}{
		{"readonly", func(c *config.Config, b *batch) { c.MCPWritePolicy = "readonly" }},
		{"unset", func(c *config.Config, b *batch) { c.MCPWritePolicy = "" }},
		{"scope", func(c *config.Config, b *batch) { b.Proposals[0].Arguments["scope"] = "personal" }},
		{"agent", func(c *config.Config, b *batch) { b.Proposals[0].ProposedBy = "muse" }},
		{"source", func(c *config.Config, b *batch) { b.Proposals[0].Arguments["source"] = "user" }},
		{"target", func(c *config.Config, b *batch) { b.Proposals[0].Arguments["target"] = "mem_existing" }},
		{"traversal", func(c *config.Config, b *batch) { b.Proposals[0].ID = "p_../../escape" }},
		{"oversized", func(c *config.Config, b *batch) { b.Proposals[0].Arguments["text"] = strings.Repeat("x", 32769) }},
		{"nonstring", func(c *config.Config, b *batch) { b.Proposals[0].Arguments["title"] = 42 }},
		{"duplicate", func(c *config.Config, b *batch) { b.Proposals = append(b.Proposals, b.Proposals[0]) }},
		{"badlater", func(c *config.Config, b *batch) {
			p := b.Proposals[0]
			p.ID = "../bad"
			b.Proposals = append(b.Proposals, p)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, b := fixture(t)
			tc.mutate(&c, &b)
			if _, err := intake(c, b); err == nil {
				t.Fatal("accepted")
			}
			if _, err := os.Stat(c.StateDir); !os.IsNotExist(err) {
				t.Fatal("invalid batch mutated state")
			}
		})
	}
}
func TestCrashClaimAndExistingDecision(t *testing.T) {
	for _, kind := range []string{"claimed", "decision", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			c, b := fixture(t)
			p := b.Proposals[0]
			id := localID(p.ID)
			if kind == "claimed" {
				body, _ := json.Marshal(p)
				dir := filepath.Join(c.StateDir, "bot-return")
				os.MkdirAll(dir, 0700)
				saveReceipt(filepath.Join(dir, id+".json"), receipt{RemoteID: p.ID, Digest: fmt.Sprintf("%x", sha256.Sum256(body)), Status: "claimed"})
			} else if kind == "decision" {
				path := decisionPath(c, id)
				os.MkdirAll(filepath.Dir(path), 0700)
				os.WriteFile(path, []byte("{}"), 0600)
			} else {
				dir := filepath.Join(c.StateDir, "bot-return")
				os.MkdirAll(dir, 0700)
				os.Symlink("/dev/null", filepath.Join(dir, id+".json"))
			}
			if _, err := intake(c, b); err == nil {
				t.Fatal("unsafe recovery")
			}
			path, _ := mcp.ProposalPath(c, id)
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("staged despite conflict")
			}
		})
	}
}
func TestConcurrentIntake(t *testing.T) {
	c, b := fixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := intake(c, b); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	ps, err := mcp.ListProposals(c)
	if err != nil || len(ps) != 1 {
		t.Fatalf("%d proposals, %v", len(ps), err)
	}
}
func TestStrictEnvelope(t *testing.T) {
	for _, s := range []string{`{"version":1,"proposals":[]} {}`, `{"version":1,"proposals":[],"extra":true}`, `{"version":`} {
		var b batch
		if decode([]byte(s), &b) == nil {
			t.Fatal("accepted malformed envelope")
		}
	}
}

func TestExportSkipsDeliveredAndOtherAgentFormat(t *testing.T) {
	remote, b := fixture(t)
	local, _ := fixture(t)
	os.MkdirAll(mcp.ProposalDir(remote), 0700)
	p := b.Proposals[0]
	body, _ := json.Marshal(p)
	os.WriteFile(filepath.Join(mcp.ProposalDir(remote), p.ID+".json"), body, 0600)
	os.WriteFile(filepath.Join(mcp.ProposalDir(remote), "p_muse.json"), []byte(`{"id":"p_muse","proposed_by":"muse","arguments":{"scope":"project:mora"},"other_field":true}`), 0600)
	if _, err := intake(local, b); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := emitSeen(local, &buf); err != nil {
		t.Fatal(err)
	}
	var seen map[string]string
	json.Unmarshal(buf.Bytes(), &seen)
	got, err := exportSeen(remote, seen)
	if err != nil || len(got.Proposals) != 0 || len(got.Problems) != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	p.Arguments["text"] = "changed upstream"
	body, _ = json.Marshal(p)
	os.WriteFile(filepath.Join(mcp.ProposalDir(remote), p.ID+".json"), body, 0600)
	got, err = exportSeen(remote, seen)
	if err != nil || len(got.Proposals) != 1 {
		t.Fatalf("changed replay hidden: %+v %v", got, err)
	}
}
func TestBadItemDoesNotBlockValidIntake(t *testing.T) {
	c, b := fixture(t)
	if _, err := intake(c, b); err != nil {
		t.Fatal(err)
	}
	b.Proposals[0].Arguments["text"] = "changed"
	_, next := fixture(t)
	next.Proposals[0].ID = "p_second"
	b.Proposals = append(b.Proposals, next.Proposals[0])
	b.Problems = []string{"invalid item upstream"}
	r, err := intake(c, b)
	if err == nil || len(r.Imported) != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	if _, _, err := mcp.ReadProposal(c, localID("p_second")); err != nil {
		t.Fatal(err)
	}
}
func TestExportBatchesPast128(t *testing.T) {
	c, b := fixture(t)
	os.MkdirAll(mcp.ProposalDir(c), 0700)
	for i := 0; i < 130; i++ {
		p := b.Proposals[0]
		p.ID = fmt.Sprintf("p_%03d", i)
		body, _ := json.Marshal(p)
		os.WriteFile(filepath.Join(mcp.ProposalDir(c), p.ID+".json"), body, 0600)
	}
	first, err := export(c)
	if err != nil || len(first.Proposals) != 128 {
		t.Fatalf("%d %v", len(first.Proposals), err)
	}
	seen := map[string]string{}
	for _, p := range first.Proposals {
		body, _ := json.Marshal(p)
		seen[p.ID] = fmt.Sprintf("%x", sha256.Sum256(body))
	}
	second, err := exportSeen(c, seen)
	if err != nil || len(second.Proposals) != 2 {
		t.Fatalf("%d %v", len(second.Proposals), err)
	}
}
