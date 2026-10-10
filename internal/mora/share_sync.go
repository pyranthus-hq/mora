package mora

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/leasefile"
)

const shareSyncUsage = "usage: mora share sync"

// shareSyncPush and shareSyncPull are the verbs one sync drives. Tests replace
// them to observe the orchestration without a git remote.
var (
	shareSyncPush = sharePush
	shareSyncPull = sharePull
)

// shareSyncLockPath is the per-installation lock that stops a slow sync from
// overlapping the next scheduled fire or a hand-run sync. It is run state, so it
// lives in the state dir, never the vault.
func shareSyncLockPath(cfg Config) string { return filepath.Join(cfg.StateDir, "share-sync.lock") }

// shareSync is `mora share sync`, the body of the share-sync scheduled job. It
// pulls every subscription and then pushes every publish, writing one
// timestamped line per share. A failed share is logged and the rest still run;
// the command then exits non-zero so the scheduler records the failure rather
// than a green run over a stale share.
func shareSync(ctx context.Context, cfg Config, args []string, stdout io.Writer, run execFunc) error {
	if len(args) > 0 {
		return errors.New(shareSyncUsage)
	}
	now := func() string { return time.Now().UTC().Format(time.RFC3339) }

	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(shareSyncLockPath(cfg), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := leasefile.TryLock(lock); err != nil {
		fmt.Fprintf(stdout, "%s share sync: skipped: another share sync is running (%s)\n", now(), shareSyncLockPath(cfg))
		return nil
	}
	defer func() { _ = leasefile.Unlock(lock) }()

	sf, err := loadShares(cfg)
	if err != nil {
		return err
	}
	total := len(sf.Subscriptions) + len(sf.Publishes)
	if total == 0 {
		fmt.Fprintf(stdout, "%s share sync: no publishes or subscriptions\n", now())
		return nil
	}
	failed := 0
	logResult := func(verb, name string, out *bytes.Buffer, err error) {
		if err != nil {
			failed++
			fmt.Fprintf(stdout, "%s %s %s: error: %s\n", now(), verb, name, shareSyncOneLine(err.Error()))
			return
		}
		fmt.Fprintf(stdout, "%s %s %s: ok: %s\n", now(), verb, name, shareSyncLastLine(out.String()))
	}
	for _, sub := range sf.Subscriptions {
		var out bytes.Buffer
		err := shareSyncPull(ctx, cfg, []string{sub.Name}, &out, run)
		logResult("pull", sub.Name, &out, err)
	}
	for _, pub := range sf.Publishes {
		var out bytes.Buffer
		err := shareSyncPush(ctx, cfg, []string{pub.Name, "--yes"}, &out, strings.NewReader(""), run)
		logResult("push", pub.Name, &out, err)
	}
	if failed > 0 {
		return fmt.Errorf("share sync: %d of %d shares failed (see the error lines above)", failed, total)
	}
	return nil
}

// shareSyncOneLine folds a multi-line message (git errors carry their output) onto one
// line so each share stays exactly one log line.
func shareSyncOneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func shareSyncLastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if last := strings.TrimSpace(lines[len(lines)-1]); last != "" {
		return last
	}
	return "done"
}
