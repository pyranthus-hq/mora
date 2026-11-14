// Package managedtree installs complete, verified file trees without replacing
// foreign files that share their destination.
package managedtree

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/pyranthus-hq/mora/internal/atomicio"
)

var ErrConflict = errors.New("managed tree conflict")

var (
	syncFile = (*os.File).Sync
	syncDir  = atomicio.SyncDir
)

// File is one regular file in a managed tree. Path is slash-separated and
// relative to the tree root.
type File struct {
	Path       string
	SHA256     string
	Executable bool
}

// Revision identifies a source folder by its exact regular-file contents.
// Source is deliberately verified again by InstallTree before it is copied.
type Revision struct {
	Source string
	Digest string
	Files  []File
}

// Manifest records exactly what this package installed. Metadata belonging to a
// resource/rule owner remains outside this narrow filesystem primitive.
type Manifest struct {
	Revision    Revision
	Destination string
	InstalledAt time.Time
}

// DigestTree rejects links and non-regular files, then produces a stable full
// digest that binds every relative path, content hash, and executable bit.
func DigestTree(source string) (Revision, error) {
	abs, err := cleanDirectory(source)
	if err != nil {
		return Revision{}, err
	}
	files, err := scanTree(abs)
	if err != nil {
		return Revision{}, err
	}
	return Revision{Source: abs, Files: files, Digest: digest(files)}, nil
}

