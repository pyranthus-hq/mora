package mora

// share_pin.go — Packet H1: the generation build input is IMMUTABLE and
// run-id-private, pinned BEFORE anything shared can move. Git pins the merge
// source into refs/mora/import/<run_id> and reads objects only from that ref;
// bucket materializes each fetch into a run-id-private fetch-<run_id> staging
// dir. A reaped holder mutating the shared working tree therefore cannot
// contaminate a successor's build. This file also orchestrates the decrypt →
// generation build → lease-fenced commit for both transports.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
)

// shareBlobEntry is one decrypted, validated share memory plus its plaintext
// bytes, ready to freeze into a generation's corpus.
type shareBlobEntry struct {
	mem  Memory
	body []byte
}

// shareBlobIter yields (stem, ciphertext) pairs from a pinned, immutable input.
type shareBlobIter func(yield func(stem string, ct []byte) error) error

// decryptShareBlobs runs the SAME untrusted-input validation the shipped import
// did (names, ids, scopes, sizes, case-fold, decrypt, per-memory cap), producing
// the validated memory set to freeze. Nothing is written to any served state.
func decryptShareBlobs(cfg Config, man shareManifest, iter shareBlobIter) ([]shareBlobEntry, error) {
	identities, err := loadShareIdentities(cfg)
	if err != nil {
		return nil, err
	}
	var out []shareBlobEntry
	foldSeen := map[string]string{}
	err = iter(func(stem string, ct []byte) error {
		if !shareExportIDRE.MatchString(stem) {
			return fmt.Errorf("share entry %q has an unsafe name — refusing to import", stem)
		}
		if prior, dup := foldSeen[strings.ToLower(stem)]; dup {
			return fmt.Errorf("share entries %s and %s differ only by letter case and cannot coexist — ask the publisher to rename one", prior, stem)
		}
		foldSeen[strings.ToLower(stem)] = stem
		if int64(len(ct)) > shareMaxMemoryBytes+(1<<20) {
			return fmt.Errorf("entry %s exceeds the %d-byte per-memory cap — refusing to import", stem, shareMaxMemoryBytes)
		}
		r, derr := age.Decrypt(bytes.NewReader(ct), identities...)
		if derr != nil {
			return fmt.Errorf("decrypting %s: %w (is your public key among this share's recipients?)", stem, derr)
		}
		plain, rerr := io.ReadAll(io.LimitReader(r, shareMaxMemoryBytes+1))
		if rerr != nil {
			return rerr
		}
		if len(plain) > shareMaxMemoryBytes {
			return fmt.Errorf("entry %s exceeds the %d-byte per-memory cap — refusing to import", stem, shareMaxMemoryBytes)
		}
		m, perr := parseMemoryBytes(filepath.Join("corpus", stem+".md"), plain)
		if perr != nil {
			return fmt.Errorf("entry %s does not parse as a memory: %v", stem, perr)
		}
		if m.ID != stem {
			return fmt.Errorf("entry %s: frontmatter id %q does not match — refusing spoofed ids", stem, m.ID)
		}
		if m.Scope != man.Scope {
			return fmt.Errorf("entry %s: scope %q differs from the manifest scope %q", stem, m.Scope, man.Scope)
		}
		if m.DeletedAt != "" {
			return nil // a tombstone: never enters the corpus
		}
		out = append(out, shareBlobEntry{mem: m, body: plain})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- git pin ----

// gitPin holds the run-private ref and the sha it resolved, with a compare-and-
// delete cleanup.
type gitPin struct {
	ref     string
	sha     string
	repo    string
	run     execFunc
	cleanup func()
}

// gitExitCode extracts a subprocess exit code from a (possibly wrapped) error.
func gitExitCode(err error) (int, bool) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), true
	}
	return 0, false
}

