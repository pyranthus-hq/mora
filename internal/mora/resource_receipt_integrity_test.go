package mora

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/pyranthus-hq/mora/internal/resource"
)

// These synthetic regressions exercise discovery after a real test-verifier
// activation. They do not establish real native-client acceptance.
func TestResourceReceiptIntegrity(t *testing.T) {
	cases := []struct {
		name          string
		mutate        func(*resourceMaterializationReceipt)
		raw           []byte
		missing, edit bool
	}{
		{name: "missing", missing: true},
		{name: "unparseable", raw: []byte("{")},
		{name: "schema", mutate: func(r *resourceMaterializationReceipt) { r.Schema = "wrong" }},
		{name: "destination", mutate: func(r *resourceMaterializationReceipt) { r.Destination += "-other" }},
		{name: "resource-id", mutate: func(r *resourceMaterializationReceipt) { r.ResourceID += "-other" }},
		{name: "revision", mutate: func(r *resourceMaterializationReceipt) { r.RevisionDigest += "-other" }},
		{name: "scope", mutate: func(r *resourceMaterializationReceipt) { r.Scope += "-other" }},
		{name: "client", mutate: func(r *resourceMaterializationReceipt) { r.Client = "claude" }},
		{name: "root", mutate: func(r *resourceMaterializationReceipt) { r.ProjectRoot += "-other" }},
		{name: "manifest-digest", mutate: func(r *resourceMaterializationReceipt) { r.ManifestDigest += "-other" }},
		{name: "local-edit", edit: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, binding, record, revision := resourceIntegrationSetup(t)
			verifier := newDesktopReviewTestVerifier(t)
			installation, err := nativeOwnerInstallationID(cfg)
			if err != nil {
				t.Fatal(err)
			}
			verifier.installationID = installation
			authority := newResourceDecisionAuthority(cfg, verifier)
			preview, _, _, _, destination, err := resourcePreview(cfg, binding.Dir, "codex", record.StableID, revision.RevisionDigest)
			if err != nil {
				t.Fatal(err)
			}
			challenge, err := authority.begin(resourceActivate, binding.Dir, "codex", record.StableID, revision.RevisionDigest, preview.DisplayDigest)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := authority.complete(context.Background(), challenge.Nonce, resourceProof(t, verifier, challenge), false)
			if err != nil || !desktopReviewApplied(outcome) {
				t.Fatalf("activation: %+v %v", outcome, err)
			}
			d, found, err := latestResourceDecision(cfg, installation, record.StableID, binding.Scope, "codex", binding.Dir)
			if err != nil || !found {
				t.Fatalf("decision found=%v err=%v", found, err)
			}
			discover := func() []resource.Result {
				t.Helper()
				ctx := withMCPWorkspace(context.WithValue(testCtx(t), mcpProjectKey{}, mcpProject{scope: binding.Scope, root: binding.Dir}))
				result, err := mcpDiscoverResources(ctx, cfg, map[string]any{"task": "safe resource integration"})
				if err != nil {
					t.Fatal(err)
				}
				rows, ok := result.(map[string]any)["resources"].([]resource.Result)
				if !ok {
					t.Fatalf("discovery: %#v", result)
				}
				return rows
			}
			rows := discover()
			if len(rows) != 1 || rows[0].ResourceID != record.StableID || rows[0].ReviewState != "approved" {
				t.Fatalf("positive control: %#v", rows)
			}
			receiptPath := resourceMaterializationPath(cfg, d)
			receipt, err := readResourceReceipt(cfg, d)
			if err != nil {
				t.Fatal(err)
			}
			if tc.missing {
				if err := os.Remove(receiptPath); err != nil {
					t.Fatal(err)
				}
			} else if tc.raw != nil {
				if err := os.WriteFile(receiptPath, tc.raw, 0600); err != nil {
					t.Fatal(err)
				}
			} else if tc.mutate != nil {
				tc.mutate(&receipt)
				b, err := json.Marshal(receipt)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(receiptPath, b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			filePath := filepath.Join(destination, "SKILL.md")
			if tc.edit {
				if err := os.WriteFile(filePath, []byte("# locally edited\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			beforeDecision, err := os.ReadFile(resourceDecisionPath(cfg))
			if err != nil {
				t.Fatal(err)
			}
			beforeFile, err := os.ReadFile(filePath)
			if err != nil {
				t.Fatal(err)
			}
			beforeReceipt, beforeErr := os.ReadFile(receiptPath)
			rows = discover()
			afterDecision, err := os.ReadFile(resourceDecisionPath(cfg))
			if err != nil {
				t.Fatal(err)
			}
			afterFile, err := os.ReadFile(filePath)
			if err != nil {
				t.Fatal(err)
			}
			afterReceipt, afterErr := os.ReadFile(receiptPath)
			if string(beforeDecision) != string(afterDecision) || string(beforeFile) != string(afterFile) || string(beforeReceipt) != string(afterReceipt) || (beforeErr == nil) != (afterErr == nil) {
				t.Error("read-only discovery changed decision, receipt, or local file")
			}
			if len(rows) != 0 {
				t.Errorf("inconsistent materialization remained discoverable as approved: %#v", rows)
			}
		})
	}
}
