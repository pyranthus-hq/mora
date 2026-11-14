package mora

import (
	"context"
	"crypto/ecdsa"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/resource"
)

func resourceProof(t *testing.T, verifier *desktopReviewTestVerifier, c resourceDecisionChallenge) []byte {
	t.Helper()
	signed, err := c.bytes()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(signed)
	proof, err := ecdsa.SignASN1(cryptorand.Reader, verifier.private, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return proof
}

func resourceIntegrationSetup(t *testing.T) (Config, projectBinding, resource.Record, resource.Revision) {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bound, _, err := bindProjectDir(testCtx(t), root, "project:resource")
	if err != nil {
		t.Fatal(err)
	}
	binding := projectBinding{Dir: bound, Scope: "project:resource"}
	record, revision, _, err := resource.Import(cfg.VaultDir, resource.Candidate{Kind: resource.KindSkill, Purpose: "safe resource integration", EffectiveScope: binding.Scope, Source: resource.SourcePasted, Pasted: []byte("# resource\n")})
	if err != nil {
		t.Fatal(err)
	}
	return cfg, binding, record, revision
}

func TestResourceActivationExactMachineClientScopeRootDestination(t *testing.T) {
	cfg, binding, record, revision := resourceIntegrationSetup(t)
	verifier := newDesktopReviewTestVerifier(t)
	authority := newResourceDecisionAuthority(cfg, verifier)
	preview, _, _, _, destination, err := resourcePreview(cfg, binding.Dir, "codex", record.StableID, revision.RevisionDigest)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := authority.begin(resourceActivate, binding.Dir, "codex", record.StableID, revision.RevisionDigest, preview.DisplayDigest)
	if err != nil {
		t.Fatal(err)
	}
	if challenge.Destination != destination || challenge.ProjectRoot != binding.Dir || challenge.Scope != binding.Scope || challenge.TreeDigest != revision.TreeDigest {
		t.Fatalf("challenge omitted exact binding: %+v", challenge)
	}
	outcome, err := authority.complete(context.Background(), challenge.Nonce, resourceProof(t, verifier, challenge), false)
	if err != nil || !desktopReviewApplied(outcome) || outcome.Status != "applied" {
		state, _ := os.ReadFile(resourceChallengePath(cfg, challenge.Nonce))
		t.Logf("challenge state: %s", state)
		t.Fatalf("activation outcome=%+v err=%v", outcome, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "SKILL.md")); err != nil {
		t.Fatalf("resource was not materialized: %v", err)
	}
	decision, found, err := latestResourceDecision(cfg, verifier.installationID, record.StableID, binding.Scope, "codex", binding.Dir)
	if err != nil || !found || decision.RevisionDigest != revision.RevisionDigest {
		t.Fatalf("durable decision=%+v found=%v err=%v", decision, found, err)
	}
	if _, err := readResourceReceipt(cfg, decision); err != nil {
		t.Fatalf("durable materialization receipt: %v", err)
	}
}

func TestResourceIntakeFolderRequiresBoundRoot(t *testing.T) {
	cfg, binding, _, _ := resourceIntegrationSetup(t)
	outside := t.TempDir()
	ctx := context.WithValue(context.Background(), mcpProjectKey{}, mcpProject{scope: binding.Scope, root: binding.Dir})
	ctx = withMCPWorkspace(ctx)
	_, err := mcpSubmitResource(ctx, cfg, map[string]any{"kind": "skill", "purpose": "outside", "folder": outside})
	if err == nil || !errors.Is(err, errMCPWorkspaceUnavailable) && !strings.Contains(err.Error(), "resource attachment") {
		t.Fatalf("outside attachment err=%v", err)
	}
}