// acquireGitPin implements the H1 git pin: discover the merge source (read-only),
// fetch it into refs/mora/import/<run_id> with the exact non-mutating invocation,
// resolve the sha, and enforce the shipped non-fast-forward refusal via an
// explicit merge-base --is-ancestor gate. base is the published head's git
// SourceRev, or HEAD for the first import with no commit record.
func acquireGitPin(ctx context.Context, cfg Config, sub shareSubscription, runID string, run execFunc) (*gitPin, error) {
	repo := shareRepoDir(cfg, sub.Name)
	// Retain the shipped origin-identity check (read-only).
	origin, err := run(ctx, repo, "git", "remote", "get-url", "origin")
	if err != nil {
		return nil, fmt.Errorf("subscription %q has no usable origin remote: %w", sub.Name, err)
	}
	if sub.Remote != "" && strings.TrimSpace(origin) != sub.Remote {
		return nil, fmt.Errorf("subscription %q: clone origin (%s) does not match the registered remote (%s) — refusing to pull", sub.Name, redactCredentials(strings.TrimSpace(origin)), redactCredentials(sub.Remote))
	}
	// Discover the checked-out branch's configured merge source (read-only).
	symref, err := run(ctx, repo, "git", "symbolic-ref", "-q", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("subscription %q: HEAD is detached or unreadable — remove and re-subscribe: %w", sub.Name, err)
	}
	symrefTrim := strings.TrimSpace(symref)
	if !strings.HasPrefix(symrefTrim, "refs/heads/") {
		return nil, fmt.Errorf("subscription %q: HEAD %q is not a local branch — remove and re-subscribe", sub.Name, symrefTrim)
	}
	local := strings.TrimPrefix(symrefTrim, "refs/heads/")
	mergeRef, err := run(ctx, repo, "git", "config", "--get", "branch."+local+".merge")
	if err != nil {
		return nil, fmt.Errorf("subscription %q: branch %q has no configured merge source — remove and re-subscribe: %w", sub.Name, local, err)
	}
	mergeRefTrim := strings.TrimSpace(mergeRef)
	if !strings.HasPrefix(mergeRefTrim, "refs/heads/") {
		return nil, fmt.Errorf("subscription %q: merge source %q is malformed — remove and re-subscribe", sub.Name, mergeRefTrim)
	}
	remoteName, err := run(ctx, repo, "git", "config", "--get", "branch."+local+".remote")
	if err != nil || strings.TrimSpace(remoteName) != "origin" {
		return nil, fmt.Errorf("subscription %q: branch %q does not track origin — remove and re-subscribe", sub.Name, local)
	}

	// Capture the pre-fetch base for the first import (no committed SourceRev yet).
	base := ""
	if pc, ok, cerr := resolvePublishedCommit(cfg, sub.Name); cerr == nil && ok && pc.SourceRev != "" {
		base = pc.SourceRev
	} else {
		head, herr := run(ctx, repo, "git", "rev-parse", "--verify", "HEAD^{commit}")
		if herr != nil {
			return nil, fmt.Errorf("subscription %q: cannot resolve HEAD before pin: %w", sub.Name, herr)
		}
		base = strings.TrimSpace(head)
	}

	pinRef := "refs/mora/import/" + runID
	if _, err := run(ctx, repo, "git", "fetch", "--atomic", "--no-write-fetch-head", "--no-tags",
		"--no-auto-maintenance", "--refmap=", "origin", "+"+mergeRefTrim+":"+pinRef); err != nil {
		return nil, fmt.Errorf("subscription %q: pin fetch failed — has the publisher pushed? %w", sub.Name, err)
	}
	cleanup := func() {
		if sha := strings.TrimSpace(mustExec(ctx, repo, run, "git", "rev-parse", "--verify", "-q", pinRef)); sha != "" {
			_, _ = run(ctx, repo, "git", "update-ref", "-d", pinRef, sha)
		}
	}

	shaOut, err := run(ctx, repo, "git", "rev-parse", "--verify", pinRef+"^{commit}")
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("subscription %q: pinned ref did not resolve to a commit: %w", sub.Name, err)
	}
	sha := strings.TrimSpace(shaOut)

	// Preserve the non-fast-forward refusal: base must be an ancestor of the pin.
	if _, err := run(ctx, repo, "git", "merge-base", "--is-ancestor", base, pinRef); err != nil {
		if code, ok := gitExitCode(err); ok && code == 1 {
			cleanup()
			return nil, fmt.Errorf("subscription %q: the publisher rotated the share (history rewrite) — remove and re-subscribe", sub.Name)
		}
		cleanup()
		return nil, fmt.Errorf("subscription %q: ancestry check failed: %w", sub.Name, err)
	}
	return &gitPin{ref: pinRef, sha: sha, repo: repo, run: run, cleanup: cleanup}, nil
}

// mustExec returns the combined output of a git command, or "" on error (used for
// best-effort cleanup only).
func mustExec(ctx context.Context, dir string, run execFunc, name string, args ...string) string {
	out, err := run(ctx, dir, name, args...)
	if err != nil {
		return ""
	}
	return out
}

