package mora

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pyranthus-hq/mora/internal/atomicio"
)

const (
	projectRuleBegin        = "<!-- MORA:BEGIN owner-rules v1 -->"
	projectRuleEnd          = "<!-- MORA:END owner-rules v1 -->"
	projectRuleMaxBytes     = 6000
	projectRuleManifestName = "codex.json"
)

var (
	errProjectRuleConflict      = errors.New("project rule materialization conflict")
	errProjectRuleUnsupported   = errors.New("project rule materialization unsupported")
	errProjectRuleIndeterminate = errors.New("project rule materialization indeterminate")
	// errProjectRuleRetiredWithLoaderConflict lets the authority integration
	// report the honest narrowed-state outcome after it durably appends retire.
	errProjectRuleRetiredWithLoaderConflict = errors.New("project rule retired with loader conflict")
)

type projectRuleManifest struct {
	Schema, InstallationID, Client, ProjectRoot, Scope, LoaderRelPath string
	FileSHA256, ManagedRegionSHA256                                   string
	CreatedFile                                                       bool
	Delivered                                                         []projectRuleKey
	Omitted                                                           int
}

type projectRuleMaterializationPlan struct {
	binding        projectBinding
	installation   string
	client         string
	path           string
	relPath        string
	existed        bool
	before         []byte
	after          []byte
	manifestPath   string
	manifest       projectRuleManifest
	removeFile     bool
	removeManifest bool
	noop           bool
}

func projectRuleSHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func projectRuleManifestPath(cfg Config, root string) string {
	return filepath.Join(cfg.StateDir, "project-rules", "materializations", projectRuleSHA256([]byte(root)), projectRuleManifestName)
}

func selectCodexProjectInstructions(binding projectBinding) (path, rel string, exists bool, err error) {
	if binding.Dir == "" || !filepath.IsAbs(binding.Dir) || filepath.Clean(binding.Dir) != binding.Dir {
		return "", "", false, fmt.Errorf("%w: invalid project root", errProjectRuleUnsupported)
	}
	rootInfo, err := os.Lstat(binding.Dir)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return "", "", false, fmt.Errorf("%w: unsafe project root", errProjectRuleUnsupported)
	}
	for _, candidate := range []string{"AGENTS.override.md", "AGENTS.md"} {
		p := filepath.Join(binding.Dir, candidate)
		if filepath.Dir(p) != binding.Dir {
			return "", "", false, fmt.Errorf("%w: loader path escaped project", errProjectRuleUnsupported)
		}
		info, statErr := os.Lstat(p)
		if errors.Is(statErr, os.ErrNotExist) {
			if candidate == "AGENTS.override.md" {
				continue
			}
			return p, candidate, false, nil
		}
		if statErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", "", false, fmt.Errorf("%w: unsafe %s", errProjectRuleUnsupported, candidate)
		}
		return p, candidate, true, nil
	}
	panic("unreachable")
}

func readProjectRuleManifest(path string) (projectRuleManifest, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return projectRuleManifest{}, false, nil
	}
	if err != nil {
		return projectRuleManifest{}, false, err
	}
	var m projectRuleManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return projectRuleManifest{}, false, err
	}
	return m, true, nil
}

func validProjectRuleManifest(m projectRuleManifest, binding projectBinding, installationID, client, rel string) bool {
	return m.Schema == projectRuleDecisionSchema && m.InstallationID == installationID && m.Client == client &&
		m.ProjectRoot == binding.Dir && m.Scope == binding.Scope && m.LoaderRelPath == rel &&
		m.FileSHA256 != "" && m.ManagedRegionSHA256 != ""
}

func projectRuleRegion(b []byte) (start, end int, found bool, err error) {
	if bytes.Count(b, []byte(projectRuleBegin)) == 0 && bytes.Count(b, []byte(projectRuleEnd)) == 0 {
		return 0, 0, false, nil
	}
	if bytes.Count(b, []byte(projectRuleBegin)) != 1 || bytes.Count(b, []byte(projectRuleEnd)) != 1 {
		return 0, 0, false, fmt.Errorf("%w: malformed managed region", errProjectRuleConflict)
	}
	start = bytes.Index(b, []byte(projectRuleBegin))
	end = bytes.Index(b, []byte(projectRuleEnd))
	if start < 0 || end < start {
		return 0, 0, false, fmt.Errorf("%w: malformed managed region", errProjectRuleConflict)
	}
	return start, end + len(projectRuleEnd), true, nil
}

