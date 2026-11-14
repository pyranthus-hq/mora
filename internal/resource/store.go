package resource

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/leasefile"
	"github.com/pyranthus-hq/mora/internal/managedtree"
)

// Import persists only intrinsic evidence. It never approves, activates,
// executes, fetches, or materializes the imported resource.
func Import(root string, candidate Candidate) (Record, Revision, ImportOutcome, error) {
	c, identity, sourceTree, err := prepareCandidate(candidate)
	if err != nil {
		return Record{}, Revision{}, ImportOutcome{}, err
	}
	id := c.StableID
	if id == "" {
		id, err = existingID(root, c.Kind, identity)
		if err != nil {
			return Record{}, Revision{}, ImportOutcome{}, err
		}
		if id == "" {
			id = stableID(c.Kind, identity)
		}
	}
	if !validID(id) {
		return Record{}, Revision{}, ImportOutcome{}, fmt.Errorf("%w: stable ID", ErrInvalidCandidate)
	}
	resources := filepath.Join(root, "resources")
	resourceRoot := filepath.Join(resources, id)
	var record Record
	var revision Revision
	var outcome ImportOutcome
	err = leasefile.WithGuard(filepath.Join(resourceRoot, ".resource.lock"), func() error {
		var exists bool
		recordPath := filepath.Join(resourceRoot, "record.json")
		record, exists, err = readRecord(recordPath)
		if err != nil {
			return err
		}
		if exists {
			if record.Kind != c.Kind || record.CanonicalSource != identity {
				return fmt.Errorf("%w: stable ID identity mismatch", ErrInvalidCandidate)
			}
		} else {
			record = Record{Schema: 1, StableID: id, Kind: c.Kind, CanonicalSource: identity, Attribution: c.Attribution, CreatedAt: time.Now().UTC()}
		}
		revision, err = buildRevision(record, c, sourceTree)
		if err != nil {
			return err
		}
		revisionRoot := filepath.Join(resourceRoot, "revisions", revision.RevisionDigest)
		if _, err := os.Stat(filepath.Join(revisionRoot, "manifest.json")); err == nil {
			outcome.Duplicate = true
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if exists && c.StableID == "" {
			return fmt.Errorf("%w: %s", ErrPriorStableIDRequired, id)
		}
		if !exists {
			if err := writeCreateExclusive(recordPath, mustJSON(record)); err != nil {
				if !errors.Is(err, os.ErrExist) {
					return err
				}
				return fmt.Errorf("%w: concurrent record publication", ErrInvalidCandidate)
			}
			outcome.CreatedRecord = true
		}
		if err := publishRevision(revisionRoot, revision, c, sourceTree); err != nil {
			return err
		}
		outcome.CreatedRevision = true
		return nil
	})
	if err != nil {
		return Record{}, Revision{}, ImportOutcome{}, err
	}
	return record, revision, outcome, nil
}

func existingID(root string, kind Kind, identity string) (string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "resources"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validID(entry.Name()) {
			continue
		}
		record, exists, err := readRecord(filepath.Join(root, "resources", entry.Name(), "record.json"))
		if err != nil {
			return "", err
		}
		if exists && record.Kind == kind && record.CanonicalSource == identity {
			return record.StableID, nil
		}
	}
	return "", nil
}

func ReadRevision(root, resourceID, revisionDigest string) (Record, Revision, error) {
	if !validID(resourceID) || !validDigest(revisionDigest) {
		return Record{}, Revision{}, ErrNotFound
	}
	recordPath := filepath.Join(root, "resources", resourceID, "record.json")
	record, exists, err := readRecord(recordPath)
	if err != nil || !exists {
		return Record{}, Revision{}, ErrNotFound
	}
	body, err := os.ReadFile(filepath.Join(root, "resources", resourceID, "revisions", revisionDigest, "manifest.json"))
	if err != nil {
		return Record{}, Revision{}, ErrNotFound
	}
	var revision Revision
	if json.Unmarshal(body, &revision) != nil || !validStoredRevision(revision, resourceID, revisionDigest) {
		return Record{}, Revision{}, ErrNotFound
	}
	if revision.Complete {
		actual, err := managedtree.DigestTree(filepath.Join(root, "resources", resourceID, "revisions", revisionDigest, "tree"))
		if err != nil || actual.Digest != revision.TreeDigest {
			return Record{}, Revision{}, ErrNotFound
		}
	}
	return record, revision, nil
}

