package mora

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/leasefile"
	"github.com/pyranthus-hq/mora/internal/managedtree"
	"github.com/pyranthus-hq/mora/internal/resource"
)

const resourceDecisionSchema = "mora-resource-decision/v1"

type resourceBindingDecision struct {
	Schema, Action, ResourceID, ResourceKind, RevisionDigest, TreeDigest string
	SourceIdentityDigest, SourceRevision, Scope, Client, ProjectRoot     string
	Destination, RootSetDigest, PriorRevision, PriorManifestDigest       string
	InstallationID, KeyID, ChallengeNonce, DisplayDigest, VerifiedAt     string
}

type resourceMaterializationReceipt struct {
	Schema, ResourceID, RevisionDigest, Scope, Client, ProjectRoot, Destination string
	Manifest                                                                    managedtree.Manifest `json:"manifest"`
	ManifestDigest                                                              string               `json:"manifest_digest"`
	Status                                                                      string               `json:"status"`
}

func resourceDecisionPath(cfg Config) string {
	return filepath.Join(cfg.StateDir, "resources", "decisions", "decisions.jsonl")
}
func resourceMaterializationPath(cfg Config, d resourceBindingDecision) string {
	s := sha256.Sum256([]byte(strings.Join([]string{d.InstallationID, d.ResourceID, d.Scope, d.Client, d.ProjectRoot}, "\x00")))
	return filepath.Join(cfg.StateDir, "resources", "materializations", hex.EncodeToString(s[:])+".json")
}
func resourceBindingLock(cfg Config, d resourceBindingDecision) string {
	return resourceMaterializationPath(cfg, d) + ".lock"
}

