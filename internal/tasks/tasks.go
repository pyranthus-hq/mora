// Package tasks owns Mora's human-readable live-task ledger.
package tasks

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
)

var p0Re = regexp.MustCompile(`^(\d+\.|-)\s+\*\*([^*]+)\*\*`)

var terminalTaskStatuses = map[string]bool{
	"done": true, "completed": true, "cancelled": true,
	"canceled": true, "wontfix": true,
}

// Config is the package-neutral task-store location.
type Config struct{ VaultDir string }

// LiveTask is one row of live-tasks.md.
type LiveTask struct {
	Task        string `json:"task"`
	Domain      string `json:"domain"`
	Owner       string `json:"owner"`
	Pri         string `json:"pri"`
	Status      string `json:"status"`
	Blocker     string `json:"blocker"`
	Horizon     string `json:"horizon"`
	LastTouched string `json:"last_touched"`
}

func syncTasks(cfg Config, write bool) (int, error) {
	p0, err := parseP0(filepath.Join(cfg.VaultDir, "priority-map.md"))
	if err != nil {
		return 0, err
	}
	livePath := filepath.Join(cfg.VaultDir, "live-tasks.md")
	bodyBytes, err := os.ReadFile(livePath)
	if err != nil {
		return 0, err
	}
	body := string(bodyBytes)
	added := 0
	var rows []string
	for _, task := range p0 {
		if strings.Contains(body, "| "+task+" |") {
			continue
		}
		rows = append(rows, fmt.Sprintf("| %s | memory | you | P0 | queued | None | this week | %s |", task, time.Now().Format("2006-01-02")))
		added++
	}
	if write && added > 0 {
		body = strings.TrimRight(body, "\n") + "\n" + strings.Join(rows, "\n") + "\n"
		if err := atomicio.Write(livePath, []byte(body), 0o644); err != nil {
			return 0, err
		}
	}
	return added, nil
}
func parseP0(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var inP0 bool
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "## ") {
			inP0 = strings.Contains(strings.ToLower(line), "p0")
			continue
		}
		if !inP0 {
			continue
		}
		m := p0Re.FindStringSubmatch(strings.TrimSpace(line))
		if len(m) == 3 {
			out = append(out, strings.TrimSpace(m[2]))
		}
	}
	return out, nil
}
func isTerminalStatus(status string) bool {
	return terminalTaskStatuses[strings.ToLower(strings.TrimSpace(status))]
}
func staleTasks(cfg Config, days int) ([]string, error) {
	b, err := os.ReadFile(filepath.Join(cfg.VaultDir, "live-tasks.md"))
	if err != nil {
		return nil, err
	}
	var stale []string
	cutoff := time.Now().AddDate(0, 0, -days)
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "| ") || strings.Contains(line, "Last touched") || strings.Contains(line, "---") {
			continue
		}
		cols := tableCols(line)
		if len(cols) < 8 {
			continue
		}
		// Status-aware (issue #19): a terminal-state row is closed work, not stale.
		if isTerminalStatus(cols[4]) {
			continue
		}
		t, err := time.Parse("2006-01-02", cols[7])
		if err == nil && t.Before(cutoff) {
			stale = append(stale, cols[0])
		}
	}
	return stale, nil
}

// markTaskDone flips the live-tasks.md row whose Task (col 0) equals name to a
// terminal Status and stamps today as the completion date (Last touched, col 7).
// The row is KEPT — it is the closed-record that makes completion resurrection-
// safe: syncTasks sees the still-present row and refuses to re-add the P0 item
// (issue #19), without needing a separate closed-task ledger. Returns the number
// of rows updated (0 => not found).
func markTaskDone(cfg Config, name string) (int, error) {
	livePath := filepath.Join(cfg.VaultDir, "live-tasks.md")
	b, err := os.ReadFile(livePath)
	if err != nil {
		return 0, err
	}
	name = strings.TrimSpace(name)
	today := time.Now().Format("2006-01-02")
	lines := strings.Split(string(b), "\n")
	updated := 0
	for i, line := range lines {
		if !strings.HasPrefix(line, "| ") || strings.Contains(line, "Last touched") || strings.Contains(line, "---") {
			continue
		}
		cols := tableCols(line)
		if len(cols) < 8 || cols[0] != name {
			continue
		}
		cols[4] = "done"
		cols[7] = today
		lines[i] = "| " + strings.Join(cols, " | ") + " |"
		updated++
	}
	if updated == 0 {
		return 0, nil
	}
	if err := atomicio.Write(livePath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return 0, err
	}
	return updated, nil
}

// listTasks parses live-tasks.md into rows (header/separator lines skipped).
func listTasks(cfg Config) ([]LiveTask, error) {
	b, err := os.ReadFile(filepath.Join(cfg.VaultDir, "live-tasks.md"))
	if err != nil {
		return nil, err
	}
	out := []LiveTask{}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "| ") || strings.Contains(line, "Last touched") || strings.Contains(line, "---") {
			continue
		}
		cols := tableCols(line)
		if len(cols) < 8 {
			continue
		}
		out = append(out, LiveTask{
			Task: cols[0], Domain: cols[1], Owner: cols[2], Pri: cols[3],
			Status: cols[4], Blocker: cols[5], Horizon: cols[6], LastTouched: cols[7],
		})
	}
	return out, nil
}

// addTask appends a queued live-task row. It is idempotent by Task name (the row
// identity, matching syncTasks's dedup): if a row with that name already exists
// it is a no-op and reports added=false, so a daily automation re-running the
// brief write-back never mints duplicates. Last touched is stamped today.
func addTask(cfg Config, lt LiveTask) (bool, error) {
	livePath := filepath.Join(cfg.VaultDir, "live-tasks.md")
	bodyBytes, err := os.ReadFile(livePath)
	if err != nil {
		return false, err
	}
	body := string(bodyBytes)
	// Idempotency by EXACT Task-name (col 0), not a substring scan of the whole
	// table — so a name that happens to appear in another row's Blocker/Horizon
	// cell, or that is a prefix of another task, does not falsely suppress the add.
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "| ") || strings.Contains(line, "Last touched") || strings.Contains(line, "---") {
			continue
		}
		if cols := tableCols(line); len(cols) >= 1 && cols[0] == lt.Task {
			return false, nil
		}
	}
	row := fmt.Sprintf("| %s | %s | %s | %s | queued | %s | %s | %s |",
		lt.Task, lt.Domain, lt.Owner, lt.Pri, lt.Blocker, lt.Horizon, time.Now().Format("2006-01-02"))
	body = strings.TrimRight(body, "\n") + "\n" + row + "\n"
	if err := atomicio.Write(livePath, []byte(body), 0o644); err != nil {
		return false, err
	}
	return true, nil
}
func tableCols(line string) []string {
	raw := strings.Split(strings.Trim(line, "|"), "|")
	out := make([]string, 0, len(raw))
	for _, c := range raw {
		out = append(out, strings.TrimSpace(c))
	}
	return out
}

// Sync reconciles P0 priorities into the live-task ledger.
func Sync(cfg Config, write bool) (int, error) { return syncTasks(cfg, write) }

// Stale returns open task names older than days.
func Stale(cfg Config, days int) ([]string, error) { return staleTasks(cfg, days) }

// MarkDone closes every row whose task name matches.
func MarkDone(cfg Config, name string) (int, error) { return markTaskDone(cfg, name) }

// List parses the task ledger.
func List(cfg Config) ([]LiveTask, error) { return listTasks(cfg) }

// Add appends a queued task unless its exact name already exists.
func Add(cfg Config, task LiveTask) (bool, error) { return addTask(cfg, task) }
