package resource

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/leasefile"
	"github.com/pyranthus-hq/mora/internal/managedtree"
)

const maxManifestBytes = 4 << 20

// Import persists only intrinsic evidence. It never approves, activates,
// executes, fetches, or materializes the imported resource.
func Import(root string, candidate Candidate) (Record, Revision, ImportOutcome, error) {
	c, identity, derived, sourceTree, err := prepareCandidate(candidate)
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
	var resourceRoot string
	if c.StableID != "" {
		resourceRoot, err = checkResourceLayout(root, id)
		if errors.Is(err, os.ErrNotExist) {
			return Record{}, Revision{}, ImportOutcome{}, ErrPriorStableIDRequired
		}
	} else {
		resourceRoot, err = ensureResourceLayout(root, id)
	}
	if err != nil {
		return Record{}, Revision{}, ImportOutcome{}, err
	}
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
			if record.StableID != id {
				return ErrCorrupt
			}
			if record.Kind != c.Kind || (!derived && record.CanonicalSource != identity) {
				return fmt.Errorf("%w: stable ID identity mismatch", ErrInvalidCandidate)
			}
		} else {
			if c.StableID != "" {
				return ErrPriorStableIDRequired
			}
			record = Record{Schema: 1, StableID: id, Kind: c.Kind, CanonicalSource: identity, Attribution: c.Attribution, CreatedAt: time.Now().UTC()}
		}
		revision, err = buildRevision(record, c, sourceTree)
		if err != nil {
			return err
		}
		revisionRoot := filepath.Join(resourceRoot, "revisions", revision.RevisionDigest)
		duplicate, err := storedRevisionMatches(revisionRoot, revision)
		if err != nil {
			return err
		}
		if duplicate {
			if !exists {
				if err := writeCreateExclusive(recordPath, mustJSON(record)); err != nil {
					return err
				}
				outcome.CreatedRecord = true
				return nil
			}
			outcome.Duplicate = true
			return atomicio.SyncDir(resourceRoot)
		}
		if exists && c.StableID == "" && c.Kind != KindRepository {
			return fmt.Errorf("%w: %s", ErrPriorStableIDRequired, id)
		}
		if err := publishRevision(revisionRoot, revision, c, sourceTree); err != nil {
			return err
		}
		outcome.CreatedRevision = true
		if !exists {
			if err := writeCreateExclusive(recordPath, mustJSON(record)); err != nil {
				return err
			}
			outcome.CreatedRecord = true
		}
		return nil
	})
	if err != nil {
		return Record{}, Revision{}, ImportOutcome{}, err
	}
	return record, revision, outcome, nil
}

func ensureResourceLayout(root, id string) (string, error) {
	if err := plainExistingDir(root); err != nil {
		return "", err
	}
	resources, err := plainChild(root, "resources", true)
	if err != nil {
		return "", err
	}
	resourceRoot, err := plainChild(resources, id, true)
	if err != nil {
		return "", err
	}
	if _, err := plainChild(resourceRoot, "revisions", true); err != nil {
		return "", err
	}
	return resourceRoot, nil
}

func checkResourceLayout(root, id string) (string, error) {
	if err := plainExistingDir(root); err != nil {
		return "", err
	}
	resources, err := plainChild(root, "resources", false)
	if err != nil {
		return "", err
	}
	resourceRoot, err := plainChild(resources, id, false)
	if err != nil {
		return "", err
	}
	if _, err := plainChild(resourceRoot, "revisions", false); err != nil {
		return "", err
	}
	return resourceRoot, nil
}

