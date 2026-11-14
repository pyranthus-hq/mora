package mora

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/leasefile"
)

const desktopReviewChallengeLifetime = 90 * time.Second

var errDesktopReviewProofUnavailable = errors.New("desktop review verification is unavailable; use mora review in your own terminal")

// desktopReviewVerifier is intentionally package-private: production has no
// trusted native adapter yet, while tests can exercise the full state machine.
type desktopReviewVerifier interface {
	Verify([]byte, []byte) bool
}

type unavailableDesktopReviewVerifier struct{}

func (unavailableDesktopReviewVerifier) Verify([]byte, []byte) bool { return false }

type desktopReviewAuthority struct {
	cfg      Config
	verifier desktopReviewVerifier
	now      func() time.Time
}

func newDesktopReviewAuthority(cfg Config, verifier desktopReviewVerifier) *desktopReviewAuthority {
	if verifier == nil {
		verifier = unavailableDesktopReviewVerifier{}
	}
	return &desktopReviewAuthority{cfg: cfg, verifier: verifier, now: time.Now}
}

func desktopReviewChallengesDir(cfg Config) string {
	return filepath.Join(cfg.StateDir, "desktop-review", "challenges")
}
func desktopReviewChallengePath(cfg Config, nonce string) string {
	return filepath.Join(desktopReviewChallengesDir(cfg), nonce+".json")
}

func (a *desktopReviewAuthority) begin(action desktopReviewAction, kind, id, revision, digest string) (desktopReviewChallenge, error) {
	if _, ok := a.verifier.(unavailableDesktopReviewVerifier); ok {
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
		Action: action, Kind: kind, ID: id, Scope: preview.Scope,
		CanonicalRevision: preview.CanonicalRevision, DisplayDigest: preview.DisplayDigest,
	}
	if _, err := c.bytes(); err != nil {
		return desktopReviewChallenge{}, err
	}
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
		if c.Consumed {
			out = desktopReviewOutcome{Status: "consumed", Applied: c.Applied, IndexVisibility: "not_checked"}
			return nil
		}
		if a.now().Unix() >= c.ExpiresAt {
			return errors.New("desktop review challenge expired")
		}
		if cancelled {
			c.Consumed = true
			if writeErr := writeDesktopReviewChallenge(path, c); writeErr != nil {
				return writeErr
			}
			out = desktopReviewOutcome{Status: "cancelled", IndexVisibility: "not_checked"}
			return nil
		}
		bytes, bytesErr := c.bytes()
		if bytesErr != nil || !a.verifier.Verify(bytes, proof) {
			return errDesktopReviewProofUnavailable
		}
		preview, previewErr := loadDesktopReviewPreview(a.cfg, c.Kind, c.ID)
		if previewErr != nil || preview.Scope != c.Scope || preview.CanonicalRevision != c.CanonicalRevision || preview.DisplayDigest != c.DisplayDigest ||
			(c.Action == desktopReviewKeep && !preview.Complete) {
			return errDesktopReviewUnavailable
		}
		// Consume before mutation. A crash is reported as consumed-but-not-applied,
		// never as an exactly-once success based on the nonce ledger alone.
		c.Consumed = true
		if writeErr := writeDesktopReviewChallenge(path, c); writeErr != nil {
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
			_, err = decideMCPWriteProposalVia(ctx, a.cfg, c.ID, verb, "", c.CanonicalRevision)
		default:
			return errDesktopReviewUnavailable
		}
		if err != nil {
			return err
		}
		c.Applied = true
		if writeErr := writeDesktopReviewChallenge(path, c); writeErr != nil {
			return fmt.Errorf("decision applied but receipt update failed: %w", writeErr)
		}
		out = desktopReviewOutcome{Status: "applied", Applied: true, IndexVisibility: "not_checked"}
		return nil
	})
	return out, err
}

func writeDesktopReviewChallenge(path string, c desktopReviewChallenge) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return atomicio.WriteDurable(path, raw, 0o600)
}

func cmdReviewDesktop(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) != 1 || args[0] != "--json" {
		return errors.New("usage: mora review desktop --json")
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	_ = cfg
	return emitReceipt(stdout, "mora.review.desktop", 1, desktopReviewOutcome{Status: "unavailable", IndexVisibility: "not_checked"})
}
