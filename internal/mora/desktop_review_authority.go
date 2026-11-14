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
		}
		return itemGuard(func() error {
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
					return errDesktopReviewUnavailable
				}
				if c.Action == desktopReviewKeep {
					_, _, err = confirmMemory(ctx, a.cfg, c.ID, note.SHA, note.Confirm, io.Discard)
				} else {
					m, findErr := findMemory(a.cfg, c.ID)
					if findErr != nil {
						return errDesktopReviewUnavailable
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
		return serveDesktopReviewStdio(ctx, cfg, newNativeOwnerVerifier(cfg), stdin, stdout)
	}
	return emitReceipt(stdout, "mora.review.desktop", 1, desktopReviewOutcome{Status: "unavailable", IndexVisibility: "not_checked"})
}
