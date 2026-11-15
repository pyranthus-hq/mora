package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

// ContenderInput is what the runner delivers to a contender process.
type ContenderInput struct {
	CaseID        string
	Role          string
	ConditionKind string
	RepIndex      int
	Workspace     Layout
	Exposure      ExposureRecord
	Harness       contract.HarnessConfig
	Timeout       time.Duration
}

// ContenderOutput is distinct I/O preserved by the runner (not scored).
type ContenderOutput struct {
	StdoutRef     string
	StderrRef     string
	ArtifactRefs  []string
	ExitClass     string // "ok" | "task_failure" | "malformed" | "timeout" | "unavailable" | "error"
	ExitCode      int
	UsageTokens   int
	CostUSDMicros int64
	Unknowns      []string
	Notes         string
}

// Contender runs one attempt inside the disposable workspace.
type Contender interface {
	Class() string
	Run(ctx context.Context, in ContenderInput) (ContenderOutput, error)
}

// SyntheticContender is a deterministic fake contender for offline tests.
// It never makes network / paid provider calls. It preserves distinct I/O
// for the three required case roles and does not score efficacy.
type SyntheticContender struct {
	// BehaviorOverride forces a specific ExitClass (for negative tests).
	BehaviorOverride string
	// Hang pretends to block until ctx cancellation (timeout tests).
	Hang bool
}

func (s *SyntheticContender) Class() string { return ContenderSyntheticLocal }

func (s *SyntheticContender) Run(ctx context.Context, in ContenderInput) (ContenderOutput, error) {
	if s.Hang {
		select {
		case <-ctx.Done():
			return ContenderOutput{
				ExitClass: "timeout",
				ExitCode:  -1,
				Unknowns:  []string{"timed_out"},
				Notes:     "synthetic hang interrupted by timeout",
			}, errf(CodeTimeout, "contender", "timed out")
		case <-time.After(24 * time.Hour):
			// unreachable in tests
		}
	}
	if s.BehaviorOverride != "" {
		return s.writeOutput(in, s.BehaviorOverride, map[string]any{
			"override": s.BehaviorOverride,
		})
	}

	// Distinct I/O by role — runner records, does not score.
	payload := map[string]any{
		"case_id":         in.CaseID,
		"role":            in.Role,
		"condition_kind":  in.ConditionKind,
		"rep_index":       in.RepIndex,
		"exposure_digest": in.Exposure.DeliveredDigest,
		"injected_memory": in.Exposure.InjectedMemoryDigest,
		"tool_results":    in.Exposure.ToolResultStatus,
		"harness_id":      in.Harness.HarnessID,
		"model_id":        in.Harness.ModelID,
	}

	switch in.Role {
	case contract.CaseRoleFailure:
		payload["task_result"] = "known_task_failure"
		payload["detail"] = "synthetic: contender produces a known task failure outcome"
		return s.writeOutput(in, "task_failure", payload)
	case contract.CaseRoleValidMemoryControl:
		payload["task_result"] = "valid_memory_constraint_present"
		payload["constraint_echo"] = "Idempotency-Key must survive edit condition"
		payload["detail"] = "synthetic: valid-memory control preserves distinct constraint-bearing I/O"
		return s.writeOutput(in, "ok", payload)
	case contract.CaseRoleNoRelevantMemoryCtrl:
		payload["task_result"] = "no_relevant_memory_task"
		payload["detail"] = "synthetic: no-relevant-memory task completes without requiring memory edit"
		return s.writeOutput(in, "ok", payload)
	default:
		payload["task_result"] = "unknown_role"
		return s.writeOutput(in, "error", payload)
	}
}

func (s *SyntheticContender) writeOutput(in ContenderInput, exitClass string, payload map[string]any) (ContenderOutput, error) {
	artDir := filepath.Join(in.Workspace.Artifacts, sanitizeID(in.CaseID), sanitizeID(in.ConditionKind), formatRep(in.RepIndex))
	if err := os.MkdirAll(artDir, 0o755); err != nil {
		return ContenderOutput{}, errf(CodeWorkspace, "artifacts", "%v", err)
	}
	stdoutPath := filepath.Join(artDir, "stdout.json")
	stderrPath := filepath.Join(artDir, "stderr.txt")
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return ContenderOutput{}, errf(CodeMalformedOutput, "stdout", "%v", err)
	}
	if err := os.WriteFile(stdoutPath, b, 0o644); err != nil {
		return ContenderOutput{}, errf(CodeWorkspace, "stdout", "%v", err)
	}
	if err := os.WriteFile(stderrPath, []byte("synthetic stderr: "+exitClass+"\n"), 0o644); err != nil {
		return ContenderOutput{}, errf(CodeWorkspace, "stderr", "%v", err)
	}
	// Contender may write observable files under contender workspace only.
	obs := filepath.Join(in.Workspace.Contender, "output", "result.json")
	if err := os.MkdirAll(filepath.Dir(obs), 0o755); err != nil {
		return ContenderOutput{}, errf(CodeWorkspace, "contender_out", "%v", err)
	}
	if err := os.WriteFile(obs, b, 0o644); err != nil {
		return ContenderOutput{}, errf(CodeWorkspace, "contender_out", "%v", err)
	}

	exitCode := 0
	if exitClass == "task_failure" || exitClass == "error" || exitClass == "malformed" {
		exitCode = 1
	}
	if exitClass == "timeout" || exitClass == "unavailable" {
		exitCode = -1
	}
	return ContenderOutput{
		StdoutRef:     stdoutPath,
		StderrRef:     stderrPath,
		ArtifactRefs:  []string{obs},
		ExitClass:     exitClass,
		ExitCode:      exitCode,
		UsageTokens:   0,
		CostUSDMicros: 0,
		Unknowns:      []string{},
		Notes:         "synthetic local contender; not an efficacy judgment",
	}, nil
}

func formatRep(i int) string {
	return "rep-" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}

// ExternalProviderStub records whether Start was attempted. Ordinary tests
// assert StartCount remains 0 on gate denial. This stub never spends money
// and never opens a network connection.
type ExternalProviderStub struct {
	StartCount atomic.Int64
}

func (e *ExternalProviderStub) Class() string { return ContenderExternalProvider }

func (e *ExternalProviderStub) Run(ctx context.Context, in ContenderInput) (ContenderOutput, error) {
	e.StartCount.Add(1)
	return ContenderOutput{
		ExitClass: "error",
		ExitCode:  1,
		Unknowns:  []string{"external_provider_disabled"},
		Notes:     "stub was started — this must not happen in ordinary gate-denied tests",
	}, errf(CodeProviderBlocked, "provider", "external provider stub must not run in ordinary tests")
}

// StartCountValue returns how many times Run was entered.
func (e *ExternalProviderStub) StartCountValue() int64 { return e.StartCount.Load() }
