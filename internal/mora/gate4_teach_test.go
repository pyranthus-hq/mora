package mora

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func gate4CommitmentMemory() Memory {
	return Memory{
		ID: "gmail_thread/gate4", Scope: "global", Type: "email",
		Title: "Reviewer list", Source: "gate4", Provider: "gmail", ProviderID: "gate4",
		CreatedAt: "2026-07-20T10:00:00Z",
		Text:      "From: Other <other@example.com>\n\nCan you send the reviewer list?",
		Meta: map[string]any{
			"from": []string{"other@example.com"},
			"to":   []string{"self@example.com"},
			"messages": []commitmentMessageEvidence{{
				MessageRef: "gmail_thread/gate4#msg-1",
				Sender:     "other@example.com", To: []string{"self@example.com"},
				At: "2026-07-20T10:00:00Z", BlockRefs: []string{"body"},
			}},
		},
	}
}

func gate4CommitmentCfg(t *testing.T) (Config, Commitment) {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "self@example.com",
		Enabled: ptr(false), CreatedAt: "2026-07-01T00:00:00Z",
	}}); err != nil {
		t.Fatal(err)
	}
	m := gate4CommitmentMemory()
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	snapshot, err := readCommitmentSnapshot(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Commitments) != 1 || snapshot.Commitments[0].ID == "" {
		t.Fatalf("seed commitment = %+v, want one identified row", snapshot.Commitments)
	}
	return cfg, snapshot.Commitments[0]
}