// gitPinManifest reads share.json from the pinned object store (never the
// working tree).
func (p *gitPin) manifest(ctx context.Context) (shareManifest, error) {
	out, err := p.run(ctx, p.repo, "git", "cat-file", "blob", p.ref+":share.json")
	if err != nil {
		return shareManifest{}, fmt.Errorf("pinned share repo has no readable share.json: %w", err)
	}
	return parseShareManifestBytes([]byte(out))
}

// gitPinBlobs iterates memories/*.md.age blobs from the pinned ref only.
func (p *gitPin) blobs(ctx context.Context) shareBlobIter {
	return func(yield func(stem string, ct []byte) error) error {
		list, err := p.run(ctx, p.repo, "git", "ls-tree", "-r", "--name-only", p.ref, "--", "memories/")
		if err != nil {
			return err
		}
		for _, line := range strings.Split(strings.TrimSpace(list), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasSuffix(line, ".md.age") {
				continue
			}
			stem := strings.TrimSuffix(filepath.Base(line), ".md.age")
			ct, cerr := p.run(ctx, p.repo, "git", "cat-file", "blob", p.ref+":"+line)
			if cerr != nil {
				return cerr
			}
			if err := yield(stem, []byte(ct)); err != nil {
				return err
			}
		}
		return nil
	}
}

// bucketDirBlobs iterates memories/*.md.age files from a run-private fetch dir.
func bucketDirBlobs(fetchDir string) shareBlobIter {
	return func(yield func(stem string, ct []byte) error) error {
		memDir := filepath.Join(fetchDir, "memories")
		entries, err := os.ReadDir(memDir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".md.age") {
				continue
			}
			ct, rerr := os.ReadFile(filepath.Join(memDir, e.Name()))
			if rerr != nil {
				return rerr
			}
			if err := yield(strings.TrimSuffix(e.Name(), ".md.age"), ct); err != nil {
				return err
			}
		}
		return nil
	}
}

// buildAndCommitGeneration is the shared tail for both transports: build the
// immutable generation from the decrypted entries, then publish it via the
// lease-fenced atomic seq-claim. Returns the committed seq and stats.
func buildAndCommitGeneration(ctx context.Context, cfg Config, sub shareSubscription, runID, sourceRev string, entries []shareBlobEntry, cp shareCommitParams) (int, shareImportStats, error) {
	// Whole-product admission (H3b): reject before building if this generation
	// would push the footprint over the configured limit (default 15 GiB). The
	// storage lease held by shareBuildAndPublish serializes this snapshot.
	if aerr := admitShareGeneration(cfg, sub.Name, entries); aerr != nil {
		return 0, shareImportStats{}, aerr
	}

	gen := "gen-" + runID
	corpusDigest, indexDigest, err := buildShareGenerationFromEntries(ctx, cfg, sub.Name, gen, entries)
	if err != nil {
		return 0, shareImportStats{}, err
	}
	cp.name = sub.Name
	cp.runID = runID
	cp.gen = gen
	cp.sourceRev = sourceRev
	cp.corpusDigest = corpusDigest
	cp.indexDigest = indexDigest
	cp.count = len(entries)
	cp.builtAt = time.Now()
	seq, err := publishShareGeneration(cfg, cp)
	if err != nil {
		return 0, shareImportStats{}, err
	}
	// Migration + legacy retirement: the durable one-way `migrated` latch is
	// written AFTER the commit (crash before commit ⇒ still fail-closed; crash
	// after commit before latch ⇒ commits/ is authoritative and wins).
	if err := writeMigratedLatch(cfg, sub.Name); err != nil {
		// The generation is already committed and therefore safe to serve, but
		// the migration cannot be reported successful until the one-way latch is
		// durable. The outer attempt lifecycle records this as a failed refresh.
		return seq, shareImportStats{}, err
	}
	return seq, shareImportStats{Imported: len(entries), Total: len(entries)}, nil
}

