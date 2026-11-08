package mora

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pyranthus-hq/mora/internal/rules"
)

func TestRulesGrantConcurrentImportOneWinner(t *testing.T) {
	grant, _, signers := grantTestSetup(t)
	withTempHome(t)
	cfg := mustConfig(t)
	stubSignersTrusted(t, true)
	const count = 8
	ctx := testCtx(t)
	start := make(chan struct{})
	results := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- rulesImport(ctx, []string{grant, "--signers", signers, "--scope", "factory:aeth-forge"}, io.Discard, strings.NewReader(""))
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !strings.Contains(err.Error(), "already imported") {
			t.Errorf("unexpected loser error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("successful imports = %d, want 1", winners)
	}
	data, err := os.ReadFile(rules.GrantsSeenPath(cfg.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(bytes.TrimSpace(data), []byte("\n")) + 1; got != 1 {
		t.Fatalf("ledger rows = %d", got)
	}
	if ids := rulesActiveIDs(t, cfg); strings.Join(ids, ",") != "no-sudo" {
		t.Fatalf("active = %v", ids)
	}
}

func TestRulesGrantFailedImportConsumesReservation(t *testing.T) {
	grant, _, signers := grantTestSetup(t)
	withTempHome(t)
	cfg := mustConfig(t)
	stubSignersTrusted(t, true)
	if err := os.MkdirAll(cfg.VaultDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Force an error after reservation and rule-file publication but before approval.
	blocker := filepath.Join(cfg.VaultDir, "grants")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{grant, "--signers", signers, "--scope", "factory:aeth-forge"}
	if err := rulesImport(testCtx(t), args, io.Discard, strings.NewReader("")); err == nil {
		t.Fatal("import unexpectedly succeeded")
	}
	approvals, err := rules.LoadApprovals(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(approvals) != 0 {
		t.Fatalf("approval written after failed archive: %+v", approvals)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := rulesImport(testCtx(t), args, io.Discard, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "already imported") {
		t.Fatalf("retry = %v, want replay refusal", err)
	}
	active, _, err := rules.LoadActive(rules.ActivePath(cfg.StateDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(active.Rules) != 0 {
		t.Fatalf("failed import activated %+v", active.Rules)
	}
}

func TestRulesGrantArchivesVerifiedSignatureSnapshot(t *testing.T) {
	grant, _, signers := grantTestSetup(t)
	g, _, body, sig, err := rulesVerifyGrant(context.Background(), grant, signers, "factory:aeth-forge")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(grant+".sig", []byte("changed after verification"), 0o600); err != nil {
		t.Fatal(err)
	}
	vault := t.TempDir()
	if err := rulesKeepGrant(vault, body, sig, g); err != nil {
		t.Fatal(err)
	}
	kept, err := os.ReadFile(filepath.Join(vault, "grants", g.Rule.ID+"-"+g.Nonce[:8]+".json.sig"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(kept, sig) {
		t.Fatal("archive did not preserve verified signature bytes")
	}
}

func TestRulesGrantFreezesCanonicalSignersPath(t *testing.T) {
	grant, _, signers := grantTestSetup(t)
	withTempHome(t)
	link := filepath.Join(t.TempDir(), "signers-link")
	if err := os.Symlink(signers, link); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(signers)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "untrusted")
	if err := os.WriteFile(bad, []byte("invalid signers"), 0o600); err != nil {
		t.Fatal(err)
	}
	prev := rulesSignersTrusted
	t.Cleanup(func() { rulesSignersTrusted = prev })
	calls := 0
	rulesSignersTrusted = func(path string) bool {
		calls++
		if path != canonical {
			t.Errorf("trust path = %q, want canonical %q", path, canonical)
		}
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(bad, link); err != nil {
			t.Fatal(err)
		}
		return true
	}
	if err := rulesImport(testCtx(t), []string{grant, "--signers", link, "--scope", "factory:aeth-forge"}, io.Discard, strings.NewReader("")); err != nil {
		t.Fatalf("canonical verification followed changed symlink: %v", err)
	}
	if calls != 1 {
		t.Fatalf("trust checks = %d", calls)
	}
}
