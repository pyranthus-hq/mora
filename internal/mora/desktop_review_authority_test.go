package mora

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

type desktopReviewTestVerifier struct{ proof []byte }

func (v desktopReviewTestVerifier) Verify(_ []byte, proof []byte) bool {
	return bytes.Equal(v.proof, proof)
}

func desktopAuthority(t *testing.T, cfg Config) (*desktopReviewAuthority, []byte) {
	t.Helper()
	proof := []byte("test-only-verified-proof")
	return newDesktopReviewAuthority(cfg, desktopReviewTestVerifier{proof: proof}), proof
}

func desktopChallenge(t *testing.T, a *desktopReviewAuthority, action desktopReviewAction, kind, id string) desktopReviewChallenge {
	t.Helper()
	p, err := loadDesktopReviewPreview(a.cfg, kind, id)
	if err != nil {
		t.Fatal(err)
	}
	c, err := a.begin(action, kind, id, p.CanonicalRevision, p.DisplayDigest)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDesktopReviewAuthorityAppliesSavedAndHeldActions(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		action     desktopReviewAction
	}{
		{"keep saved", reviewKindSaved, desktopReviewKeep},
		{"discard saved", reviewKindSaved, desktopReviewDiscard},
		{"keep held", reviewKindHeld, desktopReviewKeep},
		{"discard held", reviewKindHeld, desktopReviewDiscard},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := reviewSetup(t)
			id := reviewSaved(t, tc.name)
			if tc.kind == reviewKindHeld {
				id = reviewHeld(t, tc.name)
			}
			a, proof := desktopAuthority(t, cfg)
			out, err := a.complete(testCtx(t), desktopChallenge(t, a, tc.action, tc.kind, id).Nonce, proof, false)
			if err != nil || !out.Applied || out.Status != "applied" || out.IndexVisibility != "not_checked" {
				t.Fatalf("complete = %+v, %v", out, err)
			}
			switch tc.kind + "/" + string(tc.action) {
			case "saved/keep":
				if reviewTentative(t, cfg, id) {
					t.Fatal("kept saved note is still tentative")
				}
			case "saved/discard":
				if _, err := findMemory(cfg, id); err == nil {
					t.Fatal("discarded saved note remains")
				}
			case "held/keep":
				d, err := readMCPProposalDecision(cfg, id)
				if err != nil || d == nil || d.Status != "approved" {
					t.Fatalf("decision = %+v, %v", d, err)
				}
				m, err := findMemory(cfg, d.MemoryID)
				if err != nil || memory.IsOwnerConfirmed(m) || m.Source != "mcp" {
					t.Fatalf("approved held provenance = %+v, %v", m, err)
				}
			case "held/discard":
				d, err := readMCPProposalDecision(cfg, id)
				if err != nil || d == nil || d.Status != "rejected" {
					t.Fatalf("decision = %+v, %v", d, err)
				}
			}
		})
	}
}