// writeMigratedLatch installs the durable one-way migrated latch (present ⇒
// legacy fallback permanently OFF) and best-effort retires the legacy flat
// layout. Idempotent.
func writeMigratedLatch(cfg Config, name string) error {
	_, statErr := os.Stat(shareMigratedLatchPath(cfg, name))
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("share %q: checking migrated latch: %w", name, statErr)
	}
	if errors.Is(statErr, os.ErrNotExist) {
		if err := atomicio.WriteDurable(shareMigratedLatchPath(cfg, name), []byte("1\n"), 0o644); err != nil {
			return fmt.Errorf("share %q: persisting migrated latch: %w", name, err)
		}
	} else if err := atomicio.SyncDir(filepath.Dir(shareMigratedLatchPath(cfg, name))); err != nil {
		// A prior call may have synced the latch bytes and renamed it, then failed
		// its directory barrier. Retry that last barrier before retiring legacy.
		return fmt.Errorf("share %q: making existing migrated latch durable: %w", name, err)
	}
	// Retire the untrusted legacy flat corpus/index only after the latch is durable.
	// Repeat this best-effort cleanup when the latch already exists so a crash
	// between latch publication and retirement is self-healing on the next pull.
	_ = os.RemoveAll(shareCorpusDir(cfg, name))
	for _, p := range []string{shareIndexPath(cfg, name), shareIndexPath(cfg, name) + "-wal", shareIndexPath(cfg, name) + "-shm"} {
		_ = os.Remove(p)
	}
	return nil
}

// gitShareImport pins the merge source into a run-private ref, decrypts from that
// immutable object store, builds a generation, and commits it. Runs under the
// import lease held by shareBuildAndPublish.
func gitShareImport(ctx context.Context, cfg Config, sub shareSubscription, runID string, run execFunc) (int, shareImportStats, error) {
	pin, err := acquireGitPin(ctx, cfg, sub, runID, run)
	if err != nil {
		return 0, shareImportStats{}, err
	}
	defer pin.cleanup()
	man, err := pin.manifest(ctx)
	if err != nil {
		return 0, shareImportStats{}, err
	}
	entries, err := decryptShareBlobs(cfg, man, pin.blobs(ctx))
	if err != nil {
		return 0, shareImportStats{}, err
	}
	seq, stats, err := buildAndCommitGeneration(ctx, cfg, sub, runID, pin.sha, entries, shareCommitParams{parentFloor: -1})
	stats.Owner, stats.Scope = man.Owner, man.Scope
	return seq, stats, err
}

// bucketShareImport materializes each fetch into a run-private fetch-<run_id>
// staging dir, decrypts, builds a generation, and commits it with the inherited
// monotonic bucket floor. Returns the pin + version to persist on success.
func bucketShareImport(ctx context.Context, cfg Config, sub shareSubscription, bc bucketConfig, runID string) (int, shareImportStats, ed25519.PublicKey, int, error) {
	store, err := newObjectStore(bc)
	if err != nil {
		return 0, shareImportStats{}, nil, 0, err
	}
	return bucketShareImportWithStore(ctx, cfg, sub, bc, runID, store)
}

func bucketShareImportWithStore(ctx context.Context, cfg Config, sub shareSubscription, bc bucketConfig, runID string, store objectStore) (int, shareImportStats, ed25519.PublicKey, int, error) {
	ids, err := loadShareIdentities(cfg)
	if err != nil {
		return 0, shareImportStats{}, nil, 0, err
	}
	dest := shareFetchDir(cfg, sub.Name, runID)
	_ = os.RemoveAll(dest)
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return 0, shareImportStats{}, nil, 0, err
	}
	// Normal returns never retain run-private network staging. A SIGKILL can
	// still strand it for manual GC, but an admission refusal followed by the
	// printed storage-limit retry must not accumulate fetch-N, fetch-N+1, ... .
	defer os.RemoveAll(dest)
	pin, ver, err := bucketFetch(ctx, store, bc, sub, ids, dest)
	if err != nil {
		return 0, shareImportStats{}, nil, 0, err
	}
	man, err := readShareManifest(dest)
	if err != nil {
		return 0, shareImportStats{}, nil, 0, err
	}
	entries, err := decryptShareBlobs(cfg, man, bucketDirBlobs(dest))
	if err != nil {
		return 0, shareImportStats{}, nil, 0, err
	}
	seq, stats, err := buildAndCommitGeneration(ctx, cfg, sub, runID, fmt.Sprintf("bucket-v%d", ver), entries,
		shareCommitParams{isBucket: true, fetched: ver, subVersion: sub.LastVersion, parentFloor: -1})
	if err != nil {
		return 0, shareImportStats{}, nil, 0, err
	}
	stats.Owner, stats.Scope = man.Owner, man.Scope
	return seq, stats, pin, ver, nil
}
