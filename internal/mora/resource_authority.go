package mora

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	indexstore "github.com/pyranthus-hq/mora/internal/index"
	"github.com/pyranthus-hq/mora/internal/leasefile"
	"github.com/pyranthus-hq/mora/internal/managedtree"
	"github.com/pyranthus-hq/mora/internal/resource"
)

type resourceDecisionAction string

const (
	resourceActivate resourceDecisionAction = "activate"
	resourceDetach   resourceDecisionAction = "detach"
	resourceRollback resourceDecisionAction = "rollback"
	resourceRetire   resourceDecisionAction = "retire"
)

type resourceDecisionChallenge struct {
	Nonce, InstallationID, KeyID                                                                 string
	Action                                                                                       resourceDecisionAction
	ResourceID, ResourceKind, RevisionDigest, TreeDigest                                         string
	SourceIdentityDigest, SourceRevision, Scope, Client, ProjectRoot, LoaderRelPath, Destination string
	PriorRevision, PriorManifestDigest, RootSetDigest, DisplayDigest                             string
	ExpiresAt                                                                                    int64
	State                                                                                        string
	Consumed                                                                                     bool
}

func (c resourceDecisionChallenge) bytes() ([]byte, error) {
	if c.Nonce == "" || c.InstallationID == "" || c.KeyID == "" || c.ResourceID == "" || c.ResourceKind == "" || c.RevisionDigest == "" || c.TreeDigest == "" || c.SourceIdentityDigest == "" || c.Scope == "" || c.Client == "" || c.ProjectRoot == "" || c.Destination == "" || c.DisplayDigest == "" || c.ExpiresAt <= 0 {
		return nil, errors.New("invalid resource decision challenge")
	}
	if c.Action != resourceActivate && c.Action != resourceDetach && c.Action != resourceRollback && c.Action != resourceRetire {
		return nil, errors.New("invalid resource action")
	}
	if (c.Action == resourceDetach || c.Action == resourceRollback || c.Action == resourceRetire) && (c.PriorRevision == "" || c.PriorManifestDigest == "") {
		return nil, errors.New("resource action requires prior local materialization")
	}
	values := []string{c.Nonce, c.InstallationID, c.KeyID, string(c.Action), c.ResourceID, c.ResourceKind, c.RevisionDigest, c.TreeDigest, c.SourceIdentityDigest, c.SourceRevision, c.Scope, c.Client, c.ProjectRoot, c.LoaderRelPath, c.Destination, c.PriorRevision, c.PriorManifestDigest, c.RootSetDigest, c.DisplayDigest}
	var out bytes.Buffer
	out.WriteString("mora-resource-decision-v1\x00")
	for _, value := range values {
		if len(value) > 1<<20 {
			return nil, errors.New("resource decision field too large")
		}
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(value)))
		out.Write(n[:])
		out.WriteString(value)
	}
	var expiry [8]byte
	binary.BigEndian.PutUint64(expiry[:], uint64(c.ExpiresAt))
	out.Write(expiry[:])
	return out.Bytes(), nil
}
func resourceChallengePath(cfg Config, nonce string) string {
	return filepath.Join(cfg.StateDir, "resources", "challenges", nonce+".json")
}
func writeResourceChallenge(path string, c resourceDecisionChallenge) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return atomicio.WriteDurable(path, b, 0o600)
}

type resourceDecisionAuthority struct {
	cfg      Config
	verifier desktopReviewVerifier
	now      func() time.Time
}

func newResourceDecisionAuthority(cfg Config, verifier desktopReviewVerifier) *resourceDecisionAuthority {
	if verifier == nil {
		verifier = unavailableDesktopReviewVerifier{}
	}
	return &resourceDecisionAuthority{cfg: cfg, verifier: verifier, now: time.Now}
}

func resourceRootSetDigest(cfg Config, scope, client string) (string, error) {
	if scope != "global" {
		return "", nil
	}
	bs, err := loadProjectBindings(cfg)
	if err != nil {
		return "", err
	}
	roots := make([]string, 0, len(bs))
	for _, b := range bs {
		if _, err := resourceDestination(b.Dir, client, "probe"); err == nil {
			roots = append(roots, b.Dir)
		}
	}
	sort.Strings(roots)
	h := sha256.Sum256([]byte("resource-root-set-v1\x00" + stringsJoin(roots, "\x00")))
	return hex.EncodeToString(h[:]), nil
}
func stringsJoin(parts []string, sep string) string {
	if len(parts) == 0 {
		return ""
	}
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	n += len(sep) * (len(parts) - 1)
	b := make([]byte, 0, n)
	for i, p := range parts {
		if i > 0 {
			b = append(b, sep...)
		}
		b = append(b, p...)
	}
	return string(b)
}