func renderProjectRuleRegion(rules []projectRuleRevision) (region []byte, delivered []projectRuleKey, omitted int) {
	sort.Slice(rules, func(i, j int) bool { return rules[i].RuleID < rules[j].RuleID })
	render := func(shown int) []byte {
		var b strings.Builder
		b.WriteString(projectRuleBegin + "\n# Mora owner rules\n")
		for _, r := range rules[:shown] {
			fmt.Fprintf(&b, "\n<!-- mora-rule %s %s -->\n%s\n", r.RuleID, r.RevisionSHA256, strings.TrimSpace(r.Text))
		}
		if shown < len(rules) {
			fmt.Fprintf(&b, "\n<!-- %d owner rule(s) omitted: retrieve them from Mora before acting. -->\n", len(rules)-shown)
		}
		b.WriteString(projectRuleEnd)
		return []byte(b.String())
	}
	for shown := len(rules); shown >= 0; shown-- {
		candidate := render(shown)
		if len(candidate) <= projectRuleMaxBytes {
			keys := make([]projectRuleKey, shown)
			for i := range keys {
				keys[i] = projectRuleKey{RuleID: rules[i].RuleID, RevisionSHA256: rules[i].RevisionSHA256}
			}
			return candidate, keys, len(rules) - shown
		}
	}
	return []byte(projectRuleBegin + "\n" + projectRuleEnd), nil, len(rules)
}

func planProjectRuleMaterialization(cfg Config, binding projectBinding, installationID, client string) (projectRuleMaterializationPlan, error) {
	path, rel, exists, err := selectCodexProjectInstructions(binding)
	if err != nil {
		return projectRuleMaterializationPlan{}, err
	}
	before := []byte(nil)
	if exists {
		before, err = os.ReadFile(path)
		if err != nil {
			return projectRuleMaterializationPlan{}, fmt.Errorf("%w: read loader: %v", errProjectRuleUnsupported, err)
		}
	}
	manifestPath := projectRuleManifestPath(cfg, binding.Dir)
	prior, priorFound, err := readProjectRuleManifest(manifestPath)
	if err != nil {
		return projectRuleMaterializationPlan{}, fmt.Errorf("%w: invalid manifest", errProjectRuleConflict)
	}
	start, end, hasRegion, err := projectRuleRegion(before)
	if err != nil {
		return projectRuleMaterializationPlan{}, err
	}
	if hasRegion {
		if !priorFound || !validProjectRuleManifest(prior, binding, installationID, client, rel) || projectRuleSHA256(before[start:end]) != prior.ManagedRegionSHA256 {
			return projectRuleMaterializationPlan{}, fmt.Errorf("%w: managed region changed or unowned", errProjectRuleConflict)
		}
	} else if priorFound {
		return projectRuleMaterializationPlan{}, fmt.Errorf("%w: manifest exists without owned region", errProjectRuleConflict)
	}
	rules, err := activeProjectRuleRevisions(cfg, binding, installationID, client)
	if err != nil {
		return projectRuleMaterializationPlan{}, err
	}
	plan := projectRuleMaterializationPlan{binding: binding, installation: installationID, client: client, path: path, relPath: rel, existed: exists, before: before, manifestPath: manifestPath}
	if len(rules) == 0 {
		if !hasRegion {
			plan.noop = true
			return plan, nil
		}
		plan.removeManifest = true
		plan.after = append(append([]byte(nil), before[:start]...), before[end:]...)
		plan.removeFile = prior.CreatedFile && bytes.Equal(before, before[start:end])
		return plan, nil
	}
	region, delivered, omitted := renderProjectRuleRegion(rules)
	if hasRegion {
		plan.after = append(append(append([]byte(nil), before[:start]...), region...), before[end:]...)
	} else if len(before) == 0 {
		plan.after = region
	} else {
		plan.after = append(append(append([]byte(nil), before...), '\n'), region...)
	}
	plan.manifest = projectRuleManifest{Schema: projectRuleDecisionSchema, InstallationID: installationID, Client: client, ProjectRoot: binding.Dir, Scope: binding.Scope, LoaderRelPath: rel, FileSHA256: projectRuleSHA256(plan.after), ManagedRegionSHA256: projectRuleSHA256(region), CreatedFile: !exists, Delivered: delivered, Omitted: omitted}
	return plan, nil
}

