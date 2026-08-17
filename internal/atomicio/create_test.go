package atomicio

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

var linkPublish = os.Link

func linkUnsupported(err error) bool     { return claimLinkUnsupported(err) }
func syntheticLinkUnsupportedErr() error { return errors.ErrUnsupported }
func atomicCreate(path string, body []byte, mode os.FileMode) error {
	return CreateExclusive(path, body, mode, ClaimOptions{Link: linkPublish, Unsupported: linkUnsupported})
}

func dirBaseNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func forceLinkUnsupported(t *testing.T) {
	t.Helper()
	old := linkPublish
	t.Cleanup(func() { linkPublish = old })
	linkPublish = func(string, string) error { return syntheticLinkUnsupportedErr() }
}

func TestAtomicCreateHappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mem.md")

	// CreateTemp opens at 0600, so 0644 proves atomicCreate applies the mode.
	if err := atomicCreate(path, []byte("body\n"), 0o644); err != nil {
		t.Fatalf("atomicCreate: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "body\n" {
		t.Fatalf("content = %q, want %q", got, "body\n")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	assertPermUnix(t, info.Mode(), 0o644)

	if names := dirBaseNames(t, dir); len(names) != 1 || names[0] != "mem.md" {
		t.Fatalf("expected only mem.md, found %v (orphan temp left behind)", names)
	}
}

func TestAtomicCreateRefusesToClobber(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mem.md")

	if err := atomicCreate(path, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatalf("seed create: %v", err)
	}

	err := atomicCreate(path, []byte("REPLACEMENT"), 0o644)
	if err == nil {
		t.Fatalf("atomicCreate clobbered an existing file (want error)")
	}
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("want os.ErrExist, got %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ORIGINAL" {
		t.Fatalf("existing content changed to %q (clobbered)", got)
	}
	if names := dirBaseNames(t, dir); len(names) != 1 || names[0] != "mem.md" {
		t.Fatalf("orphan temp left after EEXIST: %v", names)
	}
}

func TestAtomicCreateConcurrentSamePathSingleWinner(t *testing.T) {
	const writers = 16
	const bodyLen = 1 << 12

	bodies := make([][]byte, writers)
	for i := range bodies {
		bodies[i] = bytes.Repeat([]byte{byte('A' + i)}, bodyLen)
	}
	matchesACandidate := func(got []byte) bool {
		for _, b := range bodies {
			if bytes.Equal(got, b) {
				return true
			}
		}
		return false
	}

	for iter := 0; iter < 200; iter++ {
		dir := t.TempDir()
		path := filepath.Join(dir, "mem.md")

		var wg sync.WaitGroup
		errs := make([]error, writers)
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = atomicCreate(path, bodies[i], 0o644)
			}(i)
		}
		wg.Wait()

		nSuccess, nExist := 0, 0
		for i, err := range errs {
			switch {
			case err == nil:
				nSuccess++
			case errors.Is(err, os.ErrExist):
				nExist++
			default:
				t.Fatalf("iter %d: writer %d unexpected error: %v", iter, i, err)
			}
		}
		if nSuccess != 1 {
			t.Fatalf("iter %d: want exactly 1 winner, got %d (clobber/lost-write risk)", iter, nSuccess)
		}
		if nExist != writers-1 {
			t.Fatalf("iter %d: want %d EEXIST losers, got %d", iter, writers-1, nExist)
		}

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("iter %d: read target: %v", iter, err)
		}
		if !matchesACandidate(got) {
			t.Fatalf("iter %d: target torn/empty: %d bytes match no single writer body", iter, len(got))
		}
	}
}

func TestAtomicCreateFallbackHappyPath(t *testing.T) {
	forceLinkUnsupported(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mem.md")

	if err := atomicCreate(path, []byte("body\n"), 0o644); err != nil {
		t.Fatalf("atomicCreate (fallback): %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "body\n" {
		t.Fatalf("content = %q, want %q", got, "body\n")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	assertPermUnix(t, info.Mode(), 0o644)
	if names := dirBaseNames(t, dir); len(names) != 1 || names[0] != "mem.md" {
		t.Fatalf("expected only mem.md, found %v (orphan temp/placeholder)", names)
	}
}

func TestAtomicCreateFallbackRefusesToClobber(t *testing.T) {
	forceLinkUnsupported(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "mem.md")

	if err := atomicCreate(path, []byte("ORIGINAL"), 0o644); err != nil {
		t.Fatalf("seed (fallback): %v", err)
	}
	err := atomicCreate(path, []byte("REPLACEMENT"), 0o644)
	if err == nil {
		t.Fatalf("fallback atomicCreate clobbered an existing file (want error)")
	}
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("want os.ErrExist from O_EXCL claim, got %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "ORIGINAL" {
		t.Fatalf("existing content changed to %q (clobbered)", got)
	}
	if names := dirBaseNames(t, dir); len(names) != 1 || names[0] != "mem.md" {
		t.Fatalf("orphan temp/placeholder left after EEXIST: %v", names)
	}
}

func TestAtomicCreateFallbackConcurrentSingleWinner(t *testing.T) {
	forceLinkUnsupported(t)
	const writers = 16
	const bodyLen = 1 << 12
	bodies := make([][]byte, writers)
	for i := range bodies {
		bodies[i] = bytes.Repeat([]byte{byte('A' + i)}, bodyLen)
	}
	matchesACandidate := func(got []byte) bool {
		for _, b := range bodies {
			if bytes.Equal(got, b) {
				return true
			}
		}
		return false
	}

	for iter := 0; iter < 100; iter++ {
		dir := t.TempDir()
		path := filepath.Join(dir, "mem.md")

		var wg sync.WaitGroup
		errs := make([]error, writers)
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = atomicCreate(path, bodies[i], 0o644)
			}(i)
		}
		wg.Wait()

		nSuccess, nExist := 0, 0
		for i, err := range errs {
			switch {
			case err == nil:
				nSuccess++
			case errors.Is(err, os.ErrExist):
				nExist++
			default:
				t.Fatalf("iter %d: writer %d unexpected error: %v", iter, i, err)
			}
		}
		if nSuccess != 1 {
			t.Fatalf("iter %d: want exactly 1 winner in fallback mode, got %d (clobber risk)", iter, nSuccess)
		}
		if nExist != writers-1 {
			t.Fatalf("iter %d: want %d EEXIST losers, got %d", iter, writers-1, nExist)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("iter %d: read: %v", iter, err)
		}
		if !matchesACandidate(got) {
			t.Fatalf("iter %d: file torn/empty: %d bytes match no single writer body", iter, len(got))
		}
	}
}

func TestAtomicCreateSurfacesRealLinkError(t *testing.T) {
	old := linkPublish
	defer func() { linkPublish = old }()
	realErr := errors.New("simulated disk failure")
	linkPublish = func(string, string) error { return realErr }

	dir := t.TempDir()
	path := filepath.Join(dir, "mem.md")
	err := atomicCreate(path, []byte("body"), 0o644)
	if !errors.Is(err, realErr) {
		t.Fatalf("want the real link error surfaced, got %v", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("no file should exist after a real link error, stat err = %v", statErr)
	}
	if names := dirBaseNames(t, dir); len(names) != 0 {
		t.Fatalf("orphan temp left after real link error: %v", names)
	}
}
