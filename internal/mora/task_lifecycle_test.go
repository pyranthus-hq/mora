package mora

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func taskTestTableCols(line string) []string {
	raw := strings.Split(strings.Trim(line, "|"), "|")
	out := make([]string, 0, len(raw))
	for _, col := range raw {
		out = append(out, strings.TrimSpace(col))
	}
	return out
}

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
		cols := taskTestTableCols(line)
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

// `mora tasks add <name>` creates a queued live-task row (the capture primitive
// for the daily-brief write-back: open-loops surfaced in triage are persisted).
func TestTasksAddCreatesQueuedRow(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	run(t, "tasks", "add", "Reply to Amaey about the game")

	row := liveTasksRow(t, cfg, "Reply to Amaey about the game")
	if row == "" {
		t.Fatalf("expected `tasks add` to create a live-tasks row")
	}
	cols := taskTestTableCols(row)
	if cols[4] != "queued" {
		t.Fatalf("expected new task Status=queued, got %q", cols[4])
	}
}

// `tasks add` is idempotent by name so a daily automation re-running does not
// mint duplicates; the second call is a no-op success.
func TestTasksAddIsIdempotent(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	run(t, "tasks", "add", "Decide Boston visit")
	out := run(t, "tasks", "add", "Decide Boston visit")

	b, err := os.ReadFile(filepath.Join(cfg.VaultDir, "live-tasks.md"))
	if err != nil {
		t.Fatalf("read live-tasks.md: %v", err)
	}
	if n := strings.Count(string(b), "| Decide Boston visit |"); n != 1 {
		t.Fatalf("expected exactly one row after re-add, got %d:\n%s", n, b)
	}
	if !strings.Contains(strings.ToLower(out), "exists") {
		t.Fatalf("expected idempotent re-add to report it already exists, got %q", out)
	}
}

// Flags follow the (quoted) name: `tasks add "<name>" --pri P0`. The flag must
// be applied, not folded into the task name.
func TestTasksAddFlagsAfterName(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	run(t, "tasks", "add", "Urgent reply", "--pri", "P0")

	row := liveTasksRow(t, cfg, "Urgent reply")
	if row == "" {
		t.Fatalf("expected row named exactly 'Urgent reply' (flags must not be folded into the name)")
	}
	if cols := taskTestTableCols(row); cols[3] != "P0" {
		t.Fatalf("expected --pri P0 applied, got Pri=%q", cols[3])
	}
}

// addTask idempotency keys on the exact Task name (col 0), so a new task whose
// name equals a non-Task cell value (e.g. "None") is still added.
func TestTasksAddNotFooledByOtherColumns(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	// Seed a row; its Blocker column is the literal "None".
	run(t, "tasks", "add", "Some task")

	out := run(t, "tasks", "add", "None") // must NOT be suppressed by the Blocker cell

	if strings.Contains(strings.ToLower(out), "exists") {
		t.Fatalf("a new task named 'None' was wrongly treated as existing (matched a non-Task cell): %q", out)
	}
	if liveTasksRow(t, cfg, "None") == "" {
		t.Fatalf("expected task 'None' to be added")
	}
}

// `tasks list --json` returns the live tasks as structured rows.
func TestTasksListJSON(t *testing.T) {
	withTempHome(t)
	run(t, "init")

	run(t, "tasks", "add", "Task A")
	run(t, "tasks", "add", "Task B")
	out := run(t, "tasks", "list", "--json")

	// Plan 01-07: `tasks list --json` carries its array under `tasks`.
	var doc struct {
		Tasks []LiveTask `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("tasks list --json: %v\n%s", err, out)
	}
	got := doc.Tasks
	names := map[string]bool{}
	for _, lt := range got {
		names[lt.Task] = true
	}
	if !names["Task A"] || !names["Task B"] {
		t.Fatalf("expected both added tasks in list, got %+v", got)
	}
}

// A row whose Status is a terminal state must NOT be reported stale, even when
// its Last-touched date is older than the staleness window.
func TestStaleTasksIgnoresTerminalStatus(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
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
	cfg, err := loadConfigFor(testCtx(t))
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
	cols := taskTestTableCols(row)
	if cols[4] != "done" {
		t.Fatalf("expected Status=done, got %q (row: %s)", cols[4], row)
	}
}

// Task name is the row identity, so `tasks done` closes every same-named row —
// and must report the count rather than silently closing extras.
func TestTasksDoneReportsMultipleRows(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
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
	cfg, err := loadConfigFor(testCtx(t))
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
	cols := taskTestTableCols(liveTasksRow(t, cfg, "Set up Mora"))
	if cols[4] != "done" {
		t.Fatalf("sync resurrected a completed task to Status=%q", cols[4])
	}
}