func commitProjectRuleMaterialization(plan projectRuleMaterializationPlan) error {
	if plan.noop {
		return nil
	}
	path, rel, exists, err := selectCodexProjectInstructions(plan.binding)
	if err != nil || path != plan.path || rel != plan.relPath || exists != plan.existed {
		return fmt.Errorf("%w: loader selection changed", errProjectRuleConflict)
	}
	current := []byte(nil)
	if exists {
		current, err = os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("%w: loader no longer readable", errProjectRuleConflict)
		}
	}
	if !bytes.Equal(current, plan.before) {
		return fmt.Errorf("%w: loader changed before commit", errProjectRuleConflict)
	}
	if plan.removeFile {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("%w: remove loader: %v", errProjectRuleIndeterminate, err)
		}
	} else if err := atomicio.Write(path, plan.after, 0o644); err != nil {
		return fmt.Errorf("%w: write loader: %v", errProjectRuleIndeterminate, err)
	}
	if plan.removeManifest {
		if err := os.Remove(plan.manifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: remove manifest: %v", errProjectRuleIndeterminate, err)
		}
		return nil
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, plan.after) {
		return fmt.Errorf("%w: loader changed during commit", errProjectRuleIndeterminate)
	}
	b, err := json.MarshalIndent(plan.manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicio.Write(plan.manifestPath, append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("%w: write manifest: %v", errProjectRuleIndeterminate, err)
	}
	return nil
}

// reconcileProjectRuleMaterialization removes a now-stale source revision from
// native projection, or updates the projection after an activation. It is a
// narrow filesystem reconciliation seam; authority and decision ordering stay
// with the native-proof integrator.
func reconcileProjectRuleMaterialization(cfg Config, binding projectBinding, installationID, client string) error {
	plan, err := planProjectRuleMaterialization(cfg, binding, installationID, client)
	if err != nil {
		return err
	}
	return commitProjectRuleMaterialization(plan)
}

// reconcileRetiredProjectRuleMaterialization is called only after the trusted
// authority path has appended a retire decision. A cleanup error cannot widen
// state again: loadProjectRuleState already excludes the rule from fallback.
func reconcileRetiredProjectRuleMaterialization(cfg Config, binding projectBinding, installationID, client string) error {
	if err := reconcileProjectRuleMaterialization(cfg, binding, installationID, client); err != nil {
		return fmt.Errorf("%w: %v", errProjectRuleRetiredWithLoaderConflict, err)
	}
	return nil
}

// verifiedProjectRuleDelivery returns no keys on every stale or foreign state,
// keeping the normal project brief as the safe fallback.
func verifiedProjectRuleDelivery(cfg Config, binding projectBinding, installationID, client string) []projectRuleKey {
	path, rel, exists, err := selectCodexProjectInstructions(binding)
	if err != nil || !exists {
		return nil
	}
	m, found, err := readProjectRuleManifest(projectRuleManifestPath(cfg, binding.Dir))
	if err != nil || !found || !validProjectRuleManifest(m, binding, installationID, client, rel) || m.Omitted != 0 {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil || projectRuleSHA256(b) != m.FileSHA256 {
		return nil
	}
	start, end, found, err := projectRuleRegion(b)
	if err != nil || !found || projectRuleSHA256(b[start:end]) != m.ManagedRegionSHA256 {
		return nil
	}
	rules, err := activeProjectRuleRevisions(cfg, binding, installationID, client)
	if err != nil || len(rules) != len(m.Delivered) {
		return nil
	}
	want := make([]projectRuleKey, len(rules))
	for i, r := range rules {
		want[i] = projectRuleKey{RuleID: r.RuleID, RevisionSHA256: r.RevisionSHA256}
	}
	if !sameProjectRuleKeys(want, m.Delivered) {
		return nil
	}
	return append([]projectRuleKey(nil), m.Delivered...)
}

func sameProjectRuleKeys(a, b []projectRuleKey) bool {
	if len(a) != len(b) {
		return false
	}
	copyAndSort := func(in []projectRuleKey) []projectRuleKey {
		out := append([]projectRuleKey(nil), in...)
		sort.Slice(out, func(i, j int) bool {
			if out[i].RuleID == out[j].RuleID {
				return out[i].RevisionSHA256 < out[j].RevisionSHA256
			}
			return out[i].RuleID < out[j].RuleID
		})
		return out
	}
	aa, bb := copyAndSort(a), copyAndSort(b)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}