// InstallTree stages an exact revision beside destination and atomically
// publishes it. Existing unmanaged files survive; edited managed files conflict.
// If the new revision is live but cleanup fails, it returns that live Manifest
// with an error; callers must retain the receipt and coordinate writers through
// the final snapshot/cleanup window.
func InstallTree(source Revision, destination string, prior *Manifest) (Manifest, error) {
	if err := validateRevision(source); err != nil {
		return Manifest{}, err
	}
	actual, err := DigestTree(source.Source)
	if err != nil {
		return Manifest{}, fmt.Errorf("digesting source: %w", err)
	}
	if !sourceMatchesRevision(source, actual) {
		return Manifest{}, fmt.Errorf("%w: source changed since its revision was calculated", ErrConflict)
	}

	dest, parent, base, err := destinationParts(destination)
	if err != nil {
		return Manifest{}, err
	}
	parentRoot, err := os.OpenRoot(parent)
	if err != nil {
		return Manifest{}, fmt.Errorf("opening destination parent: %w", err)
	}
	defer parentRoot.Close()

	marker := transactionMarker(base)
	if err := incompleteTransaction(parentRoot, parent, marker); err != nil {
		return Manifest{}, err
	}
	stagePath, err := os.MkdirTemp(parent, "."+base+".managedtree-stage-")
	if err != nil {
		return Manifest{}, fmt.Errorf("creating staging tree: %w", err)
	}
	stage := filepath.Base(stagePath)
	backup := stage + ".backup"
	markerOwned := false
	preserveTransaction := false
	defer func() {
		if !preserveTransaction {
			_ = makeTreeWritable(parentRoot, stage)
			if parentRoot.RemoveAll(stage) == nil && markerOwned {
				_ = parentRoot.Remove(marker)
			}
		}
	}()
	markerOwned, err = beginTransaction(parent, marker, stage, backup)
	if err != nil {
		if markerOwned {
			preserveTransaction = true
		}
		return Manifest{}, err
	}

	destInfo, err := parentRoot.Lstat(base)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, fmt.Errorf("checking destination: %w", err)
	}
	exists := err == nil
	if exists {
		if !destInfo.IsDir() || unsafeMode(destInfo.Mode()) {
			return Manifest{}, fmt.Errorf("%w: destination is not a plain directory", ErrConflict)
		}
		if prior == nil {
			return Manifest{}, fmt.Errorf("%w: destination exists without a manifest", ErrConflict)
		}
		if err := validateManifest(*prior, dest); err != nil {
			return Manifest{}, err
		}
		if err := validateInstalled(parentRoot, base, prior.Revision.Files); err != nil {
			return Manifest{}, err
		}
	} else if prior != nil {
		return Manifest{}, fmt.Errorf("%w: manifest destination is missing", ErrConflict)
	}

	var stagedModes map[string]os.FileMode
	if exists {
		stagedModes = make(map[string]os.FileMode)
		if err := copyTree(parentRoot, base, parentRoot, stage, stagedModes); err != nil {
			return Manifest{}, fmt.Errorf("copying destination to staging: %w", err)
		}
	}
	stageRoot, err := parentRoot.OpenRoot(stage)
	if err != nil {
		return Manifest{}, fmt.Errorf("opening staging tree: %w", err)
	}
	var copiedSnapshot []File
	if prior != nil {
		copiedSnapshot, err = scanRoot(stageRoot)
		if err != nil {
			stageRoot.Close()
			return Manifest{}, fmt.Errorf("checking copied destination: %w", err)
		}
		if err := verifyFiles(stageRoot, prior.Revision.Files); err != nil {
			stageRoot.Close()
			return Manifest{}, err
		}
		if err := removeManaged(stageRoot, prior.Revision.Files); err != nil {
			stageRoot.Close()
			return Manifest{}, err
		}
	}
	sourceRoot, err := os.OpenRoot(source.Source)
	if err == nil {
		err = copyFiles(sourceRoot, stageRoot, source.Files)
		sourceRoot.Close()
	}
	if err == nil {
		err = verifyFiles(stageRoot, source.Files)
	}
	if err == nil {
		err = applyDirectoryModes(stageRoot, stagedModes)
	}
	if closeErr := stageRoot.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Manifest{}, fmt.Errorf("staging revision: %w", err)
	}
	if err := syncTree(stagePath); err != nil {
		return Manifest{}, fmt.Errorf("syncing staged tree: %w", err)
	}
	if exists {
		if err := parentRoot.Chmod(stage, destInfo.Mode().Perm()); err != nil {
			return Manifest{}, fmt.Errorf("preserving destination mode on staging tree: %w", err)
		}
		if err := syncDir(stagePath); err != nil {
			return Manifest{}, fmt.Errorf("syncing staged root metadata: %w", err)
		}
	}

	if exists {
		if _, err := parentRoot.Lstat(backup); !errors.Is(err, os.ErrNotExist) {
			return Manifest{}, fmt.Errorf("%w: generated backup path already exists", ErrConflict)
		}
		if err := parentRoot.Rename(base, backup); err != nil {
			return Manifest{}, fmt.Errorf("parking prior destination: %w", err)
		}
		preserveTransaction = true
		if err := syncDir(parent); err != nil {
			return Manifest{}, transactionRecoveryError(parent, marker, stage, backup, err)
		}
		if err := verifySnapshot(parentRoot, backup, copiedSnapshot); err != nil {
			return Manifest{}, transactionRecoveryError(parent, marker, stage, backup, err)
		}
	}
	if err := parentRoot.Rename(stage, base); err != nil {
		preserveTransaction = true
		return Manifest{}, transactionRecoveryError(parent, marker, stage, backup, err)
	}
	preserveTransaction = true
	installed := Manifest{Revision: source, Destination: dest, InstalledAt: time.Now().UTC()}
	if err := syncDir(parent); err != nil {
		return installed, publishedRecoveryError(parent, marker, stage, backup, err)
	}
	if exists {
		if err := verifySnapshot(parentRoot, backup, copiedSnapshot); err != nil {
			return installed, publishedRecoveryError(parent, marker, stage, backup, err)
		}
		if err := makeTreeWritable(parentRoot, backup); err != nil {
			return installed, publishedRecoveryError(parent, marker, stage, backup, err)
		}
		if err := parentRoot.RemoveAll(backup); err != nil {
			return installed, publishedRecoveryError(parent, marker, stage, backup, err)
		}
		if err := syncDir(parent); err != nil {
			return installed, publishedRecoveryError(parent, marker, stage, backup, err)
		}
	}
	if err := parentRoot.Remove(marker); err != nil {
		return installed, publishedRecoveryError(parent, marker, stage, backup, err)
	}
	if err := syncDir(parent); err != nil {
		return installed, publishedRecoveryError(parent, marker, stage, backup, err)
	}
	return installed, nil
}

