package index

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/memory"
)

// ErrUnmarkable means the crash-durable marker could not be published; callers must abort the vault mutation.
var ErrUnmarkable = errors.New("index cannot be marked dirty")

const (
	KindWrite   = "write"
	KindDelete  = "delete"
	KindRebuild = "rebuild"
)

// MarkSeams supplies readiness, identity, and deterministic clock/test hooks.
type MarkSeams struct {
	Ready     func(context.Context, config.Config) (bool, string, error)
	NewID     func() string
	Clock     func() time.Time
	PostWrite func()
}

type PendingOp struct {
	OpID     string `json:"op_id"`
	Kind     string `json:"kind"`                // write | delete | rebuild
	Path     string `json:"path,omitempty"`      // filepath.Clean'd absolute vault path ("" for rebuild)
	MemoryID string `json:"memory_id,omitempty"` // delete -> the read-path suppression list (B4)
	MarkedAt string `json:"marked_at"`           // RFC3339
}

func PendingDir(cfg config.Config) string { return filepath.Join(cfg.StateDir, "pending") }

func PendingPath(cfg config.Config, opID string) string {
	return filepath.Join(PendingDir(cfg), opID+".json")
}

func CleanVaultPath(p string) string {
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}

func ListPending(cfg config.Config) ([]PendingOp, error) {
	entries, err := os.ReadDir(PendingDir(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ops []PendingOp
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		b, rerr := os.ReadFile(filepath.Join(PendingDir(cfg), e.Name()))
		if errors.Is(rerr, os.ErrNotExist) {
			continue // a concurrent committed writer retired it after ReadDir
		}
		if rerr != nil {
			return nil, rerr
		}
		var op PendingOp
		if json.Unmarshal(b, &op) != nil || op.OpID == "" {
			op = PendingOp{OpID: id} // corrupt: fail closed, no valid kind/path
		}
		ops = append(ops, op)
	}
	return ops, nil
}

func PendingDeleteIDs(cfg config.Config) map[string]bool {
	ops, err := ListPending(cfg)
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	for _, op := range ops {
		if op.Kind == KindDelete && op.MemoryID != "" {
			out[op.MemoryID] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func ShouldClear(op PendingOp, listingStartedAt time.Time, files, parsed map[string]bool) bool {
	switch op.Kind {
	case KindRebuild:
		// (a) any rebuild that LISTED after this op was marked covers it — NOT
		// only its own op_id, or a SIGKILLed rebuild's op is unclearable forever.
		t, perr := time.Parse(time.RFC3339, op.MarkedAt)
		if perr != nil {
			return true // an unparseable rebuild stamp is garbage; reap it
		}
		return !t.After(listingStartedAt)
	case KindWrite:
		// (b) parsed, NOT listed: a truncated/hand-mangled file is listed yet not
		// indexed, and must stay dirty.
		return op.Path != "" && parsed[op.Path]
	case KindDelete:
		// (c) gone OR re-ingested (a connector rewrote the memory onto its own
		// stable path, so it legitimately reappears in parsed).
		return op.Path != "" && (!files[op.Path] || parsed[op.Path])
	default:
		// Corrupt/unknown op: fail-closed while present, but a committed rebuild
		// is allowed to reap it so a permanent red banner never becomes wallpaper.
		return true
	}
}

func CleanPathSet(paths []string) map[string]bool {
	s := make(map[string]bool, len(paths))
	for _, p := range paths {
		s[CleanVaultPath(p)] = true
	}
	return s
}

func MarkDirty(ctx context.Context, cfg config.Config, op PendingOp, seams MarkSeams) (PendingOp, error) {
	if op.OpID == "" {
		op.OpID = seams.NewID()
	}
	if op.MarkedAt == "" {
		op.MarkedAt = seams.Clock().UTC().Format(time.RFC3339)
	}
	op.Path = CleanVaultPath(op.Path)

	ready, _, rerr := seams.Ready(ctx, cfg)
	if rerr != nil {
		// The readiness probe itself faulted (unreadable/locked index). That is a
		// state indexHealthOf already reports as failed (worse than dirty); do not
		// fail the mutation over it — treat as not-ready and skip the mark.
		return op, nil
	}
	if !ready {
		return op, nil
	}
	body, err := json.Marshal(op)
	if err != nil {
		return op, err
	}
	if err := atomicio.WriteDurable(PendingPath(cfg, op.OpID), body, 0o644); err != nil {
		return op, fmt.Errorf("%w: %v", ErrUnmarkable, err)
	}
	if seams.PostWrite != nil {
		seams.PostWrite()
	}
	return op, nil
}

func ClearCovered(cfg config.Config, listingStartedAt time.Time, files, parsed []string, unmark func(config.Config, string) error) error {
	filesSet := CleanPathSet(files)
	parsedSet := CleanPathSet(parsed)
	ops, err := ListPending(cfg)
	if err != nil {
		return err
	}
	var clearErr error
	for _, op := range ops {
		if ShouldClear(op, listingStartedAt, filesSet, parsedSet) {
			if err := unmark(cfg, op.OpID); err != nil {
				clearErr = errors.Join(clearErr, fmt.Errorf("retiring pending operation: %w", err))
			}
		}
	}
	return clearErr
}

func SuppressPendingDeletes(cfg config.Config, mems []memory.Memory) []memory.Memory {
	sup := PendingDeleteIDs(cfg)
	if sup == nil {
		return mems
	}
	out := make([]memory.Memory, 0, len(mems))
	for _, m := range mems {
		if sup[m.ID] {
			continue
		}
		out = append(out, m)
	}
	return out
}
