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
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
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
func shareRepoDir(cfg Config, name string) string {
	return filepath.Join(shareSubRoot(cfg, name), "repo")
}
func shareCorpusDir(cfg Config, name string) string {
	return filepath.Join(shareSubRoot(cfg, name), "corpus")
}
func shareIndexPath(cfg Config, name string) string {
	return filepath.Join(shareSubRoot(cfg, name), "index.db")
}

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

// The subscriber's age identity. Lives beside the OAuth tokens in ConfigDir —
// a secret, 0600, never inside the vault and never inside any share repo.
func shareIdentityPath(cfg Config) string {
	return filepath.Join(cfg.ConfigDir, "share", "identity.txt")
}

// shareKeygen mints the machine's one age X25519 identity and prints the public
// key for out-of-band exchange with publishers. It never overwrites: the
// identity is the only key that can decrypt shares already sent to it.
func shareKeygen(cfg Config, stdout io.Writer) error {
	path := shareIdentityPath(cfg)
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("share identity already exists at %s — refusing to overwrite (it is the only key that can decrypt shares sent to you)", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return err
	}
	body := fmt.Sprintf("# created: %s\n# public key: %s\n%s\n",
		time.Now().Format(time.RFC3339), id.Recipient(), id)
	if err := atomicWrite(path, []byte(body), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "share identity written to %s — never share or commit this file\n", path)
	fmt.Fprintf(stdout, "your public key (give this to people who want to share memories with you):\n%s\n", id.Recipient())
	return nil
}

func loadShareIdentities(cfg Config) ([]age.Identity, error) {
	f, err := os.Open(shareIdentityPath(cfg))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no share identity at %s — run `mora share keygen` first", shareIdentityPath(cfg))
		}
		return nil, err
	}
	defer f.Close()
	return age.ParseIdentities(f)
}

// parseShareRecipients accepts age X25519 public keys only (v1). An empty list
// is an error by design: sharing without encryption does not exist.
func parseShareRecipients(keys []string) ([]age.Recipient, error) {
	if len(keys) == 0 {
		return nil, errors.New("at least one --recipient age public key is required — shares are always encrypted")
	}
	out := make([]age.Recipient, 0, len(keys))
	for _, k := range keys {
		r, err := age.ParseX25519Recipient(strings.TrimSpace(k))
		if err != nil {
			return nil, fmt.Errorf("invalid recipient %q (v1 accepts age X25519 public keys only; the other party runs `mora share keygen` to get one): %w", k, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// collectShareMemories selects exactly what a share may export: AUTHORED
// memories (files under memories/ only — connector evidence under sources/ is
// structurally out of reach), frontmatter scope exact-match, tombstones and
// anything provider-stamped skipped. Path safety is a P0 here: the scope is
// validated before any filesystem access, symlinks anywhere in the tree abort
// the export loudly, and every selected file is re-verified to resolve inside
// the memories root.
func collectShareMemories(cfg Config, scope string) ([]Memory, error) {
	if !validShareScope(scope) {
		return nil, fmt.Errorf("invalid share scope %q — share accepts personal, global, or project:<name>", scope)
	}
	root := memoriesRoot(cfg)
	realRoot := resolveReal(root)
	var out []Memory
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path == root {
				return nil // empty vault: nothing to export, not an error
			}
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("refusing to export: %s is a symlink — share export never follows links", path)
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		m, perr := parseMemory(path)
		if perr != nil {
			return nil // unparseable files have no attributable scope; nothing leaves
		}
		if m.Scope != scope || m.DeletedAt != "" || m.Provider != "" {
			return nil
		}
		if rp := resolveReal(path); !strings.HasPrefix(rp, realRoot+string(os.PathSeparator)) {
			return fmt.Errorf("refusing to export: %s resolves outside the memories root", path)
		}
		m.Path = path
		out = append(out, m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
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
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "keygen":
		return shareKeygen(cfg, stdout)
	default:
		return errors.New(shareUsage)
	}
}
