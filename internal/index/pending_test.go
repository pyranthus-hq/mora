package index

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	files := map[string]bool{"/present": true}
	parsed := map[string]bool{"/parsed": true}
	cases := []struct {
		op   PendingOp
		want bool
	}{{PendingOp{Kind: KindRebuild, MarkedAt: at.Add(-time.Second).Format(time.RFC3339)}, true}, {PendingOp{Kind: KindRebuild, MarkedAt: at.Add(time.Second).Format(time.RFC3339)}, false}, {PendingOp{Kind: KindWrite, Path: "/parsed"}, true}, {PendingOp{Kind: KindWrite, Path: "/present"}, false}, {PendingOp{Kind: KindDelete, Path: "/missing"}, true}, {PendingOp{Kind: KindDelete, Path: "/present"}, false}, {PendingOp{}, true}}
	for _, tc := range cases {
		if got := ShouldClear(tc.op, at, files, parsed); got != tc.want {
			t.Errorf("ShouldClear(%+v)=%v want %v", tc.op, got, tc.want)
		}
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
	err := ClearCovered(cfg, time.Now(), nil, nil, func(_ config.Config, id string) error { removed = id; return nil })
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
	if ShouldClear(ops[0], now, nil, map[string]bool{}) {
		t.Fatal("FTS-committed marker cleared without a covering parse")
	}
	if !ShouldClear(ops[0], now, nil, map[string]bool{ops[0].Path: true}) {
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