func makeTreeWritable(parent *os.Root, name string) error {
	root, err := parent.OpenRoot(name)
	if err != nil {
		return err
	}
	defer root.Close()
	var dirs []string
	if err := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && path != "." {
			dirs = append(dirs, path)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, dir := range dirs {
		if err := root.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	return parent.Chmod(name, 0o700)
}

// DetachTree removes only files still byte-for-byte equal to the manifest.
// It never removes unknown files or directories, and reports modifications as a
// conflict while leaving them in place.
func DetachTree(destination string, manifest Manifest) ([]string, error) {
	dest, parent, base, err := destinationParts(destination)
	if err != nil {
		return nil, err
	}
	if err := validateManifest(manifest, dest); err != nil {
		return nil, err
	}
	parentRoot, err := os.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	defer parentRoot.Close()
	info, err := parentRoot.Lstat(base)
	if err != nil || !info.IsDir() || unsafeMode(info.Mode()) {
		return nil, fmt.Errorf("%w: manifest destination is unavailable", ErrConflict)
	}
	root, err := parentRoot.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if _, err := scanRoot(root); err != nil {
		return nil, fmt.Errorf("%w: foreign destination content is unsupported: %v", ErrConflict, err)
	}

	var removed []string
	var conflicts []string
	for _, file := range manifest.Revision.Files {
		info, err := root.Lstat(file.Path)
		if errors.Is(err, os.ErrNotExist) {
			continue // an earlier interrupted detach already removed this exact manifest path
		}
		unsafe, reparseErr := unsafeReparse(filepath.Join(dest, file.Path), info)
		if err != nil || !info.Mode().IsRegular() || unsafeMode(info.Mode()) || reparseErr != nil || unsafe {
			conflicts = append(conflicts, file.Path)
			continue
		}
		hash, err := hashRootFile(root, file.Path)
		if err != nil || hash != file.SHA256 || !executableMatches(file.Executable, info.Mode()) {
			conflicts = append(conflicts, file.Path)
			continue
		}
		if err := root.Remove(file.Path); err != nil {
			return removed, fmt.Errorf("removing %s: %w", file.Path, err)
		}
		removed = append(removed, file.Path)
	}
	if len(conflicts) != 0 {
		return removed, fmt.Errorf("%w: preserved modified or missing files: %s", ErrConflict, strings.Join(conflicts, ", "))
	}
	return removed, nil
}

func cleanDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	unsafe, reparseErr := unsafeReparse(abs, info)
	if !info.IsDir() || unsafeMode(info.Mode()) || reparseErr != nil || unsafe {
		return "", fmt.Errorf("%w: root is not a plain directory", ErrConflict)
	}
	return filepath.Clean(abs), nil
}

func destinationParts(destination string) (string, string, string, error) {
	abs, err := filepath.Abs(destination)
	if err != nil {
		return "", "", "", err
	}
	abs = filepath.Clean(abs)
	base := filepath.Base(abs)
	if err := validComponent(base); err != nil {
		return "", "", "", err
	}
	return abs, filepath.Dir(abs), base, nil
}

func transactionMarker(base string) string { return "." + base + ".managedtree-transaction" }

func beginTransaction(parent, marker, stage, backup string) (bool, error) {
	body := []byte("mora-managedtree-transaction-v1\n" + stage + "\n" + backup + "\n")
	path := filepath.Join(parent, marker)
	if err := atomicio.CreateExclusive(path, body, 0o600); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, fmt.Errorf("%w: incomplete transaction marker already exists at %s; preserve it and inspect its named paths before retrying", ErrConflict, path)
		}
		return false, fmt.Errorf("recording transaction marker: %w", err)
	}
	if err := syncDir(parent); err != nil {
		return true, transactionRecoveryError(parent, marker, stage, backup, err)
	}
	return true, nil
}

func incompleteTransaction(parent *os.Root, parentPath, marker string) error {
	info, err := parent.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || unsafeMode(info.Mode()) {
		return fmt.Errorf("%w: unrecognized transaction marker at %s; it was not modified", ErrConflict, filepath.Join(parentPath, marker))
	}
	return fmt.Errorf("%w: incomplete earlier install preserved at %s; inspect the marker and its named stage/backup paths before retrying", ErrConflict, filepath.Join(parentPath, marker))
}

func transactionRecoveryError(parent, marker, stage, backup string, cause error) error {
	return fmt.Errorf("%w: install state preserved after %v; inspect %s, %s, and %s before retrying", ErrConflict, cause, filepath.Join(parent, marker), filepath.Join(parent, stage), filepath.Join(parent, backup))
}

