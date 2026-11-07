package index

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestMarkAndListPending(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	hooked := false
	op, err := MarkDirty(context.Background(), cfg, PendingOp{Kind: KindWrite, Path: "relative/a.md"}, MarkSeams{Ready: func(context.Context, config.Config) (bool, string, error) { return true, "v1", nil }, NewID: func() string { return "op1" }, Clock: func() time.Time { return now }, PostWrite: func() { hooked = true }})
	if err != nil {
		t.Fatal(err)
	}
	if op.OpID != "op1" || op.MarkedAt != "2026-08-13T12:00:00Z" || !filepath.IsAbs(op.Path) || !hooked {
		t.Fatalf("op=%+v hooked=%v", op, hooked)
	}
	ops, err := ListPending(cfg)
	if err != nil || len(ops) != 1 || ops[0] != op {
		t.Fatalf("ListPending=(%+v,%v)", ops, err)
	}
}
func TestMarkSkipsUnreadyAndProbeFailure(t *testing.T) {
	for _, readyErr := range []error{nil, errors.New("probe")} {
		cfg := config.Config{StateDir: t.TempDir()}
		op, err := MarkDirty(context.Background(), cfg, PendingOp{}, MarkSeams{Ready: func(context.Context, config.Config) (bool, string, error) { return false, "", readyErr }, NewID: func() string { return "op" }, Clock: time.Now})
		if err != nil || op.OpID != "op" {
			t.Fatalf("MarkDirty=(%+v,%v)", op, err)
		}
		if _, err = os.Stat(PendingPath(cfg, "op")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("marker exists: %v", err)
		}
	}
}
func TestListPendingCorruptFailsClosed(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	if err := os.MkdirAll(PendingDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PendingPath(cfg, "bad"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	ops, err := ListPending(cfg)
	if err != nil || len(ops) != 1 || ops[0].OpID != "bad" || ops[0].Kind != "" {
		t.Fatalf("ops=%+v err=%v", ops, err)
	}
}
func TestShouldClearMatrix(t *testing.T) {
	at := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "disk.md")
	if err := os.WriteFile(onDisk, []byte("published"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, other := ContentSHA256([]byte("published")), ContentSHA256([]byte("other"))
	seed := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	reingested, abandoned, stale := seed("re.md", "other"), seed("ab.md", "published"), seed("st.md", "other")
	c := Coverage{
		ListingStartedAt: at,
		Files:            map[string]bool{"/parsed": true, "/present": true, onDisk: true, reingested: true, abandoned: true, stale: true},
		Parsed:           map[string]bool{"/parsed": true},
		ParsedSHA256:     map[string]string{"/parsed": h, onDisk: other, reingested: other, abandoned: h, stale: h},
		OwnerAlive:       func(pid int) bool { return pid == 1 },
	}
	const alive, dead = 1, 2
	cases := []struct {
		name string
		op   PendingOp
		want bool
	}{
		{"write no hash parsed", PendingOp{Kind: KindWrite, Path: "/parsed"}, true},
		{"write no hash not parsed", PendingOp{Kind: KindWrite, Path: "/present"}, false},
		{"write parsed hash equal", PendingOp{Kind: KindWrite, Path: "/parsed", ContentSHA256: h, OwnerPID: alive}, true},
		{"write listed not parsed", PendingOp{Kind: KindWrite, Path: "/present", ContentSHA256: h, OwnerPID: dead}, false},
		{"write on disk now equal", PendingOp{Kind: KindWrite, Path: onDisk, ContentSHA256: h, OwnerPID: dead}, false},
		{"write owner dead", PendingOp{Kind: KindWrite, Path: "/never", ContentSHA256: h, OwnerPID: dead}, true},
		{"write replaced owner dead", PendingOp{Kind: KindWrite, Path: "/parsed", ContentSHA256: other, OwnerPID: dead}, true},
		{"write owner alive", PendingOp{Kind: KindWrite, Path: "/never", ContentSHA256: h, OwnerPID: alive}, false},
		{"write replaced owner alive", PendingOp{Kind: KindWrite, Path: "/parsed", ContentSHA256: other, OwnerPID: alive}, false},
		{"write no owner", PendingOp{Kind: KindWrite, Path: "/never", ContentSHA256: h}, false},
		{"write hash no path", PendingOp{Kind: KindWrite, ContentSHA256: h, OwnerPID: dead}, false},
		{"write unreadable", PendingOp{Kind: KindWrite, Path: filepath.Join(onDisk, "x"), ContentSHA256: h, OwnerPID: dead}, false},
		{"delete no hash gone", PendingOp{Kind: KindDelete, Path: "/missing"}, true},
		{"delete no hash parsed", PendingOp{Kind: KindDelete, Path: "/parsed"}, true},
		{"delete no hash not parsed", PendingOp{Kind: KindDelete, Path: "/present"}, false},
		{"delete live owner gone from listing", PendingOp{Kind: KindDelete, Path: "/missing", ContentSHA256: h, OwnerPID: alive}, false},
		{"delete live owner parsed hash differs", PendingOp{Kind: KindDelete, Path: "/parsed", ContentSHA256: other, OwnerPID: alive}, false},
		{"delete live owner parsed hash equal", PendingOp{Kind: KindDelete, Path: "/parsed", ContentSHA256: h, OwnerPID: alive}, false},
		{"delete no owner", PendingOp{Kind: KindDelete, Path: "/missing", ContentSHA256: h}, false},
		{"delete unlinked gone from listing", PendingOp{Kind: KindDelete, Path: "/missing", ContentSHA256: h, OwnerPID: alive, Unlinked: true}, true},
		{"delete unlinked parsed hash differs", PendingOp{Kind: KindDelete, Path: "/parsed", ContentSHA256: other, OwnerPID: alive, Unlinked: true}, true},
		{"delete unlinked restored identical bytes", PendingOp{Kind: KindDelete, Path: "/parsed", ContentSHA256: h, OwnerPID: alive, Unlinked: true}, true},
		{"delete unlinked listed not parsed", PendingOp{Kind: KindDelete, Path: "/present", ContentSHA256: h, OwnerPID: alive, Unlinked: true}, false},
		{"delete dead gone from listing", PendingOp{Kind: KindDelete, Path: "/missing", ContentSHA256: h, OwnerPID: dead}, true},
		{"delete dead listed not parsed", PendingOp{Kind: KindDelete, Path: "/present", ContentSHA256: h, OwnerPID: dead}, false},
		{"delete dead differs disk equals parsed", PendingOp{Kind: KindDelete, Path: reingested, ContentSHA256: h, OwnerPID: dead}, true},
		{"delete dead differs file removed after parse", PendingOp{Kind: KindDelete, Path: "/parsed", ContentSHA256: other, OwnerPID: dead}, false},
		{"delete dead differs disk moved on", PendingOp{Kind: KindDelete, Path: onDisk, ContentSHA256: h, OwnerPID: dead}, false},
		{"delete dead all three match", PendingOp{Kind: KindDelete, Path: abandoned, ContentSHA256: h, OwnerPID: dead}, true},
		{"delete dead match but file gone", PendingOp{Kind: KindDelete, Path: "/parsed", ContentSHA256: h, OwnerPID: dead}, false},
		{"delete dead match but disk differs", PendingOp{Kind: KindDelete, Path: stale, ContentSHA256: h, OwnerPID: dead}, false},
		{"delete hash no path", PendingOp{Kind: KindDelete, ContentSHA256: h, OwnerPID: dead}, false},
		{"rebuild before listing", PendingOp{Kind: KindRebuild, MarkedAt: at.Add(-time.Second).Format(time.RFC3339)}, true},
		{"rebuild at listing", PendingOp{Kind: KindRebuild, MarkedAt: at.Format(time.RFC3339)}, true},
		{"rebuild after listing", PendingOp{Kind: KindRebuild, MarkedAt: at.Add(time.Second).Format(time.RFC3339)}, false},
		{"corrupt", PendingOp{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "write unreadable" && runtime.GOOS == "windows" {
				t.Skip("a path under a file reads as missing on Windows, so this fixture cannot make a read error")
			}
			if got := ShouldClear(tc.op, c); got != tc.want {
				t.Errorf("ShouldClear(%+v)=%v want %v", tc.op, got, tc.want)
			}
		})
	}
}

// Only the all-three-match retirement brings a memory back, so only it is reported.
func TestAbandonedDeleteIsReported(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	path := filepath.Join(t.TempDir(), "m.md")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := ContentSHA256([]byte("x"))
	var reported []string
	c := Coverage{
		Files:        map[string]bool{path: true},
		ParsedSHA256: map[string]string{path: h},
		OwnerAlive:   func(int) bool { return false },
		Abandoned:    func(op PendingOp) { reported = append(reported, op.OpID) },
	}
	ops := []PendingOp{
		{OpID: "abandoned", Kind: KindDelete, Path: path, ContentSHA256: h, OwnerPID: 9},
		{OpID: "gone", Kind: KindDelete, Path: "/missing", ContentSHA256: h, OwnerPID: 9},
		{OpID: "restored", Kind: KindDelete, Path: path, ContentSHA256: h, OwnerPID: 9, Unlinked: true},
	}
	var removed []string
	if err := ClearCovered(cfg, ops, c, func(_ config.Config, id string) error { removed = append(removed, id); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 3 || len(reported) != 1 || reported[0] != "abandoned" {
		t.Fatalf("removed=%v reported=%v", removed, reported)
	}
}

// The unlink rename can land between ReadDir and ReadFile. Skipping the marker
// there drops suppression while SQLite still holds the deleted row.
func TestListPendingFollowsRenameDuringRead(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	if err := os.MkdirAll(PendingDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PendingPath(cfg, "d"), []byte(`{"op_id":"d","kind":"delete","path":"/a","memory_id":"m"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	renamed := false
	readPendingFile = func(path string) ([]byte, error) {
		if !renamed {
			renamed = true
			if err := MarkFTSCommitted(cfg, "d", os.Rename); err != nil {
				t.Fatal(err)
			}
		}
		return os.ReadFile(path)
	}
	t.Cleanup(func() { readPendingFile = os.ReadFile })
	if !PendingDeleteIDs(cfg)["m"] {
		t.Fatal("a marker renamed during the read stopped suppressing its memory")
	}
	readPendingFile = os.ReadFile
	ops, err := ListPending(cfg)
	if err != nil || len(ops) != 1 || !ops[0].Unlinked {
		t.Fatalf("ops=%+v err=%v", ops, err)
	}
}

// A marker retired between ReadDir and ReadFile is gone in both forms: skip it.
func TestListPendingSkipsRetiredDuringRead(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	if err := os.MkdirAll(PendingDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PendingPath(cfg, "d"), []byte(`{"op_id":"d","kind":"delete"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	readPendingFile = func(path string) ([]byte, error) {
		_ = os.Remove(PendingPath(cfg, "d"))
		return os.ReadFile(path)
	}
	t.Cleanup(func() { readPendingFile = os.ReadFile })
	if ops, err := ListPending(cfg); err != nil || len(ops) != 0 {
		t.Fatalf("ops=%+v err=%v", ops, err)
	}
}

func TestDeleteMarkerSecondFormReadsUnlinked(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	if err := os.MkdirAll(PendingDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PendingPath(cfg, "d"), []byte(`{"op_id":"d","kind":"delete","path":"/a","memory_id":"m"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MarkFTSCommitted(cfg, "d", os.Rename); err != nil {
		t.Fatal(err)
	}
	ops, err := ListPending(cfg)
	if err != nil || len(ops) != 1 || !ops[0].Unlinked || ops[0].FTSCommitted {
		t.Fatalf("ops=%+v err=%v", ops, err)
	}
	if !PendingDeleteIDs(cfg)["m"] {
		t.Fatal("an unlinked delete marker must keep suppressing its memory")
	}
}

// A marker outside the snapshot is on disk but must not be touched.
func TestClearCoveredRetiresOnlyTheSnapshot(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	if err := os.MkdirAll(PendingDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PendingPath(cfg, "later"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var removed []string
	err := ClearCovered(cfg, []PendingOp{{OpID: "snap"}}, Coverage{}, func(_ config.Config, id string) error { removed = append(removed, id); return nil })
	if err != nil || len(removed) != 1 || removed[0] != "snap" {
		t.Fatalf("clear=(%v,%v)", removed, err)
	}
}
func TestClearCoveredAndSuppressDeletes(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	if err := os.MkdirAll(PendingDir(cfg), 0o700); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"op_id":"del","kind":"delete","path":"/gone","memory_id":"m1","marked_at":"2026-08-13T00:00:00Z"}`)
	if err := os.WriteFile(PendingPath(cfg, "del"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	got := SuppressPendingDeletes(cfg, []memory.Memory{{ID: "m1"}, {ID: "m2"}})
	if len(got) != 1 || got[0].ID != "m2" {
		t.Fatalf("suppressed=%+v", got)
	}
	var removed string
	ops, err := ListPending(cfg)
	if err != nil {
		t.Fatal(err)
	}
	err = ClearCovered(cfg, ops, Coverage{ListingStartedAt: time.Now()}, func(_ config.Config, id string) error { removed = id; return nil })
	if err != nil || removed != "del" {
		t.Fatalf("clear=(%q,%v)", removed, err)
	}
}

func TestMarkFTSCommittedRenamesWithoutRetiring(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	ready := func(context.Context, config.Config) (bool, string, error) { return true, "v1", nil }
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	mark := func(id, kind string) PendingOp {
		t.Helper()
		op, err := MarkDirty(context.Background(), cfg, PendingOp{OpID: id, Kind: kind, Path: "/v/" + id + ".md"}, MarkSeams{Ready: ready, NewID: func() string { return id }, Clock: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		return op
	}
	write := mark("w1", KindWrite)
	if err := MarkFTSCommitted(cfg, write.OpID, os.Rename); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(PendingPath(cfg, "w1")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dirty-form marker still present: %v", err)
	}
	ops, err := ListPending(cfg)
	if err != nil || len(ops) != 1 || !ops[0].FTSCommitted || ops[0].OpID != "w1" || ops[0].Kind != KindWrite {
		t.Fatalf("ListPending after FTS commit = %+v, %v; want one FTS-committed write marker", ops, err)
	}
	// The renamed marker is still a pending write: a covering rebuild retires it
	// by the same parsed-path rule, and an uncovering one keeps it.
	if ShouldClear(ops[0], Coverage{ListingStartedAt: now}) {
		t.Fatal("FTS-committed marker cleared without a covering parse")
	}
	if !ShouldClear(ops[0], Coverage{ListingStartedAt: now, Parsed: map[string]bool{ops[0].Path: true}}) {
		t.Fatal("FTS-committed marker not cleared by a covering parse")
	}

	// A marker a concurrent rebuild already retired is success, and the rename
	// must not resurrect it in either form.
	if err := MarkFTSCommitted(cfg, "gone", os.Rename); err != nil {
		t.Fatalf("missing marker: %v", err)
	}
	if _, err := os.Stat(FTSCommittedPath(cfg, "gone")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rename resurrected a retired marker: %v", err)
	}

	// Only a write has an FTS-committed form: a delete under that name stays dirty.
	del := mark("d1", KindDelete)
	if err := os.Rename(PendingPath(cfg, del.OpID), FTSCommittedPath(cfg, del.OpID)); err != nil {
		t.Fatal(err)
	}
	ops, err = ListPending(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op.OpID == "d1" && op.FTSCommitted {
			t.Fatalf("delete marker read as FTS-committed: %+v", op)
		}
	}

	// A rename failure other than absence surfaces and leaves the dirty form.
	other := mark("w2", KindWrite)
	boom := errors.New("rename refused")
	if err := MarkFTSCommitted(cfg, other.OpID, func(string, string) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("rename failure = %v, want %v", err, boom)
	}
	if _, err := os.Stat(PendingPath(cfg, "w2")); err != nil {
		t.Fatalf("dirty-form marker lost after a failed rename: %v", err)
	}
}

func TestForeignToVault(t *testing.T) {
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	if err := os.MkdirAll(filepath.Join(vault, "memories"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(vault, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	cfg := config.Config{VaultDir: vault}
	for _, c := range []struct {
		op   PendingOp
		want bool
	}{
		{PendingOp{Kind: KindWrite, Path: filepath.Join(vault, "memories", "a.md")}, false},
		{PendingOp{Kind: KindWrite, Path: filepath.Join(link, "memories", "a.md")}, false},
		// A writer marks before it creates a new parent directory.
		{PendingOp{Kind: KindWrite, Path: filepath.Join(link, "memories", "project", "new", "a.md")}, false},
		{PendingOp{Kind: KindWrite, Path: filepath.Join(root, "other", "memories", "a.md")}, true},
		{PendingOp{Kind: KindDelete, Path: filepath.Join(root, "vault2", "a.md")}, true},
		{PendingOp{Kind: KindRebuild}, false},
		{PendingOp{Kind: KindWrite}, false},
	} {
		if got := foreignToVault(cfg, c.op); got != c.want {
			t.Errorf("foreignToVault(%+v) = %v, want %v", c.op, got, c.want)
		}
	}
	// Case-insensitive filesystems (macOS default) accept a case-variant spelling.
	if fi, err := os.Stat(strings.ToUpper(vault)); err == nil {
		if real, _ := os.Stat(vault); os.SameFile(fi, real) && foreignToVault(cfg, PendingOp{Kind: KindWrite, Path: filepath.Join(strings.ToUpper(vault), "memories", "a.md")}) {
			t.Error("case-variant spelling of the vault read as foreign")
		}
	}
	// Seen from the symlinked spelling, a marker on the real path is ours too.
	if foreignToVault(config.Config{VaultDir: link}, PendingOp{Kind: KindWrite, Path: filepath.Join(vault, "memories", "a.md")}) {
		t.Error("real-path marker read as foreign from a symlinked vault_dir")
	}
}