func validStoredRevision(r Revision, resourceID, digest string) bool {
	if r.Schema != 1 || r.ResourceID != resourceID || r.RevisionDigest != digest || r.RevisionDigest != revisionDigest(r) || !validKind(r.Kind) || !validScope(r.EffectiveScope) {
		return false
	}
	if r.Complete {
		if r.Availability != AvailabilityAvailable || r.TreeDigest == "" || !filesSorted(r.Files) || r.TreeDigest != digestFiles(r.Files) {
			return false
		}
		for _, file := range r.Files {
			if safePath(file.Path) != nil || !validDigest(file.SHA256) {
				return false
			}
		}
		return true
	}
	return r.Availability == AvailabilityUnfetched && r.TreeDigest == "" && len(r.Files) == 0
}

func filesSorted(files []managedtree.File) bool {
	for i := 1; i < len(files); i++ {
		if files[i-1].Path >= files[i].Path {
			return false
		}
	}
	return true
}

func buildRevision(record Record, c Candidate, tree managedtree.Revision) (Revision, error) {
	r := Revision{Schema: 1, ResourceID: record.StableID, Kind: record.Kind, Purpose: c.Purpose, Applicability: c.Applicability, KnownLimits: c.KnownLimits, EffectiveScope: c.EffectiveScope, UntrustedScopeSuggestion: c.UntrustedScopeSuggestion, Source: c.Source, Reference: c.Reference, SourceVerified: false, Availability: AvailabilityAvailable, Complete: true}
	switch c.Source {
	case SourcePasted:
		r.Files = []managedtree.File{{Path: "SKILL.md", SHA256: digestBytes(c.Pasted)}}
		r.TreeDigest = digestFiles(r.Files)
	case SourceFolder:
		r.Files, r.TreeDigest = append([]managedtree.File(nil), tree.Files...), tree.Digest
	case SourceReference:
		r.Availability, r.Complete = AvailabilityUnfetched, false
	}
	r.RevisionDigest = revisionDigest(r)
	return r, nil
}

func publishRevision(destination string, revision Revision, c Candidate, sourceTree managedtree.Revision) error {
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".resource-revision-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if revision.Complete {
		tree := filepath.Join(stage, "tree")
		if err := os.MkdirAll(tree, 0o700); err != nil {
			return err
		}
		switch c.Source {
		case SourcePasted:
			if err := os.WriteFile(filepath.Join(tree, "SKILL.md"), c.Pasted, 0o600); err != nil {
				return err
			}
		case SourceFolder:
			if err := copyTree(sourceTree, tree); err != nil {
				return err
			}
		}
		actual, err := managedtree.DigestTree(tree)
		if err != nil || actual.Digest != revision.TreeDigest {
			return fmt.Errorf("%w: copied tree changed", ErrUnsafeSource)
		}
	}
	if err := atomicio.Write(filepath.Join(stage, "manifest.json"), mustJSON(revision), 0o600); err != nil {
		return err
	}
	if err := os.Rename(stage, destination); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	return nil
}

func copyTree(source managedtree.Revision, destination string) error {
	for _, file := range source.Files {
		if err := safePath(file.Path); err != nil {
			return err
		}
		body, err := os.ReadFile(filepath.Join(source.Source, filepath.FromSlash(file.Path)))
		if err != nil || digestBytes(body) != file.SHA256 {
			return fmt.Errorf("%w: source changed", ErrUnsafeSource)
		}
		path := filepath.Join(destination, filepath.FromSlash(file.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(0o600)
		if file.Executable {
			mode = 0o700
		}
		if err := os.WriteFile(path, body, mode); err != nil {
			return err
		}
	}
	return nil
}

func readRecord(path string) (Record, bool, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	var r Record
	if err := json.Unmarshal(body, &r); err != nil || !validID(r.StableID) || !validKind(r.Kind) || r.CanonicalSource == "" {
		return Record{}, false, fmt.Errorf("invalid resource record")
	}
	return r, true, nil
}

func writeCreateExclusive(path string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(body); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

func revisionDigest(r Revision) string { r.RevisionDigest = ""; return digestBytes(mustJSON(r)) }
func mustJSON(v any) []byte            { b, _ := json.Marshal(v); return b }
func stableID(kind Kind, identity string) string {
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + identity))
	return string(kind) + "-" + hex.EncodeToString(sum[:])[:20]
}
func validID(id string) bool {
	if len(id) < 2 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}
func validDigest(d string) bool {
	if len(d) != 64 {
		return false
	}
	_, err := hex.DecodeString(d)
	return err == nil
}
func digestFiles(files []managedtree.File) string {
	files = append([]managedtree.File(nil), files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	var b strings.Builder
	b.WriteString("mora-managedtree-v1\x00")
	for _, f := range files {
		fmt.Fprintf(&b, "%d:%s:%s:%t\n", len(f.Path), f.Path, f.SHA256, f.Executable)
	}
	return digestBytes([]byte(b.String()))
}
