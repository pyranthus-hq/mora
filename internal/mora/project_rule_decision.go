package mora

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/leasefile"
)

const projectRuleDecisionSchema = "project-rule/v1"

type projectRuleDecision struct {
	Schema, Action, RuleID, SourceMemoryID, Scope, RevisionSHA256 string
	InstallationID, KeyID, Client, ProjectRoot, LoaderRelPath     string
	ChallengeNonce, DisplayDigest, VerifiedAt                     string
}

type projectRuleState struct {
	Active map[string]projectRuleDecision
}

func projectRuleDecisionPath(cfg Config) string {
	return filepath.Join(cfg.StateDir, "project-rules", "decisions.jsonl")
}

func validProjectRuleDecision(d projectRuleDecision) bool {
	if d.Schema != projectRuleDecisionSchema || (d.Action != "activate" && d.Action != "retire") {
		return false
	}
	return d.RuleID != "" && d.SourceMemoryID != "" && d.Scope != "" && d.RevisionSHA256 != "" &&
		d.InstallationID != "" && d.KeyID != "" && d.Client == "codex" && d.ProjectRoot != "" &&
		d.LoaderRelPath != "" && d.ChallengeNonce != "" && d.DisplayDigest != "" && d.VerifiedAt != ""
}

// appendProjectRuleDecision is intentionally local state only. The authority
// integrator calls it only after it has verified the native signed challenge.
func appendProjectRuleDecision(cfg Config, d projectRuleDecision) error {
	if !validProjectRuleDecision(d) {
		return errors.New("invalid project rule decision")
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return leasefile.WithGuard(projectRuleDecisionPath(cfg)+".lock", func() error {
		return appendProjectRuleDecisionDurable(projectRuleDecisionPath(cfg), append(b, '\n'))
	})
}

// appendProjectRuleDecisionDurable is append-only under the caller's lease.
// Retire relies on this barrier: a crash after it returns must not resurrect a
// rule through a brief merely because loader cleanup has not run yet.
func appendProjectRuleDecisionDurable(path string, line []byte) (err error) {
	if existing, readErr := os.ReadFile(path); readErr == nil && len(existing) > 0 && existing[len(existing)-1] != '\n' {
		return errors.New("project rule decision log has a torn final row")
	} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if err := validateProjectRuleDecisionLog(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	if _, err := f.Write(line); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return atomicio.SyncDir(filepath.Dir(path))
}

func validateProjectRuleDecisionLog(path string) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1024), 1<<20)
	for s.Scan() {
		var d projectRuleDecision
		if json.Unmarshal(s.Bytes(), &d) != nil || !validProjectRuleDecision(d) {
			return errors.New("project rule decision log contains an invalid row")
		}
	}
	return s.Err()
}

func loadProjectRuleState(cfg Config, binding projectBinding, installationID, client string) (projectRuleState, error) {
	state := projectRuleState{Active: make(map[string]projectRuleDecision)}
	if binding.Dir == "" || binding.Scope == "" || installationID == "" || client != "codex" {
		return state, errors.New("invalid project rule state identity")
	}
	f, err := os.Open(projectRuleDecisionPath(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	// A single decision is deliberately small. Any malformed row is a refusal,
	// not an invitation to resurrect an earlier activation.
	s.Buffer(make([]byte, 1024), 1<<20)
	last := make(map[string]projectRuleDecision)
	for s.Scan() {
		var d projectRuleDecision
		if json.Unmarshal(s.Bytes(), &d) != nil || !validProjectRuleDecision(d) {
			return state, errors.New("project rule decision log contains an invalid row")
		}
		if d.InstallationID != installationID || d.Client != client || d.ProjectRoot != binding.Dir || d.Scope != binding.Scope {
			continue
		}
		last[d.RuleID] = d
	}
	if err := s.Err(); err != nil {
		return state, fmt.Errorf("read project rule decisions: %w", err)
	}
	for id, d := range last {
		if d.Action == "activate" {
			state.Active[id] = d
		}
	}
	return state, nil
}

// activeProjectRuleRevisions refuses stale, retired, changed, or unconfirmed
// source records even when an old local activation row remains on disk.
func activeProjectRuleRevisions(cfg Config, binding projectBinding, installationID, client string) ([]projectRuleRevision, error) {
	state, err := loadProjectRuleState(cfg, binding, installationID, client)
	if err != nil {
		return nil, err
	}
	_, selectedLoader, _, err := selectCodexProjectInstructions(binding)
	if err != nil {
		return nil, err
	}
	rules := make([]projectRuleRevision, 0, len(state.Active))
	for _, d := range state.Active {
		// A newly preferred override changes the signed activation target. Do
		// not carry an old opt-in onto it; the owner must activate again.
		if d.LoaderRelPath != selectedLoader {
			continue
		}
		r, err := loadProjectRuleRevision(cfg, binding, d.SourceMemoryID)
		if err != nil || r.RuleID != d.RuleID || r.RevisionSHA256 != d.RevisionSHA256 {
			continue
		}
		rules = append(rules, r)
	}
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].RuleID == rules[j].RuleID {
			return rules[i].RevisionSHA256 < rules[j].RevisionSHA256
		}
		return rules[i].RuleID < rules[j].RuleID
	})
	return rules, nil
}
