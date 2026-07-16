package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// concurrency_multiproc_test.go — Packet F / HEALTH-06.
//
// The substrate (WAL + busy_timeout + _txlock=immediate) landed with #108, but
// every existing lock storm was goroutines in ONE process. #108's own lesson is
// "test separate PROCESSES." This file is that proof for the personal index.db
// only (share DSNs and their storm belong to Packet H / PR 5).
//
// MUTATION (matrix row 28): remove busy_timeout from rwIndexDSN → this test RED.

const (
	mpEnvRole   = "MORA_MP_ROLE"
	mpEnvHome   = "MORA_MP_HOME"
	mpEnvConfig = "MORA_MP_CONFIG"
	mpEnvWrites = "MORA_MP_WRITES"
)

// TestNoUserVisibleSQLITEBUSY re-execs the test binary as separate PROCESSES
// against one shared HOME: 4 writers, 4 readers (CLI search + MCP search_memory),
// 1 index rebuild, 1 filesystem sync. Assertions:
//  1. Zero "database is locked" / SQLITE_BUSY in any process output (raw OR humanized).
//  2. The index is never observed empty/partial by a reader while clean.
//  3. For every parseable write, at every observation either it is in the index
//     OR the index is dirty — never "clean and missing."
func TestNoUserVisibleSQLITEBUSY(t *testing.T) {
	if role := os.Getenv(mpEnvRole); role != "" {
		runMPChild(t, role)
		return
	}
	if testing.Short() {
		t.Skip("multi-process lock storm is long; run without -short")
	}

	home := t.TempDir()
	configDir := filepath.Join(home, ".config", "mora")
	for _, d := range []string{home, configDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("MORA_CONFIG_DIR", configDir)
	t.Setenv("MORA_EMBEDDER", "")

	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	// Seed enough rows that readers always have hits and a rebuild has work.
	seedIDs := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		id, err := cliWrite(ctx, "global",
			fmt.Sprintf("mp-seed-%d", i),
			fmt.Sprintf("multiproc seed body %d unique-token-%d", i, i))
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		seedIDs = append(seedIDs, id)
		appendMPWrite(t, cfg, id)
	}

	// Filesystem source for the sync process.
	fsRoot := filepath.Join(home, "fs-root")
	if err := os.MkdirAll(fsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fsRoot, "note.md"), []byte("# Note\n\nfs sync body\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveSources(cfg, []Source{{
		Name: "fs", Type: "filesystem", Path: fsRoot, Enabled: ptr(true), Scope: "global",
	}}); err != nil {
		t.Fatal(err)
	}

	roles := []string{
		"writer", "writer", "writer", "writer",
		"reader-cli", "reader-cli", "reader-mcp", "reader-mcp",
		"rebuild",
		"sync",
	}

	type childResult struct {
		role   string
		stdout string
		stderr string
		err    error
	}
	results := make([]childResult, len(roles))
	var wg sync.WaitGroup
	deadline := time.After(90 * time.Second)

	for i, role := range roles {
		wg.Add(1)
		go func(i int, role string) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestNoUserVisibleSQLITEBUSY$", "-test.v", "-test.count=1")
			cmd.Env = append(os.Environ(),
				mpEnvRole+"="+role,
				mpEnvHome+"="+home,
				mpEnvConfig+"="+configDir,
				mpEnvWrites+"="+mpWritesPath(cfg),
				"HOME="+home,
				"USERPROFILE="+home,
				"MORA_CONFIG_DIR="+configDir,
				"MORA_EMBEDDER=",
			)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			results[i] = childResult{role: role, stdout: stdout.String(), stderr: stderr.String(), err: err}
		}(i, role)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-deadline:
		t.Fatal("multi-process storm exceeded 90s wall-clock budget")
	}

	var busyHits []string
	for _, r := range results {
		combined := r.stdout + "\n" + r.stderr
		if containsBusy(combined) {
			busyHits = append(busyHits, fmt.Sprintf("%s: %s", r.role, firstBusyLine(combined)))
		}
		if r.err != nil && !containsBusy(combined) {
			// Children exit non-zero on assertion failure; surface their output.
			t.Errorf("child %s failed: %v\nstdout:\n%s\nstderr:\n%s", r.role, r.err, r.stdout, r.stderr)
		}
	}
	if len(busyHits) > 0 {
		t.Fatalf("user-visible SQLITE_BUSY / database is locked in multiproc storm:\n  %s", strings.Join(busyHits, "\n  "))
	}

	// Post-storm: every seeded + written id is findable after one reconciling rebuild.
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("reconciling rebuild: %v", err)
	}
	written := readMPWrites(t, cfg)
	if len(written) <= len(seedIDs) {
		t.Fatalf("writer processes produced no new ids (got %d log lines, %d seeds) — storm did not exercise writers", len(written), len(seedIDs))
	}
	for _, id := range written {
		if !indexHasID(t, cfg, id) {
			t.Fatalf("after storm+rebuild, memory %s missing from index (last-good / eventual consistency broken)", id)
		}
	}
	st := indexHealthOf(cfg, time.Now()).State
	if st != idxFresh && st != idxDirty {
		// dirty is OK if a late pending op remains; never/failed is not.
		if st == idxNever || st == idxFailed {
			t.Fatalf("index state after storm = %q", st)
		}
	}
}

