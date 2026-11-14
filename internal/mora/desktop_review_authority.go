package mora

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/leasefile"
)

const desktopReviewChallengeLifetime = 90 * time.Second

var errDesktopReviewProofUnavailable = errOwnerVerificationUnavailable

// desktopReviewVerifier is intentionally package-private: production has no
// trusted native adapter yet, while tests can exercise the full state machine.
type desktopReviewVerifier interface {
	TrustedIdentity() (installationID, keyID string, ok bool)
	Verify(installationID, keyID string, signed, proof []byte) bool
}

type unavailableDesktopReviewVerifier struct{}

func (unavailableDesktopReviewVerifier) TrustedIdentity() (string, string, bool) {
	return "", "", false
}
func (unavailableDesktopReviewVerifier) Verify(string, string, []byte, []byte) bool { return false }

type desktopReviewAuthority struct {
	cfg            Config
	verifier       desktopReviewVerifier
	now            func() time.Time
	writeChallenge func(string, desktopReviewChallenge) error
}

func newDesktopReviewAuthority(cfg Config, verifier desktopReviewVerifier) *desktopReviewAuthority {
	if verifier == nil {
		verifier = unavailableDesktopReviewVerifier{}
	}
	return &desktopReviewAuthority{cfg: cfg, verifier: verifier, now: time.Now, writeChallenge: writeDesktopReviewChallenge}
}

func desktopReviewChallengesDir(cfg Config) string {
	return filepath.Join(cfg.StateDir, "desktop-review", "challenges")
}
func desktopReviewChallengePath(cfg Config, nonce string) string {
	return filepath.Join(desktopReviewChallengesDir(cfg), nonce+".json")
}

func (a *desktopReviewAuthority) begin(action desktopReviewAction, kind, id, revision, digest string) (desktopReviewChallenge, error) {
	installationID, keyID, trusted := a.verifier.TrustedIdentity()
	if !trusted || installationID == "" || keyID == "" {
		return desktopReviewChallenge{}, errDesktopReviewProofUnavailable
	}
	if action != desktopReviewKeep && action != desktopReviewDiscard {
		return desktopReviewChallenge{}, errors.New("invalid desktop review action")
	}
	preview, err := loadDesktopReviewPreview(a.cfg, kind, id)
	if err != nil || preview.CanonicalRevision != revision || preview.DisplayDigest != digest {
		return desktopReviewChallenge{}, errDesktopReviewUnavailable
	}
	if action == desktopReviewKeep && !preview.Complete {
		return desktopReviewChallenge{}, errors.New("cannot keep an incomplete desktop review preview")
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return desktopReviewChallenge{}, err
	}
	c := desktopReviewChallenge{
		Schema: "mora.review.desktop", SchemaVersion: desktopReviewProtocolVersion,
		Nonce: hex.EncodeToString(random[:]), ExpiresAt: a.now().Add(desktopReviewChallengeLifetime).Unix(),
		InstallationID: installationID, KeyID: keyID, Action: action, Kind: kind, ID: id, Scope: preview.Scope,
		CanonicalRevision: preview.CanonicalRevision, DisplayDigest: preview.DisplayDigest,
		State: "issued",
	}
	issued, err := c.bytes()
	if err != nil {
		return desktopReviewChallenge{}, err
	}
	issuedSum := sha256.Sum256(issued)
	c.IssuedDigest = hex.EncodeToString(issuedSum[:])
	path := desktopReviewChallengePath(a.cfg, c.Nonce)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return desktopReviewChallenge{}, err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return desktopReviewChallenge{}, err
	}
	if err := atomicio.WriteDurable(path, raw, 0o600); err != nil {
		return desktopReviewChallenge{}, err
	}
	return c, nil
}

