package mora

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// coreBTaskcfg returns a Config whose VaultDir is a fresh temp dir. Every
// task/control-file function under test reads and writes ONLY cfg.VaultDir, so
// this minimal fixture is sufficient (no index/state/config needed).
func coreBTaskcfg(t *testing.T) Config {
	t.Helper()
	return Config{VaultDir: t.TempDir()}
}

// coreBTaskHeader is the two structural lines of live-tasks.md (title + table
// header + separator) that listTasks/staleTasks/markTaskDone all skip.
const coreBTaskHeader = "# Live Tasks\n\n" +
	"| Task | Domain | Owner | Pri | Status | Blocker | Horizon | Last touched |\n" +
	"|------|--------|-------|-----|--------|---------|---------|--------------|\n"

// coreBTaskwriteLive writes live-tasks.md with the given data rows appended
// after the standard header/separator.
func coreBTaskwriteLive(t *testing.T, cfg Config, rows ...string) {
	t.Helper()
	body := coreBTaskHeader
	if len(rows) > 0 {
		body += strings.Join(rows, "\n") + "\n"
	}
	if err := os.WriteFile(filepath.Join(cfg.VaultDir, "live-tasks.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write live-tasks: %v", err)
	}
}

// coreBTaskwritePriority writes priority-map.md verbatim.
func coreBTaskwritePriority(t *testing.T, cfg Config, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cfg.VaultDir, "priority-map.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write priority-map: %v", err)
	}
}

// coreBTaskrow builds one 8-column markdown table row.
func coreBTaskrow(cols ...string) string {
	return "| " + strings.Join(cols, " | ") + " |"
}

func coreBTasktoday() string { return time.Now().Format("2006-01-02") }

// coreBTaskfindByName returns the first LiveTask whose Task matches name.
func coreBTaskfindByName(tasks []LiveTask, name string) (LiveTask, bool) {
	for _, lt := range tasks {
		if lt.Task == name {
			return lt, true
		}
	}
	return LiveTask{}, false
}

// ---------------------------------------------------------------------------
// parseP0
// ---------------------------------------------------------------------------

func TestCoreB_TaskParseP0Section(t *testing.T) {
	cfg := coreBTaskcfg(t)
	// P0 section mixes numbered and dash bold-title items (covers both regex
	// alternatives), a sub-bullet that is NOT a task, and a P1 section whose
	// bold item must be excluded once inP0 flips off.
	coreBTaskwritePriority(t, cfg, `# Priority Map

## P0 — Active This Week

1. **Alpha task** — do alpha.
   - Outcome: not a task.
2. **Beta task**
- **Gamma dash** — dash form.

## P1 — This Month

- **Delta** — should be excluded.
`)
	got, err := parseP0(filepath.Join(cfg.VaultDir, "priority-map.md"))
	if err != nil {
		t.Fatalf("parseP0: %v", err)
	}
	want := []string{"Alpha task", "Beta task", "Gamma dash"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseP0 = %#v, want %#v", got, want)
	}
}

