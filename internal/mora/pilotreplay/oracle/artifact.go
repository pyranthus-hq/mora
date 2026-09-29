package oracle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// OutputArtifact is the copied/sanitized contender output the evaluator grades.
// It must not embed gold labels, hidden test bodies, reference patches or
// post-cutoff evidence. Those live only in HiddenSpec (evaluator-only).
type OutputArtifact struct {
	Schema        string `json:"schema"`
	SchemaVersion int    `json:"schema_version"`
	ArtifactID    string `json:"artifact_id"`
	CaseRole      string `json:"case_role"`
	// TaskMarkers are executable key/value checks the grader inspects.
	TaskMarkers map[string]string `json:"task_markers"`
	// FileContents are synthetic relative-path bodies (no absolute private paths).
	FileContents map[string]string `json:"file_contents"`
	Meta         ArtifactMeta      `json:"meta"`
}

// ArtifactMeta records provenance of the artifact copy.
type ArtifactMeta struct {
	ProducedBy    string   `json:"produced_by"` // "synthetic_fixture" | "contender_copy"
	ContenderRefs []string `json:"contender_refs,omitempty"`
	Notes         string   `json:"notes,omitempty"`
}

// NewOutputArtifact returns an empty artifact with schema header filled.
func NewOutputArtifact() OutputArtifact {
	return OutputArtifact{
		Schema:        ArtifactSchema,
		SchemaVersion: ArtifactVer,
		TaskMarkers:   map[string]string{},
		FileContents:  map[string]string{},
		Meta: ArtifactMeta{
			ProducedBy:    "synthetic_fixture",
			ContenderRefs: []string{},
		},
	}
}

// ValidateShape checks schema/version and basic field hygiene. It does not
// grade. Unsupported shapes are rejected so Grade can map them to inconclusive.
func (a *OutputArtifact) ValidateShape() error {
	if a == nil {
		return errf(CodeMissingArtifact, "artifact", "artifact is nil")
	}
	if a.Schema == "" {
		return errf(CodeMalformedArtifact, "schema", "schema is required")
	}
	if a.Schema != ArtifactSchema {
		return errf(CodeUnsupportedArtifact, "schema", "want %q, got %q", ArtifactSchema, a.Schema)
	}
	if a.SchemaVersion != ArtifactVer {
		return errf(CodeUnsupportedArtifact, "schema_version", "want %d, got %d", ArtifactVer, a.SchemaVersion)
	}
	if strings.TrimSpace(a.ArtifactID) == "" {
		return errf(CodeMalformedArtifact, "artifact_id", "required")
	}
	if strings.TrimSpace(a.CaseRole) == "" {
		return errf(CodeMalformedArtifact, "case_role", "required")
	}
	if a.TaskMarkers == nil {
		return errf(CodeMalformedArtifact, "task_markers", "must be present (may be empty map)")
	}
	if a.FileContents == nil {
		return errf(CodeMalformedArtifact, "file_contents", "must be present (may be empty map)")
	}
	for _, p := range sortedKeys(a.FileContents) {
		if err := rejectPrivatePath(p); err != nil {
			return err
		}
	}
	// Contender-copied artifacts must not smuggle gold-looking keys.
	for _, k := range sortedKeys(a.TaskMarkers) {
		lk := strings.ToLower(k)
		for _, bad := range goldKeySubstrings() {
			if strings.Contains(lk, bad) {
				return errf(CodeGoldLeak, "task_markers."+k, "artifact must not embed gold/oracle keys")
			}
		}
	}
	return nil
}

func rejectPrivatePath(p string) error {
	if p == "" {
		return errf(CodeMalformedArtifact, "file_contents", "empty path")
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/Users/") || strings.HasPrefix(p, "/home/") || strings.Contains(p, "Library/") {
		return errf(CodeMalformedArtifact, "file_contents", "absolute private host paths forbidden: %q", p)
	}
	if strings.Contains(p, "..") {
		return errf(CodeMalformedArtifact, "file_contents", "path escape forbidden: %q", p)
	}
	return nil
}

func goldKeySubstrings() []string {
	return []string{
		"gold",
		"expected_answer",
		"hidden_test",
		"reference_patch",
		"post_cutoff",
		"oracle_spec",
		"hidden_spec",
	}
}

