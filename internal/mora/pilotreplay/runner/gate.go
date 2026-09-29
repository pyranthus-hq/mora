package runner

import (
	"time"

	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

// ContenderClass distinguishes local synthetic plumbing from external providers.
const (
	ContenderSyntheticLocal   = "synthetic_local"
	ContenderExternalProvider = "external_provider"
)

// Budget tracks spend / run / time ceilings for a session.
type Budget struct {
	CeilingUSDMicros int64
	SpentUSDMicros   int64
	MaxRuns          int
	RunsUsed         int
	MaxWallSeconds   int
	WallSecondsUsed  int
}

// GateDecision is the result of pre-provider authorization.
type GateDecision struct {
	Allowed          bool
	ProviderMayStart bool
	Reason           string
	ErrorCode        string
}

// EvaluateGate checks whether an external provider may start. Synthetic local
// contenders do not require spend authorization (plumbing only) but still
// honor explicit run/time ceilings when set on the session budget.
func EvaluateGate(gate contract.RunGate, class string, budget Budget) GateDecision {
	switch class {
	case ContenderSyntheticLocal:
		if budget.MaxRuns > 0 && budget.RunsUsed >= budget.MaxRuns {
			return GateDecision{Allowed: false, ProviderMayStart: false, Reason: "run ceiling exhausted", ErrorCode: CodeBudgetExhausted}
		}
		if budget.MaxWallSeconds > 0 && budget.WallSecondsUsed >= budget.MaxWallSeconds {
			return GateDecision{Allowed: false, ProviderMayStart: false, Reason: "time ceiling exhausted", ErrorCode: CodeBudgetExhausted}
		}
		return GateDecision{Allowed: true, ProviderMayStart: false, Reason: "synthetic local contender; no provider invocation"}
	case ContenderExternalProvider:
		if err := gate.AuthorizeProviderInvocation(); err != nil {
			return GateDecision{Allowed: false, ProviderMayStart: false, Reason: err.Error(), ErrorCode: CodeGateDenied}
		}
		if budget.CeilingUSDMicros <= 0 {
			return GateDecision{Allowed: false, ProviderMayStart: false, Reason: "monetary ceiling missing", ErrorCode: CodeGateDenied}
		}
		if budget.SpentUSDMicros >= budget.CeilingUSDMicros {
			return GateDecision{Allowed: false, ProviderMayStart: false, Reason: "monetary ceiling exhausted", ErrorCode: CodeBudgetExhausted}
		}
		if budget.MaxRuns > 0 && budget.RunsUsed >= budget.MaxRuns {
			return GateDecision{Allowed: false, ProviderMayStart: false, Reason: "run ceiling exhausted", ErrorCode: CodeBudgetExhausted}
		}
		if budget.MaxWallSeconds > 0 && budget.WallSecondsUsed >= budget.MaxWallSeconds {
			return GateDecision{Allowed: false, ProviderMayStart: false, Reason: "time ceiling exhausted", ErrorCode: CodeBudgetExhausted}
		}
		// Even with an authorized shape, this package never starts a real paid
		// provider. Ordinary product commands and tests remain spend-free.
		return GateDecision{
			Allowed:          false,
			ProviderMayStart: false,
			Reason:           "external provider execution is disabled in this runner package; requires #545 authorization and a separate adapter",
			ErrorCode:        CodeProviderBlocked,
		}
	default:
		return GateDecision{Allowed: false, ProviderMayStart: false, Reason: "unknown contender class", ErrorCode: CodeGateDenied}
	}
}

// TimeoutFor returns the attempt timeout, preferring the gate policy default.
func TimeoutFor(gate contract.RunGate, overrideSeconds int) time.Duration {
	sec := gate.Policy.DefaultTimeoutSeconds
	if overrideSeconds > 0 {
		sec = overrideSeconds
	}
	if sec <= 0 {
		sec = 60
	}
	return time.Duration(sec) * time.Second
}
