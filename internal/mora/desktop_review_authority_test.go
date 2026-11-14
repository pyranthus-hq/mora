package mora

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

type desktopReviewTestVerifier struct {
	private        *ecdsa.PrivateKey
	installationID string
	keyID          string
}

func newDesktopReviewTestVerifier(t *testing.T) *desktopReviewTestVerifier {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &desktopReviewTestVerifier{private: private, installationID: "test-installation", keyID: "test-p256-key"}
}

func (v *desktopReviewTestVerifier) TrustedIdentity() (string, string, bool) {
	return v.installationID, v.keyID, true
}

func (v *desktopReviewTestVerifier) Verify(installationID, keyID string, signed, proof []byte) bool {
	if installationID != v.installationID || keyID != v.keyID {
		return false
	}
	sum := sha256.Sum256(signed)
	return ecdsa.VerifyASN1(&v.private.PublicKey, sum[:], proof)
}

func (v *desktopReviewTestVerifier) proof(t *testing.T, c desktopReviewChallenge) []byte {
	t.Helper()
	signed, err := c.bytes()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(signed)
	proof, err := ecdsa.SignASN1(cryptorand.Reader, v.private, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return proof
}

func desktopAuthority(t *testing.T, cfg Config) (*desktopReviewAuthority, *desktopReviewTestVerifier) {
	t.Helper()
	v := newDesktopReviewTestVerifier(t)
	return newDesktopReviewAuthority(cfg, v), v
}

func desktopReviewApplied(out desktopReviewOutcome) bool { return out.Applied != nil && *out.Applied }
func desktopReviewNotApplied(out desktopReviewOutcome) bool {
	return out.Applied != nil && !*out.Applied
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
			a, verifier := desktopAuthority(t, cfg)
			c := desktopChallenge(t, a, tc.action, tc.kind, id)
			out, err := a.complete(testCtx(t), c.Nonce, verifier.proof(t, c), false)
			if err != nil || !desktopReviewApplied(out) || out.Status != "applied" || out.IndexVisibility != "not_checked" {
				t.Fatalf("complete = %+v, %v", out, err)
			}
			switch tc.kind + "/" + string(tc.action) {
			case "saved/keep":
				m, err := findMemory(cfg, id)
				if err != nil || memory.IsTentative(m) || !memory.IsOwnerConfirmed(m) || !strings.Contains(strings.Join(m.Tags, ","), memory.OwnerConfirmedTagPrefix+memory.OwnerConfirmedDigest(m)) {
					t.Fatalf("kept saved owner confirmation = %+v, %v", m, err)
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
				if err != nil || !memory.IsOwnerConfirmed(m) || m.Source != "mcp" || !strings.Contains(strings.Join(m.Tags, ","), memory.OwnerConfirmedTagPrefix+memory.OwnerConfirmedDigest(m)) {
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
	a, _ := desktopAuthority(t, cfg)
	if _, err := a.complete(testCtx(t), "../not-a-challenge", nil, false); !errors.Is(err, errDesktopReviewUnavailable) {
		t.Fatalf("invalid nonce = %v", err)
	}
}

func TestDesktopReviewAuthorityRefusesWrongActionAndCompetingDecision(t *testing.T) {
	cfg := reviewSetup(t)
	id := reviewSaved(t, "Competing desktop preview")
	a, verifier := desktopAuthority(t, cfg)
	p, err := loadDesktopReviewPreview(cfg, reviewKindSaved, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.begin("wrong", reviewKindSaved, id, p.CanonicalRevision, p.DisplayDigest); err == nil {
		t.Fatal("invalid action issued a challenge")
	}
	first := desktopChallenge(t, a, desktopReviewKeep, reviewKindSaved, id)
	second := desktopChallenge(t, a, desktopReviewDiscard, reviewKindSaved, id)
	if _, err := a.complete(testCtx(t), first.Nonce, verifier.proof(t, first), false); err != nil {
		t.Fatal(err)
	}
	if out, err := a.complete(testCtx(t), second.Nonce, verifier.proof(t, second), false); err != nil || out.Status != "stale" || !desktopReviewNotApplied(out) {
		t.Fatalf("competing decision = %+v, %v", out, err)
	}
	if reviewTentative(t, cfg, id) {
		t.Fatal("first decision did not apply")
	}
}

func TestDesktopReviewAuthorityRefusesChangedAndIncompletePreviews(t *testing.T) {
	cfg := reviewSetup(t)
	id := reviewSaved(t, "Stale desktop preview")
	a, verifier := desktopAuthority(t, cfg)
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
	if out, err := a.complete(testCtx(t), c.Nonce, verifier.proof(t, c), false); err != nil || out.Status != "stale" || !desktopReviewNotApplied(out) {
		t.Fatalf("stale completion = %+v, %v", out, err)
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
	a, verifier := desktopAuthority(t, cfg)
	c := desktopChallenge(t, a, desktopReviewDiscard, reviewKindSaved, id)
	if out, err := a.complete(testCtx(t), c.Nonce, verifier.proof(t, c), true); err != nil || out.Status != "cancelled" || !desktopReviewNotApplied(out) {
		t.Fatalf("cancel = %+v, %v", out, err)
	}
	if out, err := newDesktopReviewAuthority(cfg, verifier).complete(testCtx(t), c.Nonce, verifier.proof(t, c), false); err != nil || out.Status != "cancelled" || !desktopReviewNotApplied(out) {
		t.Fatalf("replay after restart = %+v, %v", out, err)
	}
	if !reviewTentative(t, cfg, id) {
		t.Fatal("cancel/replay changed note")
	}
}

func TestDesktopReviewAuthorityRefusesWrongProofAndExpiry(t *testing.T) {
	cfg := reviewSetup(t)
	id := reviewHeld(t, "Expired desktop preview")
	a, verifier := desktopAuthority(t, cfg)
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	c := desktopChallenge(t, a, desktopReviewDiscard, reviewKindHeld, id)
	if _, err := a.complete(testCtx(t), c.Nonce, []byte("wrong"), false); !errors.Is(err, errDesktopReviewProofUnavailable) {
		t.Fatalf("wrong proof = %v", err)
	}
	a.now = func() time.Time { return now.Add(desktopReviewChallengeLifetime) }
	if _, err := a.complete(testCtx(t), c.Nonce, verifier.proof(t, c), false); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired proof = %v", err)
	}
	if d, _ := readMCPProposalDecision(cfg, id); d != nil {
		t.Fatalf("refused held proposal gained decision: %+v", d)
	}
}

func TestDesktopReviewAuthorityBindsEverySignedFieldAndIssuedDigest(t *testing.T) {
	cfg := reviewSetup(t)
	id := reviewSaved(t, "Bound desktop preview")
	a, verifier := desktopAuthority(t, cfg)
	c := desktopChallenge(t, a, desktopReviewKeep, reviewKindSaved, id)

	for _, tc := range []struct {
		name   string
		mutate func(*desktopReviewChallenge)
	}{
		{"nonce", func(c *desktopReviewChallenge) { c.Nonce = strings.Repeat("a", 64) }},
		{"installation", func(c *desktopReviewChallenge) { c.InstallationID = "other-installation" }},
		{"key", func(c *desktopReviewChallenge) { c.KeyID = "other-key" }},
		{"action", func(c *desktopReviewChallenge) { c.Action = desktopReviewDiscard }},
		{"kind", func(c *desktopReviewChallenge) { c.Kind = reviewKindHeld }},
		{"item", func(c *desktopReviewChallenge) { c.ID = "mem_other" }},
		{"scope", func(c *desktopReviewChallenge) { c.Scope = "project:other" }},
		{"revision", func(c *desktopReviewChallenge) { c.CanonicalRevision = strings.Repeat("b", 64) }},
		{"display", func(c *desktopReviewChallenge) { c.DisplayDigest = strings.Repeat("c", 64) }},
		{"expiry", func(c *desktopReviewChallenge) { c.ExpiresAt++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			altered := c
			tc.mutate(&altered)
			if _, err := a.complete(testCtx(t), c.Nonce, verifier.proof(t, altered), false); !errors.Is(err, errDesktopReviewProofUnavailable) {
				t.Fatalf("mutated %s proof = %v", tc.name, err)
			}
		})
	}
	other := newDesktopReviewTestVerifier(t)
	other.installationID, other.keyID = verifier.installationID, verifier.keyID
	if _, err := a.complete(testCtx(t), c.Nonce, other.proof(t, c), false); !errors.Is(err, errDesktopReviewProofUnavailable) {
		t.Fatalf("wrong key proof = %v", err)
	}

	path := desktopReviewChallengePath(cfg, c.Nonce)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var persisted desktopReviewChallenge
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	persisted.KeyID = "changed-on-disk"
	raw, err = json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.complete(testCtx(t), c.Nonce, verifier.proof(t, c), false); !errors.Is(err, errDesktopReviewProofUnavailable) {
		t.Fatalf("changed issued digest = %v", err)
	}
}

func TestDesktopReviewAuthorityFinalReceiptFailureIsIndeterminateAndNeverReapplied(t *testing.T) {
	cfg := reviewSetup(t)
	id := reviewSaved(t, "Receipt failure desktop preview")
	a, verifier := desktopAuthority(t, cfg)
	c := desktopChallenge(t, a, desktopReviewKeep, reviewKindSaved, id)
	writeChallenge := a.writeChallenge
	a.writeChallenge = func(path string, state desktopReviewChallenge) error {
		if state.State == "applied" {
			return errors.New("injected final receipt failure")
		}
		return writeChallenge(path, state)
	}
	if out, err := a.complete(testCtx(t), c.Nonce, verifier.proof(t, c), false); err != nil || out.Status != "indeterminate" || out.Applied != nil {
		t.Fatalf("receipt failure = %+v, %v", out, err)
	}
	if reviewTentative(t, cfg, id) {
		t.Fatal("vault change did not precede final receipt failure")
	}
	if out, err := newDesktopReviewAuthority(cfg, verifier).complete(testCtx(t), c.Nonce, verifier.proof(t, c), false); err != nil || out.Status != "indeterminate" || out.Applied != nil {
		t.Fatalf("indeterminate replay = %+v, %v", out, err)
	}
}

func TestDesktopReviewAuthorityConcurrentKeepDiscardHasOneWinner(t *testing.T) {
	for _, tc := range []struct{ name, kind string }{{"saved", reviewKindSaved}, {"held", reviewKindHeld}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := reviewSetup(t)
			id := reviewSaved(t, "Concurrent saved desktop preview")
			if tc.kind == reviewKindHeld {
				id = reviewHeld(t, "Concurrent held desktop preview")
			}
			a, verifier := desktopAuthority(t, cfg)
			keep := desktopChallenge(t, a, desktopReviewKeep, tc.kind, id)
			discard := desktopChallenge(t, a, desktopReviewDiscard, tc.kind, id)
			start := make(chan struct{})
			results := make(chan desktopReviewOutcome, 2)
			errs := make(chan error, 2)
			var wg sync.WaitGroup
			for _, challenge := range []desktopReviewChallenge{keep, discard} {
				challenge := challenge
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					out, err := a.complete(testCtx(t), challenge.Nonce, verifier.proof(t, challenge), false)
					results <- out
					errs <- err
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			close(errs)
			applied := 0
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			for out := range results {
				if desktopReviewApplied(out) {
					applied++
				}
			}
			if applied != 1 {
				t.Fatalf("concurrent actions applied %d times", applied)
			}
			if tc.kind == reviewKindSaved {
				if m, err := findMemory(cfg, id); err == nil && memory.IsTentative(m) {
					t.Fatal("saved winner left the note tentative")
				}
				return
			}
			d, err := readMCPProposalDecision(cfg, id)
			if err != nil || d == nil || (d.Status != "approved" && d.Status != "rejected") {
				t.Fatalf("held decision = %+v, %v", d, err)
			}
			if d.Status == "approved" {
				m, err := findMemory(cfg, d.MemoryID)
				if err != nil || m.Source != "mcp" || !memory.IsOwnerConfirmed(m) {
					t.Fatalf("held approval provenance = %+v, %v", m, err)
				}
			}
		})
	}
}

func TestDesktopReviewAuthoritySerializesWithPublicMemoryMutations(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(context.Context, Config, string) error
	}{
		{
			name: "confirm",
			mutate: func(ctx context.Context, cfg Config, id string) error {
				_, _, err := confirmMemory(ctx, cfg, id, "", "", io.Discard)
				return err
			},
		},
		{
			name: "cli delete",
			mutate: func(ctx context.Context, cfg Config, id string) error {
				m, err := findMemory(cfg, id)
				if err != nil {
					return err
				}
				_, err = deleteMemoryFile(ctx, cfg, m)
				return err
			},
		},
		{
			name: "mcp delete",
			mutate: func(ctx context.Context, cfg Config, id string) error {
				_, err := mcpDeleteMemory(ctx, cfg, map[string]any{"id": id})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := reviewSetup(t)
			id := reviewSaved(t, "Desktop versus public "+tc.name)
			a, verifier := desktopAuthority(t, cfg)
			challenge := desktopChallenge(t, a, desktopReviewKeep, reviewKindSaved, id)
			start := make(chan struct{})
			var desktop desktopReviewOutcome
			var desktopErr, publicErr error
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				desktop, desktopErr = a.complete(testCtx(t), challenge.Nonce, verifier.proof(t, challenge), false)
			}()
			go func() {
				defer wg.Done()
				<-start
				publicErr = tc.mutate(testCtx(t), cfg, id)
			}()
			close(start)
			wg.Wait()

			if desktopErr != nil {
				t.Fatalf("desktop completion = %v", desktopErr)
			}
			if tc.name == "confirm" && publicErr == nil && desktopReviewApplied(desktop) {
				t.Fatal("both desktop and public confirmation applied")
			}
			m, err := findMemory(cfg, id)
			if strings.Contains(tc.name, "delete") && publicErr == nil && err != nil {
				return // deletion won, and the desktop action cannot resurrect it.
			}
			if err != nil {
				t.Fatalf("final memory = %v (public mutation = %v)", err, publicErr)
			}
			if memory.IsTentative(m) {
				t.Fatalf("stale mutation resurrected a tentative memory: %+v", m)
			}
			if desktopReviewApplied(desktop) && !memory.IsOwnerConfirmed(m) {
				t.Fatalf("desktop approval lost its exact owner-confirmed state: %+v", m)
			}
		})
	}
}

func TestDesktopReviewAuthorityRecordsStaleWhenPublicMutationWinsBeforeKernel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action desktopReviewAction
		mutate func(context.Context, Config, string) error
	}{
		{"confirm", desktopReviewKeep, func(ctx context.Context, cfg Config, id string) error {
			_, _, err := confirmMemory(ctx, cfg, id, "", "", io.Discard)
			return err
		}},
		{"cli delete", desktopReviewKeep, func(ctx context.Context, cfg Config, id string) error {
			m, err := findMemory(cfg, id)
			if err != nil {
				return err
			}
			_, err = deleteMemoryFile(ctx, cfg, m)
			return err
		}},
		{"mcp delete", desktopReviewDiscard, func(ctx context.Context, cfg Config, id string) error {
			_, err := mcpDeleteMemory(ctx, cfg, map[string]any{"id": id})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := reviewSetup(t)
			id := reviewSaved(t, "durable stale "+tc.name)
			a, verifier := desktopAuthority(t, cfg)
			challenge := desktopChallenge(t, a, tc.action, reviewKindSaved, id)
			write := a.writeChallenge
			a.writeChallenge = func(path string, state desktopReviewChallenge) error {
				if err := write(path, state); err != nil {
					return err
				}
				if state.State == "applying" {
					return tc.mutate(testCtx(t), cfg, id)
				}
				return nil
			}
			out, err := a.complete(testCtx(t), challenge.Nonce, verifier.proof(t, challenge), false)
			if err != nil || out.Status != "stale" || !desktopReviewNotApplied(out) {
				t.Fatalf("completion = %+v, %v", out, err)
			}
			replay, err := a.complete(testCtx(t), challenge.Nonce, verifier.proof(t, challenge), false)
			if err != nil || replay.Status != "stale" || !desktopReviewNotApplied(replay) {
				t.Fatalf("replay = %+v, %v", replay, err)
			}
		})
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
	if _, err := runErr(t, "review", "desktop"); err == nil || !strings.Contains(err.Error(), "usage: mora review desktop --json") {
		t.Fatalf("desktop status refusal = %v", err)
	}
}
