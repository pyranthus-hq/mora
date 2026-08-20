package index

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