func resourcePreview(cfg Config, projectDir, client, id, revision string) (desktopReviewPreview, resource.Record, resource.Revision, projectBinding, string, error) {
	binding, found, err := lookupProjectBinding(cfg, projectDir)
	if err != nil || !found {
		return desktopReviewPreview{}, resource.Record{}, resource.Revision{}, projectBinding{}, "", errDesktopReviewUnavailable
	}
	if client != "codex" && client != "claude" {
		return desktopReviewPreview{}, resource.Record{}, resource.Revision{}, projectBinding{}, "", errDesktopReviewUnavailable
	}
	record, rev, err := resource.ReadRevision(cfg.VaultDir, id, revision)
	if err != nil || !rev.Complete {
		return desktopReviewPreview{}, resource.Record{}, resource.Revision{}, projectBinding{}, "", errDesktopReviewUnavailable
	}
	if rev.EffectiveScope != binding.Scope && rev.EffectiveScope != "global" {
		return desktopReviewPreview{}, resource.Record{}, resource.Revision{}, projectBinding{}, "", errDesktopReviewUnavailable
	}
	destination, err := resourceDestination(binding.Dir, client, record.StableID)
	if err != nil {
		return desktopReviewPreview{}, resource.Record{}, resource.Revision{}, projectBinding{}, "", errDesktopReviewUnavailable
	}
	display := desktopReviewDisplay{ID: id, Kind: "resource", Title: record.StableID, Text: rev.Purpose, Scope: rev.EffectiveScope, Type: string(rev.Kind), Source: string(rev.Source), State: "unreviewed", Complete: true, Details: []desktopReviewField{{Label: "revision", Value: rev.RevisionDigest}, {Label: "tree digest", Value: rev.TreeDigest}, {Label: "destination", Value: destination}, {Label: "files", Value: fmt.Sprint(len(rev.Files))}}}
	for _, f := range rev.Files {
		display.addDetail("file", f.Path)
	}
	p, err := newDesktopReviewPreview(display, rev.RevisionDigest)
	if err == nil {
		p.Files = append([]managedtree.File(nil), rev.Files...)
		raw, marshalErr := json.Marshal(struct {
			Display desktopReviewDisplay `json:"display"`
			Files   []managedtree.File   `json:"files"`
		}{display, p.Files})
		if marshalErr != nil {
			return desktopReviewPreview{}, resource.Record{}, resource.Revision{}, projectBinding{}, "", errDesktopReviewUnavailable
		}
		p.DisplayDigest = indexstore.ContentSHA256(append([]byte("mora-resource-review-display-v1\n"), raw...))
	}
	return p, record, rev, binding, destination, err
}

func (a *resourceDecisionAuthority) begin(action resourceDecisionAction, projectDir, client, id, revision, digest string) (resourceDecisionChallenge, error) {
	installation, key, trusted := a.verifier.TrustedIdentity()
	if !trusted || installation == "" || key == "" {
		return resourceDecisionChallenge{}, errDesktopReviewProofUnavailable
	}
	p, record, rev, binding, destination, err := resourcePreview(a.cfg, projectDir, client, id, revision)
	if err != nil || p.DisplayDigest != digest {
		return resourceDecisionChallenge{}, errDesktopReviewUnavailable
	}
	priorRevision, priorManifest := "", ""
	if action == resourceActivate || action == resourceRollback || action == resourceDetach || action == resourceRetire {
		d, found, err := latestResourceDecision(a.cfg, installation, id, rev.EffectiveScope, client, binding.Dir)
		if found && (d.Action == "retire" || d.Action == "detach") {
			found = false
		}
		if err != nil || (!found && action != resourceActivate) {
			return resourceDecisionChallenge{}, errDesktopReviewUnavailable
		}
		if found {
			receipt, err := readResourceReceipt(a.cfg, d)
			if err != nil {
				return resourceDecisionChallenge{}, errDesktopReviewUnavailable
			}
			priorRevision = d.RevisionDigest
			priorManifest = receipt.ManifestDigest
		}
	}
	rootset, err := resourceRootSetDigest(a.cfg, rev.EffectiveScope, client)
	if err != nil {
		return resourceDecisionChallenge{}, errDesktopReviewUnavailable
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return resourceDecisionChallenge{}, err
	}
	c := resourceDecisionChallenge{Nonce: hex.EncodeToString(random[:]), InstallationID: installation, KeyID: key, Action: action, ResourceID: id, ResourceKind: string(record.Kind), RevisionDigest: rev.RevisionDigest, TreeDigest: rev.TreeDigest, SourceIdentityDigest: resourceSourceIdentityDigest(record, rev), SourceRevision: rev.Reference.Version, Scope: rev.EffectiveScope, Client: client, ProjectRoot: binding.Dir, Destination: destination, PriorRevision: priorRevision, PriorManifestDigest: priorManifest, RootSetDigest: rootset, DisplayDigest: digest, ExpiresAt: a.now().Add(desktopReviewChallengeLifetime).Unix(), State: "issued"}
	if _, err := c.bytes(); err != nil {
		return resourceDecisionChallenge{}, err
	}
	if err := writeResourceChallenge(resourceChallengePath(a.cfg, c.Nonce), c); err != nil {
		return resourceDecisionChallenge{}, err
	}
	return c, nil
}

