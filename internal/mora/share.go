package mora

// mora share — scoped, encrypted, read-only memory sharing over a private git
// remote (issue #51, v1.0).
//
// Publish: one scope of AUTHORED memories (files under memories/, frontmatter
// scope match) is exported, age-encrypted to every recipient, committed to a
// DEDICATED staging git repo, and pushed to a private remote the user
// configured. Encryption is mandatory: push refuses to run without at least one
// recipient, and only *.md.age ciphertext ever enters the repo.
//
// Subscribe: the share repo is cloned into a share root OUTSIDE the vault,
// decrypted into a plaintext corpus, and indexed into its own FTS index. Search
// and think union that corpus in at query time with owner attribution; the
// subscriber's own vault, personal index, and identity graph are never touched.
//
// Placement invariant: everything share-related lives under <DataDir>/share/
// (staging repos, clones, corpora, share indexes). The personal index rebuild
// walks only <VaultDir>/memories + <VaultDir>/sources, `mora backup` tars only
// the vault, and vault git-sync stages only the vault — so share content is
// structurally invisible to all three, and vice versa.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

// shareFile is the on-disk registry at <ConfigDir>/shares.json — the durable
// record ("grant ledger") of what this machine publishes and subscribes to.
type shareFile struct {
	Schema        int                 `json:"schema"`
	Publishes     []sharePublish      `json:"publishes,omitempty"`
	Subscriptions []shareSubscription `json:"subscriptions,omitempty"`
}

// sharePublish is one outbound grant: this scope, to these recipients, at this
// remote. Recipients are age X25519 public keys exchanged out of band.
type sharePublish struct {
	Name       string   `json:"name"`
	Scope      string   `json:"scope"`
	Recipients []string `json:"recipients"`
	Remote     string   `json:"remote,omitempty"`
	Owner      string   `json:"owner,omitempty"`
	CreatedAt  string   `json:"created_at"`
}

// shareSubscription is one inbound corpus. Name is chosen by the SUBSCRIBER and
// is the attribution label on unioned results — publisher-controlled metadata
// is never used as the trust label.
type shareSubscription struct {
	Name      string `json:"name"`
	Remote    string `json:"remote"`
	CreatedAt string `json:"created_at"`
}

const shareFileSchema = 1

// Share/subscription names become directory names and attribution labels;
// scopes expand to export paths and travel between machines. Both are therefore
// validated strictly — unlike `mora write --scope`, which accepts any string.
var (
	shareNameRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	shareScopeRE = regexp.MustCompile(`^(personal|global|project:[A-Za-z0-9][A-Za-z0-9._-]*)$`)
)

func validShareName(s string) bool  { return shareNameRE.MatchString(s) }
func validShareScope(s string) bool { return shareScopeRE.MatchString(s) }

func sharesPath(cfg Config) string { return filepath.Join(cfg.ConfigDir, "shares.json") }

// Publisher-side staging repo (git worktree holding manifest + ciphertext).
func shareStagingDir(cfg Config, name string) string {
	return filepath.Join(cfg.DataDir, "share", "publish", name)
}

// Subscriber-side share root: clone, decrypted corpus, and per-share index.
func shareSubRoot(cfg Config, name string) string {
	return filepath.Join(cfg.DataDir, "share", "subs", name)
}
func shareRepoDir(cfg Config, name string) string   { return filepath.Join(shareSubRoot(cfg, name), "repo") }
func shareCorpusDir(cfg Config, name string) string { return filepath.Join(shareSubRoot(cfg, name), "corpus") }
func shareIndexPath(cfg Config, name string) string { return filepath.Join(shareSubRoot(cfg, name), "index.db") }

func loadShares(cfg Config) (shareFile, error) {
	var sf shareFile
	b, err := os.ReadFile(sharesPath(cfg))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return shareFile{Schema: shareFileSchema}, nil
		}
		return sf, err
	}
	if err := json.Unmarshal(b, &sf); err != nil {
		return sf, err
	}
	return sf, nil
}

// saveShares persists the registry. Same caveat as saveSources: atomicWrite
// makes the write itself safe, not the surrounding read-modify-write.
func saveShares(cfg Config, sf shareFile) error {
	sf.Schema = shareFileSchema
	b, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(sharesPath(cfg), append(b, '\n'), 0o600)
}

const shareUsage = "usage: mora share <keygen | init <name> --scope <scope> --recipient <age1...> [--remote <url> | --github] | preview [<name>] | push [<name>] [--yes] | subscribe <name> --remote <url> | pull [<name>] | list [--json] | remove <name> --yes>"

func cmdShare(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) error {
	if len(args) == 0 {
		return errors.New(shareUsage)
	}
	if isHelpFlag(args[0]) {
		_, err := io.WriteString(stdout, shareUsage+"\n")
		return err
	}
	switch args[0] {
	default:
		return errors.New(shareUsage)
	}
}