func publishedRecoveryError(parent, marker, stage, backup string, cause error) error {
	return fmt.Errorf("%w: new revision is live but cleanup state is preserved after %v; retain the returned manifest and inspect %s, %s, and %s before retrying", ErrConflict, cause, filepath.Join(parent, marker), filepath.Join(parent, stage), filepath.Join(parent, backup))
}

func scanTree(path string) ([]File, error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return scanRoot(root)
}

func scanRoot(root *os.Root) ([]File, error) {
	var files []File
	seen := make(map[string]string)
	err := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." {
			return nil
		}
		rel := filepath.ToSlash(path)
		if err := validRelative(rel, seen); err != nil {
			return err
		}
		info, err := root.Lstat(path)
		if err != nil {
			return err
		}
		unsafe, reparseErr := unsafeReparse(filepath.Join(root.Name(), path), info)
		if unsafeMode(info.Mode()) || reparseErr != nil || unsafe {
			return fmt.Errorf("%w: links or reparse points are not supported: %s", ErrConflict, rel)
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: non-regular file %s", ErrConflict, rel)
		}
		hash, err := hashRootFile(root, path)
		if err != nil {
			return err
		}
		files = append(files, File{Path: rel, SHA256: hash, Executable: executable(info.Mode())})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func copyTree(from *os.Root, fromName string, to *os.Root, toName string, modes map[string]os.FileMode) error {
	source, err := from.OpenRoot(fromName)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := to.OpenRoot(toName)
	if err != nil {
		return err
	}
	defer target.Close()
	files, err := scanRoot(source)
	if err != nil {
		return err
	}
	if err := fs.WalkDir(source.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || path == "." || !entry.IsDir() {
			return walkErr
		}
		info, err := source.Lstat(path)
		if err != nil {
			return err
		}
		if err := target.MkdirAll(path, 0o700); err != nil {
			return err
		}
		modes[path] = info.Mode().Perm()
		return nil
	}); err != nil {
		return err
	}
	return copyFiles(source, target, files)
}

func applyDirectoryModes(root *os.Root, modes map[string]os.FileMode) error {
	for path, mode := range modes {
		if err := root.Chmod(path, mode); err != nil {
			return err
		}
	}
	return nil
}

