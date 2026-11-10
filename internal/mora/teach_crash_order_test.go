package mora

import (
	"bytes"
	"fmt"
	"os"
	"testing"
)

func TestTeachCrashOrder(t *testing.T) {
	// AC-001.1: correct remains single-current at every durable marker boundary.
	// AC-001.2: supersede remains single-current at every durable marker boundary.
	// AC-001.3: every replacement file has a ledger entry after each stop.
	for _, decision := range []string{"correct", "supersede"} {
		t.Run(decision, func(t *testing.T) {
			stopped := 0
			for stop := 1; ; stop++ {
				completed := false
				t.Run(fmt.Sprintf("marker-%d", stop), func(t *testing.T) {
					cfg, original := teachCASFixture(t)
					writes := 0
					crash := new(int)
					testHookPostMarkerWrite = func() {
						writes++
						if writes == stop {
							panic(crash)
						}
					}
					t.Cleanup(func() { testHookPostMarkerWrite = nil })
					var commandErr error
					panicked := false
					func() {
						defer func() {
							if v := recover(); v != nil {
								if v != crash {
									panic(v)
								}
								panicked = true
							}
						}()
						var out bytes.Buffer
						commandErr = teachMemory(testCtx(t), []string{decision, "--id", original, "--title", "Replacement", "--text", "Revised instructions", "--yes"}, &out)
					}()
					testHookPostMarkerWrite = nil
					if commandErr != nil {
						t.Fatal(commandErr)
					}
					if panicked {
						stopped++
					} else {
						completed = true
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
						if m.ID == original {
							continue
						}
						recorded := false
						for _, entry := range g.Entries {
							if entry.ReplacementID == m.ID {
								recorded = true
							}
						}
						if !recorded {
							t.Errorf("replacement %s has no ledger entry", m.ID)
						}
					}
					if current > 1 {
						t.Errorf("%d current memories after marker %d", current, stop)
					}
				})
				if completed {
					break
				}
				if stop >= 20 {
					t.Fatal("command did not complete within 20 marker writes")
				}
			}
			if stopped == 0 {
				t.Fatal("no marker boundaries exercised")
			}
		})
	}
}

func TestTeachReplacementMintsOnce(t *testing.T) {
	// AC-001.4: the single minted id is both the published file id and ledger id.
	for _, decision := range []string{"correct", "supersede"} {
		t.Run(decision, func(t *testing.T) {
			cfg, original := teachCASFixture(t)
			old := newIDFn
			calls := 0
			const replacement = "mem_crash_order_replacement"
			newIDFn = func() string { calls++; return replacement }
			t.Cleanup(func() { newIDFn = old })
			var out bytes.Buffer
			if err := teachMemory(testCtx(t), []string{decision, "--id", original, "--title", "Replacement", "--text", "Revised instructions", "--yes"}, &out); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("minted %d times, want 1", calls)
			}
			g, err := loadGovernance(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(g.Entries) != 1 || g.Entries[0].ReplacementID != replacement {
				t.Fatalf("ledger: %+v", g)
			}
			m, err := findMemoryRaw(cfg, replacement)
			if err != nil || m.ID != replacement {
				t.Fatalf("replacement: %+v, %v", m, err)
			}
			files, err := allMemoryFiles(cfg)
			if err != nil || len(files) != 2 {
				t.Fatalf("files: %v, %v", files, err)
			}
		})
	}
}

func TestTeachCreateFailureRevokesWithoutRemovingExistingFile(t *testing.T) {
	// AC-002.1: failed creation revokes the appended entry and restores the original.
	// AC-002.2: an existing replacement path is never removed or overwritten.
	cfg, original := teachCASFixture(t)
	const replacement = "mem_existing_replacement"
	path, err := memoryPath(cfg, Memory{ID: replacement, Scope: "project:cas"})
	if err != nil {
		t.Fatal(err)
	}
	const content = "pre-existing file must survive byte for byte"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	old := newIDFn
	newIDFn = func() string { return replacement }
	t.Cleanup(func() { newIDFn = old })
	var out bytes.Buffer
	err = teachMemory(testCtx(t), []string{"correct", "--id", original, "--title", "Replacement", "--text", "Revised instructions", "--yes"}, &out)
	if err == nil {
		t.Fatal("expected create failure")
	}
	g, err := loadGovernance(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Entries) != 1 || !govEntryRevoked(g.Entries[0]) || g.Entries[0].ReplacementID != replacement || !g.memoryVisible(original) {
		t.Fatalf("failed create did not restore original and revoke entry: %+v", g)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != content {
		t.Fatalf("existing file changed: %q, %v", got, err)
	}
}

func TestTeachCrashOrderExistingRegressions(t *testing.T) {
	// AC-002.3: retain append, rebuild, concurrent CAS, undo, and scope safeguards.
	t.Run("FailedAppend", TestTeachCorrectionFailedAppendRemovesOnlyItsReplacement)
	t.Run("RebuildFailure", TestTeachCorrectionRebuildFailureRetainsCommittedTruth)
	t.Run("ConcurrentCorrections", TestTeachConcurrentCorrectionsCompareCurrentBeforeCreating)
	t.Run("ConcurrentUndo", TestTeachUndoCannotRestoreAncestorAfterConcurrentCorrection)
	t.Run("UnsafeScope", TestTeachUnsafeScopeOverride)
}