func plainChild(parent, name string, create bool) (string, error) {
	if !validID(name) && name != "resources" && name != "revisions" {
		return "", ErrUnsafeSource
	}
	child := filepath.Join(parent, name)
	info, err := os.Lstat(child)
	if errors.Is(err, os.ErrNotExist) && create {
		if err := os.Mkdir(child, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		if err := atomicio.SyncDir(parent); err != nil {
			return "", err
		}
		info, err = os.Lstat(child)
	}
	if err != nil {
		return "", err
	}
	if info.Mode().Type() != os.ModeDir {
		return "", fmt.Errorf("%w: non-plain vault directory", ErrUnsafeSource)
	}
	return child, nil
}

func existingID(root string, kind Kind, identity string) (string, error) {
	if err := plainExistingDir(root); err != nil {
		return "", err
	}
	resources, err := plainChild(root, "resources", false)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(resources)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !validID(entry.Name()) {
			continue
		}
		if info, err := os.Lstat(filepath.Join(resources, entry.Name())); err != nil || info.Mode().Type() != os.ModeDir {
			continue
		}
		record, exists, err := readRecord(filepath.Join(resources, entry.Name(), "record.json"))
		if err != nil {
			continue // A corrupt unrelated record must not block a fresh resource.
		}
		if exists && record.StableID == entry.Name() && record.Kind == kind && record.CanonicalSource == identity {
			return record.StableID, nil
		}
	}
	return "", nil
}

func storedRevisionMatches(revisionRoot string, expected Revision) (bool, error) {
	info, err := os.Lstat(revisionRoot)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode().Type() != os.ModeDir {
		return false, ErrCorrupt
	}
	body, err := readRegular(filepath.Join(revisionRoot, "manifest.json"), maxManifestBytes)
	if errors.Is(err, os.ErrNotExist) {
		return false, ErrCorrupt
	}
	if err != nil {
		return false, err
	}
	var actual Revision
	if json.Unmarshal(body, &actual) != nil || !bytes.Equal(body, mustJSON(expected)) || !validStoredRevision(actual, expected.ResourceID, expected.RevisionDigest) {
		return false, ErrCorrupt
	}
	if actual.TreeDigest != "" {
		tree, err := managedtree.DigestTree(filepath.Join(revisionRoot, "tree"))
		if err != nil || !treeMatches(actual, tree) {
			return false, ErrCorrupt
		}
	}
	return true, nil
}

func ReadRevision(root, resourceID, revisionDigest string) (Record, Revision, error) {
	if !validID(resourceID) || !validDigest(revisionDigest) {
		return Record{}, Revision{}, ErrNotFound
	}
	resourceRoot, err := checkResourceLayout(root, resourceID)
	if err != nil {
		return Record{}, Revision{}, ErrNotFound
	}
	recordPath := filepath.Join(resourceRoot, "record.json")
	record, exists, err := readRecord(recordPath)
	if err != nil || !exists {
		return Record{}, Revision{}, ErrNotFound
	}
	if record.StableID != resourceID {
		return Record{}, Revision{}, ErrNotFound
	}
	revisionRoot, err := plainChild(filepath.Join(resourceRoot, "revisions"), revisionDigest, false)
	if err != nil {
		return Record{}, Revision{}, ErrNotFound
	}
	body, err := readRegular(filepath.Join(revisionRoot, "manifest.json"), maxManifestBytes)
	if err != nil {
		return Record{}, Revision{}, ErrNotFound
	}
	var revision Revision
	if json.Unmarshal(body, &revision) != nil || revision.Kind != record.Kind || !validStoredRevision(revision, resourceID, revisionDigest) {
		return Record{}, Revision{}, ErrNotFound
	}
	if revision.TreeDigest != "" {
		actual, err := managedtree.DigestTree(filepath.Join(resourceRoot, "revisions", revisionDigest, "tree"))
		if err != nil || !treeMatches(revision, actual) {
			return Record{}, Revision{}, ErrNotFound
		}
	}
	return record, revision, nil
}