func resourceDestination(root, client, id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\\`) || !mcpRootDirectoryChain(root) {
		return "", errors.New("unsafe resource destination")
	}
	switch client {
	case "codex":
		return filepath.Join(root, ".agents", "skills", id), nil
	case "claude":
		return filepath.Join(root, ".claude", "skills", id), nil
	default:
		return "", errors.New("unsupported resource client")
	}
}

// prepareResourceDestination creates only the selected client-owned ancestry.
// It rechecks every created component as a plain directory, so a symlink or
// reparse point cannot redirect a native approval outside the signed root.
func prepareResourceDestination(destination string) error {
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	for current := parent; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe resource destination ancestry")
		}
		up := filepath.Dir(current)
		if up == current {
			break
		}
	}
	return nil
}

func resourceSourceIdentityDigest(record resource.Record, revision resource.Revision) string {
	s := sha256.Sum256([]byte(record.CanonicalSource + "\x00" + revision.Reference.URL + "\x00" + revision.Reference.Version + "\x00" + revision.Reference.Subdirectory))
	return hex.EncodeToString(s[:])
}
func resourceManifestDigest(m managedtree.Manifest) string {
	b, _ := json.Marshal(m)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func validResourceDecision(d resourceBindingDecision) bool {
	if d.Schema != resourceDecisionSchema || (d.Action != "activate" && d.Action != "rollback" && d.Action != "detach" && d.Action != "retire") {
		return false
	}
	return d.ResourceID != "" && d.ResourceKind != "" && d.RevisionDigest != "" && d.Scope != "" && d.Client != "" && d.ProjectRoot != "" && d.Destination != "" && d.InstallationID != "" && d.KeyID != "" && d.ChallengeNonce != "" && d.DisplayDigest != "" && d.VerifiedAt != ""
}
func appendResourceDecision(cfg Config, d resourceBindingDecision) error {
	if !validResourceDecision(d) {
		return errors.New("invalid resource decision")
	}
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	path := resourceDecisionPath(cfg)
	return leasefile.WithGuard(path+".lock", func() error {
		old, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if len(old) > 0 && old[len(old)-1] != '\n' {
			return errors.New("torn resource decision log")
		}
		return atomicio.WriteDurable(path, append(old, append(b, '\n')...), 0o600)
	})
}
func resourceDecisionRows(cfg Config) ([]resourceBindingDecision, error) {
	f, err := os.Open(resourceDecisionPath(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []resourceBindingDecision
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1024), 1<<20)
	for s.Scan() {
		var d resourceBindingDecision
		if json.Unmarshal(s.Bytes(), &d) != nil || !validResourceDecision(d) {
			return nil, errors.New("invalid resource decision log")
		}
		out = append(out, d)
	}
	return out, s.Err()
}
func latestResourceDecision(cfg Config, installation, id, scope, client, root string) (resourceBindingDecision, bool, error) {
	rows, err := resourceDecisionRows(cfg)
	if err != nil {
		return resourceBindingDecision{}, false, err
	}
	var found resourceBindingDecision
	ok := false
	for _, d := range rows {
		if d.InstallationID == installation && d.ResourceID == id && d.Scope == scope && d.Client == client && d.ProjectRoot == root {
			found, ok = d, true
		}
	}
	return found, ok, nil
}
func writeResourceReceipt(cfg Config, d resourceBindingDecision, receipt resourceMaterializationReceipt) error {
	b, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	return atomicio.WriteDurable(resourceMaterializationPath(cfg, d), append(b, '\n'), 0o600)
}
func readResourceReceipt(cfg Config, d resourceBindingDecision) (resourceMaterializationReceipt, error) {
	b, err := os.ReadFile(resourceMaterializationPath(cfg, d))
	if err != nil {
		return resourceMaterializationReceipt{}, err
	}
	var r resourceMaterializationReceipt
	if json.Unmarshal(b, &r) != nil || r.Schema != resourceDecisionSchema || r.Destination != d.Destination {
		return resourceMaterializationReceipt{}, errors.New("invalid resource materialization receipt")
	}
	return r, nil
}

// activeResourceBindings performs scope suppression before resource discovery
// ranking and treats any absent/corrupt local receipt as inactive. Lost local
// state therefore cannot recreate authority from vault content.
func activeResourceBindings(cfg Config, p mcpProject) ([]resource.Binding, error) {
	installation, enrolled, err := existingNativeOwnerInstallationID(cfg)
	if err != nil || !enrolled {
		return []resource.Binding{}, nil
	}
	rows, err := resourceDecisionRows(cfg)
	if err != nil {
		return nil, err
	}
	latest := map[string]resourceBindingDecision{}
	for _, d := range rows {
		if d.InstallationID != installation || d.Client != "codex" || d.ProjectRoot != p.root || (d.Scope != p.scope && d.Scope != "global") {
			continue
		}
		key := d.ResourceID + "\x00" + d.Scope
		latest[key] = d
	}
	out := make([]resource.Binding, 0, len(latest))
	for _, d := range latest {
		if d.Action == "retire" || d.Action == "detach" {
			continue
		}
		if _, err := readResourceReceipt(cfg, d); err != nil {
			continue
		}
		_, rev, err := resource.ReadRevision(cfg.VaultDir, d.ResourceID, d.RevisionDigest)
		if err != nil || !rev.Complete {
			continue
		}
		out = append(out, resource.Binding{ResourceID: d.ResourceID, RevisionDigest: d.RevisionDigest, Scope: d.Scope, Eligible: true, Active: true})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ResourceID == out[j].ResourceID {
			return out[i].Scope < out[j].Scope
		}
		return out[i].ResourceID < out[j].ResourceID
	})
	return out, nil
}

func materializeResource(cfg Config, d resourceBindingDecision, prior *managedtree.Manifest) (resourceMaterializationReceipt, error) {
	_, rev, err := resource.ReadRevision(cfg.VaultDir, d.ResourceID, d.RevisionDigest)
	if err != nil || !rev.Complete {
		return resourceMaterializationReceipt{}, errors.New("resource revision unavailable")
	}
	source := filepath.Join(cfg.VaultDir, "resources", d.ResourceID, "revisions", d.RevisionDigest, "tree")
	tree, err := managedtree.DigestTree(source)
	if err != nil || tree.Digest != d.TreeDigest {
		return resourceMaterializationReceipt{}, errors.New("resource tree changed")
	}
	if err := prepareResourceDestination(d.Destination); err != nil {
		return resourceMaterializationReceipt{}, err
	}
	manifest, installErr := managedtree.InstallTree(tree, d.Destination, prior)
	receipt := resourceMaterializationReceipt{Schema: resourceDecisionSchema, ResourceID: d.ResourceID, RevisionDigest: d.RevisionDigest, Scope: d.Scope, Client: d.Client, ProjectRoot: d.ProjectRoot, Destination: d.Destination, Manifest: manifest, ManifestDigest: resourceManifestDigest(manifest), Status: "active"}
	if manifest.Destination != "" {
		if err := writeResourceReceipt(cfg, d, receipt); err != nil {
			return receipt, err
		}
	}
	if installErr != nil {
		return receipt, installErr
	}
	return receipt, nil
}
func detachResource(cfg Config, d resourceBindingDecision) error {
	receipt, err := readResourceReceipt(cfg, d)
	if err != nil {
		return err
	}
	removed, err := managedtree.DetachTree(d.Destination, receipt.Manifest)
	receipt.Status = "detached"
	receipt.ManifestDigest = resourceManifestDigest(receipt.Manifest)
	if err != nil {
		receipt.Status = "conflict"
	}
	if writeErr := writeResourceReceipt(cfg, d, receipt); writeErr != nil {
		return writeErr
	}
	_ = removed
	return err
}

// materializeGlobalResource mirrors one signed logical global binding into all
// currently bound project destinations. Each destination keeps a local receipt
// and therefore fails closed independently during later discovery.
func materializeGlobalResource(cfg Config, d resourceBindingDecision) error {
	bindings, err := loadProjectBindings(cfg)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		if binding.Dir == d.ProjectRoot {
			continue
		}
		destination, err := resourceDestination(binding.Dir, d.Client, d.ResourceID)
		if err != nil {
			return err
		}
		mirror := d
		mirror.ProjectRoot, mirror.Destination = binding.Dir, destination
		var prior *managedtree.Manifest
		current, found, err := latestResourceDecision(cfg, d.InstallationID, d.ResourceID, d.Scope, d.Client, binding.Dir)
		if err != nil {
			return err
		}
		if found && current.Action != "retire" && current.Action != "detach" {
			receipt, err := readResourceReceipt(cfg, current)
			if err != nil {
				return err
			}
			prior = &receipt.Manifest
		}
		if err := appendResourceDecision(cfg, mirror); err != nil {
			return err
		}
		if _, err := materializeResource(cfg, mirror, prior); err != nil {
			return err
		}
	}
	return nil
}

var _ = time.RFC3339
