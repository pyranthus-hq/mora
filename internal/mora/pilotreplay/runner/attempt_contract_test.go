package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/mora/pilotreplay/contract"
)

func TestTrialReceiptContract(t *testing.T) {
	for _, tt := range []struct {
		name, status, code string
		setup              func(*testing.T, *Runner, *TrialRequest)
	}{
		{"gate_denial", contract.AttemptSkipped, CodeGateDenied, func(t *testing.T, r *Runner, req *TrialRequest) {
			req.Gate = DeniedGate()
			req.Contender = &ExternalProviderStub{}
		}},
		{"ceiling_exhausted", contract.AttemptSkipped, CodeBudgetExhausted, func(t *testing.T, r *Runner, req *TrialRequest) {
			r.budget.RunsUsed = r.budget.MaxRuns
		}},
		{"reset_bleed", contract.AttemptFailed, CodeResetBleed, func(t *testing.T, r *Runner, req *TrialRequest) {
			if err := os.WriteFile(filepath.Join(r.ws.preserved, "vault", "bleed"), []byte("synthetic bleed"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"admit_reject", contract.AttemptFailed, CodeAdmitReject, func(t *testing.T, r *Runner, req *TrialRequest) {
			req.Case.Role = "invalid-role"
		}},
		{"inject_failure", contract.AttemptFailed, CodeAdmitReject, func(t *testing.T, r *Runner, req *TrialRequest) {
			req.Condition.CaseID = "another-case"
		}},
		{"oracle_leak", contract.AttemptFailed, CodeOracleLeak, func(t *testing.T, r *Runner, req *TrialRequest) {
			req.SkipReset = true
			if err := os.WriteFile(filepath.Join(r.ws.Layout.Contender, "oracle.txt"), []byte("synthetic leak"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"normal", contract.AttemptFailed, "task_failure", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestRunner(t)
			c := SynthFailureCase()
			req := TrialRequest{Case: c, Condition: ConditionsFor(c.CaseID)[0], Gate: SyntheticPlumbingGate()}
			if tt.setup != nil {
				tt.setup(t, r, &req)
			}
			before := time.Now().UTC().Truncate(time.Second)
			rec, err := r.RunTrial(context.Background(), req)
			after := time.Now().UTC()
			if tt.name == "normal" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("expected refusal error")
			}
			if rec.Attempt.Status != tt.status || rec.Attempt.ErrorCode != tt.code {
				t.Fatalf("status/code = %s/%s, want %s/%s (err=%v)", rec.Attempt.Status, rec.Attempt.ErrorCode, tt.status, tt.code, err)
			}
			// Verify the durable evidence as well as the caller's receipt.
			data, err := os.ReadFile(filepath.Join(r.ws.Layout.Artifacts, "receipts", sanitizeID(rec.Attempt.AttemptID)+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var saved AttemptReceipt
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			for _, rec := range []AttemptReceipt{rec, saved} {
				if err := rec.Attempt.Validate(); err != nil {
					t.Errorf("attempt contract: %v", err)
				}
				if rec.Attempt.Status != tt.status || rec.Attempt.ErrorCode != tt.code {
					t.Errorf("saved status/code changed: %+v", rec.Attempt)
				}
				start, startErr := time.Parse(time.RFC3339, rec.Attempt.StartedAt)
				finish, finishErr := time.Parse(time.RFC3339, rec.Attempt.FinishedAt)
				if startErr != nil || finishErr != nil {
					t.Errorf("invalid trial timestamps: started_at=%q finished_at=%q", rec.Attempt.StartedAt, rec.Attempt.FinishedAt)
					continue
				}
				if start.Format(time.RFC3339) != start.UTC().Format(time.RFC3339) || finish.Format(time.RFC3339) != finish.UTC().Format(time.RFC3339) || start.Before(before) || finish.After(after) || finish.Before(start) {
					t.Errorf("timestamps must be UTC, ordered, and within this trial: %s .. %s", start, finish)
				}
			}
		})
	}
}
