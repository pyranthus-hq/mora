package mora

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTeachConcurrentCorrectionsCompareCurrentBeforeCreating(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	var original writeReceipt
	body := run(t, "write", "--scope", "project:cas", "--type", "note", "--title", "Package manager", "--text", "Original instructions", "--json")
	if err := json.Unmarshal([]byte(body), &original); err != nil || original.ID == "" {
		t.Fatalf("original receipt: %s, %v", body, err)
	}

	const contenders = 8
	minting := make(chan struct{}, contenders)
	releaseMint := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseMint) }) }
	defer release()
	oldNewID := newIDFn
	newIDFn = func() string {
		minting <- struct{}{}
		<-releaseMint
		return oldNewID()
	}
	t.Cleanup(func() { newIDFn = oldNewID })

	ctx := testCtx(t)
	start := make(chan struct{})
	results := make(chan error, contenders)
	var workers sync.WaitGroup
	for range contenders {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			var out, errout bytes.Buffer
			results <- cmdTeach(ctx, []string{"memory", "correct", "--id", original.ID, "--title", "Use pnpm", "--text", "Always install pnpm", "--yes"}, &out, &errout)
		}()
	}
	close(start)
	select {
	case <-minting:
	case <-time.After(5 * time.Second):
		release()
		workers.Wait()
		t.Fatal("no correction reached replacement creation")
	}
	// The old path checks currentness without the lease: hold every replacement
	// mint until all eight stale checks have passed. A corrected path already
	// holds the ledger lease at the first mint, so its losers cannot reach mint.
	if _, err := os.Stat(governanceLockPath(cfg)); os.IsNotExist(err) {
		for i := 1; i < contenders; i++ {
			select {
			case <-minting:
			case <-time.After(5 * time.Second):
				release()
				workers.Wait()
				t.Fatalf("only %d stale corrections reached replacement creation", i)
			}
		}
	} else if err != nil {
		release()
		workers.Wait()
		t.Fatal(err)
	}
	release()
	workers.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if strings.Contains(strings.ToLower(err.Error()), "conflict") {
			conflicts++
		} else {
			t.Errorf("unexpected correction refusal: %v", err)
		}
	}
	files, err := allMemoryFiles(cfg)
	if err != nil {
		t.Fatal(err)
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	active, current := 0, 0
	var winner govEntry
	for _, entry := range g.Entries {
		if entry.Kind == govKindTeachMemory && entry.TargetID == original.ID && !govEntryRevoked(entry) {
			active++
			winner = entry
		}
	}
	for _, path := range files {
		m, err := parseMemory(path)
		if err != nil {
			t.Fatal(err)
		}
		if g.memoryVisible(m.ID) {
			current++
		}
	}
	if success != 1 || conflicts != contenders-1 || active != 1 || current != 1 || len(files) != 2 || g.memoryVisible(original.ID) || winner.ReplacementID == "" || !g.memoryVisible(winner.ReplacementID) {
		t.Fatalf("corrections: success=%d conflict=%d active=%d current=%d files=%d; want 1,7,1,1,2 with only the winner current", success, conflicts, active, current, len(files))
	}
}

func TestTeachUndoCannotRestoreAncestorAfterConcurrentCorrection(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	var original writeReceipt
	body := run(t, "write", "--scope", "project:cas", "--type", "note", "--title", "Package manager", "--text", "Original instructions", "--json")
	if err := json.Unmarshal([]byte(body), &original); err != nil || original.ID == "" {
		t.Fatalf("original receipt: %s, %v", body, err)
	}
	var first struct {
		Decision    govEntry `json:"decision"`
		Replacement Memory   `json:"replacement"`
	}
	body = run(t, "teach", "memory", "correct", "--id", original.ID, "--title", "Use pnpm", "--text", "Always use pnpm", "--yes", "--json")
	if err := json.Unmarshal([]byte(body), &first); err != nil || first.Decision.ID == "" || first.Replacement.ID == "" {
		t.Fatalf("first correction: %s, %v", body, err)
	}

	minting, releaseMint := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseMint) }) }
	defer release()
	oldNewID := newIDFn
	newIDFn = func() string {
		close(minting)
		<-releaseMint
		return oldNewID()
	}
	t.Cleanup(func() { newIDFn = oldNewID })
	ctx := testCtx(t)
	correctionResult, undoResult := make(chan error, 1), make(chan error, 1)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		var out, errout bytes.Buffer
		correctionResult <- cmdTeach(ctx, []string{"memory", "correct", "--id", first.Replacement.ID, "--title", "Use pnpm consistently", "--text", "Always install dependencies with pnpm", "--yes"}, &out, &errout)
	}()
	select {
	case <-minting:
	case <-time.After(5 * time.Second):
		release()
		workers.Wait()
		t.Fatal("new correction did not reach replacement creation")
	}
	_, leaseErr := os.Stat(governanceLockPath(cfg))
	if leaseErr != nil && !os.IsNotExist(leaseErr) {
		release()
		workers.Wait()
		t.Fatal(leaseErr)
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		var out, errout bytes.Buffer
		undoResult <- cmdTeach(ctx, []string{"undo", first.Decision.ID}, &out, &errout)
	}()
	var undoErr error
	if os.IsNotExist(leaseErr) {
		// In the old path the paused correction holds no ledger lease. Let
		// undo finish before that correction appends its stale descendant.
		select {
		case undoErr = <-undoResult:
		case <-time.After(5 * time.Second):
			release()
			workers.Wait()
			t.Fatal("unguarded correction unexpectedly blocked undo")
		}
		release()
	} else {
		// With the CAS lease held, undo must wait for the new edge and check
		// descendants from the ledger loaded after that commit.
		release()
		undoErr = <-undoResult
	}
	workers.Wait()
	if err := <-correctionResult; err != nil {
		t.Fatalf("new correction failed: %v", err)
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	files, err := allMemoryFiles(cfg)
	if err != nil {
		t.Fatal(err)
	}
	current := 0
	for _, path := range files {
		m, err := parseMemory(path)
		if err != nil {
			t.Fatal(err)
		}
		if g.memoryVisible(m.ID) {
			current++
		}
	}
	if undoErr == nil || !strings.Contains(undoErr.Error(), "later revision") || current != 1 || len(files) != 3 || g.memoryVisible(original.ID) || g.memoryVisible(first.Replacement.ID) {
		t.Fatalf("ancestor undo: err=%v current=%d files=%d; want descendant conflict,1current,3history files", undoErr, current, len(files))
	}
}
