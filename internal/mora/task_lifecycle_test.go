package mora

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Task lifecycle (issue #19): completed work must stop resurfacing as "stale".
// The schema is | Task | Domain | Owner | Pri | Status | Blocker | Horizon | Last touched |.

// liveTasksRow returns the | Task | ... | row for taskName, or "" if absent.
func liveTasksRow(t *testing.T, cfg Config, taskName string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cfg.VaultDir, "live-tasks.md"))
	if err != nil {
		t.Fatalf("read live-tasks.md: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "| ") || strings.Contains(line, "Last touched") || strings.Contains(line, "---") {
			continue
		}
		cols := tableCols(line)
		if len(cols) >= 1 && cols[0] == taskName {
			return line
		}
	}
	return ""
}

func writeLiveTasks(t *testing.T, cfg Config, rows ...string) {
	t.Helper()
	body := "# Live Tasks\n\n" +
		"| Task | Domain | Owner | Pri | Status | Blocker | Horizon | Last touched |\n" +
		"|------|--------|-------|-----|--------|---------|---------|--------------|\n" +
		strings.Join(rows, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(cfg.VaultDir, "live-tasks.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write live-tasks.md: %v", err)
	}
}

// A row whose Status is a terminal state must NOT be reported stale, even when
// its Last-touched date is older than the staleness window.
func TestStaleTasksIgnoresTerminalStatus(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	old := "2000-01-01" // far older than any window
	writeLiveTasks(t, cfg,
		"| Open thing | memory | you | P0 | queued | None | this week | "+old+" |",
		"| Finished thing | memory | you | P0 | done | None | this week | "+old+" |",
	)

	stale, err := staleTasks(cfg, 3)
	if err != nil {
		t.Fatalf("staleTasks: %v", err)
	}
	for _, s := range stale {
		if s == "Finished thing" {
			t.Fatalf("a done task was reported stale: %v", stale)
		}
	}
	found := false
	for _, s := range stale {
		if s == "Open thing" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the queued task to still be stale, got %v", stale)
	}
}

// `mora tasks done <name>` flips the row's Status to done (the row is kept).
func TestTasksDoneMarksRowDone(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	writeLiveTasks(t, cfg,
		"| Ship X | memory | you | P0 | queued | None | this week | 2000-01-01 |",
	)

	run(t, "tasks", "done", "Ship X")

	row := liveTasksRow(t, cfg, "Ship X")
	if row == "" {
		t.Fatalf("Ship X row vanished after `tasks done` (it must be kept as the closed-record)")
	}
	cols := tableCols(row)
	if cols[4] != "done" {
		t.Fatalf("expected Status=done, got %q (row: %s)", cols[4], row)
	}
}

// Task name is the row identity, so `tasks done` closes every same-named row —
// and must report the count rather than silently closing extras.
func TestTasksDoneReportsMultipleRows(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	writeLiveTasks(t, cfg,
		"| Dup | a | you | P0 | queued | None | this week | 2000-01-01 |",
		"| Dup | b | you | P0 | queued | None | this week | 2000-01-01 |",
	)

	out := run(t, "tasks", "done", "Dup")

	if !strings.Contains(out, "2 rows") {
		t.Fatalf("expected the count of closed rows surfaced, got %q", out)
	}
	stale, err := staleTasks(cfg, 3)
	if err != nil {
		t.Fatalf("staleTasks: %v", err)
	}
	for _, s := range stale {
		if s == "Dup" {
			t.Fatalf("a closed duplicate-named task still reported stale: %v", stale)
		}
	}
}

// After completion, `tasks sync` must NOT resurrect the task (re-add it or flip
// it back to queued) even though it is still listed under ## P0 in
// priority-map.md. The default vault seeds a P0 item "Set up Mora".
func TestSyncDoesNotResurrectDoneTask(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	run(t, "tasks", "sync", "--write") // seed "Set up Mora" from priority-map P0
	if liveTasksRow(t, cfg, "Set up Mora") == "" {
		t.Fatalf("expected `tasks sync` to seed 'Set up Mora' from priority-map P0")
	}

	run(t, "tasks", "done", "Set up Mora")
	run(t, "tasks", "sync", "--write") // must not resurrect

	b, err := os.ReadFile(filepath.Join(cfg.VaultDir, "live-tasks.md"))
	if err != nil {
		t.Fatalf("read live-tasks.md: %v", err)
	}
	if n := strings.Count(string(b), "| Set up Mora |"); n != 1 {
		t.Fatalf("expected 'Set up Mora' to appear exactly once after sync, got %d:\n%s", n, b)
	}
	cols := tableCols(liveTasksRow(t, cfg, "Set up Mora"))
	if cols[4] != "done" {
		t.Fatalf("sync resurrected a completed task to Status=%q", cols[4])
	}
}