func validStoredRevision(r Revision, resourceID, digest string) bool {
	if r.Schema != 1 || r.ResourceID != resourceID || r.RevisionDigest != digest || r.RevisionDigest != revisionDigest(r) || !validKind(r.Kind) || !validScope(r.EffectiveScope) || !pathsSorted(r.RequiredFiles) {
		return false
	}
	if r.TreeDigest != "" {
		if !filesSorted(r.Files) || r.TreeDigest != digestFiles(r.Files) {
			return false
		}
		for _, file := range r.Files {
			if safePath(file.Path) != nil || !validDigest(file.SHA256) {
				return false
			}
		}
	}
	if r.Complete {
		return r.Availability == AvailabilityAvailable && r.TreeDigest != "" && requiredPresent(r.Files, r.RequiredFiles)
	}
	if r.Availability == AvailabilityIncomplete {
		return r.TreeDigest != "" && !requiredPresent(r.Files, r.RequiredFiles)
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
func pathsSorted(paths []string) bool {
	for i, p := range paths {
		if safePath(p) != nil || (i > 0 && paths[i-1] >= p) {
			return false
		}
	}
	return true
}
func requiredPresent(files []managedtree.File, required []string) bool {
	present := make(map[string]struct{}, len(files))
	for _, file := range files {
		present[file.Path] = struct{}{}
	}
	for _, path := range required {
		if _, ok := present[path]; !ok {
			return false
		}
	}
	return true
}

func buildRevision(record Record, c Candidate, tree managedtree.Revision) (Revision, error) {
	r := Revision{Schema: 1, ResourceID: record.StableID, Kind: record.Kind, Purpose: c.Purpose, Applicability: c.Applicability, KnownLimits: c.KnownLimits, EffectiveScope: c.EffectiveScope, UntrustedScopeSuggestion: c.UntrustedScopeSuggestion, Source: c.Source, Reference: c.Reference, RequiredFiles: append([]string(nil), c.RequiredFiles...), SourceVerified: false, Availability: AvailabilityAvailable, Complete: true}
	switch c.Source {
	case SourcePasted:
		r.Files = []managedtree.File{{Path: "SKILL.md", SHA256: digestBytes(c.Pasted)}}
		r.TreeDigest = digestFiles(r.Files)
	case SourceFolder:
		r.Files, r.TreeDigest = append([]managedtree.File(nil), tree.Files...), tree.Digest
	case SourceReference:
		r.Availability, r.Complete = AvailabilityUnfetched, false
	}
	if c.Kind == KindSkill && c.Source != SourceReference {
		links, err := skillLinks(c, tree)
		if err != nil {
			return Revision{}, err
		}
		r.RequiredFiles = mergePaths(r.RequiredFiles, links)
	}
	if r.TreeDigest != "" && !requiredPresent(r.Files, r.RequiredFiles) {
		r.Availability, r.Complete = AvailabilityIncomplete, false
	}
	r.RevisionDigest = revisionDigest(r)
	if len(mustJSON(r)) > maxManifestBytes {
		return Revision{}, ErrUnsafeSource
	}
	return r, nil
}

func skillLinks(c Candidate, tree managedtree.Revision) ([]string, error) {
	body := c.Pasted
	if c.Source == SourceFolder {
		var err error
		body, err = readRegular(filepath.Join(tree.Source, "SKILL.md"), maxFileBytes)
		if err != nil {
			return nil, fmt.Errorf("%w: reading skill links", ErrUnsafeSource)
		}
	}
	var out []string
	for rest := string(body); ; {
		start := strings.Index(rest, "](")
		if start < 0 {
			break
		}
		rest = rest[start+2:]
		end := strings.IndexByte(rest, ')')
		if end < 0 {
			break
		}
		target, next := strings.TrimSpace(rest[:end]), rest[end+1:]
		rest = next
		if target == "" || strings.HasPrefix(target, "#") || strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
			continue
		}
		if strings.ContainsAny(target, " \t\r\n") || strings.Contains(target, "#") || safePath(target) != nil {
			return nil, fmt.Errorf("%w: unsafe local skill link", ErrUnsafeSource)
		}
		out = append(out, target)
	}
	return normalizedPaths(out, 128)
}
func mergePaths(a, b []string) []string {
	set := make(map[string]struct{}, len(a)+len(b))
	for _, p := range append(a, b...) {
		set[p] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func publishRevision(destination string, revision Revision, c Candidate, sourceTree managedtree.Revision) error {
	parent := filepath.Dir(destination)
	stage, err := os.MkdirTemp(parent, ".resource-revision-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if revision.TreeDigest != "" {
		tree := filepath.Join(stage, "tree")
		if err := os.MkdirAll(tree, 0o700); err != nil {
			return err
		}
		switch c.Source {
		case SourcePasted:
			if err := writeFileDurable(filepath.Join(tree, "SKILL.md"), c.Pasted, 0o600); err != nil {
				return err
			}
		case SourceFolder:
			if err := copyTree(sourceTree, tree); err != nil {
				return err
			}
		}
		actual, err := managedtree.DigestTree(tree)
		if err != nil || !treeMatches(revision, actual) {
			return fmt.Errorf("%w: copied tree changed", ErrUnsafeSource)
		}
		if err := syncTree(tree); err != nil {
			return err
		}
	}
	if err := atomicio.WriteDurable(filepath.Join(stage, "manifest.json"), mustJSON(revision), 0o600); err != nil {
		return err
	}
	if err := atomicio.SyncDir(stage); err != nil {
		return err
	}
	if err := os.Rename(stage, destination); err != nil {
		return err
	}
	return atomicio.SyncDir(parent)
}

func copyTree(source managedtree.Revision, destination string) error {
	if err := validateTree(source.Source); err != nil {
		return err
	}
	actual, err := managedtree.DigestTree(source.Source)
	if err != nil || actual.Digest != source.Digest {
		return fmt.Errorf("%w: source changed", ErrUnsafeSource)
	}
	root, err := os.OpenRoot(source.Source)
	if err != nil {
		return fmt.Errorf("%w: opening source", ErrUnsafeSource)
	}
	defer root.Close()
	for _, file := range source.Files {
		if err := safePath(file.Path); err != nil {
			return err
		}
		f, err := root.Open(file.Path)
		if err != nil {
			return fmt.Errorf("%w: source changed", ErrUnsafeSource)
		}
		info, statErr := f.Stat()
		body, readErr := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
		closeErr := f.Close()
		if statErr != nil || readErr != nil || closeErr != nil || !info.Mode().IsRegular() || info.Size() > maxFileBytes || int64(len(body)) > maxFileBytes || digestBytes(body) != file.SHA256 {
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
		if err := writeFileDurable(path, body, mode); err != nil {
			return err
		}
	}
	return nil
}

func readRecord(path string) (Record, bool, error) {
	body, err := readRegular(path, 128<<10)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	var r Record
	if err := json.Unmarshal(body, &r); err != nil || r.Schema != 1 || !validID(r.StableID) || !validKind(r.Kind) || r.CanonicalSource == "" {
		return Record{}, false, fmt.Errorf("invalid resource record")
	}
	return r, true, nil
}

func readRegular(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > max {
		return nil, ErrCorrupt
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(filepath.Base(path))
	if err != nil {
		return nil, err
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		f.Close()
		return nil, ErrCorrupt
	}
	body, readErr := io.ReadAll(io.LimitReader(f, max+1))
	closeErr := f.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(body)) > max {
		return nil, ErrCorrupt
	}
	return body, nil
}

// Hard-link publication never exposes an empty record or replaces foreign bytes.
// Filesystems without hard links are refused rather than using a crash-unsafe placeholder.
func writeCreateExclusive(path string, body []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".resource-record-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(body); err == nil {
		err = atomicio.MarkerSyncFn(f)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Link(f.Name(), path); err != nil {
		return err
	}
	return atomicio.SyncDir(filepath.Dir(path))
}

func treeMatches(expected Revision, actual managedtree.Revision) bool {
	if len(expected.Files) != len(actual.Files) {
		return false
	}
	for i, file := range expected.Files {
		got := actual.Files[i]
		if file.Path != got.Path || file.SHA256 != got.SHA256 || (runtime.GOOS != "windows" && file.Executable != got.Executable) {
			return false
		}
	}
	return true
}

func writeFileDurable(path string, body []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
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

func syncTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := atomicio.SyncDir(dirs[i]); err != nil {
			return err
		}
	}
	return nil
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
	if !(id[0] >= 'a' && id[0] <= 'z' || id[0] >= '0' && id[0] <= '9') || windowsDeviceName(id) {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
func windowsDeviceName(id string) bool {
	base := strings.ToLower(strings.TrimRight(id, ". "))
	if base == "con" || base == "prn" || base == "aux" || base == "nul" {
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt")) && base[3] >= '1' && base[3] <= '9' {
		return true
	}
	return false
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
