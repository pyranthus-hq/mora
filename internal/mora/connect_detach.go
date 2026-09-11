package mora

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type connectStartedReceipt struct {
	Source       string `json:"source"`
	PID          int    `json:"pid"`
	ProgressPath string `json:"progress_path"`
	StartedAt    string `json:"started_at"`
}

func detachArgs(args []string) []string {
	out := make([]string, 0, len(args)+1)
	for _, a := range args {
		if a != "--detach" {
			out = append(out, a)
		}
	}
	return append(out, "--mora-detached-child")
}

func validateDetachedConnect(args []string) error {
	fs := flag.NewFlagSet("connect imessage", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Int("since-days", 0, "")
	if err := fs.Parse(args); err != nil {
		return newCodedError(errCodeUsageUnknownFlag, err, "%v", err)
	}
	if fs.NArg() != 0 {
		return newCodedError(errCodeUsageUnknownValue, nil, "unexpected connect argument %q", fs.Arg(0))
	}
	return nil
}

func readConnectProgress(path string) (connectProgressFile, error) {
	var p connectProgressFile
	b, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, err
	}
	if p.Schema != schemaConnectProgress || p.SchemaVersion != 1 || p.PID <= 0 {
		return p, fmt.Errorf("invalid connect progress file %s", path)
	}
	if _, err := time.Parse(time.RFC3339Nano, p.StartedAt); err != nil {
		return p, err
	}
	if _, err := time.Parse(time.RFC3339Nano, p.UpdatedAt); err != nil {
		return p, err
	}
	return p, nil
}

func staleConnectProgress(p connectProgressFile, now time.Time) bool {
	updated, err := time.Parse(time.RFC3339Nano, p.UpdatedAt)
	return err == nil && now.Sub(updated) > 30*time.Second && !connectPIDAlive(p.PID)
}

func cleanupConnectProgress(cfg Config) error {
	paths, err := filepath.Glob(filepath.Join(cfg.StateDir, "sync", "*.progress.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		p, err := readConnectProgress(path)
		if err == nil && staleConnectProgress(p, time.Now()) {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// Reserve before the atomic ticker writes, so another connect cannot replace
// an active reader's progress or remove it on exit.
func (s *connectProgressSink) reserveFile(cfg Config, source string) error {
	path := connectProgressPath(cfg, source)
	p := connectProgressFile{Schema: schemaConnectProgress, SchemaVersion: 1, Source: source, PID: os.Getpid(),
		StartedAt: s.start.UTC().Format(time.RFC3339Nano), UpdatedAt: s.start.UTC().Format(time.RFC3339Nano), Phase: "checking_access"}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			// Not connector.unavailable: the app maps that to a Full Disk Access card,
			// and a busy reader is a retry, not a permission problem.
			return newCodedError(errCodeConnectorUnclassified, err, "connect progress is already reserved; retry with --json --progress --detach to attach to a live read")
		}
		return fmt.Errorf("reserve connect progress: %w", err)
	}
	// Owning the reservation makes this a new run. Clear its predecessor's
	// receipt before publishing progress, so consumers cannot mistake it for completion.
	if err := os.Remove(strings.TrimSuffix(path, ".progress.json") + ".receipt.json"); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	_, werr := f.Write(b)
	err = errors.Join(werr, f.Close())
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	s.filePath, s.source = path, source
	return nil
}

// spawnDetached deliberately has no CommandContext: ending the parent's
// context must not kill a successfully detached read. Wait reaps in embedders.
func spawnDetached(ctx context.Context, cfg Config, source, exe string, args []string) (connectStartedReceipt, error) {
	var r connectStartedReceipt
	path := connectProgressPath(cfg, source)
	if p, err := readConnectProgress(path); err == nil && p.Source == source &&
		!staleConnectProgress(p, time.Now()) && connectPIDAlive(p.PID) {
		return connectStartedReceipt{source, p.PID, path, p.StartedAt}, nil
	}
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return r, err
	}
	defer null.Close()
	cmd := exec.Command(exe, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, null
	configureDetached(cmd)
	if err := cmd.Start(); err != nil {
		return r, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	check := func() bool {
		p, err := readConnectProgress(path)
		if err == nil && p.PID == cmd.Process.Pid && p.Source == source {
			r = connectStartedReceipt{source, p.PID, path, p.StartedAt}
			return true
		}
		// A short read may finish between polls. Its persisted receipt proves the
		// same child completed, rather than mistaking an older run for this one.
		b, err := os.ReadFile(strings.TrimSuffix(path, ".progress.json") + ".receipt.json")
		var completed struct {
			PID       int    `json:"pid"`
			StartedAt string `json:"started_at"`
		}
		if err == nil && json.Unmarshal(b, &completed) == nil && completed.PID == cmd.Process.Pid && completed.StartedAt != "" {
			r = connectStartedReceipt{source, completed.PID, path, completed.StartedAt}
			return true
		}
		return false
	}
	for {
		select {
		case <-ticker.C:
			if check() {
				return r, nil
			}
		case err := <-done:
			if check() {
				return r, nil
			}
			return r, fmt.Errorf("detached connect exited before publishing progress: %v", err)
		case <-timer.C:
			_ = terminateDetached(cmd.Process)
			return r, errors.New("detached connect did not publish progress within 5s")
		case <-ctx.Done():
			_ = terminateDetached(cmd.Process)
			return r, ctx.Err()
		}
	}
}

type connectReadInFlight struct {
	Source       string `json:"source"`
	PID          int    `json:"pid"`
	StartedAt    string `json:"started_at"`
	MessagesRead int    `json:"messages_read"`
	Chats        int    `json:"chats"`
	ElapsedMs    int64  `json:"elapsed_ms"`
	UpdatedAt    string `json:"updated_at"`
}

func connectReadsInFlight(cfg Config, now time.Time) []connectReadInFlight {
	rows := []connectReadInFlight{}
	paths, _ := filepath.Glob(filepath.Join(cfg.StateDir, "sync", "*.progress.json"))
	for _, path := range paths {
		p, err := readConnectProgress(path)
		if err != nil || staleConnectProgress(p, now) {
			continue
		}
		rows = append(rows, connectReadInFlight{p.Source, p.PID, p.StartedAt, p.MessagesRead, p.Chats, p.ElapsedMs, p.UpdatedAt})
	}
	return rows
}
