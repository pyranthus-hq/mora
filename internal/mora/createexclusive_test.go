package mora

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// dirBaseNames lists the base names in a directory, for orphan-temp assertions.
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

// TestAtomicCreateHappyPath: a create onto a free path writes the body, applies
// the caller's mode, and leaves no staging temp behind.
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

// TestAtomicCreateRefusesToClobber is the core anti-clobber guarantee: a create
// onto an already-existing path must fail with os.ErrExist and MUST NOT overwrite
// the existing bytes (unlike atomicWrite's replace-on-rename). No orphan temp.
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

// TestAtomicCreateConcurrentSamePathSingleWinner is the concurrency proof: N
// writers racing to create the SAME path yield EXACTLY ONE success; the rest get
// os.ErrExist. The published file always equals exactly one writer's full body —
// never empty, never torn. This is the property newID collisions used to violate
// (last-rename-wins clobber). Run under -race.
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

// TestCreateMemoryPersistsWithUniqueID: the happy path mints an id, writes the
// rendered memory, and returns the memory with ID + Path populated.
func TestCreateMemoryPersistsWithUniqueID(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{VaultDir: dir, DataDir: dir, StateDir: dir}

	m := Memory{Scope: "global", Type: "insight", Title: "hi", Text: "hello world", Source: "manual", CreatedAt: "2020-01-01T00:00:00Z"}
	got, err := createMemory(cfg, m)
	if err != nil {
		t.Fatalf("createMemory: %v", err)
	}
	if !strings.HasPrefix(got.ID, "mem_") {
		t.Fatalf("ID not minted: %q", got.ID)
	}
	if got.Path != memoryPath(cfg, got) {
		t.Fatalf("Path = %q, want %q", got.Path, memoryPath(cfg, got))
	}
	body, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatalf("memory not written: %v", err)
	}
	if !strings.Contains(string(body), "hello world") || !strings.Contains(string(body), got.ID) {
		t.Fatalf("rendered body missing content or id:\n%s", body)
	}
}

// TestCreateMemoryRetriesPastIDCollision: when a freshly minted id collides with
// an existing memory on disk, createMemory re-mints and retries (bounded),
// landing on a fresh id — WITHOUT clobbering the colliding memory.
func TestCreateMemoryRetriesPastIDCollision(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{VaultDir: dir, DataDir: dir, StateDir: dir}

	const collidingID = "mem_20200101_000000_deadbeef"
	const freshID = "mem_20200101_000000_feedface"

	existing := Memory{ID: collidingID, Scope: "global", Type: "insight", Title: "existing", Text: "DO NOT CLOBBER", Source: "manual", CreatedAt: "2020-01-01T00:00:00Z"}
	if err := writeMemory(cfg, existing); err != nil {
		t.Fatalf("seed existing memory: %v", err)
	}

	old := newIDFn
	defer func() { newIDFn = old }()
	var calls int
	newIDFn = func() string {
		calls++
		if calls == 1 {
			return collidingID
		}
		return freshID
	}

	m := Memory{Scope: "global", Type: "insight", Title: "new", Text: "fresh body", Source: "manual", CreatedAt: "2020-01-01T00:00:00Z"}
	got, err := createMemory(cfg, m)
	if err != nil {
		t.Fatalf("createMemory: %v", err)
	}
	if got.ID != freshID {
		t.Fatalf("want retry to land %q, got %q", freshID, got.ID)
	}
	if calls != 2 {
		t.Fatalf("want 2 mint calls (1 collision + 1 success), got %d", calls)
	}

	existingBody, err := os.ReadFile(memoryPath(cfg, existing))
	if err != nil {
		t.Fatalf("read existing: %v", err)
	}
	if !strings.Contains(string(existingBody), "DO NOT CLOBBER") {
		t.Fatalf("createMemory clobbered the pre-existing colliding memory:\n%s", existingBody)
	}
	freshBody, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatalf("fresh memory not written: %v", err)
	}
	if !strings.Contains(string(freshBody), "fresh body") {
		t.Fatalf("fresh memory body wrong:\n%s", freshBody)
	}
}

// TestCreateMemoryExhaustsBoundedRetries: if every minted id collides, the loop
// is bounded — it returns an error after maxCreateAttempts tries and never
// clobbers the existing memory.
func TestCreateMemoryExhaustsBoundedRetries(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{VaultDir: dir, DataDir: dir, StateDir: dir}

	const collidingID = "mem_20200101_000000_deadbeef"
	existing := Memory{ID: collidingID, Scope: "global", Type: "insight", Title: "existing", Text: "SENTINEL", Source: "manual", CreatedAt: "2020-01-01T00:00:00Z"}
	if err := writeMemory(cfg, existing); err != nil {
		t.Fatalf("seed existing memory: %v", err)
	}

	old := newIDFn
	defer func() { newIDFn = old }()
	var calls int
	newIDFn = func() string { calls++; return collidingID } // always collide

	m := Memory{Scope: "global", Type: "insight", Title: "new", Text: "body", Source: "manual", CreatedAt: "2020-01-01T00:00:00Z"}
	if _, err := createMemory(cfg, m); err == nil {
		t.Fatalf("want error after exhausting retries, got nil")
	}
	if calls != maxCreateAttempts {
		t.Fatalf("want %d mint attempts, got %d", maxCreateAttempts, calls)
	}

	body, err := os.ReadFile(memoryPath(cfg, existing))
	if err != nil {
		t.Fatalf("read existing: %v", err)
	}
	if !strings.Contains(string(body), "SENTINEL") {
		t.Fatalf("clobbered existing memory on exhaustion:\n%s", body)
	}
}

