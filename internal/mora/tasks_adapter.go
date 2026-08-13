package mora

import taskstore "github.com/pyranthus-hq/mora/internal/tasks"

type LiveTask = taskstore.LiveTask

func taskConfig(cfg Config) taskstore.Config        { return taskstore.Config{VaultDir: cfg.VaultDir} }
func syncTasks(cfg Config, write bool) (int, error) { return taskstore.Sync(taskConfig(cfg), write) }
func staleTasks(cfg Config, days int) ([]string, error) {
	return taskstore.Stale(taskConfig(cfg), days)
}
func markTaskDone(cfg Config, name string) (int, error) {
	return taskstore.MarkDone(taskConfig(cfg), name)
}
func listTasks(cfg Config) ([]LiveTask, error)        { return taskstore.List(taskConfig(cfg)) }
func addTask(cfg Config, task LiveTask) (bool, error) { return taskstore.Add(taskConfig(cfg), task) }
