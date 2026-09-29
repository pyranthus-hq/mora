package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

// AdmittedPackage is a validated private case package that stays outside
// public fixtures and default logs. Contents are staged only under the
// disposable workspace PrivateAdm directory.
type AdmittedPackage struct {
	SourcePath string
	Case       contract.CaseDocument
	StagedDir  string // under workspace PrivateAdm; empty if synthetic in-memory
	Synthetic  bool
}

// AdmitOptions controls private-package admission.
type AdmitOptions struct {
	// SourcePath is a caller-selected private directory containing case.json.
	// Optional when Case is provided in-memory (synthetic fixtures).
	SourcePath string
	// Case, when non-nil, is validated in-memory without reading SourcePath.
	Case *contract.CaseDocument
	// AllowSynthetic permits synthetic plumbing cases (not historical attribution).
	AllowSynthetic bool
}

// AdmitCasePackage validates a caller-selected case before any process start.
// It never copies the package into public fixtures or default logs.
func (r *Runner) AdmitCasePackage(opts AdmitOptions) (AdmittedPackage, error) {
	var c contract.CaseDocument
	var staged string
	synthetic := false

	switch {
	case opts.Case != nil:
		c = *opts.Case
		synthetic = c.MemorySnapshot.Synthetic || c.DeliveredContext.Synthetic
	case opts.SourcePath != "":
		if looksLikeProductionPath(opts.SourcePath) {
			return AdmittedPackage{}, errf(CodePathRejected, "source_path", "refusing production-looking private package path")
		}
		// Reject paths that look like public fixture trees.
		clean := filepath.Clean(opts.SourcePath)
		if strings.Contains(clean, "docs/experiments/memory-replay") ||
			strings.Contains(clean, "internal/mora/pilotreplay/cases") ||
			strings.Contains(clean, "internal/mora/pilotreplay/contract") {
			return AdmittedPackage{}, errf(CodeAdmitReject, "source_path", "refusing to admit from public fixture/docs trees; pass a private path")
		}
		casePath := filepath.Join(opts.SourcePath, "case.json")
		b, err := os.ReadFile(casePath)
		if err != nil {
			return AdmittedPackage{}, errf(CodeAdmitReject, "case.json", "cannot read: %v", err)
		}
		if err := json.Unmarshal(b, &c); err != nil {
			return AdmittedPackage{}, errf(CodeAdmitReject, "case.json", "malformed: %v", err)
		}
		synthetic = c.MemorySnapshot.Synthetic || c.DeliveredContext.Synthetic
		// Stage under disposable PrivateAdm only (not public fixtures / default logs).
		staged = filepath.Join(r.ws.Layout.PrivateAdm, sanitizeID(c.CaseID))
		if err := os.MkdirAll(staged, 0o755); err != nil {
			return AdmittedPackage{}, errf(CodeAdmitReject, "stage", "%v", err)
		}
		if err := copyDir(opts.SourcePath, staged); err != nil {
			return AdmittedPackage{}, err
		}
	default:
		return AdmittedPackage{}, errf(CodeAdmitReject, "opts", "Case or SourcePath is required")
	}

	if err := c.Validate(); err != nil {
		return AdmittedPackage{}, errf(CodeAdmitReject, "case", "contract validation failed: %v", err)
	}
	if c.RunnerPackage.ContainsHiddenTests ||
		c.RunnerPackage.ContainsExpectedAnswers ||
		c.RunnerPackage.ContainsReferencePatch ||
		c.RunnerPackage.ContainsPostCutoff {
		return AdmittedPackage{}, errf(CodeOracleLeak, "runner_package", "hidden/oracle/post-cutoff material must not be contender-visible")
	}
	if !c.OraclePackage.InaccessibleToContender || !c.OraclePackage.InaccessibleToRepair {
		return AdmittedPackage{}, errf(CodeOracleLeak, "oracle_package", "oracle must be inaccessible to contender and repair")
	}
	if synthetic && !opts.AllowSynthetic {
		return AdmittedPackage{}, errf(CodeAdmitReject, "synthetic", "synthetic case requires AllowSynthetic")
	}
	if !synthetic && c.Eligibility.EligibleForHistoricalAttribution {
		// Real historical cases stay blocked until #545 authorization.
		r.isolationLimits = appendUnique(r.isolationLimits,
			"Real private-case execution remains blocked until case eligibility, oracle readiness and explicit execution/spend authorization (#545).")
	}
	return AdmittedPackage{
		SourcePath: opts.SourcePath,
		Case:       c,
		StagedDir:  staged,
		Synthetic:  synthetic,
	}, nil
}

func sanitizeID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "unnamed"
	}
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func appendUnique(ss []string, s string) []string {
	for _, x := range ss {
		if x == s {
			return ss
		}
	}
	return append(ss, s)
}
