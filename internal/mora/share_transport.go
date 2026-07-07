package mora

// Transport seam for `mora share`.
//
// v1 hardwired publishing to a private git remote: the push path wrote
// `memories/<id>.md.age` into a staging worktree, ran a `git add -A` +
// `ls-files` allowlist, then committed and pushed. Everything ABOVE that — scope
// collection, change diffing, and age encryption — is backend-neutral and never
// names git.
//
// This file names that boundary as a `shareTransport` interface so a second
// delivery path (a user-owned S3/R2 bucket, Phase 3) can be added WITHOUT a
// parallel push path. Phase 1 introduces the seam for the publish side only and
// relocates v1's git logic verbatim into gitTransport — same command sequence,
// same messages, zero behavior change (proven by the existing share tests). The
// fetch side (clone/pull → import) keeps its caller-specific wording for now and
// moves behind the seam in Phase 3 alongside the bucket backend.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// shareTransport durably moves a share's ciphertext to its destination. The
// payload is opaque to it: the neutral core encrypts before calling publish, so
// a transport only ever handles ciphertext.
type shareTransport interface {
	// publish durably writes the changed ciphertext set and removes stale
	// entries, preserving the backend's atomicity (git: one commit + push). It is
	// the ONLY place a backend's write semantics live.
	publish(ctx context.Context, set shareSet) error
}

// shareSet is one publish's payload: staged base names ("<id>.md.age") whose
// ciphertext to (over)write, and staged base names to remove. Ciphertext is
// produced by the neutral core (encryptShareBytes) before it reaches here.
type shareSet struct {
	put    map[string][]byte
	remove []string
}

// gitTransport publishes a share over a private git remote — v1's behavior,
// relocated behind the seam. It is constructed from the same execFunc the CLI
// (realExec) or a test (fakeExec) injects, so the exact git-command sequence is
// unchanged.
type gitTransport struct {
	run    execFunc
	dir    string // staging worktree (<DataDir>/share/publish/<name>)
	remote string // registered remote to match origin against ("" skips the check)
	name   string // share name, for user-facing messages
}

func newGitPublisher(run execFunc, cfg Config, pub sharePublish) *gitTransport {
	return &gitTransport{
		run:    run,
		dir:    shareStagingDir(cfg, pub.Name),
		remote: pub.Remote,
		name:   pub.Name,
	}
}

// publish is v1's sharePush git block, moved verbatim. Ordering is load-bearing:
// the origin-vs-registry and detached-HEAD guards run before anything mutates,
// the tracked-file allowlist runs after `git add` and before commit, and nothing
// is written to push state until the remote accepts the push (that lives in the
// caller, after this returns).
func (g *gitTransport) publish(ctx context.Context, set shareSet) error {
	if _, err := g.run(ctx, "", "git", "--version"); err != nil {
		return fmt.Errorf("git is required for sharing but was not found: %w", err)
	}
	isRepo, repoErr := vaultRepoState(filepath.Join(g.dir, ".git"))
	if repoErr != nil {
		return repoErr
	}
	if !isRepo {
		return fmt.Errorf("share %q has no staging repo — run `mora share init %s …` first", g.name, g.name)
	}
	origin, err := g.run(ctx, g.dir, "git", "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("share %q has no usable origin remote (%v) — re-run `mora share init`", g.name, err)
	}
	// The staging repo's origin must be the remote the grant was created for — a
	// swapped origin would publish to somewhere the user never approved.
	if g.remote != "" && strings.TrimSpace(origin) != g.remote {
		return fmt.Errorf("staging repo origin (%s) does not match the share's configured remote (%s) — refusing to publish to an unapproved destination", redactCredentials(strings.TrimSpace(origin)), redactCredentials(g.remote))
	}
	if _, err := g.run(ctx, g.dir, "git", "symbolic-ref", "-q", "HEAD"); err != nil {
		return fmt.Errorf("share staging repo is in detached HEAD — check out a branch before publishing: %w", err)
	}

	memDir := filepath.Join(g.dir, "memories")
	for name, ct := range set.put {
		if err := atomicWrite(filepath.Join(memDir, name), ct, 0o644); err != nil {
			return err
		}
	}
	for _, f := range set.remove {
		if err := os.Remove(filepath.Join(memDir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	if _, err := g.run(ctx, g.dir, "git", "add", "-A"); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	// Hard stop, stronger than sync git's denylist: `git add -A` stages the whole
	// staging tree, so the tracked set is checked against an ALLOWLIST — only the
	// manifest, the .gitignore, and safe-named ciphertext may ever be tracked.
	// Anything else (stray plaintext, secrets, files another tool dropped here)
	// would leave without appearing in the preview.
	tracked, lsErr := g.run(ctx, g.dir, "git", "ls-files")
	if lsErr != nil {
		return fmt.Errorf("git ls-files: %w", lsErr)
	}
	var offenders []string
	for _, line := range strings.Split(tracked, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == ".gitignore" || line == "share.json" {
			continue
		}
		if stem, ok := strings.CutPrefix(line, "memories/"); ok {
			if stem, ok := strings.CutSuffix(stem, ".md.age"); ok && shareExportIDRE.MatchString(stem) {
				continue
			}
		}
		offenders = append(offenders, line)
	}
	if len(offenders) > 0 {
		return fmt.Errorf("refusing to publish: the share repo tracks files that are not manifest/.gitignore/encrypted memories:\n%s\nuntrack them first (the working copy is kept): git -C %s rm -r --cached <path>", strings.Join(offenders, "\n"), g.dir)
	}
	status, err := g.run(ctx, g.dir, "git", "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		commitArgs := append(commitIdentityArgs(ctx, g.dir, g.run), "commit", "-m",
			fmt.Sprintf("mora share push %s %s", g.name, time.Now().Format(time.RFC3339)))
		if _, err := g.run(ctx, g.dir, "git", commitArgs...); err != nil {
			return fmt.Errorf("git commit: %w", err)
		}
	}
	// Always push, even with no content changes — the remote may be behind after
	// an earlier failed push. Never --force.
	if _, err := g.run(ctx, g.dir, "git", "push", "origin", "HEAD"); err != nil {
		return fmt.Errorf("git push failed (the share was NOT published): %w", err)
	}
	return nil
}