// Digest returns SHA-256 hex of the canonical JSON encoding.
func (a OutputArtifact) Digest() (string, error) {
	return canonicalDigest(a)
}

// EvaluatorEnv is a separate directory holding a sanitized artifact copy for
// grading. Contender and repair processes must not read this path.
type EvaluatorEnv struct {
	Root         string
	ArtifactPath string
	Artifact     OutputArtifact
	ArtifactDig  string
}

// PrepareEvaluatorEnv copies and sanitizes the artifact into a fresh directory.
// Gold / hidden material is never written here — only the contender-visible
// output shape.
func PrepareEvaluatorEnv(parent string, art OutputArtifact) (EvaluatorEnv, error) {
	if err := art.ValidateShape(); err != nil {
		return EvaluatorEnv{}, err
	}
	root, err := os.MkdirTemp(parent, "pilotreplay-oracle-eval-*")
	if err != nil {
		return EvaluatorEnv{}, errf(CodeInternal, "evaluator_env", "mkdir: %v", err)
	}
	sanitized := sanitizeArtifact(art)
	b, err := json.MarshalIndent(sanitized, "", "  ")
	if err != nil {
		_ = os.RemoveAll(root)
		return EvaluatorEnv{}, errf(CodeInternal, "evaluator_env", "marshal: %v", err)
	}
	path := filepath.Join(root, "artifact.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		_ = os.RemoveAll(root)
		return EvaluatorEnv{}, errf(CodeInternal, "evaluator_env", "write: %v", err)
	}
	// Also materialize file_contents under files/ for executable path checks.
	filesRoot := filepath.Join(root, "files")
	for _, rel := range sortedKeys(sanitized.FileContents) {
		dst := filepath.Join(filesRoot, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			_ = os.RemoveAll(root)
			return EvaluatorEnv{}, errf(CodeInternal, "evaluator_env", "mkdir files: %v", err)
		}
		if err := os.WriteFile(dst, []byte(sanitized.FileContents[rel]), 0o600); err != nil {
			_ = os.RemoveAll(root)
			return EvaluatorEnv{}, errf(CodeInternal, "evaluator_env", "write file: %v", err)
		}
	}
	dig := sha256.Sum256(b)
	return EvaluatorEnv{
		Root:         root,
		ArtifactPath: path,
		Artifact:     sanitized,
		ArtifactDig:  hex.EncodeToString(dig[:]),
	}, nil
}

// Close removes the evaluator environment. Safe to call multiple times.
func (e *EvaluatorEnv) Close() error {
	if e == nil || e.Root == "" {
		return nil
	}
	err := os.RemoveAll(e.Root)
	e.Root = ""
	return err
}

// sanitizeArtifact strips any accidental forbidden keys and clones maps.
func sanitizeArtifact(in OutputArtifact) OutputArtifact {
	out := NewOutputArtifact()
	out.ArtifactID = in.ArtifactID
	out.CaseRole = in.CaseRole
	out.Meta = ArtifactMeta{
		ProducedBy:    in.Meta.ProducedBy,
		ContenderRefs: append([]string(nil), in.Meta.ContenderRefs...),
		Notes:         in.Meta.Notes,
	}
	for _, k := range sortedKeys(in.TaskMarkers) {
		lk := strings.ToLower(k)
		skip := false
		for _, bad := range goldKeySubstrings() {
			if strings.Contains(lk, bad) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		out.TaskMarkers[k] = in.TaskMarkers[k]
	}
	for _, k := range sortedKeys(in.FileContents) {
		out.FileContents[k] = in.FileContents[k]
	}
	return out
}

// LoadArtifactJSON loads an artifact from evaluator-env bytes.
func LoadArtifactJSON(b []byte) (OutputArtifact, error) {
	if len(b) == 0 {
		return OutputArtifact{}, errf(CodeMissingArtifact, "artifact", "empty bytes")
	}
	var a OutputArtifact
	if err := json.Unmarshal(b, &a); err != nil {
		return OutputArtifact{}, errf(CodeMalformedArtifact, "artifact", "json: %v", err)
	}
	if a.TaskMarkers == nil {
		a.TaskMarkers = map[string]string{}
	}
	if a.FileContents == nil {
		a.FileContents = map[string]string{}
	}
	if err := a.ValidateShape(); err != nil {
		return OutputArtifact{}, err
	}
	return a, nil
}
