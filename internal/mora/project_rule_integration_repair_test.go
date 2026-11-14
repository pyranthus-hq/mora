package mora

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func projectRulePreviewDigest(t *testing.T, cfg Config, b projectBinding, id string, action desktopReviewAction) string {
	t.Helper()
	preview := showDesktopProjectRule(cfg, desktopReviewFrame{Client: "codex", ProjectDir: b.Dir, ID: id, Action: action})
	if preview.Error != nil || preview.Preview == nil {
		t.Fatalf("preview: %+v", preview)
	}
	return preview.Preview.DisplayDigest
}
func projectRuleNativeAuthority(t *testing.T, cfg Config) (*desktopReviewAuthority, *desktopReviewTestVerifier) {
	t.Helper()
	id, err := nativeOwnerInstallationID(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, v := desktopAuthority(t, cfg)
	v.installationID = id
	return a, v
}
func projectRuleNativeActivate(t *testing.T, cfg Config, b projectBinding, a *desktopReviewAuthority, v *desktopReviewTestVerifier, id, text string) projectRuleRevision {
	t.Helper()
	r := projectRuleTestMemory(t, cfg, b, id, text)
	c, err := a.beginProjectRule(desktopReviewActivate, b.Dir, r.RuleID, r.RevisionSHA256, projectRulePreviewDigest(t, cfg, b, r.RuleID, desktopReviewActivate), "codex")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := a.complete(testCtx(t), c.Nonce, v.proof(t, c), false); err != nil || out.Status != "applied" {
		t.Fatalf("activate: %+v %v", out, err)
	}
	return r
}
func TestProjectRuleRetireConflictReplayRecoversWithoutAnotherDecision(t *testing.T) {
	cfg, b := projectRuleTestBinding(t)
	if err := saveProjectBindings(cfg, []projectBinding{b}); err != nil {
		t.Fatal(err)
	}
	a, v := projectRuleNativeAuthority(t, cfg)
	r := projectRuleNativeActivate(t, cfg, b, a, v, "mem_retire_retry", "Use the approved formatter.")
	path := filepath.Join(b.Dir, "AGENTS.md")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(original, []byte(r.Text), []byte("Locally edited instruction."), 1)
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := a.beginProjectRule(desktopReviewRetire, b.Dir, r.RuleID, r.RevisionSHA256, projectRulePreviewDigest(t, cfg, b, r.RuleID, desktopReviewRetire), "codex")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		out, err := a.complete(testCtx(t), c.Nonce, v.proof(t, c), false)
		if err != nil || out.Status != "retired_loader_conflict" {
			t.Fatalf("retire/replay%d: %+v %v", i, out, err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, changed) {
		t.Fatal("conflict bytes changed", err)
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := a.complete(testCtx(t), c.Nonce, v.proof(t, c), false)
	if err != nil || out.Status != "retired" {
		t.Fatalf("retry cleanup: %+v %v", out, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loader still present: %v", err)
	}
	rows, err := os.ReadFile(projectRuleDecisionPath(cfg))
	if err != nil || bytes.Count(rows, []byte{'\n'}) != 2 {
		t.Fatalf("replay appended a decision: %s %v", rows, err)
	}
}
func TestProjectRuleRetireUnavailableSourceOrManifest(t *testing.T) {
	for _, fault := range []string{"missing_manifest", "corrupt_manifest", "changed_loader", "deleted_source"} {
		t.Run(fault, func(t *testing.T) {
			cfg, b := projectRuleTestBinding(t)
			if err := saveProjectBindings(cfg, []projectBinding{b}); err != nil {
				t.Fatal(err)
			}
			a, v := projectRuleNativeAuthority(t, cfg)
			r := projectRuleNativeActivate(t, cfg, b, a, v, "mem_retire_fault", "Keep the exact approved rule.")
			switch fault {
			case "missing_manifest":
				if err := os.Remove(projectRuleManifestPath(cfg, b.Dir)); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(b.Dir, "AGENTS.md")); err != nil {
					t.Fatal(err)
				}
			case "corrupt_manifest":
				if err := os.WriteFile(projectRuleManifestPath(cfg, b.Dir), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "changed_loader":
				if err := os.WriteFile(filepath.Join(b.Dir, "AGENTS.override.md"), []byte("Foreign instructions."), 0600); err != nil {
					t.Fatal(err)
				}
			case "deleted_source":
				m, err := findMemory(cfg, r.RuleID)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(m.Path); err != nil {
					t.Fatal(err)
				}
			}
			list := listDesktopProjectRules(cfg, desktopReviewFrame{Client: "codex", ProjectDir: b.Dir})
			if list.Error != nil || len(list.Rules) != 1 {
				t.Fatalf("hidden activation: %+v", list)
			}
			preview := showDesktopProjectRule(cfg, desktopReviewFrame{Client: "codex", ProjectDir: b.Dir, ID: r.RuleID, Action: desktopReviewRetire})
			if preview.Error != nil || preview.Preview == nil || preview.Preview.CanonicalRevision != r.RevisionSHA256 || preview.Preview.State != "active" {
				t.Fatalf("retirement preview: %+v", preview)
			}
			c, err := a.beginProjectRule(desktopReviewRetire, b.Dir, r.RuleID, preview.Preview.CanonicalRevision, preview.Preview.DisplayDigest, "codex")
			if err != nil {
				t.Fatal(err)
			}
			out, err := a.complete(testCtx(t), c.Nonce, v.proof(t, c), false)
			if err != nil || (out.Status != "retired" && out.Status != "retired_loader_conflict") {
				t.Fatalf("retirement: %+v %v", out, err)
			}
			state, err := loadProjectRuleState(cfg, b, v.installationID, "codex")
			if err != nil || len(state.Active) != 0 {
				t.Fatalf("retirement did not narrow: %+v %v", state, err)
			}
			if fault == "changed_loader" {
				got, err := os.ReadFile(filepath.Join(b.Dir, "AGENTS.override.md"))
				if err != nil || string(got) != "Foreign instructions." {
					t.Fatalf("foreign loader changed: %q %v", got, err)
				}
			}
		})
	}
}
func TestProjectRuleCleanupFailurePreservesConfirmationReceipts(t *testing.T) {
	cfg, b := projectRuleTestBinding(t)
	if err := saveProjectBindings(cfg, []projectBinding{b}); err != nil {
		t.Fatal(err)
	}
	a, v := projectRuleNativeAuthority(t, cfg)
	r := projectRuleNativeActivate(t, cfg, b, a, v, "mem_conflict", "Original instruction.")
	path := filepath.Join(b.Dir, "AGENTS.md")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Replace(original, []byte(r.Text), []byte("Locally edited."), 1), 0600); err != nil {
		t.Fatal(err)
	}
	m := Memory{ID: "mem_unrelated", Scope: b.Scope, Type: "fact", Title: "Unrelated", Text: "Preserve audit and index receipts.", CreatedAt: time.Now().UTC().Format(time.RFC3339), Tags: []string{memory.TentativeTag}}
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
	confirmed, logged, err := confirmMemory(testCtx(t), cfg, m.ID, "", "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "project rule reconciliation") || !logged || confirmed.ID != m.ID || memory.IsTentative(confirmed) {
		t.Fatalf("confirmation receipts: %q logged=%v error=%v", confirmed.ID, logged, err)
	}
}
func TestProjectRuleDeleteNarrowsLoaderEvenWhenIndexFails(t *testing.T) {
	cfg, b := projectRuleTestBinding(t)
	if err := saveProjectBindings(cfg, []projectBinding{b}); err != nil {
		t.Fatal(err)
	}
	a, v := projectRuleNativeAuthority(t, cfg)
	r := projectRuleNativeActivate(t, cfg, b, a, v, "mem_delete_index", "Delete this instruction.")
	m, err := findMemory(cfg, r.RuleID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.DataDir, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := deleteMemoryFile(testCtx(t), cfg, m); err == nil {
		t.Fatal("expected index failure")
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "AGENTS.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale loader survived index failure: %v", err)
	}
}
func TestProjectRuleHookConflictKeepsBudgetedBriefFallback(t *testing.T) {
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
	b := projectBinding{Dir: root, Scope: "project:hook-rule"}
	if err := saveProjectBindings(cfg, []projectBinding{b}); err != nil {
		t.Fatal(err)
	}
	a, v := projectRuleNativeAuthority(t, cfg)
	r := projectRuleNativeActivate(t, cfg, b, a, v, "mem_hook_rule", "Fallback must still deliver this rule.")
	path := filepath.Join(root, "AGENTS.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Replace(body, []byte(r.Text), []byte("Local edit."), 1), 0600); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]string{"cwd": root, "source": "startup"})
	var out bytes.Buffer
	if err := hookSessionStart(testCtx(t), nil, &out, bytes.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	text := decoded.HookSpecificOutput.AdditionalContext
	if !strings.Contains(text, "needs attention") || !strings.Contains(text, r.Text) || len(text) > projectBriefMaxChars {
		t.Fatalf("fallback/budget: %q", text)
	}
}

func TestProjectRulePreflightRefusalDoesNotAppend(t *testing.T) {
	cfg, b := projectRuleTestBinding(t)
	if err := saveProjectBindings(cfg, []projectBinding{b}); err != nil {
		t.Fatal(err)
	}
	a, v := projectRuleNativeAuthority(t, cfg)
	projectRuleNativeActivate(t, cfg, b, a, v, "mem_before_intent", "Prior approved rule.")
	r := projectRuleTestMemory(t, cfg, b, "mem_after_intent", "Never append before intent recovery.")
	c, err := a.beginProjectRule(desktopReviewActivate, b.Dir, r.RuleID, r.RevisionSHA256, projectRulePreviewDigest(t, cfg, b, r.RuleID, desktopReviewActivate), "codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectRuleIntentPath(cfg, b.Dir), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := a.complete(testCtx(t), c.Nonce, v.proof(t, c), false)
	if err != nil || out.Status != "stale" {
		t.Fatalf("preflight: %+v %v", out, err)
	}
	rows, err := os.ReadFile(projectRuleDecisionPath(cfg))
	if err != nil || bytes.Count(rows, []byte{'\n'}) != 1 {
		t.Fatalf("refusal appended: %q %v", rows, err)
	}
}
func TestProjectRuleApplyingReceiptSurvivesLostReply(t *testing.T) {
	for _, failAt := range []string{"applying", "applied"} {
		t.Run(failAt, func(t *testing.T) {
			cfg, b := projectRuleTestBinding(t)
			if err := saveProjectBindings(cfg, []projectBinding{b}); err != nil {
				t.Fatal(err)
			}
			a, v := projectRuleNativeAuthority(t, cfg)
			r := projectRuleTestMemory(t, cfg, b, "mem_lost_reply", "Only apply once.")
			c, err := a.beginProjectRule(desktopReviewActivate, b.Dir, r.RuleID, r.RevisionSHA256, projectRulePreviewDigest(t, cfg, b, r.RuleID, desktopReviewActivate), "codex")
			if err != nil {
				t.Fatal(err)
			}
			a.writeChallenge = func(path string, next desktopReviewChallenge) error {
				if next.State == failAt {
					return errors.New("injected durable receipt failure")
				}
				return writeDesktopReviewChallenge(path, next)
			}
			out, err := a.complete(testCtx(t), c.Nonce, v.proof(t, c), false)
			if failAt == "applying" {
				if err == nil {
					t.Fatal("missing durable applying accepted")
				}
				if _, err := os.Stat(projectRuleDecisionPath(cfg)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("premature decision: %v", err)
				}
				if _, err := os.Stat(filepath.Join(b.Dir, "AGENTS.md")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("premature loader: %v", err)
				}
			} else {
				if err != nil || out.Status != "indeterminate" {
					t.Fatalf("lost reply: %+v %v", out, err)
				}
				replay, err := newDesktopReviewAuthority(cfg, v).complete(testCtx(t), c.Nonce, v.proof(t, c), false)
				if err != nil || replay.Status != "indeterminate" {
					t.Fatalf("restarted replay: %+v %v", replay, err)
				}
				rows, err := os.ReadFile(projectRuleDecisionPath(cfg))
				if err != nil || bytes.Count(rows, []byte{'\n'}) != 1 {
					t.Fatalf("duplicate after lost reply: %q %v", rows, err)
				}
			}
		})
	}
}
func TestProjectRulePublicProofAndExpiryNeverCreateAuthority(t *testing.T) {
	cfg, b := projectRuleTestBinding(t)
	if err := saveProjectBindings(cfg, []projectBinding{b}); err != nil {
		t.Fatal(err)
	}
	r := projectRuleTestMemory(t, cfg, b, "mem_deny_rule", "Never accept public authority.")
	if _, err := newDesktopReviewAuthority(cfg, nil).beginProjectRule(desktopReviewActivate, b.Dir, r.RuleID, r.RevisionSHA256, projectRulePreviewDigest(t, cfg, b, r.RuleID, desktopReviewActivate), "codex"); !errors.Is(err, errDesktopReviewProofUnavailable) {
		t.Fatalf("public begin: %v", err)
	}
	a, v := projectRuleNativeAuthority(t, cfg)
	c, err := a.beginProjectRule(desktopReviewActivate, b.Dir, r.RuleID, r.RevisionSHA256, projectRulePreviewDigest(t, cfg, b, r.RuleID, desktopReviewActivate), "codex")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.complete(testCtx(t), c.Nonce, []byte("not proof"), false); !errors.Is(err, errDesktopReviewProofUnavailable) {
		t.Fatalf("bad proof: %v", err)
	}
	a.now = func() time.Time { return time.Unix(c.ExpiresAt, 0) }
	if _, err := a.complete(testCtx(t), c.Nonce, v.proof(t, c), false); err == nil {
		t.Fatal("expired proof accepted")
	}
	if _, err := os.Stat(projectRuleDecisionPath(cfg)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denial created decision: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "AGENTS.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denial created loader: %v", err)
	}
}
func TestProjectRuleNativeOverflowKeepsOnlyOmittedBriefFallback(t *testing.T) {
	cfg, b := projectRuleTestBinding(t)
	if err := saveProjectBindings(cfg, []projectBinding{b}); err != nil {
		t.Fatal(err)
	}
	a, v := projectRuleNativeAuthority(t, cfg)
	for _, id := range []string{"mem_overflow_a", "mem_overflow_b", "mem_overflow_c"} {
		projectRuleNativeActivate(t, cfg, b, a, v, id, strings.Repeat("bounded guidance ", 160))
	}
	manifest, found, err := readProjectRuleManifest(projectRuleManifestPath(cfg, b.Dir))
	if err != nil || !found || manifest.Omitted != 1 || len(manifest.Delivered) != 2 {
		t.Fatalf("overflow: %+v %v", manifest, err)
	}
	_, ids, err := buildProjectBrief(cfg, b.Scope, b.Dir, time.Now())
	if err != nil || len(ids) != 1 || ids[0] != "mem_overflow_c" {
		t.Fatalf("fallback IDs: %v %v", ids, err)
	}
}

func TestProjectRuleApprovalBindsWholeDisplayedPreview(t *testing.T) {
	for _, change := range []string{"loader_before_begin", "source_before_complete", "omissions_before_complete"} {
		t.Run(change, func(t *testing.T) {
			cfg, b := projectRuleTestBinding(t)
			if err := saveProjectBindings(cfg, []projectBinding{b}); err != nil {
				t.Fatal(err)
			}
			a, v := projectRuleNativeAuthority(t, cfg)
			r := projectRuleTestMemory(t, cfg, b, "mem_preview_target", strings.Repeat("Target guidance. ", 160))
			digest := projectRulePreviewDigest(t, cfg, b, r.RuleID, desktopReviewActivate)
			if change == "loader_before_begin" {
				if err := os.WriteFile(filepath.Join(b.Dir, "AGENTS.override.md"), []byte("Foreign loader."), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := a.beginProjectRule(desktopReviewActivate, b.Dir, r.RuleID, r.RevisionSHA256, digest, "codex"); !errors.Is(err, errDesktopReviewUnavailable) {
					t.Fatalf("unseen destination accepted: %v", err)
				}
				return
			}
			c, err := a.beginProjectRule(desktopReviewActivate, b.Dir, r.RuleID, r.RevisionSHA256, digest, "codex")
			if err != nil {
				t.Fatal(err)
			}
			if change == "source_before_complete" {
				m, err := findMemory(cfg, r.RuleID)
				if err != nil {
					t.Fatal(err)
				}
				m.Text = "Changed after preview."
				if err := writeMemory(cfg, m); err != nil {
					t.Fatal(err)
				}
			} else {
				projectRuleNativeActivate(t, cfg, b, a, v, "mem_preview_other_a", strings.Repeat("Other guidance. ", 180))
				projectRuleNativeActivate(t, cfg, b, a, v, "mem_preview_other_b", strings.Repeat("More guidance. ", 180))
			}
			out, err := a.complete(testCtx(t), c.Nonce, v.proof(t, c), false)
			if err != nil || out.Status != "stale" {
				t.Fatalf("changed preview accepted: %+v %v", out, err)
			}
			if _, found, err := latestProjectRuleDecision(cfg, b, v.installationID, "codex", r.RuleID); err != nil || found {
				t.Fatalf("stale approval added authority: %v %v", found, err)
			}
		})
	}
}
