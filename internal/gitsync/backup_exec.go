package gitsync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var errBackupOutputLimit = errors.New("backup Git output exceeded its bound")

type backupBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *backupBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		return 0, errBackupOutputLimit
	}
	return b.buffer.Write(p)
}

func selectedCommand(ctx context.Context, opts BackupOptions, limit int, args ...string) ([]byte, error) {
	if opts.run != nil {
		return opts.run(ctx, opts.Vault, limit, args...)
	}
	hooks := filepath.Join(opts.StateDir, "backup", "hooks-disabled")
	if err := safeBackupDirectory(hooks); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(hooks, 0700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(hooks)
	if err != nil || len(entries) != 0 {
		return nil, errors.New("disabled hook directory is not empty")
	}
	prefix := []string{"--git-dir=" + filepath.Join(opts.Vault, ".git"), "--work-tree=" + opts.Vault,
		"-c", "core.hooksPath=" + hooks, "-c", "core.fsmonitor=false", "-c", "commit.gpgsign=false",
		"-c", "core.autocrlf=false", "-c", "core.safecrlf=false", "-c", "transfer.fsckObjects=true",
		"-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "protocol.ext.allow=never", "-c", "fetch.recurseSubmodules=false"}
	for _, name := range opts.disabledFilters {
		prefix = append(prefix, "-c", "filter."+name+".clean=", "-c", "filter."+name+".smudge=", "-c", "filter."+name+".process=", "-c", "filter."+name+".required=false")
	}
	if !opts.testTransport {
		prefix = append(prefix, "-c", "protocol.file.allow=never")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append(prefix, args...)...)
	cmd.Dir = opts.Vault
	cmd.WaitDelay = 2 * time.Second
	for _, value := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(value), "GIT_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
	stdout, stderr := &backupBuffer{limit: limit}, &backupBuffer{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("backup Git command failed: %w", err)
	}
	return stdout.buffer.Bytes(), nil
}

func absentGitConfig(err error) bool {
	var status *exec.ExitError
	return errors.As(err, &status) && status.ExitCode() == 1
}
