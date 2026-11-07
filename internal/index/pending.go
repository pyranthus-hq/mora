package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// RunID ties a rebuild's own self-op to its operation receipt, so health can
	// tell a live rebuild's marker from one a failed rebuild left behind.
	RunID string `json:"run_id,omitempty"`
	// ContentSHA256 is the hash of the exact bytes a write publishes or a delete
	// removes. A marker without one came from an older binary.
	ContentSHA256 string `json:"content_sha256,omitempty"`
	// OwnerPID names the process that made the marker, so a rebuild can tell a
	// write still in flight from one whose owner died.
	OwnerPID int `json:"owner_pid,omitempty"`
	// FTSCommitted is derived from the marker's file name, never stored in it: a
	// write marker renamed to <op_id>.fts.json after its FTS upsert committed.
	// The marker still pins the graph, vector and commitment projections until a
	// covering full rebuild retires it.
	FTSCommitted bool `json:"-"`
	// Unlinked is derived the same way, for a delete marker: the deleter finished
	// removing the file. Older binaries already list and retire that file name,
	// which a new suffix would strand.
	Unlinked bool `json:"-"`
}

// ftsCommittedSuffix names a write marker whose FTS row has committed. The
// transition is one rename, so it cannot resurrect a marker a concurrent
// rebuild already retired: a rename of a missing file fails instead.
const ftsCommittedSuffix = ".fts.json"

func PendingDir(cfg config.Config) string { return filepath.Join(cfg.StateDir, "pending") }

func PendingPath(cfg config.Config, opID string) string {
	return filepath.Join(PendingDir(cfg), opID+".json")
}

// FTSCommittedPath is where a write marker lives after its FTS upsert committed.
func FTSCommittedPath(cfg config.Config, opID string) string {
	return filepath.Join(PendingDir(cfg), opID+ftsCommittedSuffix)
}