func copyFiles(from, to *os.Root, files []File) error {
	for _, file := range files {
		if err := to.MkdirAll(filepath.ToSlash(filepath.Dir(file.Path)), 0o700); err != nil && filepath.Dir(file.Path) != "." {
			return err
		}
		info, err := from.Lstat(file.Path)
		if err != nil {
			return err
		}
		in, err := from.Open(file.Path)
		if err != nil {
			return err
		}
		out, err := to.OpenFile(file.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			_ = in.Close()
			return fmt.Errorf("%w: managed path collides with existing destination content: %s", ErrConflict, file.Path)
		}
		if err == nil {
			_, err = io.Copy(out, in)
		}
		if closeErr := in.Close(); err == nil {
			err = closeErr
		}
		if out != nil {
			if err == nil {
				err = syncFile(out)
			}
			if closeErr := out.Close(); err == nil {
				err = closeErr
			}
		}
		if err != nil {
			return err
		}
		if err := to.Chmod(file.Path, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

func removeManaged(root *os.Root, files []File) error {
	for _, file := range files {
		if err := root.Remove(file.Path); err != nil {
			return fmt.Errorf("removing prior managed %s: %w", file.Path, err)
		}
	}
	return nil
}

func validateInstalled(root *os.Root, name string, files []File) error {
	dest, err := root.OpenRoot(name)
	if err != nil {
		return err
	}
	defer dest.Close()
	if _, err := scanRoot(dest); err != nil {
		return fmt.Errorf("%w: foreign destination content is unsupported: %v", ErrConflict, err)
	}
	return verifyFiles(dest, files)
}

func verifySnapshot(parent *os.Root, name string, want []File) error {
	root, err := parent.OpenRoot(name)
	if err != nil {
		return fmt.Errorf("opening parked destination: %w", err)
	}
	defer root.Close()
	got, err := scanRoot(root)
	if err != nil {
		return fmt.Errorf("scanning parked destination: %w", err)
	}
	if !sameFiles(want, got) {
		return fmt.Errorf("%w: destination changed while install was in progress", ErrConflict)
	}
	return nil
}

func verifyFiles(root *os.Root, files []File) error {
	for _, file := range files {
		info, err := root.Lstat(file.Path)
		if err != nil || !info.Mode().IsRegular() || unsafeMode(info.Mode()) {
			return fmt.Errorf("%w: managed file is missing or unsafe: %s", ErrConflict, file.Path)
		}
		hash, err := hashRootFile(root, file.Path)
		unsafe, reparseErr := unsafeReparse(filepath.Join(root.Name(), file.Path), info)
		if err != nil || hash != file.SHA256 || reparseErr != nil || unsafe || !executableMatches(file.Executable, info.Mode()) {
			return fmt.Errorf("%w: managed file was edited: %s", ErrConflict, file.Path)
		}
	}
	return nil
}

func validateRevision(revision Revision) error {
	if revision.Source == "" || revision.Digest == "" || revision.Digest != digest(revision.Files) {
		return fmt.Errorf("%w: malformed source revision", ErrConflict)
	}
	seen := make(map[string]string)
	for _, file := range revision.Files {
		if err := validRelative(file.Path, seen); err != nil {
			return err
		}
		if len(file.SHA256) != sha256.Size*2 {
			return fmt.Errorf("%w: malformed hash for %s", ErrConflict, file.Path)
		}
		if _, err := hex.DecodeString(file.SHA256); err != nil {
			return fmt.Errorf("%w: malformed hash for %s", ErrConflict, file.Path)
		}
	}
	return nil
}

func validateManifest(manifest Manifest, destination string) error {
	if manifest.Destination != destination {
		return fmt.Errorf("%w: manifest does not belong to destination", ErrConflict)
	}
	return validateRevision(manifest.Revision)
}

func sameRevision(a, b Revision) bool {
	if a.Digest != b.Digest || len(a.Files) != len(b.Files) {
		return false
	}
	for i := range a.Files {
		if a.Files[i] != b.Files[i] {
			return false
		}
	}
	return true
}

func sameFiles(a, b []File) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func digest(files []File) string {
	h := sha256.New()
	_, _ = h.Write([]byte("mora-managedtree-v1\x00"))
	for _, file := range files {
		_, _ = fmt.Fprintf(h, "%d:%s:%s:%t\n", len(file.Path), file.Path, file.SHA256, file.Executable)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashRootFile(root *os.Root, name string) (string, error) {
	f, err := root.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func executable(mode os.FileMode) bool { return mode.Perm()&0o111 != 0 }

func syncTree(path string) error {
	var dirs []string
	err := filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			dirs = append(dirs, current)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := syncDir(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

func unsafeMode(mode os.FileMode) bool {
	return mode&os.ModeSymlink != 0 || mode&os.ModeIrregular != 0
}

func validRelative(path string, seen map[string]string) error {
	if path == "" || filepath.IsAbs(path) || strings.HasPrefix(path, "/") || strings.Contains(path, "\\") {
		return fmt.Errorf("%w: invalid relative path %q", ErrConflict, path)
	}
	var parent string
	for _, component := range strings.Split(path, "/") {
		if err := validComponent(component); err != nil {
			return err
		}
		key := parent + "/" + portableKey(component)
		if existing, ok := seen[key]; ok && existing != component {
			return fmt.Errorf("%w: platform alias collision between %q and %q", ErrConflict, existing, component)
		}
		seen[key] = component
		parent = key
	}
	return nil
}

func validComponent(component string) error {
	if component == "" || component == "." || component == ".." || strings.ContainsAny(component, "<>:\"/\\|?*") || strings.TrimSpace(component) == "" || strings.TrimRight(component, ". ") != component {
		return fmt.Errorf("%w: invalid path component %q", ErrConflict, component)
	}
	for _, r := range component {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: control character in path component %q", ErrConflict, component)
		}
	}
	if portableKey(component) == "" || windowsDeviceName(portableKey(component)) {
		return fmt.Errorf("%w: unsupported platform path component %q", ErrConflict, component)
	}
	return nil
}

func portableKey(component string) string { return strings.ToUpper(strings.TrimRight(component, ". ")) }

func windowsDeviceName(component string) bool {
	base := component
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" {
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		return base[3] >= '1' && base[3] <= '9'
	}
	return false
}