func TestCoreB_TaskParseP0NoSection(t *testing.T) {
	cfg := coreBTaskcfg(t)
	// No P0 header at all -> nothing is ever in-section -> empty result.
	coreBTaskwritePriority(t, cfg, `# Priority Map

## P1 — This Month

- **Only P1** — never selected.
`)
	got, err := parseP0(filepath.Join(cfg.VaultDir, "priority-map.md"))
	if err != nil {
		t.Fatalf("parseP0: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("parseP0 with no P0 section = %#v, want empty", got)
	}
}

func TestCoreB_TaskParseP0MissingFile(t *testing.T) {
	cfg := coreBTaskcfg(t)
	_, err := parseP0(filepath.Join(cfg.VaultDir, "does-not-exist.md"))
	if err == nil {
		t.Fatal("parseP0 on missing file: expected error, got nil")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("parseP0 missing-file error = %v, want not-exist", err)
	}
}

// ---------------------------------------------------------------------------
// listTasks
// ---------------------------------------------------------------------------

func TestCoreB_TaskListTasksParsesRows(t *testing.T) {
	cfg := coreBTaskcfg(t)
	coreBTaskwriteLive(t, cfg,
		coreBTaskrow("Ship v1", "mora", "adit", "P0", "queued", "None", "this week", "2026-06-01"),
		coreBTaskrow("Fix bug", "mora", "neil", "P1", "blocked", "review", "this month", "2026-06-02"),
		"| bad | row |", // <8 cols: must be skipped
	)
	got, err := listTasks(cfg)
	if err != nil {
		t.Fatalf("listTasks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listTasks len = %d, want 2 (header/separator/short-row skipped): %#v", len(got), got)
	}
	want0 := LiveTask{Task: "Ship v1", Domain: "mora", Owner: "adit", Pri: "P0", Status: "queued", Blocker: "None", Horizon: "this week", LastTouched: "2026-06-01"}
	if got[0] != want0 {
		t.Fatalf("listTasks[0] = %#v, want %#v", got[0], want0)
	}
	want1 := LiveTask{Task: "Fix bug", Domain: "mora", Owner: "neil", Pri: "P1", Status: "blocked", Blocker: "review", Horizon: "this month", LastTouched: "2026-06-02"}
	if got[1] != want1 {
		t.Fatalf("listTasks[1] = %#v, want %#v", got[1], want1)
	}
}

func TestCoreB_TaskListTasksEmptyTable(t *testing.T) {
	cfg := coreBTaskcfg(t)
	coreBTaskwriteLive(t, cfg) // header/separator only
	got, err := listTasks(cfg)
	if err != nil {
		t.Fatalf("listTasks: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("listTasks empty = %#v, want non-nil empty slice", got)
	}
}

func TestCoreB_TaskListTasksMissingFile(t *testing.T) {
	cfg := coreBTaskcfg(t) // no live-tasks.md written
	_, err := listTasks(cfg)
	if err == nil {
		t.Fatal("listTasks on missing file: expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// addTask
// ---------------------------------------------------------------------------

func TestCoreB_TaskAddTaskNewAndDuplicate(t *testing.T) {
	cfg := coreBTaskcfg(t)
	coreBTaskwriteLive(t, cfg)

	// Status on the input is intentionally NOT "queued": addTask must force
	// "queued" and stamp Last touched to today regardless of input.
	added, err := addTask(cfg, LiveTask{Task: "Write tests", Domain: "mora", Owner: "adit", Pri: "P1", Status: "in-progress", Blocker: "none", Horizon: "today"})
	if err != nil {
		t.Fatalf("addTask new: %v", err)
	}
	if !added {
		t.Fatal("addTask new = false, want true")
	}

	tasks, err := listTasks(cfg)
	if err != nil {
		t.Fatalf("listTasks: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("after add: %d rows, want 1: %#v", len(tasks), tasks)
	}
	want := LiveTask{Task: "Write tests", Domain: "mora", Owner: "adit", Pri: "P1", Status: "queued", Blocker: "none", Horizon: "today", LastTouched: coreBTasktoday()}
	if tasks[0] != want {
		t.Fatalf("added row = %#v, want %#v", tasks[0], want)
	}

	// Duplicate by exact Task name -> no-op, count unchanged.
	added2, err := addTask(cfg, LiveTask{Task: "Write tests", Domain: "other", Owner: "someone", Pri: "P0"})
	if err != nil {
		t.Fatalf("addTask dup: %v", err)
	}
	if added2 {
		t.Fatal("addTask duplicate = true, want false")
	}
	tasks2, _ := listTasks(cfg)
	if len(tasks2) != 1 {
		t.Fatalf("after duplicate add: %d rows, want 1 (unchanged)", len(tasks2))
	}
}

func TestCoreB_TaskAddTaskPrefixNotSuppressed(t *testing.T) {
	cfg := coreBTaskcfg(t)
	coreBTaskwriteLive(t, cfg,
		coreBTaskrow("Task", "d", "o", "P0", "queued", "None", "this week", "2026-06-01"),
	)
	// "Task2" is not an exact match of the existing "Task" row, so it must add.
	added, err := addTask(cfg, LiveTask{Task: "Task2", Domain: "d", Owner: "o", Pri: "P0"})
	if err != nil {
		t.Fatalf("addTask: %v", err)
	}
	if !added {
		t.Fatal("addTask prefix-only match suppressed the add; want true")
	}
	tasks, _ := listTasks(cfg)
	if _, ok := coreBTaskfindByName(tasks, "Task2"); !ok {
		t.Fatalf("Task2 not present after add: %#v", tasks)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(tasks))
	}
}

func TestCoreB_TaskAddTaskMissingFile(t *testing.T) {
	cfg := coreBTaskcfg(t) // no live-tasks.md
	added, err := addTask(cfg, LiveTask{Task: "X"})
	if err == nil {
		t.Fatal("addTask on missing file: expected error, got nil")
	}
	if added {
		t.Fatal("addTask error path returned added=true")
	}
}

// ---------------------------------------------------------------------------
// markTaskDone
// ---------------------------------------------------------------------------

func TestCoreB_TaskMarkTaskDoneOpenRow(t *testing.T) {
	cfg := coreBTaskcfg(t)
	coreBTaskwriteLive(t, cfg,
		coreBTaskrow("OpenOne", "mora", "adit", "P0", "queued", "None", "this week", "2026-01-01"),
		coreBTaskrow("Other", "mora", "adit", "P1", "queued", "None", "this month", "2026-01-01"),
	)
	// Leading/trailing whitespace on the name must be trimmed before matching.
	n, err := markTaskDone(cfg, "  OpenOne  ")
	if err != nil {
		t.Fatalf("markTaskDone: %v", err)
	}
	if n != 1 {
		t.Fatalf("markTaskDone count = %d, want 1", n)
	}
	tasks, _ := listTasks(cfg)
	done, ok := coreBTaskfindByName(tasks, "OpenOne")
	if !ok {
		t.Fatal("OpenOne missing after markTaskDone (row must be KEPT)")
	}
	if done.Status != "done" {
		t.Fatalf("OpenOne status = %q, want done", done.Status)
	}
	if done.LastTouched != coreBTasktoday() {
		t.Fatalf("OpenOne last touched = %q, want today %q", done.LastTouched, coreBTasktoday())
	}
	// The untouched row keeps its status.
	other, _ := coreBTaskfindByName(tasks, "Other")
	if other.Status != "queued" || other.LastTouched != "2026-01-01" {
		t.Fatalf("Other row mutated: %#v", other)
	}
}

func TestCoreB_TaskMarkTaskDoneNotFound(t *testing.T) {
	cfg := coreBTaskcfg(t)
	coreBTaskwriteLive(t, cfg,
		coreBTaskrow("Present", "mora", "adit", "P0", "queued", "None", "this week", "2026-01-01"),
	)
	before, _ := os.ReadFile(filepath.Join(cfg.VaultDir, "live-tasks.md"))
	n, err := markTaskDone(cfg, "Nonexistent")
	if err != nil {
		t.Fatalf("markTaskDone: %v", err)
	}
	if n != 0 {
		t.Fatalf("markTaskDone missing = %d, want 0", n)
	}
	after, _ := os.ReadFile(filepath.Join(cfg.VaultDir, "live-tasks.md"))
	if string(before) != string(after) {
		t.Fatal("markTaskDone not-found rewrote the file; want unchanged")
	}
}

func TestCoreB_TaskMarkTaskDoneAlreadyDone(t *testing.T) {
	cfg := coreBTaskcfg(t)
	coreBTaskwriteLive(t, cfg,
		coreBTaskrow("Closed", "mora", "adit", "P0", "done", "None", "this week", "2026-01-01"),
	)
	n, err := markTaskDone(cfg, "Closed")
	if err != nil {
		t.Fatalf("markTaskDone: %v", err)
	}
	if n != 1 {
		t.Fatalf("markTaskDone already-done = %d, want 1 (re-stamped)", n)
	}
	tasks, _ := listTasks(cfg)
	closed, _ := coreBTaskfindByName(tasks, "Closed")
	if closed.Status != "done" {
		t.Fatalf("status = %q, want done", closed.Status)
	}
	if closed.LastTouched != coreBTasktoday() {
		t.Fatalf("last touched = %q, want today (completion re-stamped)", closed.LastTouched)
	}
}

func TestCoreB_TaskMarkTaskDoneMultipleMatches(t *testing.T) {
	cfg := coreBTaskcfg(t)
	coreBTaskwriteLive(t, cfg,
		coreBTaskrow("Dup", "a", "x", "P0", "queued", "None", "this week", "2026-01-01"),
		coreBTaskrow("Dup", "b", "y", "P1", "blocked", "None", "this month", "2026-01-02"),
	)
	n, err := markTaskDone(cfg, "Dup")
	if err != nil {
		t.Fatalf("markTaskDone: %v", err)
	}
	if n != 2 {
		t.Fatalf("markTaskDone count = %d, want 2 (both rows)", n)
	}
	tasks, _ := listTasks(cfg)
	for _, lt := range tasks {
		if lt.Task == "Dup" && lt.Status != "done" {
			t.Fatalf("row not marked done: %#v", lt)
		}
	}
}

func TestCoreB_TaskMarkTaskDoneMissingFile(t *testing.T) {
	cfg := coreBTaskcfg(t)
	n, err := markTaskDone(cfg, "X")
	if err == nil {
		t.Fatal("markTaskDone on missing file: expected error, got nil")
	}
	if n != 0 {
		t.Fatalf("markTaskDone error path count = %d, want 0", n)
	}
}

// ---------------------------------------------------------------------------
// staleTasks
// ---------------------------------------------------------------------------

func TestCoreB_TaskStaleTasksFiltering(t *testing.T) {
	cfg := coreBTaskcfg(t)
	old := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	recent := time.Now().Format("2006-01-02")
	coreBTaskwriteLive(t, cfg,
		coreBTaskrow("OldOpen", "mora", "adit", "P0", "queued", "None", "this week", old),        // stale
		coreBTaskrow("RecentOpen", "mora", "adit", "P0", "queued", "None", "this week", recent),  // fresh
		coreBTaskrow("OldDone", "mora", "adit", "P0", "done", "None", "this week", old),          // terminal -> excluded
		coreBTaskrow("BadDate", "mora", "adit", "P0", "queued", "None", "this week", "notadate"), // unparseable -> excluded
		"| short | row |", // <8 cols -> skipped
	)
	got, err := staleTasks(cfg, 7)
	if err != nil {
		t.Fatalf("staleTasks: %v", err)
	}
	want := []string{"OldOpen"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("staleTasks = %#v, want %#v", got, want)
	}
}

func TestCoreB_TaskStaleTasksNoneStale(t *testing.T) {
	cfg := coreBTaskcfg(t)
	recent := time.Now().Format("2006-01-02")
	coreBTaskwriteLive(t, cfg,
		coreBTaskrow("Fresh", "mora", "adit", "P0", "queued", "None", "this week", recent),
	)
	got, err := staleTasks(cfg, 7)
	if err != nil {
		t.Fatalf("staleTasks: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("staleTasks = %#v, want empty", got)
	}
}

func TestCoreB_TaskStaleTasksMissingFile(t *testing.T) {
	cfg := coreBTaskcfg(t)
	_, err := staleTasks(cfg, 7)
	if err == nil {
		t.Fatal("staleTasks on missing file: expected error, got nil")
	}
}

// ---------------------------------------------------------------------------
// syncTasks
// ---------------------------------------------------------------------------

func coreBTasksyncPriority(t *testing.T, cfg Config) {
	t.Helper()
	coreBTaskwritePriority(t, cfg, `# Priority Map

## P0 — Active This Week

1. **Alpha task** — desc.
2. **Beta task** — desc.
`)
}

func TestCoreB_TaskSyncTasksDryRun(t *testing.T) {
	cfg := coreBTaskcfg(t)
	coreBTasksyncPriority(t, cfg)
	coreBTaskwriteLive(t, cfg) // empty table -> both P0 items missing

	livePath := filepath.Join(cfg.VaultDir, "live-tasks.md")
	before, _ := os.ReadFile(livePath)

	added, err := syncTasks(cfg, false)
	if err != nil {
		t.Fatalf("syncTasks dry-run: %v", err)
	}
	if added != 2 {
		t.Fatalf("syncTasks dry-run added = %d, want 2", added)
	}
	after, _ := os.ReadFile(livePath)
	if string(before) != string(after) {
		t.Fatal("syncTasks(write=false) mutated live-tasks.md; want unchanged")
	}
}

func TestCoreB_TaskSyncTasksWriteReconciles(t *testing.T) {
	cfg := coreBTaskcfg(t)
	coreBTasksyncPriority(t, cfg)
	coreBTaskwriteLive(t, cfg)

	added, err := syncTasks(cfg, true)
	if err != nil {
		t.Fatalf("syncTasks write: %v", err)
	}
	if added != 2 {
		t.Fatalf("syncTasks write added = %d, want 2", added)
	}
	tasks, _ := listTasks(cfg)
	alpha, ok := coreBTaskfindByName(tasks, "Alpha task")
	if !ok {
		t.Fatalf("Alpha task not created: %#v", tasks)
	}
	// syncTasks stamps a fixed shape for reconciled P0 rows.
	wantAlpha := LiveTask{Task: "Alpha task", Domain: "memory", Owner: "you", Pri: "P0", Status: "queued", Blocker: "None", Horizon: "this week", LastTouched: coreBTasktoday()}
	if alpha != wantAlpha {
		t.Fatalf("Alpha row = %#v, want %#v", alpha, wantAlpha)
	}
	if _, ok := coreBTaskfindByName(tasks, "Beta task"); !ok {
		t.Fatalf("Beta task not created: %#v", tasks)
	}

	// Re-running now finds both present -> nothing added, file unchanged.
	livePath := filepath.Join(cfg.VaultDir, "live-tasks.md")
	before, _ := os.ReadFile(livePath)
	added2, err := syncTasks(cfg, true)
	if err != nil {
		t.Fatalf("syncTasks re-run: %v", err)
	}
	if added2 != 0 {
		t.Fatalf("syncTasks re-run added = %d, want 0 (idempotent)", added2)
	}
	after, _ := os.ReadFile(livePath)
	if string(before) != string(after) {
		t.Fatal("syncTasks re-run rewrote file despite added=0")
	}
}

func TestCoreB_TaskSyncTasksMissingPriorityMap(t *testing.T) {
	cfg := coreBTaskcfg(t)
	coreBTaskwriteLive(t, cfg) // live present, priority-map absent
	added, err := syncTasks(cfg, true)
	if err == nil {
		t.Fatal("syncTasks with no priority-map: expected error, got nil")
	}
	if added != 0 {
		t.Fatalf("syncTasks error path added = %d, want 0", added)
	}
}

// coreBTaskseal makes cfg.VaultDir read-only (no write bit) so that
// atomicWrite's in-dir temp-file creation fails, exercising the write-error
// branches. The permission is restored before t.TempDir cleanup runs.
func coreBTaskseal(t *testing.T, cfg Config) {
	t.Helper()
	if err := os.Chmod(cfg.VaultDir, 0o500); err != nil {
		t.Fatalf("chmod vault ro: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfg.VaultDir, 0o700) })
}

func TestCoreB_TaskMarkTaskDoneWriteError(t *testing.T) {
	cfg := coreBTaskcfg(t)
	coreBTaskwriteLive(t, cfg,
		coreBTaskrow("T", "d", "o", "P0", "queued", "None", "this week", "2026-01-01"),
	)
	coreBTaskseal(t, cfg)
	n, err := markTaskDone(cfg, "T")
	if err == nil {
		t.Fatal("markTaskDone with read-only vault: expected write error, got nil")
	}
	if n != 0 {
		t.Fatalf("markTaskDone write-error count = %d, want 0", n)
	}
}

func TestCoreB_TaskAddTaskWriteError(t *testing.T) {
	cfg := coreBTaskcfg(t)
	coreBTaskwriteLive(t, cfg)
	coreBTaskseal(t, cfg)
	added, err := addTask(cfg, LiveTask{Task: "New"})
	if err == nil {
		t.Fatal("addTask with read-only vault: expected write error, got nil")
	}
	if added {
		t.Fatal("addTask write-error returned added=true")
	}
}

func TestCoreB_TaskSyncTasksWriteError(t *testing.T) {
	cfg := coreBTaskcfg(t)
	coreBTasksyncPriority(t, cfg)
	coreBTaskwriteLive(t, cfg)
	coreBTaskseal(t, cfg)
	added, err := syncTasks(cfg, true)
	if err == nil {
		t.Fatal("syncTasks(write) with read-only vault: expected write error, got nil")
	}
	if added != 0 {
		t.Fatalf("syncTasks write-error added = %d, want 0", added)
	}
}

func TestCoreB_TaskSyncTasksMissingLiveTasks(t *testing.T) {
	cfg := coreBTaskcfg(t)
	coreBTasksyncPriority(t, cfg) // priority present, live-tasks absent
	added, err := syncTasks(cfg, true)
	if err == nil {
		t.Fatal("syncTasks with no live-tasks: expected error, got nil")
	}
	if added != 0 {
		t.Fatalf("syncTasks error path added = %d, want 0", added)
	}
}