// MarkFTSCommitted records that a write's FTS upsert committed by renaming its
// marker. It never retires the marker: graph, vectors and commitments still lag
// until a covering rebuild. A marker that is already gone was covered by a
// concurrent rebuild, which is success. Any other failure leaves the marker in
// its dirty form, which is the safe direction.
func MarkFTSCommitted(cfg config.Config, opID string, rename func(oldPath, newPath string) error) error {
	if opID == "" {
		return nil
	}
	err := rename(PendingPath(cfg, opID), FTSCommittedPath(cfg, opID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// Best-effort: losing the rename on power loss only reverts the marker to
	// its dirty form.
	_ = atomicio.SyncDir(PendingDir(cfg))
	return nil
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
		ftsCommitted := strings.HasSuffix(e.Name(), ftsCommittedSuffix)
		id := strings.TrimSuffix(e.Name(), ".json")
		if ftsCommitted {
			id = strings.TrimSuffix(e.Name(), ftsCommittedSuffix)
		}
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
		// Only write and delete markers have a second form. Any other kind under
		// that name is not a state Mora writes, so it stays dirty.
		op.FTSCommitted = ftsCommitted && op.Kind == KindWrite && op.OpID == id
		op.Unlinked = ftsCommitted && op.Kind == KindDelete && op.OpID == id
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

// Coverage is what one committed rebuild proved it indexed.
type Coverage struct {
	ListingStartedAt time.Time
	Files            map[string]bool   // listed paths
	Parsed           map[string]bool   // parsed and visible: the rule for hashless markers
	ParsedSHA256     map[string]string // every parsed path, governance-hidden ones too
	OwnerAlive       func(int) bool
	Abandoned        func(PendingOp) // told when a retired delete brings its memory back
}

// ContentSHA256 is the hash a marker stores and a rebuild compares.
func ContentSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ShouldClear decides one marker from the rebuild's snapshot. No rule uses age
// alone: a paused writer and an unparseable file both look old.
func ShouldClear(op PendingOp, c Coverage) bool {
	clear, _ := decide(op, c)
	return clear
}

// decide also reports a clear that abandons a delete, so its memory comes back.
func decide(op PendingOp, c Coverage) (clear, abandoned bool) {
	switch op.Kind {
	case KindRebuild:
		// (a) any rebuild that LISTED after this op was marked covers it — NOT
		// only its own op_id, or a SIGKILLed rebuild's op is unclearable forever.
		t, perr := time.Parse(time.RFC3339, op.MarkedAt)
		if perr != nil {
			return true, false // an unparseable rebuild stamp is garbage; reap it
		}
		return !t.After(c.ListingStartedAt), false
	case KindWrite:
		if op.ContentSHA256 == "" {
			// (b) parsed, NOT listed: a truncated/hand-mangled file is listed yet
			// not indexed, and must stay dirty.
			return op.Path != "" && c.Parsed[op.Path], false
		}
		return writeCovered(op, c), false
	case KindDelete:
		if op.ContentSHA256 == "" {
			// (c) gone OR re-ingested (a connector rewrote the memory onto its own
			// stable path, so it legitimately reappears in parsed).
			return op.Path != "" && (!c.Files[op.Path] || c.Parsed[op.Path]), false
		}
		return deleteCovered(op, c)
	default:
		// Corrupt/unknown op: fail-closed while present, but a committed rebuild
		// is allowed to reap it so a permanent red banner never becomes wallpaper.
		return true, false
	}
}

// deleteCovered applies only after the unlink finished or the owner died. A live
// deleter can still remove whatever this rebuild indexed, and only the marker
// would then suppress it. op.Unlinked comes from the snapshot, so that unlink
// came before listing.
func deleteCovered(op PendingOp, c Coverage) (clear, abandoned bool) {
	if op.Path == "" {
		return false, false
	}
	// Read the owner before the disk: a process seen dead can remove nothing
	// after the read below, so that read is final.
	dead := op.OwnerPID > 0 && !c.OwnerAlive(op.OwnerPID)
	if !op.Unlinked && !dead {
		return false, false
	}
	if !c.Files[op.Path] {
		return true, false
	}
	got, parsed := c.ParsedSHA256[op.Path]
	if !parsed {
		return false, false
	}
	if op.Unlinked && got != op.ContentSHA256 {
		return true, false // a connector re-ingested it after the unlink
	}
	if !dead {
		return false, false
	}
	// The owner may have removed the file after this rebuild parsed it, so the
	// indexed bytes count only while they are still the bytes on disk.
	b, err := os.ReadFile(op.Path)
	if err != nil || ContentSHA256(b) != got {
		return false, false
	}
	return true, got == op.ContentSHA256
}

func writeCovered(op PendingOp, c Coverage) bool {
	if op.Path == "" {
		return false
	}
	got, parsed := c.ParsedSHA256[op.Path]
	if parsed && got == op.ContentSHA256 {
		return true
	}
	if c.Files[op.Path] && !parsed {
		return false
	}
	// Read the owner before the disk: a process seen dead can publish nothing
	// after the read below, so that read is final.
	dead := op.OwnerPID > 0 && !c.OwnerAlive(op.OwnerPID)
	b, err := os.ReadFile(op.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false // cannot prove the write is not on disk
	}
	if err == nil && ContentSHA256(b) == op.ContentSHA256 {
		return false // landed after parsing
	}
	return dead
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
	if op.OwnerPID == 0 {
		op.OwnerPID = os.Getpid()
	}

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

// ClearCovered retires only ops, the snapshot taken before the rebuild listed
// the vault. A marker made later says nothing this rebuild can have covered.
func ClearCovered(cfg config.Config, ops []PendingOp, c Coverage, unmark func(config.Config, string) error) error {
	var clearErr error
	for _, op := range ops {
		clear, abandoned := decide(op, c)
		if clear || foreignToVault(cfg, op) {
			if err := unmark(cfg, op.OpID); err != nil {
				clearErr = errors.Join(clearErr, fmt.Errorf("retiring pending operation: %w", err))
			} else if abandoned && c.Abandoned != nil {
				c.Abandoned(op)
			}
		}
	}
	return clearErr
}

// foreignToVault reports a write or delete marker whose path lies outside this
// vault. No rebuild ever lists such a path, so without this rule the marker pins
// the index dirty forever (seen live: a run with MORA_VAULT on a temp vault but
// state_dir on the real one). Containment is decided by file identity on every
// existing ancestor, so symlinked or case-variant spellings of the vault, and a
// parent directory a concurrent writer has not created yet, all stay ours.
func foreignToVault(cfg config.Config, op PendingOp) bool {
	if (op.Kind != KindWrite && op.Kind != KindDelete) || op.Path == "" || cfg.VaultDir == "" {
		return false
	}
	vault, err := os.Stat(cfg.VaultDir)
	if err != nil {
		return false // cannot identify our own vault: fail closed, keep the marker
	}
	for dir := filepath.Dir(CleanVaultPath(op.Path)); ; dir = filepath.Dir(dir) {
		if fi, err := os.Stat(dir); err == nil && os.SameFile(fi, vault) {
			return false
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false // unreadable ancestor: cannot prove foreign
		}
		if parent := filepath.Dir(dir); parent == dir {
			return true
		}
	}
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