func runMPChild(t *testing.T, role string) {
	t.Helper()
	home := os.Getenv(mpEnvHome)
	configDir := os.Getenv(mpEnvConfig)
	if home == "" || configDir == "" {
		t.Fatalf("helper missing env HOME/config")
	}
	_ = os.Setenv("HOME", home)
	_ = os.Setenv("USERPROFILE", home)
	_ = os.Setenv("MORA_CONFIG_DIR", configDir)
	_ = os.Setenv("MORA_EMBEDDER", "")

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadConfig: %v\n", err)
		os.Exit(1)
	}
	ctx := context.Background()

	switch role {
	case "writer":
		for i := 0; i < 4; i++ {
			id, werr := cliWrite(ctx, "global",
				fmt.Sprintf("mp-w-%d-%d", os.Getpid(), i),
				fmt.Sprintf("writer body pid=%d i=%d", os.Getpid(), i))
			if werr != nil {
				fmt.Fprintf(os.Stderr, "writer: %v\n", werr)
				if containsBusy(werr.Error()) {
					os.Exit(2)
				}
				os.Exit(1)
			}
			appendMPWrite(t, cfg, id)
			assertNeverCleanAndMissing(t, cfg, id)
		}
	case "reader-cli":
		for i := 0; i < 6; i++ {
			var out bytes.Buffer
			if err := Run(ctx, []string{"search", "multiproc", "--json", "--limit", "20"}, &out, &out, strings.NewReader("")); err != nil {
				fmt.Fprintf(os.Stderr, "search: %v\n%s\n", err, out.String())
				if containsBusy(err.Error()) || containsBusy(out.String()) {
					os.Exit(2)
				}
				os.Exit(1)
			}
			if containsBusy(out.String()) {
				fmt.Fprintf(os.Stderr, "busy in search output: %s\n", out.String())
				os.Exit(2)
			}
			observeCleanMissing(t, cfg)
			time.Sleep(15 * time.Millisecond)
		}
	case "reader-mcp":
		for i := 0; i < 6; i++ {
			res, err := callMCPTool(ctx, "search_memory", map[string]any{
				"query": "multiproc", "limit": 20,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "search_memory: %v\n", err)
				if containsBusy(err.Error()) {
					os.Exit(2)
				}
				os.Exit(1)
			}
			b, _ := json.Marshal(res)
			if containsBusy(string(b)) {
				fmt.Fprintf(os.Stderr, "busy in mcp payload: %s\n", b)
				os.Exit(2)
			}
			observeCleanMissing(t, cfg)
			time.Sleep(15 * time.Millisecond)
		}
	case "rebuild":
		for i := 0; i < 2; i++ {
			if _, err := rebuildIndex(ctx, cfg); err != nil {
				fmt.Fprintf(os.Stderr, "rebuild: %v\n", err)
				if containsBusy(err.Error()) {
					os.Exit(2)
				}
				os.Exit(1)
			}
			time.Sleep(20 * time.Millisecond)
		}
	case "sync":
		fsRoot := filepath.Join(home, "fs-root")
		for i := 0; i < 3; i++ {
			_ = os.WriteFile(filepath.Join(fsRoot, fmt.Sprintf("n-%d.md", i)),
				[]byte(fmt.Sprintf("# N%d\n\nsync-%d\n", i, i)), 0o600)
			sources, _ := loadSources(cfg)
			for _, s := range sources {
				if s.Type != "filesystem" {
					continue
				}
				if _, err := ingestSource(cfg, s, ioDiscard{}); err != nil {
					fmt.Fprintf(os.Stderr, "sync: %v\n", err)
					if containsBusy(err.Error()) {
						os.Exit(2)
					}
					// Non-busy ingest errors are not SQLITE_BUSY — continue storm.
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown role %q\n", role)
		os.Exit(1)
	}
	os.Exit(0)
}

// ioDiscard is a silent writer for sync children (avoid *testing.T races).
type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }

func mpWritesPath(cfg Config) string {
	return filepath.Join(cfg.StateDir, "mp-writes.log")
}

func appendMPWrite(t *testing.T, cfg Config, id string) {
	t.Helper()
	path := mpWritesPath(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%s\n", id); err != nil {
		t.Fatal(err)
	}
}

func readMPWrites(t *testing.T, cfg Config) []string {
	t.Helper()
	b, err := os.ReadFile(mpWritesPath(cfg))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var ids []string
	for _, line := range strings.Split(string(b), "\n") {
		if id := strings.TrimSpace(line); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func containsBusy(s string) bool {
	low := strings.ToLower(s)
	return strings.Contains(low, "database is locked") ||
		strings.Contains(low, "sqlite_busy") ||
		strings.Contains(low, "the index is busy")
}

func firstBusyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if containsBusy(line) {
			return strings.TrimSpace(line)
		}
	}
	return strings.TrimSpace(s)
}

func indexHasID(t *testing.T, cfg Config, id string) bool {
	t.Helper()
	db, err := openIndexRO(context.Background(), cfg)
	if err != nil {
		return false
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id = ?`, id).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

func assertNeverCleanAndMissing(t *testing.T, cfg Config, id string) {
	t.Helper()
	h := indexHealthOf(cfg, time.Now())
	if h.State == idxFresh && !indexHasID(t, cfg, id) {
		fmt.Fprintf(os.Stderr, "clean-and-missing: id %s absent while index is fresh\n", id)
		os.Exit(1)
	}
}

func observeCleanMissing(t *testing.T, cfg Config) {
	t.Helper()
	writes := os.Getenv(mpEnvWrites)
	if writes == "" {
		return
	}
	b, err := os.ReadFile(writes)
	if err != nil {
		return
	}
	h := indexHealthOf(cfg, time.Now())
	if h.State != idxFresh {
		return // dirty/failed/degraded may miss rows; that is the invariant escape hatch
	}
	for _, line := range strings.Split(string(b), "\n") {
		id := strings.TrimSpace(line)
		if id == "" {
			continue
		}
		if !indexHasID(t, cfg, id) {
			fmt.Fprintf(os.Stderr, "clean-and-missing observation: %s\n", id)
			os.Exit(1)
		}
	}
}
