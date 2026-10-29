package mora

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func teachCASFixture(t *testing.T) (Config, string) {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	var original writeReceipt
	body := run(t, "write", "--scope", "project:cas", "--type", "note", "--title", "Package manager", "--text", "Original instructions", "--json")
	if err := json.Unmarshal([]byte(body), &original); err != nil || original.ID == "" {
		t.Fatalf("receipt %s: %v", body, err)
	}
	return cfg, original.ID
}

func TestTeachCorrectionFailedAppendRemovesOnlyItsReplacement(t *testing.T) {
	cfg, id := teachCASFixture(t)
	old := newIDFn
	newIDFn = func() string {
		// The current snapshot is already loaded. Make the subsequent atomic
		// ledger save fail after replacement creation without mutating real data.
		if err := os.Mkdir(governancePath(cfg), 0700); err != nil {
			t.Errorf("inject ledger save failure: %v", err)
		}
		return old()
	}
	t.Cleanup(func() { newIDFn = old })
	var out bytes.Buffer
	err := teachMemory(testCtx(t), []string{"correct", "--id", id, "--title", "Use pnpm", "--text", "Always pnpm", "--yes"}, &out)
	if err == nil {
		t.Fatal("expected append failure")
	}
	files, err := allMemoryFiles(cfg)
	if err != nil || len(files) != 1 {
		t.Fatalf("failed append left orphan: %v %v", files, err)
	}
	pending, err := listPendingOps(cfg)
	if err != nil || len(pending) != 0 {
		t.Fatalf("failed append left pending markers: %v %v", pending, err)
	}
}

func TestTeachCorrectionRebuildFailureRetainsCommittedTruth(t *testing.T) {
	cfg, id := teachCASFixture(t)
	old := listRebuildFiles
	listRebuildFiles = func(Config) ([]string, error) {
		if _, err := os.Stat(governanceLockPath(cfg)); !os.IsNotExist(err) {
			t.Errorf("rebuild holds governance lease: %v", err)
		}
		return nil, errors.New("synthetic rebuild failure")
	}
	t.Cleanup(func() { listRebuildFiles = old })
	var out bytes.Buffer
	err := teachMemory(testCtx(t), []string{"correct", "--id", id, "--title", "Use pnpm", "--text", "Always pnpm", "--yes"}, &out)
	if err == nil || !strings.Contains(err.Error(), "synthetic rebuild failure") {
		t.Fatalf("rebuild error: %v", err)
	}
	g, err := loadGovernance(cfg)
	if err != nil || len(g.Entries) != 1 || g.memoryVisible(id) || !g.memoryVisible(g.Entries[0].ReplacementID) {
		t.Fatalf("committed revision lost: %+v %v", g, err)
	}
	files, err := allMemoryFiles(cfg)
	if err != nil || len(files) != 2 {
		t.Fatalf("committed files lost: %v %v", files, err)
	}
	pending, err := listPendingOps(cfg)
	if err != nil || len(pending) == 0 {
		t.Fatalf("missing recovery markers: %v %v", pending, err)
	}
}

func TestTeachReplacementCleanupFailureRemainsVisibleForRecovery(t *testing.T) {
	cfg, _ := teachCASFixture(t)
	// An unremovable path simulates a filesystem cleanup failure. The helper
	// must report remaining data and preserve the dirty marker for recovery.
	path := filepath.Join(cfg.VaultDir, "blocked-replacement")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "child"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	op, err := markIndexDirty(testCtx(t), cfg, pendingOp{Kind: opKindWrite, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	err = rollbackTeachReplacement(cfg, Memory{ID: "synthetic", Path: path}, op, errors.New("append failed"))
	if err == nil || !strings.Contains(err.Error(), "requires cleanup") {
		t.Fatalf("misleading cleanup result: %v", err)
	}
	pending, err := listPendingOps(cfg)
	if err != nil || len(pending) != 1 {
		t.Fatalf("cleanup erased recovery marker: %v %v", pending, err)
	}
}
