package runner

import (
	"time"

	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

// SessionReceipt aggregates offline synthetic runner validation. It must not
// claim historical efficacy.
type SessionReceipt struct {
	Header           contract.Header   `json:"header"`
	ReceiptID        string            `json:"receipt_id"`
	GeneratedAt      string            `json:"generated_at"`
	AttemptIDs       []string          `json:"attempt_ids"`
	ExampleHashes    map[string]string `json:"example_hashes"`
	TestsPassed      bool              `json:"tests_passed"`
	UnresolvedLimits []string          `json:"unresolved_limits"`
	ClaimsEfficacy   bool              `json:"claims_efficacy"`
	Notes            string            `json:"notes,omitempty"`
}

// BuildSessionReceipt hashes key fixtures and records attempt ids.
func BuildSessionReceipt(r *Runner, testsPassed bool) (SessionReceipt, error) {
	out := SessionReceipt{
		Header: contract.Header{
			Schema:        "mora.pilotreplay.runner_receipt",
			SchemaVersion: contract.SchemaVersion,
		},
		ReceiptID:        "receipt-synth-runner-v1",
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		AttemptIDs:       []string{},
		ExampleHashes:    map[string]string{},
		TestsPassed:      testsPassed,
		UnresolvedLimits: append([]string{}, r.IsolationLimits()...),
		ClaimsEfficacy:   false,
		Notes:            "Offline runner receipt. Infrastructure readiness only — not a historical efficacy claim.",
	}
	for _, rec := range r.Receipts() {
		out.AttemptIDs = append(out.AttemptIDs, rec.Attempt.AttemptID)
	}
	examples := map[string]any{
		"synth_failure_case":       SynthFailureCase(),
		"synth_valid_memory_case":  SynthValidMemoryCase(),
		"synth_no_relevant_memory": SynthNoRelevantMemoryCase(),
		"conditions_failure":       ConditionsFor(SynthFailureCase().CaseID),
		"denied_gate":              DeniedGate(),
		"synthetic_plumbing_gate":  SyntheticPlumbingGate(),
	}
	for name, ex := range examples {
		h, err := contract.CanonicalJSONDigest(ex)
		if err != nil {
			return SessionReceipt{}, err
		}
		out.ExampleHashes[name] = h
	}
	return out, nil
}