// beginProjectRule is intentionally separate from generic review. It resolves
// only an existing binding and signs the exact loader/root/client tuple.
func (a *desktopReviewAuthority) beginProjectRule(action desktopReviewAction, projectDir, id, revision, digest, client string) (desktopReviewChallenge, error) {
	installationID, keyID, trusted := a.verifier.TrustedIdentity()
	if !trusted || installationID == "" || keyID == "" {
		return desktopReviewChallenge{}, errDesktopReviewProofUnavailable
	}
	if (action != desktopReviewActivate && action != desktopReviewRetire) || client != "codex" {
		return desktopReviewChallenge{}, errDesktopReviewUnavailable
	}
	binding, found, err := lookupProjectBinding(a.cfg, projectDir)
	if err != nil || !found {
		return desktopReviewChallenge{}, errDesktopReviewUnavailable
	}
	_, loader, _, err := selectCodexProjectInstructions(binding)
	if err != nil {
		return desktopReviewChallenge{}, errDesktopReviewUnavailable
	}
	sourceID, materialization := id, ""
	if action == desktopReviewActivate {
		r, loadErr := loadProjectRuleRevision(a.cfg, binding, id)
		if loadErr != nil || r.RevisionSHA256 != revision || projectRuleDisplayDigest(r) != digest {
			return desktopReviewChallenge{}, errDesktopReviewUnavailable
		}
	} else {
		d, active, stateErr := latestProjectRuleDecision(a.cfg, binding, installationID, client, id)
		if stateErr != nil || !active || d.Action != "activate" || d.RevisionSHA256 != revision || d.DisplayDigest != digest || d.LoaderRelPath != loader {
			return desktopReviewChallenge{}, errDesktopReviewUnavailable
		}
		sourceID = d.SourceMemoryID
		materialization, err = projectRuleMaterializationDigest(a.cfg, binding, installationID, client)
		if err != nil {
			return desktopReviewChallenge{}, errDesktopReviewUnavailable
		}
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return desktopReviewChallenge{}, err
	}
	c := desktopReviewChallenge{
		Schema: "mora.review.desktop", SchemaVersion: desktopReviewProtocolVersion,
		Nonce: hex.EncodeToString(random[:]), ExpiresAt: a.now().Add(desktopReviewChallengeLifetime).Unix(),
		InstallationID: installationID, KeyID: keyID, Action: action, Kind: desktopReviewProjectRule, ID: id, Scope: binding.Scope,
		CanonicalRevision: revision, DisplayDigest: digest, Client: client, ProjectRoot: binding.Dir, LoaderRelPath: loader,
		SourceMemoryID: sourceID, MaterializationDigest: materialization, State: "issued",
	}
	signed, err := c.bytes()
	if err != nil {
		return desktopReviewChallenge{}, err
	}
	sum := sha256.Sum256(signed)
	c.IssuedDigest = hex.EncodeToString(sum[:])
	path := desktopReviewChallengePath(a.cfg, c.Nonce)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return desktopReviewChallenge{}, err
	}
	if err := a.writeChallenge(path, c); err != nil {
		return desktopReviewChallenge{}, err
	}
	return c, nil
}