// TestNewIDFallsBackWhenCSPRNGUnavailable: if crypto/rand fails, newID must NOT
// emit an all-zero suffix (which would collide every time within a second and
// stall createMemory's re-mint loop). The fallback derives varying suffixes so
// distinct ids are still produced.
func TestNewIDFallsBackWhenCSPRNGUnavailable(t *testing.T) {
	old := randRead
	defer func() { randRead = old }()
	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy source") }

	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := newIDFn() // newIDFn defaults to newID; exercises the real minting path
		if !strings.HasPrefix(id, "mem_") {
			t.Fatalf("bad id format: %q", id)
		}
		suffix := id[len(id)-8:]
		if suffix == "00000000" {
			t.Fatalf("all-zero suffix at draw %d: rand error not handled", i)
		}
		seen[id] = true
	}
	// Within one wall-clock second the timestamp is constant, so id distinctness
	// comes entirely from the fallback suffix. Require near-total distinctness.
	if len(seen) < 90 {
		t.Fatalf("fallback suffix not varying: only %d distinct of 100", len(seen))
	}
}

// TestNewIDWarnsOnCSPRNGFallback: the crypto/rand fallback must not be silent —
// it emits one stderr warning per mint (never fails the write). No warning when
// crypto/rand succeeds.
func TestNewIDWarnsOnCSPRNGFallback(t *testing.T) {
	oldRand := randRead
	defer func() { randRead = oldRand }()
	oldWarn := warnRandFallback
	defer func() { warnRandFallback = oldWarn }()

	var warns int
	warnRandFallback = func() { warns++ }

	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy source") }
	if id := newID(); !strings.HasPrefix(id, "mem_") { // still produces a valid id
		t.Fatalf("fallback did not produce a valid id: %q", id)
	}
	if warns != 1 {
		t.Fatalf("want exactly 1 stderr warning on the fallback mint, got %d", warns)
	}

	warns = 0
	randRead = oldRand // crypto/rand works
	_ = newID()
	if warns != 0 {
		t.Fatalf("want no warning when crypto/rand succeeds, got %d", warns)
	}
}

// forceLinkUnsupported drives atomicCreate's no-hardlink fallback on an ordinary
// filesystem by making the os.Link publish fail with a platform-appropriate
// "hard links unsupported" error. Restores the seam on cleanup.
func forceLinkUnsupported(t *testing.T) {
	t.Helper()
	old := linkPublish
	t.Cleanup(func() { linkPublish = old })
	linkPublish = func(string, string) error { return syntheticLinkUnsupportedErr() }
}

// TestAtomicCreateFallbackHappyPath: on a no-hardlink filesystem, atomicCreate
// still publishes the body (O_EXCL claim + rename), applies the mode, and leaves
// no staging temp or placeholder residue.
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

// TestAtomicCreateFallbackRefusesToClobber: the fallback preserves the no-clobber
// guarantee — a create onto an existing path fails at the O_EXCL claim with
// os.ErrExist and does not overwrite the existing bytes.
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

// TestAtomicCreateFallbackConcurrentSingleWinner is the no-clobber proof in
// FALLBACK mode: N writers racing to create the SAME path via the O_EXCL claim
// yield exactly one success; the rest get os.ErrExist and the file equals exactly
// one writer's full body. Run under -race.
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

// TestAtomicCreateSurfacesRealLinkError: a link error that is NEITHER os.ErrExist
// NOR link-unsupported must surface as-is — NOT masked as a collision and NOT
// silently routed through the fallback (conservative classification). No file is
// created.
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

// TestCreateMemoryWorksWhenLinkUnsupported: the end-to-end user-write path saves a
// memory even when the vault filesystem has no hard links (the blocking-finding
// regression scenario).
func TestCreateMemoryWorksWhenLinkUnsupported(t *testing.T) {
	forceLinkUnsupported(t)
	dir := t.TempDir()
	cfg := Config{VaultDir: dir, DataDir: dir, StateDir: dir}

	m := Memory{Scope: "global", Type: "insight", Title: "t", Text: "no-hardlink body", Source: "manual", CreatedAt: "2020-01-01T00:00:00Z"}
	got, err := createMemory(cfg, m)
	if err != nil {
		t.Fatalf("createMemory (fallback): %v", err)
	}
	body, err := os.ReadFile(got.Path)
	if err != nil {
		t.Fatalf("memory not written in fallback mode: %v", err)
	}
	if !strings.Contains(string(body), "no-hardlink body") {
		t.Fatalf("body missing: %s", body)
	}
}
