package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pyranthus-hq/mora/internal/resource"
)

// A challenge binds the precise previous materialization. A valid proof alone
// cannot authorize a later receipt, edit, or a different predecessor.
func TestResourceReceiptLifecycle(t *testing.T) {
	for _, tc := range []string{"detach", "rollback", "two-first-activations", "edit-after-preview", "manifest-after-preview", "inactive-status", "tree-decision", "extra-file", "aliased-receipt", "oversize-file"} {
		t.Run(tc, func(t *testing.T) {
			cfg, binding, record, rev := resourceIntegrationSetup(t)
			v := newDesktopReviewTestVerifier(t)
			var err error
			v.installationID, err = nativeOwnerInstallationID(cfg)
			if err != nil {
				t.Fatal(err)
			}
			a := newResourceDecisionAuthority(cfg, v)
			p, _, _, _, dest, err := resourcePreview(cfg, binding.Dir, "codex", record.StableID, rev.RevisionDigest)
			if err != nil {
				t.Fatal(err)
			}
			begin := func(action resourceDecisionAction) resourceDecisionChallenge {
				t.Helper()
				c, err := a.begin(action, binding.Dir, "codex", record.StableID, rev.RevisionDigest, p.DisplayDigest)
				if err != nil {
					t.Fatal(err)
				}
				return c
			}
			complete := func(c resourceDecisionChallenge) (desktopReviewOutcome, error) {
				return a.complete(context.Background(), c.Nonce, resourceProof(t, v, c), false)
			}
			first := begin(resourceActivate)
			var c resourceDecisionChallenge
			if tc == "two-first-activations" {
				c = begin(resourceActivate)
			}
			out, err := complete(first)
			if err != nil || !desktopReviewApplied(out) {
				t.Fatalf("activation %v %v", out, err)
			}
			if tc == "rollback" {
				// The new row and its predecessor have different revision and tree digests.
				_, rev, _, err = resource.Import(cfg.VaultDir, resource.Candidate{StableID: record.StableID, Kind: resource.KindSkill, Source: resource.SourcePasted, Pasted: []byte("second revision"), Purpose: "second revision", EffectiveScope: binding.Scope})
				if err != nil {
					t.Fatal(err)
				}
				p, _, _, _, _, err = resourcePreview(cfg, binding.Dir, "codex", record.StableID, rev.RevisionDigest)
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc != "two-first-activations" {
				action := resourceDetach
				if tc == "rollback" {
					action = resourceRollback
				}
				c = begin(action)
			}
			d, found, err := latestResourceDecision(cfg, v.installationID, record.StableID, binding.Scope, "codex", binding.Dir)
			if err != nil || !found {
				t.Fatalf("selecting decision: %v", err)
			}
			receiptPath := resourceMaterializationPath(cfg, d)
			receipt, err := readResourceReceipt(cfg, d)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dest, "SKILL.md")
			switch tc {
			case "detach", "rollback":
				out, err = complete(c)
				if err != nil || !desktopReviewApplied(out) {
					t.Fatalf("%s: %+v %v", tc, out, err)
				}
				if tc == "detach" {
					if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("detach: %v", err)
					}
				} else {
					d, _, err = latestResourceDecision(cfg, v.installationID, record.StableID, binding.Scope, "codex", binding.Dir)
					if err != nil {
						t.Fatal(err)
					}
					if _, err = readResourceReceipt(cfg, d); err != nil {
						t.Fatal(err)
					}
				}
				return
			case "edit-after-preview":
				err = os.WriteFile(path, []byte("saved edit"), 0600)
			case "oversize-file":
				err = os.Truncate(path, 1<<22)
			case "extra-file":
				err = os.WriteFile(filepath.Join(dest, "notes"), []byte("saved note"), 0600)
			case "aliased-receipt":
				target := filepath.Join(filepath.Dir(receiptPath), "old-receipt")
				if err = os.Rename(receiptPath, target); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(target, receiptPath); err != nil {
					t.Fatalf("establish receipt symlink: %v", err)
				}
			case "tree-decision":
				d.TreeDigest = "unavailable"
				err = appendResourceDecision(cfg, d)
			case "manifest-after-preview", "inactive-status":
				if tc == "inactive-status" {
					receipt.Status = "conflict"
				} else {
					receipt.Manifest.Revision.Files[0].Path = "other"
					receipt.ManifestDigest = resourceManifestDigest(receipt.Manifest)
				}
				err = writeResourceReceipt(cfg, d, receipt)
			}
			if err != nil {
				t.Fatal(err)
			}
			paths := []string{resourceDecisionPath(cfg), receiptPath, path, resourceChallengePath(cfg, c.Nonce)}
			if tc == "extra-file" {
				paths = append(paths, filepath.Join(dest, "notes"))
			}
			read := func(path string) []byte {
				t.Helper()
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				return body
			}
			snapshot := make([][]byte, len(paths))
			for i, p := range paths {
				snapshot[i] = read(p)
			}
			if tc != "two-first-activations" {
				ctx := withMCPWorkspace(context.WithValue(testCtx(t), mcpProjectKey{}, mcpProject{scope: binding.Scope, root: binding.Dir}))
				resp, err := mcpDiscoverResources(ctx, cfg, map[string]any{"task": "safe resource integration"})
				if err != nil {
					t.Fatal(err)
				}
				result := resp.(map[string]any)
				rows, err := json.Marshal(result["resources"])
				if err != nil || string(rows) != "[]" || result["materialization_status"] != "conflicted_or_unavailable" {
					t.Fatal("missing bounded conflict indication")
				}
			}
			out, err = complete(c)
			if !errors.Is(err, errResourceMaterialization) || desktopReviewApplied(out) {
				t.Fatalf("conflicting completion: %+v %v", out, err)
			}
			for i, p := range paths {
				if !bytes.Equal(snapshot[i], read(p)) {
					t.Fatalf("%s changed", filepath.Base(p))
				}
			}
		})
	}
}
