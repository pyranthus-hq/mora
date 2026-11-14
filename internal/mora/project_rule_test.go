package mora

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

const (
	projectRuleTestInstallation = "installation-a"
	projectRuleTestClient       = "codex"
)

func projectRuleTestBinding(t *testing.T) (Config, projectBinding) {
	t.Helper()
	cfg := coreADirsCfg(t)
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, projectBinding{Dir: root, Scope: "project:rules"}
}

func projectRuleTestMemory(t *testing.T, cfg Config, binding projectBinding, id, text string) projectRuleRevision {
	t.Helper()
	// Corrections avoid decision-validity normalization; the leaf accepts both
	// owner-confirmed decision and correction records.
	m := Memory{ID: id, Scope: binding.Scope, Type: memory.TypeCorrection, Title: id, Text: text, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	m.Tags = []string{memory.OwnerConfirmedTagPrefix + memory.OwnerConfirmedDigest(m)}
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
	r, err := loadProjectRuleRevision(cfg, binding, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func projectRuleTestDecision(r projectRuleRevision, binding projectBinding, action string) projectRuleDecision {
	return projectRuleDecision{
		Schema: projectRuleDecisionSchema, Action: action, RuleID: r.RuleID, SourceMemoryID: r.SourceMemoryID, Scope: r.Scope, RevisionSHA256: r.RevisionSHA256,
		InstallationID: projectRuleTestInstallation, KeyID: "key-a", Client: projectRuleTestClient, ProjectRoot: binding.Dir, LoaderRelPath: "AGENTS.md",
		ChallengeNonce: "nonce-" + action, DisplayDigest: "display", VerifiedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

func activateProjectRule(t *testing.T, cfg Config, binding projectBinding, r projectRuleRevision) {
	t.Helper()
	d := projectRuleTestDecision(r, binding, "activate")
	if _, rel, _, err := selectCodexProjectInstructions(binding); err != nil {
		t.Fatal(err)
	} else {
		d.LoaderRelPath = rel
	}
	if err := appendProjectRuleDecision(cfg, d); err != nil {
		t.Fatal(err)
	}
}

func TestProjectRuleActivationRequiresCurrentConfirmedSource(t *testing.T) {
	cfg, binding := projectRuleTestBinding(t)
	r := projectRuleTestMemory(t, cfg, binding, "mem_rule", "Use the project formatter.")
	activateProjectRule(t, cfg, binding, r)
	if active, err := activeProjectRuleRevisions(cfg, binding, projectRuleTestInstallation, projectRuleTestClient); err != nil || len(active) != 1 {
		t.Fatalf("active = %#v, %v", active, err)
	}
	m, err := findMemory(cfg, r.RuleID)
	if err != nil {
		t.Fatal(err)
	}
	m.Text = "Changed after the owner kept it."
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
	if active, err := activeProjectRuleRevisions(cfg, binding, projectRuleTestInstallation, projectRuleTestClient); err != nil || len(active) != 0 {
		t.Fatalf("changed source must not stay active: %#v, %v", active, err)
	}
}

func TestProjectRuleDecisionLastValidLocalRowWins(t *testing.T) {
	cfg, binding := projectRuleTestBinding(t)
	r := projectRuleTestMemory(t, cfg, binding, "mem_decision", "Use the exact local action.")
	activateProjectRule(t, cfg, binding, r)
	wrongRoot := projectRuleTestDecision(r, binding, "retire")
	wrongRoot.ProjectRoot = filepath.Join(binding.Dir, "other")
	wrongRoot.ChallengeNonce = "wrong-root"
	if err := appendProjectRuleDecision(cfg, wrongRoot); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(projectRuleDecisionPath(cfg), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("not json\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if active, err := activeProjectRuleRevisions(cfg, binding, projectRuleTestInstallation, projectRuleTestClient); err != nil || len(active) != 1 {
		t.Fatalf("wrong-root/corrupt rows changed local active state: %#v, %v", active, err)
	}
	retire := projectRuleTestDecision(r, binding, "retire")
	retire.ChallengeNonce = "valid-retire"
	if err := appendProjectRuleDecision(cfg, retire); err != nil {
		t.Fatal(err)
	}
	if state, err := loadProjectRuleState(cfg, binding, projectRuleTestInstallation, projectRuleTestClient); err != nil || len(state.Active) != 0 {
		t.Fatalf("last retire did not win: %#v, %v", state.Active, err)
	}
}

func TestProjectRuleMaterializationPrefersOverrideAndPreservesForeignBytes(t *testing.T) {
	cfg, binding := projectRuleTestBinding(t)
	base := filepath.Join(binding.Dir, "AGENTS.md")
	override := filepath.Join(binding.Dir, "AGENTS.override.md")
	baseBody := []byte("base foreign instructions\n")
	foreign := []byte("override foreign instructions\n")
	if err := os.WriteFile(base, baseBody, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(override, foreign, 0o644); err != nil {
		t.Fatal(err)
	}
	r := projectRuleTestMemory(t, cfg, binding, "mem_override", "Always use the approved formatter.")
	activateProjectRule(t, cfg, binding, r)
	plan, err := planProjectRuleMaterialization(cfg, binding, projectRuleTestInstallation, projectRuleTestClient)
	if err != nil {
		t.Fatal(err)
	}
	if plan.relPath != "AGENTS.override.md" || !bytes.HasPrefix(plan.after, foreign) {
		t.Fatalf("plan selected %q:\n%s", plan.relPath, plan.after)
	}
	if err := commitProjectRuleMaterialization(plan); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(base); !bytes.Equal(got, baseBody) {
		t.Fatalf("shadowed base changed: %q", got)
	}
	if got, _ := os.ReadFile(override); !bytes.HasPrefix(got, foreign) || !bytes.Contains(got, []byte(projectRuleBegin)) {
		t.Fatalf("override did not preserve foreign bytes:\n%s", got)
	}
	if got := verifiedProjectRuleDelivery(cfg, binding, projectRuleTestInstallation, projectRuleTestClient); len(got) != 1 || got[0].RuleID != r.RuleID {
		t.Fatalf("verified delivery = %#v", got)
	}
}

func TestProjectRuleActivationDoesNotTransferToNewOverride(t *testing.T) {
	cfg, binding := projectRuleTestBinding(t)
	r := projectRuleTestMemory(t, cfg, binding, "mem_loader", "Stay on the approved loader.")
	activateProjectRule(t, cfg, binding, r)
	if active, err := activeProjectRuleRevisions(cfg, binding, projectRuleTestInstallation, projectRuleTestClient); err != nil || len(active) != 1 {
		t.Fatalf("initial active = %#v, %v", active, err)
	}
	if err := os.WriteFile(filepath.Join(binding.Dir, "AGENTS.override.md"), []byte("new selected loader\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if active, err := activeProjectRuleRevisions(cfg, binding, projectRuleTestInstallation, projectRuleTestClient); err != nil || len(active) != 0 {
		t.Fatalf("override must require a new activation: %#v, %v", active, err)
	}
}

func TestProjectRuleMaterializationRejectsManagedEditsAndFallsBackOnStaleManifest(t *testing.T) {
	cfg, binding := projectRuleTestBinding(t)
	r := projectRuleTestMemory(t, cfg, binding, "mem_edit", "Do not bypass the review.")
	activateProjectRule(t, cfg, binding, r)
	plan, err := planProjectRuleMaterialization(cfg, binding, projectRuleTestInstallation, projectRuleTestClient)
	if err != nil {
		t.Fatal(err)
	}
	if err := commitProjectRuleMaterialization(plan); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(binding.Dir, "AGENTS.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append([]byte("foreign prefix\n"), body...), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := verifiedProjectRuleDelivery(cfg, binding, projectRuleTestInstallation, projectRuleTestClient); got != nil {
		t.Fatalf("stale full-file manifest delivered %#v", got)
	}
	plan, err = planProjectRuleMaterialization(cfg, binding, projectRuleTestInstallation, projectRuleTestClient)
	if err != nil {
		t.Fatalf("foreign edit outside region must be retained, got %v", err)
	}
	if err := os.WriteFile(path, bytes.Replace(plan.before, []byte("Do not bypass"), []byte("Edited rule bytes"), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := planProjectRuleMaterialization(cfg, binding, projectRuleTestInstallation, projectRuleTestClient); !errors.Is(err, errProjectRuleConflict) {
		t.Fatalf("managed edit error = %v", err)
	}
}

func TestProjectRuleMaterializationCapsAndRetiresWithRetry(t *testing.T) {
	cfg, binding := projectRuleTestBinding(t)
	first := projectRuleTestMemory(t, cfg, binding, "mem_alpha", strings.Repeat("alpha ", 800))
	second := projectRuleTestMemory(t, cfg, binding, "mem_beta", strings.Repeat("beta ", 800))
	activateProjectRule(t, cfg, binding, first)
	activateProjectRule(t, cfg, binding, second)
	plan, err := planProjectRuleMaterialization(cfg, binding, projectRuleTestInstallation, projectRuleTestClient)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.manifest.Delivered) != 1 || plan.manifest.Omitted != 1 || len(plan.after) > projectRuleMaxBytes {
		t.Fatalf("cap plan = delivered:%d omitted:%d bytes:%d", len(plan.manifest.Delivered), plan.manifest.Omitted, len(plan.after))
	}
	if err := commitProjectRuleMaterialization(plan); err != nil {
		t.Fatal(err)
	}
	if got := verifiedProjectRuleDelivery(cfg, binding, projectRuleTestInstallation, projectRuleTestClient); got != nil {
		t.Fatalf("partial materialization must not suppress brief: %#v", got)
	}
	// A retirement is append-first. A cleanup race must leave the retired rule
	// excluded from active state, then allow a later reconciliation retry.
	retire := projectRuleTestDecision(first, binding, "retire")
	retire.ChallengeNonce = "nonce-retire-first"
	if err := appendProjectRuleDecision(cfg, retire); err != nil {
		t.Fatal(err)
	}
	plan, err = planProjectRuleMaterialization(cfg, binding, projectRuleTestInstallation, projectRuleTestClient)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(binding.Dir, "AGENTS.md")
	if err := os.WriteFile(path, bytes.Replace(plan.before, []byte("alpha "), []byte("edited"), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reconcileRetiredProjectRuleMaterialization(cfg, binding, projectRuleTestInstallation, projectRuleTestClient); !errors.Is(err, errProjectRuleRetiredWithLoaderConflict) {
		t.Fatalf("retired conflict error = %v", err)
	}
	if active, err := activeProjectRuleRevisions(cfg, binding, projectRuleTestInstallation, projectRuleTestClient); err != nil || len(active) != 1 || active[0].RuleID != second.RuleID {
		t.Fatalf("retire must narrow state before cleanup: %#v, %v", active, err)
	}
	if err := os.WriteFile(path, plan.before, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reconcileRetiredProjectRuleMaterialization(cfg, binding, projectRuleTestInstallation, projectRuleTestClient); err != nil {
		t.Fatalf("retry after conflict: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || bytes.Contains(got, []byte("alpha ")) || !bytes.Contains(got, []byte("beta ")) {
		t.Fatalf("retirement = %q, %v", got, err)
	}
}

func TestProjectRuleReconciliationRemovesChangedSourceProjection(t *testing.T) {
	cfg, binding := projectRuleTestBinding(t)
	r := projectRuleTestMemory(t, cfg, binding, "mem_drift", "Use stable source content.")
	activateProjectRule(t, cfg, binding, r)
	if err := reconcileProjectRuleMaterialization(cfg, binding, projectRuleTestInstallation, projectRuleTestClient); err != nil {
		t.Fatal(err)
	}
	m, err := findMemory(cfg, r.RuleID)
	if err != nil {
		t.Fatal(err)
	}
	m.Text = "changed and no longer confirmed"
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
	if err := reconcileProjectRuleMaterialization(cfg, binding, projectRuleTestInstallation, projectRuleTestClient); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(binding.Dir, "AGENTS.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale projection was not removed: %v", err)
	}
	if got := verifiedProjectRuleDelivery(cfg, binding, projectRuleTestInstallation, projectRuleTestClient); got != nil {
		t.Fatalf("changed source must not be deduplicated: %#v", got)
	}
}

func TestProjectRuleReconciliationRemovesUnconfirmedAndForgottenSources(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, cfg Config, id string)
	}{
		{"unconfirmed", func(t *testing.T, cfg Config, id string) {
			m, err := findMemory(cfg, id)
			if err != nil {
				t.Fatal(err)
			}
			m.Tags = nil
			if err := writeMemory(cfg, m); err != nil {
				t.Fatal(err)
			}
		}},
		{"discarded", func(t *testing.T, cfg Config, id string) {
			m, err := findMemory(cfg, id)
			if err != nil {
				t.Fatal(err)
			}
			// A discarded Keep has no matching owner-confirmed tag, just as a
			// changed tag must not preserve a stale native activation.
			m.Tags = nil
			if err := writeMemory(cfg, m); err != nil {
				t.Fatal(err)
			}
		}},
		{"forgotten", func(t *testing.T, cfg Config, id string) {
			m, err := findMemory(cfg, id)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(m.Path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, binding := projectRuleTestBinding(t)
			r := projectRuleTestMemory(t, cfg, binding, "mem_"+tc.name, "Remove stale loader guidance.")
			activateProjectRule(t, cfg, binding, r)
			if err := reconcileProjectRuleMaterialization(cfg, binding, projectRuleTestInstallation, projectRuleTestClient); err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, cfg, r.RuleID)
			if err := reconcileProjectRuleMaterialization(cfg, binding, projectRuleTestInstallation, projectRuleTestClient); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(binding.Dir, "AGENTS.md")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stale %s projection was not removed: %v", tc.name, err)
			}
		})
	}
}

func TestCodexProjectInstructionsRejectSymlink(t *testing.T) {
	cfg, binding := projectRuleTestBinding(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(binding.Dir, "AGENTS.override.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	r := projectRuleTestMemory(t, cfg, binding, "mem_symlink", "safe")
	d := projectRuleTestDecision(r, binding, "activate")
	d.LoaderRelPath = "AGENTS.override.md"
	if err := appendProjectRuleDecision(cfg, d); err != nil {
		t.Fatal(err)
	}
	if _, err := planProjectRuleMaterialization(cfg, binding, projectRuleTestInstallation, projectRuleTestClient); !errors.Is(err, errProjectRuleUnsupported) {
		t.Fatalf("symlink error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(binding.Dir, "AGENTS.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe selection created fallback: %v", err)
	}
}