func TestDesktopReviewAuthorityRefusesWithoutTrustedVerifier(t *testing.T) {
	cfg := reviewSetup(t)
	id := reviewSaved(t, "No trusted verifier")
	p, err := loadDesktopReviewPreview(cfg, reviewKindSaved, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newDesktopReviewAuthority(cfg, nil).begin(desktopReviewKeep, reviewKindSaved, id, p.CanonicalRevision, p.DisplayDigest); !errors.Is(err, errDesktopReviewProofUnavailable) {
		t.Fatalf("begin without verifier = %v", err)
	}
	if !reviewTentative(t, cfg, id) {
		t.Fatal("untrusted enrollment changed saved note")
	}
}

func TestDesktopReviewAuthorityRefusesInvalidNonce(t *testing.T) {
	cfg := reviewSetup(t)
	a, proof := desktopAuthority(t, cfg)
	if _, err := a.complete(testCtx(t), "../not-a-challenge", proof, false); !errors.Is(err, errDesktopReviewUnavailable) {
		t.Fatalf("invalid nonce = %v", err)
	}
}

func TestDesktopReviewAuthorityRefusesWrongActionAndCompetingDecision(t *testing.T) {
	cfg := reviewSetup(t)
	id := reviewSaved(t, "Competing desktop preview")
	a, proof := desktopAuthority(t, cfg)
	p, err := loadDesktopReviewPreview(cfg, reviewKindSaved, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.begin("wrong", reviewKindSaved, id, p.CanonicalRevision, p.DisplayDigest); err == nil {
		t.Fatal("invalid action issued a challenge")
	}
	first := desktopChallenge(t, a, desktopReviewKeep, reviewKindSaved, id)
	second := desktopChallenge(t, a, desktopReviewDiscard, reviewKindSaved, id)
	if _, err := a.complete(testCtx(t), first.Nonce, proof, false); err != nil {
		t.Fatal(err)
	}
	if _, err := a.complete(testCtx(t), second.Nonce, proof, false); !errors.Is(err, errDesktopReviewUnavailable) {
		t.Fatalf("competing decision = %v", err)
	}
	if reviewTentative(t, cfg, id) {
		t.Fatal("first decision did not apply")
	}
}

func TestDesktopReviewAuthorityRefusesChangedAndIncompletePreviews(t *testing.T) {
	cfg := reviewSetup(t)
	id := reviewSaved(t, "Stale desktop preview")
	a, proof := desktopAuthority(t, cfg)
	c := desktopChallenge(t, a, desktopReviewKeep, reviewKindSaved, id)
	m, err := findMemory(cfg, id)
	if err != nil {
		t.Fatal(err)
	}
	m.Title, m.Scope, m.Text = "Changed title", "project:other", "Changed text."
	raw, err := renderMemory(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.Path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.complete(testCtx(t), c.Nonce, proof, false); !errors.Is(err, errDesktopReviewUnavailable) {
		t.Fatalf("stale completion = %v", err)
	}
	if !reviewTentative(t, cfg, id) {
		t.Fatal("stale completion changed note")
	}

	incomplete := reviewSaved(t, "Incomplete desktop preview")
	m, err = findMemory(cfg, incomplete)
	if err != nil {
		t.Fatal(err)
	}
	m.Text, m.Truncated = "Bearer hidden-test-value", true
	if err := writeMemory(cfg, m); err != nil {
		t.Fatal(err)
	}
	p, err := loadDesktopReviewPreview(cfg, reviewKindSaved, incomplete)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.begin(desktopReviewKeep, reviewKindSaved, incomplete, p.CanonicalRevision, p.DisplayDigest); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete keep = %v", err)
	}
	if !reviewTentative(t, cfg, incomplete) {
		t.Fatal("incomplete keep changed note")
	}
}

func TestDesktopReviewAuthorityConsumesCancelAndReplay(t *testing.T) {
	cfg := reviewSetup(t)
	id := reviewSaved(t, "Replay desktop preview")
	a, proof := desktopAuthority(t, cfg)
	c := desktopChallenge(t, a, desktopReviewDiscard, reviewKindSaved, id)
	if out, err := a.complete(testCtx(t), c.Nonce, proof, true); err != nil || out.Status != "cancelled" || out.Applied {
		t.Fatalf("cancel = %+v, %v", out, err)
	}
	if out, err := newDesktopReviewAuthority(cfg, desktopReviewTestVerifier{proof: proof}).complete(testCtx(t), c.Nonce, proof, false); err != nil || out.Status != "consumed" || out.Applied {
		t.Fatalf("replay after restart = %+v, %v", out, err)
	}
	if !reviewTentative(t, cfg, id) {
		t.Fatal("cancel/replay changed note")
	}
}

func TestDesktopReviewAuthorityRefusesWrongProofAndExpiry(t *testing.T) {
	cfg := reviewSetup(t)
	id := reviewHeld(t, "Expired desktop preview")
	a, proof := desktopAuthority(t, cfg)
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	c := desktopChallenge(t, a, desktopReviewDiscard, reviewKindHeld, id)
	if _, err := a.complete(testCtx(t), c.Nonce, []byte("wrong"), false); !errors.Is(err, errDesktopReviewProofUnavailable) {
		t.Fatalf("wrong proof = %v", err)
	}
	a.now = func() time.Time { return now.Add(desktopReviewChallengeLifetime) }
	if _, err := a.complete(testCtx(t), c.Nonce, proof, false); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired proof = %v", err)
	}
	if d, _ := readMCPProposalDecision(cfg, id); d != nil {
		t.Fatalf("refused held proposal gained decision: %+v", d)
	}
}

func TestReviewDesktopStatusIsReadOnly(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	out := run(t, "review", "desktop", "--json")
	var receipt struct {
		Schema string `json:"schema"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &receipt); err != nil || receipt.Schema != "mora.review.desktop" || receipt.Status != "unavailable" {
		t.Fatalf("desktop status = %q, %+v, %v", out, receipt, err)
	}
}