func (a *desktopReviewAuthority) complete(ctx context.Context, nonce string, proof []byte, cancelled bool) (out desktopReviewOutcome, err error) {
	if len(nonce) != 64 {
		return out, errDesktopReviewUnavailable
	}
	if _, decodeErr := hex.DecodeString(nonce); decodeErr != nil {
		return out, errDesktopReviewUnavailable
	}
	path := desktopReviewChallengePath(a.cfg, nonce)
	err = leasefile.WithGuard(path+".lock", func() error {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return errDesktopReviewUnavailable
		}
		var c desktopReviewChallenge
		if json.Unmarshal(raw, &c) != nil || c.Nonce != nonce {
			return errDesktopReviewUnavailable
		}
		bytes, bytesErr := c.bytes()
		issuedSum := sha256.Sum256(bytes)
		issuedDigest := hex.EncodeToString(issuedSum[:])
		if bytesErr != nil || c.IssuedDigest == "" || issuedDigest != c.IssuedDigest {
			return errDesktopReviewProofUnavailable
		}
		if c.Consumed {
			out = desktopReviewRecordedOutcome(c)
			return nil
		}
		if a.now().Unix() >= c.ExpiresAt {
			return errors.New("desktop review challenge expired")
		}
		if cancelled {
			c.Consumed, c.State = true, "cancelled"
			if writeErr := a.writeChallenge(path, c); writeErr != nil {
				return writeErr
			}
			out = desktopReviewOutcome{Status: "cancelled", Applied: desktopReviewBool(false), IndexVisibility: "not_checked"}
			return nil
		}
		if !a.verifier.Verify(c.InstallationID, c.KeyID, bytes, proof) {
			return errDesktopReviewProofUnavailable
		}
		itemGuard := func(fn func() error) error { return fn() }
		if c.Kind == reviewKindHeld {
			itemGuard = func(fn func() error) error {
				return leasefile.WithGuard(desktopReviewItemLockPath(a.cfg, c.Kind, c.ID), fn)
			}
		} else if c.Kind == desktopReviewProjectRule {
			itemGuard = func(fn func() error) error {
				return withMemoryMutationGuard(a.cfg, c.SourceMemoryID, func() error {
					binding := projectBinding{Dir: c.ProjectRoot, Scope: c.Scope}
					return leasefile.WithGuard(projectRuleMaterializationLockPath(a.cfg, binding), fn)
				})
			}
		}
		return itemGuard(func() error {
			if c.Kind == desktopReviewProjectRule {
				// "applying" is durable before every project-rule mutation, so a
				// replay cannot append another decision after a crash.
				c.Consumed, c.State = true, "applying"
				if writeErr := a.writeChallenge(path, c); writeErr != nil {
					return writeErr
				}
				out, err = applyProjectRuleDecision(ctx, a.cfg, c)
				if err != nil {
					if desktopReviewIsStale(err) {
						c.State = "stale"
						if writeErr := a.writeChallenge(path, c); writeErr != nil {
							out = desktopReviewIndeterminateOutcome()
							return nil
						}
						out = desktopReviewStaleOutcome()
						return nil
					}
					out = desktopReviewIndeterminateOutcome()
					return nil
				}
				c.Applied, c.State = true, "applied"
				if writeErr := a.writeChallenge(path, c); writeErr != nil {
					out = desktopReviewIndeterminateOutcome()
				}
				return nil
			}
			preview, previewErr := loadDesktopReviewPreview(a.cfg, c.Kind, c.ID)
			if previewErr != nil || preview.Scope != c.Scope || preview.CanonicalRevision != c.CanonicalRevision || preview.DisplayDigest != c.DisplayDigest ||
				(c.Action == desktopReviewKeep && !preview.Complete) {
				out = desktopReviewStaleOutcome()
				return nil
			}
			// "applying" is durable before the kernel mutation. A later replay never
			// retries it: the mutation may have reached the vault before a crash.
			c.Consumed, c.State = true, "applying"
			if writeErr := a.writeChallenge(path, c); writeErr != nil {
				return writeErr
			}
			switch c.Kind {
			case reviewKindSaved:
				note, noteErr := loadDesktopSavedReviewNote(a.cfg, c.ID)
				if noteErr != nil || note.SHA != c.CanonicalRevision || note.Scope != c.Scope {
					c.State = "stale"
					if writeErr := a.writeChallenge(path, c); writeErr != nil {
						out = desktopReviewIndeterminateOutcome()
						return nil
					}
					out = desktopReviewStaleOutcome()
					return nil
				}
				if c.Action == desktopReviewKeep {
					_, _, err = confirmMemory(ctx, a.cfg, c.ID, note.SHA, note.Confirm, io.Discard)
				} else {
					m, findErr := findMemory(a.cfg, c.ID)
					if findErr != nil {
						c.State = "stale"
						if writeErr := a.writeChallenge(path, c); writeErr != nil {
							out = desktopReviewIndeterminateOutcome()
							return nil
						}
						out = desktopReviewStaleOutcome()
						return nil
					}
					_, _, _, err = undoMemory(ctx, a.cfg, m, false, note.SHA, io.Discard)
				}
			case reviewKindHeld:
				verb := "approve"
				if c.Action == desktopReviewDiscard {
					verb = "reject"
				}
				opts := []createOption(nil)
				if c.Action == desktopReviewKeep {
					opts = []createOption{withOwnerConfirmed()}
				}
				_, err = decideMCPWriteProposalVia(ctx, a.cfg, c.ID, verb, "", c.CanonicalRevision, opts...)
			default:
				return errDesktopReviewUnavailable
			}
			if err != nil {
				if desktopReviewIsStale(err) {
					c.State = "stale"
					if writeErr := a.writeChallenge(path, c); writeErr != nil {
						out = desktopReviewIndeterminateOutcome()
						return nil
					}
					out = desktopReviewStaleOutcome()
					return nil
				}
				// The shared kernels can report after a vault change (for example an
				// index or durable decision receipt failure). Do not infer a negative.
				out = desktopReviewIndeterminateOutcome()
				return nil
			}
			c.Applied, c.State = true, "applied"
			if writeErr := a.writeChallenge(path, c); writeErr != nil {
				out = desktopReviewIndeterminateOutcome()
				return nil
			}
			out = desktopReviewOutcome{Status: "applied", Applied: desktopReviewBool(true), IndexVisibility: "not_checked"}
			return nil
		})
	})
	return out, err
}