func (a *resourceDecisionAuthority) complete(ctx context.Context, nonce string, proof []byte, cancelled bool) (desktopReviewOutcome, error) {
	if len(nonce) != 64 {
		return desktopReviewOutcome{}, errDesktopReviewUnavailable
	}
	path := resourceChallengePath(a.cfg, nonce)
	var outcome desktopReviewOutcome
	err := leasefile.WithGuard(path+".lock", func() error {
		raw, err := os.ReadFile(path)
		if err != nil {
			return errDesktopReviewUnavailable
		}
		var c resourceDecisionChallenge
		if json.Unmarshal(raw, &c) != nil || c.Nonce != nonce {
			return errDesktopReviewUnavailable
		}
		signed, err := c.bytes()
		if err != nil {
			return errDesktopReviewUnavailable
		}
		if c.Consumed {
			return nil
		}
		if a.now().Unix() >= c.ExpiresAt {
			return errDesktopReviewUnavailable
		}
		if cancelled {
			c.Consumed = true
			c.State = "cancelled"
			if err := writeResourceChallenge(path, c); err != nil {
				return err
			}
			outcome = desktopReviewOutcome{Status: "cancelled", Applied: desktopReviewBool(false), IndexVisibility: "not_checked"}
			return nil
		}
		if !a.verifier.Verify(c.InstallationID, c.KeyID, signed, proof) {
			return errDesktopReviewProofUnavailable
		}
		d := resourceBindingDecision{Schema: resourceDecisionSchema, Action: string(c.Action), ResourceID: c.ResourceID, ResourceKind: c.ResourceKind, RevisionDigest: c.RevisionDigest, TreeDigest: c.TreeDigest, SourceIdentityDigest: c.SourceIdentityDigest, SourceRevision: c.SourceRevision, Scope: c.Scope, Client: c.Client, ProjectRoot: c.ProjectRoot, Destination: c.Destination, RootSetDigest: c.RootSetDigest, PriorRevision: c.PriorRevision, PriorManifestDigest: c.PriorManifestDigest, InstallationID: c.InstallationID, KeyID: c.KeyID, ChallengeNonce: c.Nonce, DisplayDigest: c.DisplayDigest, VerifiedAt: a.now().UTC().Format(time.RFC3339)}
		return leasefile.WithGuard(resourceBindingLock(a.cfg, d), func() error {
			p, record, rev, binding, destination, err := resourcePreview(a.cfg, c.ProjectRoot, c.Client, c.ResourceID, c.RevisionDigest)
			if err != nil || p.DisplayDigest != c.DisplayDigest || destination != c.Destination || binding.Scope != c.Scope || string(record.Kind) != c.ResourceKind || rev.TreeDigest != c.TreeDigest || resourceSourceIdentityDigest(record, rev) != c.SourceIdentityDigest {
				return errDesktopReviewUnavailable
			}
			c.Consumed = true
			c.State = "applying"
			if err := writeResourceChallenge(path, c); err != nil {
				return err
			}
			if got, err := resourceRootSetDigest(a.cfg, c.Scope, c.Client); err != nil || got != c.RootSetDigest {
				return errDesktopReviewUnavailable
			}
			if err := appendResourceDecision(a.cfg, d); err != nil {
				return err
			}
			if c.Action == resourceDetach || c.Action == resourceRetire {
				old, found, err := latestResourceDecision(a.cfg, c.InstallationID, c.ResourceID, c.Scope, c.Client, c.ProjectRoot)
				if err != nil || !found {
					return errDesktopReviewUnavailable
				}
				_ = old
				if err := detachResource(a.cfg, d); err != nil {
					c.State = "retired_loader_conflict"
					_ = writeResourceChallenge(path, c)
					outcome = desktopReviewOutcome{Status: "retired_loader_conflict", Applied: desktopReviewBool(true), IndexVisibility: "not_checked"}
					return nil
				}
				c.State = string(c.Action)
				_ = writeResourceChallenge(path, c)
				outcome = desktopReviewOutcome{Status: c.State, Applied: desktopReviewBool(true), IndexVisibility: "not_checked"}
				return nil
			}
			var prior *managedtree.Manifest
			if c.PriorRevision != "" {
				old, found, _ := latestResourceDecision(a.cfg, c.InstallationID, c.ResourceID, c.Scope, c.Client, c.ProjectRoot)
				if found {
					r, err := readResourceReceipt(a.cfg, old)
					if err == nil {
						prior = &r.Manifest
					}
				}
			}
			_, err = materializeResource(a.cfg, d, prior)
			if err != nil {
				c.State = "indeterminate"
				_ = writeResourceChallenge(path, c)
				outcome = desktopReviewIndeterminateOutcome()
				return nil
			}
			if c.Scope == "global" {
				if err := materializeGlobalResource(a.cfg, d); err != nil {
					c.State = "indeterminate"
					_ = writeResourceChallenge(path, c)
					outcome = desktopReviewIndeterminateOutcome()
					return nil
				}
			}
			c.State = "applied"
			if err := writeResourceChallenge(path, c); err != nil {
				outcome = desktopReviewIndeterminateOutcome()
				return nil
			}
			outcome = desktopReviewOutcome{Status: "applied", Applied: desktopReviewBool(true), IndexVisibility: "not_checked"}
			return nil
		})
	})
	if err != nil {
		return outcome, err
	}
	return outcome, nil
}
