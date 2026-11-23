package managedtree

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
)

// ReadPlainFile reads only a bounded regular file. The caller supplies a
// validated parent directory. Opening relative to it confines the final lookup.
func ReadPlainFile(path string, max int64) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return readPlain(root, filepath.Base(path), max)
}

func readPlain(root *os.Root, name string, max int64) ([]byte, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root.Name(), name)
	if !before.Mode().IsRegular() || before.Size() > max || !plainEntry(path, before) {
		return nil, ErrConflict
	}
	f, err := root.OpenFile(name, os.O_RDONLY|readFlags, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !sameSnapshot(before, opened) {
		return nil, ErrConflict
	}
	data, err := io.ReadAll(io.LimitReader(f, before.Size()+1))
	after, statErr := root.Lstat(name)
	if err != nil || statErr != nil || int64(len(data)) != before.Size() || !sameSnapshot(before, after) || !plainEntry(path, after) {
		return nil, ErrConflict
	}
	return data, nil
}

func plainEntry(path string, info os.FileInfo) bool {
	unsafe, err := unsafeReparse(path, info)
	return err == nil && !unsafe && !unsafeMode(info.Mode())
}

// PlainDirectory also rejects Windows reparse points that look like directories.
func PlainDirectory(path string, info os.FileInfo) bool {
	return info.IsDir() && plainEntry(path, info)
}

func sameSnapshot(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// VerifyTree checks the exact approved file set without reading unlisted file
// contents. Resource trees persist files only; their directory prefixes are
// implied. ReadDir is batched so a foreign directory cannot exhaust memory.
func VerifyTree(path string, files []File, maxFile, maxTotal int64) error {
	root, err := os.OpenRoot(path)
	if err != nil {
		return ErrConflict
	}
	defer root.Close()
	expected := make(map[string]File, len(files))
	dirs := map[string]bool{".": true}
	for _, f := range files {
		expected[f.Path] = f
		for dir := filepath.Dir(filepath.FromSlash(f.Path)); dir != "." && !dirs[dir]; dir = filepath.Dir(dir) {
			dirs[dir] = true
		}
	}
	pending := []string{"."}
	observed := make(map[string]os.FileInfo)
	for len(pending) > 0 {
		dir := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		info, err := root.Lstat(dir)
		if err != nil || !info.IsDir() || !plainEntry(filepath.Join(path, dir), info) {
			return ErrConflict
		}
		observed[dir] = info
		err = eachName(root, dir, func(base string) error {
			name := filepath.Join(dir, base)
			if dirs[name] {
				pending = append(pending, name)
				delete(dirs, name)
				return nil
			}
			key := filepath.ToSlash(name)
			file, ok := expected[key]
			if !ok {
				return ErrConflict
			}
			delete(expected, key)
			info, err := root.Lstat(name)
			if err != nil || !executableMatches(file.Executable, info.Mode()) {
				return ErrConflict
			}
			data, err := readPlain(root, name, min(maxFile, maxTotal))
			if err != nil {
				return ErrConflict
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != file.SHA256 {
				return ErrConflict
			}
			maxTotal -= int64(len(data))
			observed[name] = info
			return nil
		})
		if err != nil {
			return ErrConflict
		}
	}
	if len(expected) != 0 {
		return ErrConflict
	}
	for name, before := range observed {
		full := filepath.Join(path, name)
		after, err := os.Lstat(full)
		if err != nil || !sameSnapshot(before, after) || !plainEntry(full, after) {
			return ErrConflict
		}
	}
	return nil
}

func eachName(root *os.Root, dir string, visit func(string) error) error {
	f, err := root.OpenFile(dir, os.O_RDONLY|readFlags, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		names, err := f.Readdirnames(32)
		for _, name := range names {
			if err := visit(name); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