func applyProjectRuleDecision(_ context.Context, cfg Config, c desktopReviewChallenge) (desktopReviewOutcome, error) {
	binding, found, err := lookupProjectBinding(cfg, c.ProjectRoot)
	if err != nil || !found || binding.Dir != c.ProjectRoot || binding.Scope != c.Scope || c.Client != "codex" {
		return desktopReviewStaleOutcome(), errDesktopReviewUnavailable
	}
	_, loader, _, err := selectCodexProjectInstructions(binding)
	if err != nil || loader != c.LoaderRelPath {
		return desktopReviewStaleOutcome(), errDesktopReviewUnavailable
	}
	if c.Action == desktopReviewActivate {
		r, loadErr := loadProjectRuleRevision(cfg, binding, c.SourceMemoryID)
		if loadErr != nil || r.RuleID != c.ID || r.RevisionSHA256 != c.CanonicalRevision || projectRuleDisplayDigest(r) != c.DisplayDigest {
			return desktopReviewStaleOutcome(), errDesktopReviewUnavailable
		}
		if err := preflightProjectRuleActivation(cfg, binding, c.InstallationID, c.Client, r); err != nil {
			return desktopReviewStaleOutcome(), err
		}
		if err := appendProjectRuleDecision(cfg, projectRuleDecision{Schema: projectRuleDecisionSchema, Action: "activate", RuleID: r.RuleID, SourceMemoryID: r.SourceMemoryID, Scope: r.Scope, RevisionSHA256: r.RevisionSHA256, InstallationID: c.InstallationID, KeyID: c.KeyID, Client: c.Client, ProjectRoot: binding.Dir, LoaderRelPath: loader, ChallengeNonce: c.Nonce, DisplayDigest: c.DisplayDigest, VerifiedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
			return desktopReviewIndeterminateOutcome(), err
		}
		if err := reconcileProjectRuleMaterializationLocked(cfg, binding, c.InstallationID, c.Client); err != nil {
			return desktopReviewIndeterminateOutcome(), err
		}
		return desktopReviewOutcome{Status: "applied", Applied: desktopReviewBool(true), IndexVisibility: "not_checked"}, nil
	}
	if c.Action != desktopReviewRetire {
		return desktopReviewStaleOutcome(), errDesktopReviewUnavailable
	}
	d, active, stateErr := latestProjectRuleDecision(cfg, binding, c.InstallationID, c.Client, c.ID)
	if stateErr != nil || !active || d.Action != "activate" || d.SourceMemoryID != c.SourceMemoryID || d.RevisionSHA256 != c.CanonicalRevision || d.DisplayDigest != c.DisplayDigest || d.LoaderRelPath != loader {
		return desktopReviewStaleOutcome(), errDesktopReviewUnavailable
	}
	materialization, materializationErr := projectRuleMaterializationDigest(cfg, binding, c.InstallationID, c.Client)
	if materializationErr != nil || materialization != c.MaterializationDigest {
		return desktopReviewStaleOutcome(), errDesktopReviewUnavailable
	}
	retire := d
	retire.Action, retire.ChallengeNonce, retire.KeyID, retire.VerifiedAt = "retire", c.Nonce, c.KeyID, time.Now().UTC().Format(time.RFC3339)
	if err := appendProjectRuleDecision(cfg, retire); err != nil {
		return desktopReviewIndeterminateOutcome(), err
	}
	if err := reconcileProjectRuleMaterializationLocked(cfg, binding, c.InstallationID, c.Client); err != nil {
		return desktopReviewOutcome{Status: "retired_loader_conflict", Applied: desktopReviewBool(true), IndexVisibility: "not_checked"}, nil
	}
	return desktopReviewOutcome{Status: "retired", Applied: desktopReviewBool(true), IndexVisibility: "not_checked"}, nil
}

func desktopReviewItemLockPath(cfg Config, kind, id string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + id))
	return filepath.Join(desktopReviewChallengesDir(cfg), "items", hex.EncodeToString(sum[:])+".lock")
}

func desktopReviewBool(value bool) *bool { return &value }

func desktopReviewStaleOutcome() desktopReviewOutcome {
	return desktopReviewOutcome{Status: "stale", Applied: desktopReviewBool(false), IndexVisibility: "not_checked"}
}

func desktopReviewIndeterminateOutcome() desktopReviewOutcome {
	return desktopReviewOutcome{Status: "indeterminate", IndexVisibility: "not_checked"}
}

func desktopReviewRecordedOutcome(c desktopReviewChallenge) desktopReviewOutcome {
	switch c.State {
	case "cancelled":
		return desktopReviewOutcome{Status: "cancelled", Applied: desktopReviewBool(false), IndexVisibility: "not_checked"}
	case "applied":
		return desktopReviewOutcome{Status: "applied", Applied: desktopReviewBool(true), IndexVisibility: "not_checked"}
	case "stale":
		return desktopReviewStaleOutcome()
	default:
		return desktopReviewIndeterminateOutcome()
	}
}

func desktopReviewIsStale(err error) bool {
	return errors.Is(err, errDesktopReviewUnavailable) || strings.Contains(err.Error(), "changed after it was shown") ||
		strings.Contains(err.Error(), "proposal arguments changed") || strings.Contains(err.Error(), "decision conflict")
}

func writeDesktopReviewChallenge(path string, c desktopReviewChallenge) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return atomicio.WriteDurable(path, raw, 0o600)
}

func cmdReviewDesktop(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) error {
	if len(args) != 1 || (args[0] != "--json" && args[0] != "--stdio") {
		return errors.New("usage: mora review desktop --json")
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	if args[0] == "--stdio" {
		return serveNativeDesktopReview(ctx, cfg, stdin, stdout)
	}
	return emitReceipt(stdout, "mora.review.desktop", 1, desktopReviewOutcome{Status: "unavailable", IndexVisibility: "not_checked"})
}