func TestGate4CommitmentDecisionRebuildAndUndo(t *testing.T) {
	cfg, before := gate4CommitmentCfg(t)
	run(t, "teach", "commitment", "wrong-direction",
		"--memory-id", before.OpenedBy.MemoryID,
		"--commitment-id", before.ID,
		"--direction", "owed_by_counterparty",
		"--yes")

	after, err := readCommitmentSnapshot(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Commitments) != 1 ||
		after.Commitments[0].Direction != commitOwedByCounterparty ||
		!atomEqual(after.Commitments[0].Owner, after.Commitments[0].Counterparty) {
		t.Fatalf("wrong-direction decision did not govern rebuilt output: %+v", after.Commitments)
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	entry := g.Entries[len(g.Entries)-1]
	run(t, "teach", "undo", entry.ID)
	undone, err := readCommitmentSnapshot(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(undone.Commitments) != 1 || undone.Commitments[0].Direction != before.Direction {
		t.Fatalf("undo did not restore derived commitment: %+v", undone.Commitments)
	}
}

func TestGate4AllCommitmentVerdictsAreTypedAndReversible(t *testing.T) {
	cfg := Config{}
	base := Commitment{
		ID:               "commit:v1:test",
		Owner:            govAtom{Kind: atomAddress, Value: "self@example.com"},
		Counterparty:     govAtom{Kind: atomAddress, Value: "old@example.com"},
		CounterpartyKeys: []string{"person:old@example.com"},
		Direction:        commitOwedBySelf,
		OpenedBy:         commitSpan{MemoryID: "m1"},
		State:            commitOpen, ClosureRef: commitClosureNone,
	}
	newPerson := govAtom{Kind: atomAddress, Value: "new@example.com"}
	tests := []struct {
		name  string
		entry govEntry
		check func(t *testing.T, got []Commitment)
	}{
		{
			name:  "not a commitment",
			entry: govEntry{Kind: govKindTeachCommitment, Action: govActionRecord, TargetID: "m1", CommitmentID: base.ID, Decision: teachNotCommitment},
			check: func(t *testing.T, got []Commitment) {
				if len(got) != 0 {
					t.Fatalf("not-a-commitment remained visible: %+v", got)
				}
			},
		},
		{
			name:  "wrong person",
			entry: govEntry{Kind: govKindTeachCommitment, Action: govActionRecord, TargetID: "m1", CommitmentID: base.ID, Decision: teachWrongPerson, CorrectedAtom: &newPerson},
			check: func(t *testing.T, got []Commitment) {
				if len(got) != 1 || !atomEqual(got[0].Counterparty, newPerson) {
					t.Fatalf("wrong-person correction = %+v", got)
				}
			},
		},
		{
			name:  "wrong direction",
			entry: govEntry{Kind: govKindTeachCommitment, Action: govActionRecord, TargetID: "m1", CommitmentID: base.ID, Decision: teachWrongDirection, CorrectedDirection: commitOwedByCounterparty},
			check: func(t *testing.T, got []Commitment) {
				if len(got) != 1 || got[0].Direction != commitOwedByCounterparty {
					t.Fatalf("wrong-direction correction = %+v", got)
				}
			},
		},
		{
			name:  "already closed",
			entry: govEntry{ID: "gov_closed", Kind: govKindTeachCommitment, Action: govActionRecord, TargetID: "m1", CommitmentID: base.ID, Decision: teachAlreadyClosed},
			check: func(t *testing.T, got []Commitment) {
				if len(got) != 1 || got[0].State != commitClosed || got[0].ClosureRef != "governance:gov_closed" {
					t.Fatalf("already-closed correction = %+v", got)
				}
			},
		},
		{
			name:  "duplicate",
			entry: govEntry{Kind: govKindTeachCommitment, Action: govActionRecord, TargetID: "m1", CommitmentID: base.ID, Decision: teachDuplicate, DuplicateOf: "commit:v1:canonical"},
			check: func(t *testing.T, got []Commitment) {
				if len(got) != 1 || got[0].DuplicateOf != "commit:v1:canonical" {
					t.Fatalf("duplicate correction = %+v", got)
				}
			},
		},
		{
			name:  "useful",
			entry: govEntry{Kind: govKindTeachCommitment, Action: govActionRecord, TargetID: "m1", CommitmentID: base.ID, Decision: teachUseful},
			check: func(t *testing.T, got []Commitment) {
				if len(got) != 1 || !got[0].ReviewedUseful {
					t.Fatalf("useful correction = %+v", got)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			active := governance{Schema: governanceSchema, Entries: []govEntry{tc.entry}}
			tc.check(t, applyTeachCommitments([]Commitment{base}, active, cfg))
			revoked := tc.entry
			revoked.RevokedAt = "2026-07-25T00:00:00Z"
			got := applyTeachCommitments([]Commitment{base}, governance{Schema: governanceSchema, Entries: []govEntry{revoked}}, cfg)
			if len(got) != 1 || got[0].Direction != base.Direction ||
				got[0].State != base.State || got[0].DuplicateOf != "" ||
				got[0].ReviewedUseful || !atomEqual(got[0].Counterparty, base.Counterparty) {
				t.Fatalf("revoked %s still affects output: %+v", tc.name, got)
			}
		})
	}
}

func gate4CommitmentForMemory(t *testing.T, commitments []Commitment, memoryID string) Commitment {
	t.Helper()
	for _, commitment := range commitments {
		if commitment.OpenedBy.MemoryID == memoryID {
			return commitment
		}
	}
	t.Fatalf("no commitment opened by %s in %+v", memoryID, commitments)
	return Commitment{}
}

func TestGate4EveryCommitmentVerdictRoundTripsThroughCLIRebuildAndUndo(t *testing.T) {
	for _, decision := range []string{
		"not-a-commitment",
		"wrong-person",
		"wrong-direction",
		"already-closed",
		"duplicate",
		"useful",
	} {
		t.Run(decision, func(t *testing.T) {
			cfg, before := gate4CommitmentCfg(t)
			var canonicalID string
			if decision == "duplicate" {
				canonical := gate4CommitmentMemory()
				canonical.ID = "gmail_thread/gate4-canonical"
				canonical.ProviderID = "gate4-canonical"
				canonical.Title = "Launch checklist"
				canonical.Text = "From: Other <other@example.com>\n\nCan you send the launch checklist?"
				canonical.Meta["messages"] = []commitmentMessageEvidence{{
					MessageRef: "gmail_thread/gate4-canonical#msg-1",
					Sender:     "other@example.com", To: []string{"self@example.com"},
					At: "2026-07-20T11:00:00Z", BlockRefs: []string{"body"},
				}}
				if err := writeMemory(cfg, canonical); err != nil {
					t.Fatal(err)
				}
				if _, err := rebuildIndex(context.Background(), cfg); err != nil {
					t.Fatal(err)
				}
				snapshot, err := readCommitmentSnapshot(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				before = gate4CommitmentForMemory(t, snapshot.Commitments, "gmail_thread/gate4")
				canonicalID = gate4CommitmentForMemory(t, snapshot.Commitments, canonical.ID).ID
			}

			args := []string{
				"teach", "commitment", decision,
				"--memory-id", before.OpenedBy.MemoryID,
				"--commitment-id", before.ID,
			}
			switch decision {
			case "wrong-person":
				args = append(args, "--person", "corrected@example.net")
			case "wrong-direction":
				args = append(args, "--direction", "owed_by_counterparty")
			case "duplicate":
				args = append(args, "--duplicate-of", canonicalID)
			}
			args = append(args, "--yes")
			run(t, args...)

			governance, err := loadGovernance(cfg)
			if err != nil {
				t.Fatal(err)
			}
			entry := governance.Entries[len(governance.Entries)-1]
			after, err := readCommitmentSnapshot(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if decision == "not-a-commitment" {
				for _, commitment := range after.Commitments {
					if commitment.ID == before.ID {
						t.Fatalf("not-a-commitment remained in rebuilt output: %+v", after.Commitments)
					}
				}
			} else {
				got := gate4CommitmentForMemory(t, after.Commitments, before.OpenedBy.MemoryID)
				switch decision {
				case "wrong-person":
					if got.Counterparty.Kind != atomAddress || got.Counterparty.Value != "corrected@example.net" {
						t.Fatalf("wrong-person CLI result = %+v", got)
					}
				case "wrong-direction":
					if got.Direction != commitOwedByCounterparty || !atomEqual(got.Owner, got.Counterparty) {
						t.Fatalf("wrong-direction CLI result = %+v", got)
					}
				case "already-closed":
					if got.State != commitClosed || got.ClosureRef != "governance:"+entry.ID {
						t.Fatalf("already-closed CLI result = %+v", got)
					}
				case "duplicate":
					if got.DuplicateOf != canonicalID {
						t.Fatalf("duplicate CLI result = %+v", got)
					}
				case "useful":
					if !got.ReviewedUseful {
						t.Fatalf("useful CLI result = %+v", got)
					}
				}
			}

			if _, err := rebuildIndex(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			repeated, err := readCommitmentSnapshot(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after.Commitments, repeated.Commitments) {
				t.Fatalf("Teach projection changed across identical rebuilds:\nfirst=%+v\nsecond=%+v", after.Commitments, repeated.Commitments)
			}

			run(t, "teach", "undo", entry.ID)
			undone, err := readCommitmentSnapshot(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			restored := gate4CommitmentForMemory(t, undone.Commitments, before.OpenedBy.MemoryID)
			if !reflect.DeepEqual(restored, before) {
				t.Fatalf("undo did not restore the original commitment:\n got=%+v\nwant=%+v", restored, before)
			}
		})
	}
}

func TestGate4AuthoredMemoryCorrectSupersedeRetractUndo(t *testing.T) {
	for _, decision := range []string{"correct", "supersede"} {
		t.Run(decision, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			cfg := mustConfig(t)
			original := Memory{
				ID: "mem_gate4_original", Scope: "project:gate4", Type: "fact",
				Title: "Original", Text: "original evidence", Source: "mcp",
				CreatedAt: "2026-07-20T10:00:00Z",
			}
			if err := writeMemory(cfg, original); err != nil {
				t.Fatal(err)
			}
			if _, err := rebuildIndex(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			run(t, "teach", "memory", decision, "--id", original.ID,
				"--title", "Replacement", "--text", "governed current truth", "--yes")
			g, _ := loadGovernance(cfg)
			entry := g.Entries[len(g.Entries)-1]
			if _, err := findMemory(cfg, original.ID); err == nil {
				t.Fatal("superseded original remained in current-state read")
			}
			if raw, err := findMemoryRaw(cfg, original.ID); err != nil || raw.Text != "original evidence" {
				t.Fatalf("original evidence was not auditable: %+v, %v", raw, err)
			}
			if replacement, err := findMemory(cfg, entry.ReplacementID); err != nil ||
				replacement.Text != "governed current truth" {
				t.Fatalf("replacement not current: %+v, %v", replacement, err)
			}
			historyJSON := run(t, "teach", "history", "--memory-id", original.ID, "--json")
			var history []govEntry
			if err := json.Unmarshal([]byte(historyJSON), &history); err != nil ||
				len(history) != 1 || history[0].ID != entry.ID ||
				history[0].TargetID != original.ID ||
				history[0].ReplacementID != entry.ReplacementID ||
				history[0].Decision != decision {
				t.Fatalf("revision history did not preserve the governed link: %s, err=%v", historyJSON, err)
			}
			listed, err := listMemories(cfg, "project:gate4", 10)
			if err != nil || len(listed) != 1 || listed[0].ID != entry.ReplacementID {
				t.Fatalf("current list did not exclude the superseded original: %+v, %v", listed, err)
			}
			if oldHits, err := defaultSearch(context.Background(), cfg, "original evidence", "project:gate4", 5); err != nil || len(oldHits) != 0 {
				t.Fatalf("superseded original remained searchable: %+v, %v", oldHits, err)
			}
			if newHits, err := defaultSearch(context.Background(), cfg, "governed current truth", "project:gate4", 5); err != nil ||
				len(newHits) != 1 || newHits[0].ID != entry.ReplacementID {
				t.Fatalf("replacement not searchable: %+v, %v", newHits, err)
			}
			if shared, err := collectShareMemories(cfg, "project:gate4"); err != nil ||
				len(shared) != 1 || shared[0].ID != entry.ReplacementID {
				t.Fatalf("share export did not honor current revision: %+v, %v", shared, err)
			}
			if _, err := rebuildIndex(context.Background(), cfg); err != nil {
				t.Fatal(err)
			}
			run(t, "teach", "undo", entry.ID)
			if _, err := findMemory(cfg, original.ID); err != nil {
				t.Fatalf("undo did not restore original: %v", err)
			}
			if _, err := findMemory(cfg, entry.ReplacementID); err == nil {
				t.Fatal("undone replacement remained current")
			}
			listed, err = listMemories(cfg, "project:gate4", 10)
			if err != nil || len(listed) != 1 || listed[0].ID != original.ID {
				t.Fatalf("undo did not restore the original list projection: %+v, %v", listed, err)
			}
			if oldHits, err := defaultSearch(context.Background(), cfg, "original evidence", "project:gate4", 5); err != nil ||
				len(oldHits) != 1 || oldHits[0].ID != original.ID {
				t.Fatalf("undo did not restore original search result: %+v, %v", oldHits, err)
			}
			if shared, err := collectShareMemories(cfg, "project:gate4"); err != nil ||
				len(shared) != 1 || shared[0].ID != original.ID {
				t.Fatalf("share export did not honor undo: %+v, %v", shared, err)
			}

			run(t, "teach", "memory", "retract", "--id", original.ID, "--yes")
			g, _ = loadGovernance(cfg)
			retract := g.Entries[len(g.Entries)-1]
			if _, err := findMemory(cfg, original.ID); err == nil {
				t.Fatal("retracted memory remained current")
			}
			if _, err := findMemoryRaw(cfg, original.ID); err != nil {
				t.Fatalf("retracted evidence was deleted: %v", err)
			}
			listed, err = listMemories(cfg, "project:gate4", 10)
			if err != nil || len(listed) != 0 {
				t.Fatalf("retracted memory remained in current list: %+v, %v", listed, err)
			}
			run(t, "teach", "undo", retract.ID)
			if _, err := findMemory(cfg, original.ID); err != nil {
				t.Fatalf("retraction undo did not restore memory: %v", err)
			}
		})
	}
}

func TestGate4AuthoredMemoryHistoryTraversesRevisionChainNewestFirst(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	original := Memory{
		ID: "mem_gate4_chain_a", Scope: "project:gate4", Type: "fact",
		Title: "Revision A", Text: "revision a", Source: "mcp",
		CreatedAt: "2026-07-20T10:00:00Z",
	}
	if err := writeMemory(cfg, original); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	run(t, "teach", "memory", "correct", "--id", original.ID,
		"--title", "Revision B", "--text", "revision b", "--yes")
	governance, err := loadGovernance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	first := governance.Entries[len(governance.Entries)-1]
	run(t, "teach", "memory", "supersede", "--id", first.ReplacementID,
		"--title", "Revision C", "--text", "revision c", "--yes")
	governance, err = loadGovernance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	second := governance.Entries[len(governance.Entries)-1]

	historyJSON := run(t, "teach", "history", "--memory-id", original.ID, "--json")
	var history []govEntry
	if err := json.Unmarshal([]byte(historyJSON), &history); err != nil ||
		len(history) != 2 || history[0].ID != first.ID || history[1].ID != second.ID {
		t.Fatalf("original-id history did not traverse A -> B -> C: %s, err=%v", historyJSON, err)
	}
	if current, err := findMemory(cfg, second.ReplacementID); err != nil || current.Text != "revision c" {
		t.Fatalf("latest revision was not current: %+v, %v", current, err)
	}
	if _, err := runErr(t, "teach", "undo", first.ID); err == nil ||
		!strings.Contains(err.Error(), "undo newest first") {
		t.Fatalf("older revision undo bypassed active descendant: %v", err)
	}
	run(t, "teach", "undo", second.ID)
	if current, err := findMemory(cfg, first.ReplacementID); err != nil || current.Text != "revision b" {
		t.Fatalf("undoing C did not restore B: %+v, %v", current, err)
	}
	run(t, "teach", "undo", first.ID)
	if current, err := findMemory(cfg, original.ID); err != nil || current.Text != "revision a" {
		t.Fatalf("undoing B did not restore A: %+v, %v", current, err)
	}
}

func TestGate4DecisionValiditySurfacesNeedsReview(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	complete := Memory{
		Type: "decision", CreatedAt: "2026-07-20T00:00:00Z",
		Decision: &DecisionValidity{
			AsOf: "2026-07-20T00:00:00Z", Durability: decisionWorking,
			FlipConditions: []string{"the customer rejects the contract"},
			ReviewBy:       "2026-08-01T00:00:00Z",
		},
	}
	if got := decorateDecision(complete, now); got.DecisionStatus != decisionCurrent || !got.Decision.Complete {
		t.Fatalf("complete decision = %+v", got)
	}
	expired := complete
	expired.Decision = &DecisionValidity{
		AsOf: "2026-07-20T00:00:00Z", Durability: decisionWorking,
		FlipConditions: []string{"the customer rejects the contract"},
		ReviewBy:       "2026-07-24T00:00:00Z",
	}
	if got := decorateDecision(expired, now); got.DecisionStatus != decisionNeedsReview {
		t.Fatalf("expired decision status = %+v", got)
	}
	incomplete := Memory{Type: "decision", CreatedAt: "2026-07-20T00:00:00Z"}
	if got := decorateDecision(incomplete, now); got.DecisionStatus != decisionNeedsReview || got.Decision.Complete {
		t.Fatalf("legacy/incomplete decision = %+v", got)
	}
}

func TestGate4NeedsReviewDecisionNeverGovernsCommitments(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	base := Memory{
		ID:        "manual_decision_promise",
		Type:      "decision",
		Source:    "manual",
		CreatedAt: "2026-07-20T00:00:00Z",
		Text:      "I told Jordan I'd return the borrowed lens before the workshop.",
	}
	current := base
	current.Decision = &DecisionValidity{
		AsOf:           "2026-07-20T00:00:00Z",
		Durability:     decisionWorking,
		FlipConditions: []string{"Jordan no longer needs the lens"},
		ReviewBy:       "2026-08-01T00:00:00Z",
	}
	expired := current
	expired.Decision = &DecisionValidity{
		AsOf:           "2026-07-20T00:00:00Z",
		Durability:     decisionWorking,
		FlipConditions: []string{"Jordan no longer needs the lens"},
		ReviewBy:       "2026-07-24T00:00:00Z",
	}

	cfg := Config{SelfEmails: []string{"self@example.com"}}
	if got := materializeCommitments([]Memory{base}, cfg, now); len(got) != 0 {
		t.Fatalf("incomplete decision governed commitments: %+v", got)
	}
	if got := materializeCommitments([]Memory{expired}, cfg, now); len(got) != 0 {
		t.Fatalf("expired decision governed commitments: %+v", got)
	}
	if got := materializeCommitments([]Memory{current}, cfg, now); len(got) != 1 {
		t.Fatalf("current complete decision did not govern commitments: %+v", got)
	}
}

func TestGate4ReviewDeadlineInvalidatesInventoryWithoutRebuild(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	builtAt := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	gate2PinClock(t, builtAt)
	m := Memory{
		ID:        "manual_decision_expiring",
		Scope:     "global",
		Type:      "decision",
		Title:     "Return the borrowed lens",
		Source:    "manual",
		CreatedAt: builtAt.Add(-time.Hour).Format(time.RFC3339),
		Text:      "I told Jordan I'd return the borrowed lens before the workshop.",
		Decision: &DecisionValidity{
			AsOf:           builtAt.Add(-time.Hour).Format(time.RFC3339),
			Durability:     decisionWorking,
			FlipConditions: []string{"Jordan no longer needs the lens"},
			ReviewBy:       builtAt.Add(time.Hour).Format(time.RFC3339),
		},
	}
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	before, err := readCommitmentInventory(context.Background(), cfg, builtAt.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(before[m.ID]) != 1 {
		t.Fatalf("current decision inventory = %+v, want one commitment", before)
	}
	after, err := readCommitmentInventory(context.Background(), cfg, builtAt.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("expired decision remained authoritative without rebuild: %+v", after)
	}
	snapshot, err := readCommitmentSnapshot(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Commitments) != 1 {
		t.Fatalf("test did not prove read-time invalidation over a stale row: %+v", snapshot.Commitments)
	}
}

func TestGate4DecisionValidityRoundTripsThroughCurrentReadAndSearch(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	raw := run(t, "write",
		"--scope", "project:gate4",
		"--type", "decision",
		"--title", "Gate 4 launch rule",
		"--text", "Keep the launch local until the quality gate passes.",
		"--as-of", "2026-07-20T00:00:00Z",
		"--durability", "working",
		"--flip-conditions", "Gate 4 fails verification;the product contract changes",
		"--review-by", "2026-07-24T00:00:00Z",
		"--json",
	)
	var written Memory
	if err := json.Unmarshal([]byte(raw), &written); err != nil {
		t.Fatal(err)
	}
	current, err := findMemory(cfg, written.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Decision == nil || !current.Decision.Complete ||
		current.DecisionStatus != decisionNeedsReview ||
		len(current.Decision.FlipConditions) != 2 {
		t.Fatalf("typed decision did not round-trip: %+v", current)
	}
	results, err := defaultSearch(context.Background(), cfg, "launch local quality", "project:gate4", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].DecisionStatus != decisionNeedsReview {
		t.Fatalf("search hid decision validity: %+v", results)
	}
	contextText := buildContext(cfg, results, 6000, true)
	if !strings.Contains(contextText, "Decision status: needs_review") ||
		!strings.Contains(contextText, "Flip conditions:") {
		t.Fatalf("context did not surface decision validity:\n%s", contextText)
	}
	_, byInstance, _, err := digestInputs(cfg, time.Now(), briefOpts{sinceHours: 24})
	if err != nil {
		t.Fatal(err)
	}
	if len(byInstance["manual"]) != 1 ||
		!strings.HasPrefix(byInstance["manual"][0].Title, "[NEEDS REVIEW] ") ||
		!strings.Contains(byInstance["manual"][0].Text, "Review before relying on it") {
		t.Fatalf("digest did not mark expired decision as needs-review: %+v", byInstance)
	}
}

func TestGate4EvalExamplesRequireConsentAndMinimizeContent(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	rawTarget := "mem_private_customer_name"
	if _, err := appendGovernanceEntry(cfg, govEntry{
		Kind: govKindTeachMemory, Action: govActionRecord,
		TargetID: rawTarget, Decision: teachMemoryRetract,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runErr(t, "teach", "examples", "--json"); err == nil {
		t.Fatal("evaluation examples exported without consent")
	}
	run(t, "teach", "consent", "enable", "--yes")
	out := run(t, "teach", "examples", "--json")
	if strings.Contains(out, rawTarget) || strings.Contains(out, "customer_name") ||
		strings.Contains(out, "created_at") || strings.Contains(out, "2026-") {
		t.Fatalf("privacy-minimized export leaked target identity: %s", out)
	}
	var examples []teachExample
	if err := json.Unmarshal([]byte(out), &examples); err != nil || len(examples) != 1 {
		t.Fatalf("minimized examples = %s, err %v", out, err)
	}
	if examples[0].Ref != "example-0001" {
		t.Fatalf("privacy-minimized export used an identity-derived reference: %+v", examples)
	}
	run(t, "teach", "consent", "disable", "--yes")
	if _, err := runErr(t, "teach", "examples", "--json"); err == nil {
		t.Fatal("evaluation examples remained enabled after consent was withdrawn")
	}
}

func TestGate4ConnectorEvidenceCannotBeRevisedAsAuthoredMemory(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	evidence := gate4CommitmentMemory()
	if err := writeMemory(cfg, evidence); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := runErr(t, "teach", "memory", "correct",
		"--id", evidence.ID, "--title", "Rewritten", "--text", "rewritten evidence", "--yes"); err == nil ||
		!strings.Contains(err.Error(), "connector evidence is immutable") {
		t.Fatalf("connector evidence correction was not refused clearly: %v", err)
	}
	raw, err := findMemoryRaw(cfg, evidence.ID)
	if err != nil || raw.Title != evidence.Title || raw.Text != evidence.Text {
		t.Fatalf("refused connector correction changed evidence: %+v, %v", raw, err)
	}
	governance, err := loadGovernance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(governance.Entries) != 0 {
		t.Fatalf("refused connector correction wrote governance history: %+v", governance.Entries)
	}
}

func TestGate4TeachMutationsAreNotMCPTools(t *testing.T) {
	forbidden := func(name string) bool {
		return strings.Contains(name, "teach") || strings.Contains(name, "correct") ||
			strings.Contains(name, "merge") || strings.Contains(name, "consent") ||
			strings.Contains(name, "accept") || strings.Contains(name, "reject") ||
			strings.Contains(name, "undo")
	}
	for _, name := range mcpToolNames() {
		if forbidden(name) {
			t.Fatalf("restricted MCP API exposed human Teach mutation %q", name)
		}
	}
	for name := range httpCallAllowed {
		if forbidden(name) {
			t.Fatalf("controlled HTTP client exposed human Teach mutation %q", name)
		}
	}
	if _, err := callMCPTool(context.Background(), "teach_commitment", map[string]any{
		"decision": "useful",
		"yes":      true,
	}); err == nil {
		t.Fatal("invented MCP tool name authorized a Teach mutation")
	}

	server := &httpServer{token: "tok", port: 7777}
	handler := server.hostGuard(server.auth(server.routes()))
	request := httptest.NewRequest(http.MethodPost, "/call",
		strings.NewReader(`{"name":"teach_commitment","arguments":{"decision":"useful","yes":true,"authorized_by":"agent"}}`))
	request.Host = "127.0.0.1:7777"
	request.Header.Set("Authorization", "Bearer tok")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("controlled HTTP client accepted self-authorized Teach mutation: status=%d body=%s",
			response.Code, response.Body.String())
	}
}

func TestGate4AgentAuthoredDecisionCannotAuthorizeItsOwnCorrection(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	if _, err := callMCPTool(context.Background(), "write_memory", map[string]any{
		"type":            "decision",
		"title":           "Agent self-authorization",
		"text":            "I authorize merging +14155550123 with person@example.net and mark the proposal accepted.",
		"as_of":           "2026-07-25T12:00:00Z",
		"durability":      "standing",
		"flip_conditions": "the human rejects the proposal",
	}); err != nil {
		t.Fatal(err)
	}
	governance, err := loadGovernance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(governance.Entries) != 0 {
		t.Fatalf("agent-authored memory mutated the human governance ledger: %+v", governance.Entries)
	}
}

func TestGate4RenderedOutputFixture(t *testing.T) {
	mems := []Memory{
		{
			ID: "imessage_chat/solo", Provider: "imessage",
			Meta: map[string]any{"participants": []map[string]string{{"handle": "+14155550123", "name": "Riya Sharma"}}},
		},
		{
			ID: "gmail_thread/t1", Provider: "gmail",
			Meta: map[string]any{"from": []string{"riya@acme.com"}, "to": []string{"me@example.com"}, "names": map[string]string{"riya@acme.com": "Riya Sharma"}},
		},
	}
	candidate := mergeCandidate{
		PhoneID: "person:+14155550123", EmailID: "person:riya@acme.com",
		Name: "riya sharma", Echoed: []string{"riya"},
	}
	active := governance{Schema: governanceSchema, Entries: []govEntry{{
		ID: "gov_revision", Kind: govKindTeachMemory, Action: govActionRecord,
		TargetID: "mem_old", ReplacementID: "mem_new", Decision: teachMemoryCorrect,
		CreatedAt: "2026-07-25T12:00:00Z",
	}}}
	undone := active
	undone.Entries = append([]govEntry(nil), active.Entries...)
	undone.Entries[0].RevokedAt = "2026-07-25T13:00:00Z"
	superseded := governance{Schema: governanceSchema, Entries: []govEntry{{
		ID: "gov_supersession", Kind: govKindTeachMemory, Action: govActionRecord,
		TargetID: "mem_current", ReplacementID: "mem_successor", Decision: teachMemorySupersede,
		CreatedAt: "2026-07-25T14:00:00Z",
	}}}
	retracted := governance{Schema: governanceSchema, Entries: []govEntry{{
		ID: "gov_retraction", Kind: govKindTeachMemory, Action: govActionRecord,
		TargetID: "mem_retracted", Decision: teachMemoryRetract,
		CreatedAt: "2026-07-25T15:00:00Z",
	}}}
	var rebuiltIDs []string
	for _, m := range filterCurrentMemories(active, []Memory{{ID: "mem_old"}, {ID: "mem_new"}}) {
		rebuiltIDs = append(rebuiltIDs, m.ID)
	}
	fixture := map[string]any{
		"identity_proposal": pendingMergeOf(candidate, mems),
		"memory_current_after_correction": map[string]bool{
			"mem_old": active.memoryVisible("mem_old"),
			"mem_new": active.memoryVisible("mem_new"),
		},
		"memory_current_after_undo": map[string]bool{
			"mem_old": undone.memoryVisible("mem_old"),
			"mem_new": undone.memoryVisible("mem_new"),
		},
		"memory_current_after_supersession": map[string]bool{
			"mem_current":   superseded.memoryVisible("mem_current"),
			"mem_successor": superseded.memoryVisible("mem_successor"),
		},
		"memory_current_after_retraction": map[string]bool{
			"mem_retracted": retracted.memoryVisible("mem_retracted"),
		},
		"rebuild_projection": rebuiltIDs,
		"expired_decision": decorateDecision(Memory{
			ID: "mem_decision", Type: "decision", CreatedAt: "2026-07-01T00:00:00Z",
			Decision: &DecisionValidity{
				AsOf: "2026-07-01T00:00:00Z", Durability: decisionStanding,
				FlipConditions: []string{"law changes"}, ReviewBy: "2026-07-24T00:00:00Z",
			},
		}, time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)),
	}
	got, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "gate4-teach.golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Git may check text fixtures out with CRLF on Windows. Compare the JSON
	// document rather than its incidental whitespace, and force a CRLF version
	// here so removing the semantic comparison fails on every development OS.
	want = []byte(strings.ReplaceAll(
		strings.ReplaceAll(string(want), "\r\n", "\n"),
		"\n", "\r\n",
	))
	decodeFixture := func(label string, raw []byte) any {
		t.Helper()
		if !json.Valid(raw) {
			t.Fatalf("%s Gate 4 fixture is invalid JSON", label)
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber() // preserve large numeric identities exactly
		var fixture any
		if err := dec.Decode(&fixture); err != nil {
			t.Fatalf("decode %s Gate 4 fixture: %v", label, err)
		}
		return fixture
	}
	gotFixture := decodeFixture("generated", got)
	wantFixture := decodeFixture("golden", want)
	if !reflect.DeepEqual(gotFixture, wantFixture) {
		t.Fatalf("Gate 4 rendered fixture drifted:\ngot:\n%s\nwant:\n%s", got, want)
	}
}
