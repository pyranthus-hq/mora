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
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/charmbracelet/huh"
	"github.com/mattn/go-isatty"
	"github.com/pyranthus-hq/mora/internal/memory"
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
	Name       string        `json:"name"`
	Scope      string        `json:"scope"`
	Recipients []string      `json:"recipients"`
	Remote     string        `json:"remote,omitempty"`
	Transport  *transportRef `json:"transport,omitempty"` // nil ⇒ git (v1 remote)
	Owner      string        `json:"owner,omitempty"`
	CreatedAt  string        `json:"created_at"`
}

// shareSubscription is one inbound corpus. Name is chosen by the SUBSCRIBER and
// is the attribution label on unioned results — publisher-controlled metadata
// is never used as the trust label.
type shareSubscription struct {
	Name         string        `json:"name"`
	Remote       string        `json:"remote"`
	Transport    *transportRef `json:"transport,omitempty"`     // nil ⇒ git (v1 remote)
	PinnedPubkey []byte        `json:"pinned_pubkey,omitempty"` // TOFU-pinned publisher ed25519 key (non-git)
	LastVersion  int           `json:"last_version,omitempty"`  // highest manifest version accepted (anti-rollback)
	CreatedAt    string        `json:"created_at"`
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
		// This error surfaces from every search/think once subscriptions exist,
		// so it must name the file and the fix, not just the parse failure.
		return sf, fmt.Errorf("%s is corrupt (%v) — fix or delete the file; it holds share/subscription registrations", sharesPath(cfg), err)
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
		// The id becomes a filename in the share repo and in every subscriber's
		// corpus — a hand-edited id with separators or dot-tricks must never
		// travel (codex review P1). Loud, not skipped: the file IS in the shared
		// scope, so silently dropping it would falsify the preview.
		if !shareExportIDRE.MatchString(m.ID) {
			return fmt.Errorf("memory %s has an id (%q) unsafe for export — rename it to letters/digits/._- before sharing", path, m.ID)
		}
		// Mirror the import-side cap at export, where the PUBLISHER can fix it:
		// one oversized memory would otherwise wedge every subscriber's pull.
		if fi, ierr := d.Info(); ierr != nil {
			return ierr
		} else if fi.Size() > shareMaxMemoryBytes {
			return fmt.Errorf("memory %s exceeds the %d-byte per-memory share cap — split or trim it before sharing", path, shareMaxMemoryBytes)
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
	// Ids become filenames in subscriber corpora, which may sit on case-
	// insensitive filesystems (macOS/Windows): ids differing only by letter
	// case would silently collide there, so they are refused here where the
	// publisher can rename (review finding).
	folded := make(map[string]string, len(out))
	for _, m := range out {
		lower := strings.ToLower(m.ID)
		if prior, dup := folded[lower]; dup {
			return nil, fmt.Errorf("memories %s and %s differ only by letter case — subscribers on case-insensitive filesystems cannot store both; rename one before sharing", prior, m.ID)
		}
		folded[lower] = m.ID
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// shareManifest is the small plaintext descriptor at the root of every share
// repo. It carries no memory content — only enough for a subscriber to see
// what they cloned. The subscriber's OWN subscription name, not this file, is
// the attribution label (publisher-controlled metadata is never a trust label).
type shareManifest struct {
	Schema    int    `json:"schema"`
	Name      string `json:"name"`
	Scope     string `json:"scope"`
	Owner     string `json:"owner,omitempty"`
	CreatedAt string `json:"created_at"`
	Client    string `json:"client"`
}

const shareManifestSchema = 1

// shareGitignoreBody inverts the vault list: in a share repo the SENSITIVE
// thing is plaintext markdown — only *.md.age ciphertext and the manifest
// belong here. Identity/keys and index files are excluded defensively too.
const shareGitignoreBody = `# Mora share staging (managed by ` + "`mora share`" + `)
# Only age-encrypted memories (*.md.age) and share.json belong in this repo.
*.md
index.db
*.db
*.db-shm
*.db-wal
.DS_Store
tokens/
*.token
identity*
`

// shareInitDisclosure prints once per init. Honesty requirements from #51:
// remote must be private, and revocation cannot recall already-pulled content.
const shareInitDisclosure = `
  ⚠ This share publishes age-ENCRYPTED memories to a git remote on every push.
    Only the recipients' keys can decrypt them, but the remote must still be a
    PRIVATE repository you control — Mora runs no server.
    Revocation is honest, not magic: git history is durable, so removing a share
    stops future pushes but cannot recall what a subscriber already pulled.
    Every push shows a preview of exactly what leaves this machine.`

// multiFlag collects a repeatable string flag (e.g. --recipient given N times).
type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

const shareInitUsage = "usage: mora share init <name> --scope <scope> --recipient <age1...> [--recipient ...] (--remote <URL> | --github [--repo <name>] | --via r2 --bucket <name> [--endpoint <url>] [--prefix <p>]) [--owner <label>]"

// shareInit creates the named publish grant: a dedicated staging git repo
// (separate from the personal vault-backup repo), its manifest and defensive
// .gitignore, the origin remote, and the registry entry in shares.json.
func shareInit(ctx context.Context, cfg Config, args []string, stdout io.Writer, run execFunc) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New(shareInitUsage)
	}
	name := args[0]
	fs := flag.NewFlagSet("share init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	scope := fs.String("scope", "", "scope to share")
	var recipients multiFlag
	fs.Var(&recipients, "recipient", "age X25519 public key (repeatable)")
	remote := fs.String("remote", "", "private git remote URL for this share")
	github := fs.Bool("github", false, "create a PRIVATE GitHub repo via gh and wire it as origin")
	repoName := fs.String("repo", "", "repo name to create with --github (default mora-share-<name>)")
	owner := fs.String("owner", "", "informational publisher label written to the manifest")
	tflags := registerTransportFlags(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	// Same flag discipline as sync git: this command configures a plaintext-
	// adjacent egress destination, so unusable combinations fail loudly.
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q — %s", fs.Arg(0), shareInitUsage)
	}
	if !validShareName(name) {
		return fmt.Errorf("invalid share name %q (lowercase letters, digits, . _ -; max 64 chars)", name)
	}
	if !validShareScope(*scope) {
		return fmt.Errorf("invalid share scope %q — share accepts personal, global, or project:<name>", *scope)
	}
	if _, err := parseShareRecipients(recipients); err != nil {
		return err
	}
	tref, terr := tflags.resolve()
	if terr != nil {
		return terr
	}
	if bucketOf(tref) != nil {
		if *github || *remote != "" {
			return errors.New("--via r2|s3|bucket is exclusive with --github/--remote — pick one destination")
		}
		return shareInitBucket(cfg, name, *scope, []string(recipients), *owner, tref, stdout)
	}
	if *github && *remote != "" {
		return errors.New("--github and --remote are mutually exclusive — pick one destination")
	}
	if !*github && *remote == "" {
		return errors.New("no destination: pass --remote <URL> for a git-generic remote, or --github to create a private GitHub repo")
	}
	if *repoName != "" && !*github {
		return errors.New("--repo only applies with --github")
	}
	if *repoName == "" {
		*repoName = "mora-share-" + name
	}

	sf, err := loadShares(cfg)
	if err != nil {
		return err
	}
	for _, p := range sf.Publishes {
		if p.Name == name {
			return fmt.Errorf("share %q already exists — remove it first with `mora share remove %s --yes`", name, name)
		}
	}
	for _, s := range sf.Subscriptions {
		if s.Name == name {
			return fmt.Errorf("%q already names a subscription — share and subscription names share one namespace", name)
		}
	}

	if _, err := run(ctx, "", "git", "--version"); err != nil {
		return fmt.Errorf("git is required for sharing but was not found: %w", err)
	}
	staging := shareStagingDir(cfg, name)
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return err
	}
	isRepo, repoErr := vaultRepoState(filepath.Join(staging, ".git"))
	if repoErr != nil {
		return repoErr
	}
	if !isRepo {
		if _, err := run(ctx, staging, "git", "init"); err != nil {
			return fmt.Errorf("git init: %w", err)
		}
	}
	giPath := filepath.Join(staging, ".gitignore")
	if _, err := os.Stat(giPath); os.IsNotExist(err) {
		if werr := atomicWrite(giPath, []byte(shareGitignoreBody), 0o644); werr != nil {
			return fmt.Errorf("writing .gitignore: %w", werr)
		}
	}
	man := shareManifest{
		Schema: shareManifestSchema, Name: name, Scope: *scope, Owner: *owner,
		CreatedAt: time.Now().Format(time.RFC3339), Client: "mora " + BuildVersion,
	}
	mb, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(staging, "share.json"), append(mb, '\n'), 0o644); err != nil {
		return err
	}
	if err := configureRemote(ctx, staging, *github, *repoName, *remote, run); err != nil {
		return err
	}
	// Record the RESOLVED origin URL, not just the flag: with --github the URL
	// is minted by gh, and the push-time origin-vs-registry check needs a
	// reference or a later origin swap would publish to an unapproved
	// destination (codex review).
	registryRemote := *remote
	if registryRemote == "" {
		origin, err := run(ctx, staging, "git", "remote", "get-url", "origin")
		if err != nil {
			return fmt.Errorf("reading the origin gh configured: %w", err)
		}
		registryRemote = strings.TrimSpace(origin)
	}

	sf.Publishes = append(sf.Publishes, sharePublish{
		Name: name, Scope: *scope, Recipients: recipients, Remote: registryRemote,
		Owner: *owner, CreatedAt: man.CreatedAt,
	})
	if err := saveShares(cfg, sf); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "share %q initialized — scope %s, %d recipient key(s). Publish with `mora share push %s`.\n", name, *scope, len(recipients), name)
	fmt.Fprintln(stdout, shareInitDisclosure)
	return nil
}

// shareExportIDRE is the hard gate on ids that become filenames in the share
// repo and in subscriber corpora: safe charset, no separators, no leading dot.
var shareExportIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// sharePushState is the LOCAL change-detection record for one publish, at
// <StateDir>/share/publish/<name>.json. Plaintext content hashes stay on this
// machine by design — putting them in the repo would let anyone holding the
// ciphertext confirm guessed plaintext. Losing the file is safe: everything is
// re-encrypted on the next push. It is written only AFTER a successful git
// push, so a failed push re-publishes rather than silently leaving the remote
// stale.
type sharePushState struct {
	Schema     int               `json:"schema"`
	Recipients []string          `json:"recipients"`
	Files      map[string]string `json:"files"`
}

func sharePushStatePath(cfg Config, name string) string {
	return filepath.Join(cfg.StateDir, "share", "publish", name+".json")
}

func loadSharePushState(cfg Config, name string) (sharePushState, error) {
	st := sharePushState{Schema: shareFileSchema, Files: map[string]string{}}
	b, err := os.ReadFile(sharePushStatePath(cfg, name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return st, nil
		}
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, err
	}
	if st.Files == nil {
		st.Files = map[string]string{}
	}
	return st, nil
}

func saveSharePushState(cfg Config, name string, st sharePushState) error {
	st.Schema = shareFileSchema
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(sharePushStatePath(cfg, name), append(b, '\n'), 0o600)
}

// shareChanges is one push's delta: what gets (re)encrypted, what gets removed
// from the staging repo, and the full current id→hash map for the post-push
// state record.
type shareChanges struct {
	Add, Update []Memory
	RemoveFiles []string          // staged base names to delete (stems shown in preview)
	Plain       map[string][]byte // id → exact plaintext bytes queued for encryption
	Current     map[string]string // id → content hash of every exported memory
	Reencrypt   bool              // recipient set changed: every file re-encrypts
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func computeShareChanges(cfg Config, pub sharePublish, mems []Memory) (shareChanges, error) {
	ch := shareChanges{Plain: map[string][]byte{}, Current: map[string]string{}}
	st, err := loadSharePushState(cfg, pub.Name)
	if err != nil {
		return ch, err
	}
	ch.Reencrypt = !slices.Equal(sortedStrings(pub.Recipients), sortedStrings(st.Recipients))
	memDir := filepath.Join(shareStagingDir(cfg, pub.Name), "memories")
	for _, m := range mems {
		b, err := os.ReadFile(m.Path)
		if err != nil {
			return ch, err
		}
		h := memory.ContentHash(string(b))
		ch.Current[m.ID] = h
		prev, known := st.Files[m.ID]
		switch {
		case !known:
			ch.Add = append(ch.Add, m)
			ch.Plain[m.ID] = b
		case ch.Reencrypt || prev != h:
			ch.Update = append(ch.Update, m)
			ch.Plain[m.ID] = b
		default:
			// State says published, but the change detector must trust the
			// STAGING TREE for existence: a staged ciphertext lost to a crash,
			// `git clean`, or a partial restore would otherwise ride the next
			// `git add -A` as an unpreviewed DELETION and then be recorded as
			// current forever (review finding). Missing on disk ⇒ re-publish.
			if _, statErr := os.Stat(filepath.Join(memDir, m.ID+".md.age")); statErr != nil {
				ch.Add = append(ch.Add, m)
				ch.Plain[m.ID] = b
			}
		}
	}
	// Removals are filesystem-driven (not state-driven) so a stray staged file
	// from a crashed run is cleaned up too: anything under memories/ whose stem
	// is not currently exported leaves the repo on this push.
	entries, err := os.ReadDir(filepath.Join(shareStagingDir(cfg, pub.Name), "memories"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return ch, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		stem := strings.TrimSuffix(e.Name(), ".md.age")
		if _, ok := ch.Current[stem]; !ok || stem == e.Name() {
			ch.RemoveFiles = append(ch.RemoveFiles, e.Name())
		}
	}
	sort.Strings(ch.RemoveFiles)
	return ch, nil
}

func encryptShareBytes(recipients []age.Recipient, plaintext []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, recipients...)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(plaintext); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// resolvePublish picks the named publish, or the only one when unnamed.
func resolvePublish(sf shareFile, name string) (sharePublish, error) {
	if name != "" {
		for _, p := range sf.Publishes {
			if p.Name == name {
				return p, nil
			}
		}
		return sharePublish{}, fmt.Errorf("no share named %q — see `mora share list`", name)
	}
	switch len(sf.Publishes) {
	case 0:
		return sharePublish{}, errors.New("no shares configured — run `mora share init <name> …` first")
	case 1:
		return sf.Publishes[0], nil
	default:
		return sharePublish{}, errors.New("multiple shares configured — name one: `mora share push <name>`")
	}
}

// printPushPreview is the mandatory pre-push preview (#51 P0): the exact files
// about to leave this machine, every time, before anything is encrypted.
func printPushPreview(w io.Writer, pub sharePublish, ch shareChanges, nRecipients int) {
	fmt.Fprintf(w, "share %q — scope %s, encrypted to %d recipient key(s), remote %s\n",
		pub.Name, pub.Scope, nRecipients, redactCredentials(pub.Remote))
	if len(ch.Add)+len(ch.Update)+len(ch.RemoveFiles) == 0 {
		fmt.Fprintf(w, "no changes to publish (%d memories already current)\n", len(ch.Current))
		return
	}
	fmt.Fprintf(w, "will publish %d new, %d updated; remove %d:\n", len(ch.Add), len(ch.Update), len(ch.RemoveFiles))
	for _, m := range ch.Add {
		fmt.Fprintf(w, "  + %s\t%s\t(%d bytes)\n", m.ID, m.Title, len(ch.Plain[m.ID]))
	}
	for _, m := range ch.Update {
		fmt.Fprintf(w, "  ~ %s\t%s\t(%d bytes)\n", m.ID, m.Title, len(ch.Plain[m.ID]))
	}
	for _, f := range ch.RemoveFiles {
		fmt.Fprintf(w, "  - %s\t(removed from share)\n", strings.TrimSuffix(f, ".md.age"))
	}
	fmt.Fprintf(w, "full content: `mora share preview %s`\n", pub.Name)
}

// confirmSharePushFn is a test seam like confirmVaultRepointFn: the real gate
// refuses non-interactive pushes without --yes by design.
var confirmSharePushFn = confirmSharePush

func confirmSharePush(stdin io.Reader, stdout io.Writer, name string) error {
	f, ok := stdin.(*os.File)
	if !ok || !isatty.IsTerminal(f.Fd()) {
		return fmt.Errorf("refusing to publish non-interactively without --yes — review with `mora share preview %s`, then re-run `mora share push %s --yes`", name, name)
	}
	var yes bool
	confirm := huh.NewConfirm().
		Title(fmt.Sprintf("Publish share %q to its git remote?", name)).
		Description("The files listed above leave this machine, age-encrypted.").
		Affirmative("Publish").
		Negative("Cancel").
		Value(&yes)
	if err := confirm.Run(); err != nil {
		return err
	}
	if !yes {
		return errors.New("push cancelled — nothing left this machine")
	}
	return nil
}

const sharePushUsage = "usage: mora share push [<name>] [--yes]"

// sharePush publishes one share: preview → confirm → encrypt → commit → push →
// record state. Ordering is load-bearing: nothing (not even the local staging
// repo) mutates before the preview is shown and confirmed, the tracked-
// plaintext hard-stop runs after `git add` and before commit, and the push
// state is written only after the remote accepted the push.
func sharePush(ctx context.Context, cfg Config, args []string, stdout io.Writer, stdin io.Reader, run execFunc) error {
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("share push", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	yes := fs.Bool("yes", false, "publish without interactive confirmation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New(sharePushUsage)
	}
	sf, err := loadShares(cfg)
	if err != nil {
		return err
	}
	pub, err := resolvePublish(sf, name)
	if err != nil {
		return err
	}
	// Mandatory encryption (#51 P0): no recipients — even via a hand-edited
	// registry — means no push, full stop.
	recipients, err := parseShareRecipients(pub.Recipients)
	if err != nil {
		return fmt.Errorf("share %q cannot encrypt: %w", pub.Name, err)
	}
	mems, err := collectShareMemories(cfg, pub.Scope)
	if err != nil {
		return err
	}
	// Bucket shares publish content-addressed blobs + a signed manifest; git keeps
	// the staging-repo delta path below.
	if bc := bucketOf(pub.Transport); bc != nil {
		return sharePushBucket(ctx, cfg, pub, mems, recipients, *bc, stdout, stdin, *yes)
	}
	ch, err := computeShareChanges(cfg, pub, mems)
	if err != nil {
		return err
	}
	printPushPreview(stdout, pub, ch, len(recipients))
	if !*yes {
		if err := confirmSharePushFn(stdin, stdout, pub.Name); err != nil {
			return err
		}
	}

	// Encrypt the pending set in memory, then hand it to the transport. Encryption
	// is backend-neutral (same ciphertext for any destination); only the durable
	// write/commit/push is transport-specific and lives behind the seam.
	set := shareSet{put: make(map[string][]byte, len(ch.Plain)), remove: ch.RemoveFiles}
	for id, plain := range ch.Plain {
		ct, err := encryptShareBytes(recipients, plain)
		if err != nil {
			return fmt.Errorf("encrypting %s: %w", id, err)
		}
		set.put[id+".md.age"] = ct
	}
	var t shareTransport = newGitPublisher(run, cfg, pub)
	if err := t.publish(ctx, set); err != nil {
		return err
	}
	if err := saveSharePushState(cfg, pub.Name, sharePushState{
		Recipients: sortedStrings(pub.Recipients), Files: ch.Current,
	}); err != nil {
		return err
	}
	if len(ch.Plain)+len(ch.RemoveFiles) == 0 {
		fmt.Fprintf(stdout, "share %q pushed — no changes.\n", pub.Name)
	} else {
		fmt.Fprintf(stdout, "share %q published: %d memories encrypted, %d removed.\n", pub.Name, len(ch.Plain), len(ch.RemoveFiles))
	}
	return nil
}

// sharePreview prints the EXACT content a push would publish — full raw file
// bytes, not summaries — plus pending removals. Read-only: no git, no writes.
func sharePreview(cfg Config, args []string, stdout io.Writer) error {
	name := ""
	if len(args) > 0 {
		if strings.HasPrefix(args[0], "-") || len(args) > 1 {
			return errors.New("usage: mora share preview [<name>]")
		}
		name = args[0]
	}
	sf, err := loadShares(cfg)
	if err != nil {
		return err
	}
	pub, err := resolvePublish(sf, name)
	if err != nil {
		return err
	}
	mems, err := collectShareMemories(cfg, pub.Scope)
	if err != nil {
		return err
	}
	ch, err := computeShareChanges(cfg, pub, mems)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "share %q — scope %s: %d memories would be published, encrypted to %d recipient key(s)\n",
		pub.Name, pub.Scope, len(mems), len(pub.Recipients))
	for _, m := range mems {
		b, err := os.ReadFile(m.Path)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "\n── %s — %s\n%s", m.ID, m.Title, b)
	}
	for _, f := range ch.RemoveFiles {
		fmt.Fprintf(stdout, "\n── %s — would be REMOVED from the share on next push\n", strings.TrimSuffix(f, ".md.age"))
	}
	return nil
}

// ---------- subscriber side ----------

// shareMaxMemoryBytes caps one decrypted memory. Authored notes are small;
// anything larger in a share repo is malformed or hostile (decompression-bomb
// class), and the whole import stops loudly rather than filling the disk.
const shareMaxMemoryBytes = 4 << 20

type shareImportStats struct {
	Imported int    // files newly written or changed this import
	Removed  int    // corpus files pruned because the repo no longer has them
	Total    int    // corpus size after import
	Owner    string // informational label from the manifest
	Scope    string
}

func readShareManifest(repoDir string) (shareManifest, error) {
	b, err := os.ReadFile(filepath.Join(repoDir, "share.json"))
	if err != nil {
		return shareManifest{}, fmt.Errorf("share repo has no readable share.json manifest: %w", err)
	}
	return parseShareManifestBytes(b)
}

// parseShareManifestBytes validates a share.json manifest from raw bytes (used
// by the git-pin path, which reads it via `git cat-file blob <pin>:share.json`
// rather than from a mutable working tree).
func parseShareManifestBytes(b []byte) (shareManifest, error) {
	var man shareManifest
	if err := json.Unmarshal(b, &man); err != nil {
		return man, fmt.Errorf("share.json: %w", err)
	}
	if man.Schema != shareManifestSchema {
		return man, fmt.Errorf("share.json declares schema %d; this mora supports %d — upgrade mora", man.Schema, shareManifestSchema)
	}
	if !validShareScope(man.Scope) {
		return man, fmt.Errorf("share.json declares invalid scope %q", man.Scope)
	}
	return man, nil
}

// openShareIndexRO opens a resolved generation's IMMUTABLE index.db for querying
// — never through openIndexRO (whose auto-heal would rebuild it from the WRONG
// personal vault). It keeps the shipped mode=ro spelling + query_only + the
// user_version gate, and ADDS an integrity check binding the index.db to its
// generation: sha256(file) must equal the committed index_digest, recomputed on
// EVERY serve — a positive result is NEVER cached (a byte-flip need not change
// the mtime, so any (gen,mtime)-keyed cache would mask exactly the post-commit
// corruption this check exists to catch). A gen that fails to open or fails the
// digest is excluded + surfaced; heal may re-cut it from the frozen corpus.
func openShareIndexRO(ctx context.Context, path, committedDigest string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	if committedDigest != "" {
		got, derr := fileDigestOf(path)
		if derr != nil {
			return nil, derr
		}
		if got != committedDigest {
			return nil, fmt.Errorf("share index %s failed its integrity digest — the file was corrupted or substituted since it was committed", path)
		}
	}
	db, err := sql.Open("sqlite", path+"?mode=ro&_pragma=busy_timeout(15000)&_pragma=query_only(1)")
	if err != nil {
		return nil, err
	}
	var v int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		_ = db.Close()
		return nil, err
	}
	if v != indexSchemaVersion {
		_ = db.Close()
		return nil, fmt.Errorf("share index %s has schema %d (this mora expects %d) — run `mora share pull` to rebuild it", path, v, indexSchemaVersion)
	}
	return db, nil
}

const shareSubscribeUsage = "usage: mora share subscribe <name> (--remote <URL> | --via r2 --bucket <name> [--endpoint <url>] [--prefix <p>] --confirm-pin <fingerprint>)"

// shareSubscribe clones a share repo into the share root, decrypts it, and
// registers the subscription. The name chosen here is the attribution label on
// every unioned search/think result. Read-only by construction: subscribing
// never writes inside the vault.
func shareSubscribe(ctx context.Context, cfg Config, args []string, stdout io.Writer, run execFunc) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New(shareSubscribeUsage)
	}
	name := args[0]
	fs := flag.NewFlagSet("share subscribe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	remote := fs.String("remote", "", "git remote URL of the share to subscribe to")
	tflags := registerTransportFlags(fs)
	confirmPin := fs.String("confirm-pin", "", "publisher fingerprint to confirm on a first bucket subscribe")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New(shareSubscribeUsage)
	}
	if !validShareName(name) {
		return fmt.Errorf("invalid subscription name %q (lowercase letters, digits, . _ -; max 64 chars)", name)
	}
	tref, terr := tflags.resolve()
	if terr != nil {
		return terr
	}
	if bc := bucketOf(tref); bc != nil {
		return shareSubscribeBucket(ctx, cfg, name, *bc, *confirmPin, stdout)
	}
	if *remote == "" {
		return errors.New(shareSubscribeUsage)
	}
	// Fail fast, before any network: without a decryption identity the clone
	// would only produce unreadable ciphertext.
	if _, err := loadShareIdentities(cfg); err != nil {
		return err
	}
	sf, err := loadShares(cfg)
	if err != nil {
		return err
	}
	for _, s := range sf.Subscriptions {
		if s.Name == name {
			return fmt.Errorf("subscription %q already exists — `mora share pull %s` updates it", name, name)
		}
	}
	for _, p := range sf.Publishes {
		if p.Name == name {
			return fmt.Errorf("%q already names a share you publish — share and subscription names share one namespace", name)
		}
	}

	repo := shareRepoDir(cfg, name)
	isRepo, repoErr := vaultRepoState(filepath.Join(repo, ".git"))
	if repoErr != nil {
		return repoErr
	}
	freshClone := false
	if !isRepo {
		if _, err := run(ctx, "", "git", "--version"); err != nil {
			return fmt.Errorf("git is required for sharing but was not found: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(repo), 0o700); err != nil {
			return err
		}
		if _, err := run(ctx, "", "git", "clone", *remote, repo); err != nil {
			return fmt.Errorf("git clone: %w", err)
		}
		freshClone = true
	} else {
		// A leftover clone under this name must actually point at the remote
		// being subscribed to — importing a stale repo from somewhere else
		// would poison search/think under a trusted attribution label
		// (codex review).
		origin, err := run(ctx, repo, "git", "remote", "get-url", "origin")
		if err != nil {
			return fmt.Errorf("existing clone at %s has no usable origin (%v) — delete it and re-subscribe", repo, err)
		}
		if strings.TrimSpace(origin) != *remote {
			return fmt.Errorf("existing clone at %s has origin %s, not the requested remote %s — delete it (or pick another subscription name) and re-subscribe", repo, redactCredentials(strings.TrimSpace(origin)), redactCredentials(*remote))
		}
		// No `git pull --ff-only` freshen: the run-private pin fetch (H1) brings the
		// merge source into an immutable per-run ref and reads objects only from it.
	}
	sub := shareSubscription{Name: name, Remote: *remote, CreatedAt: time.Now().Format(time.RFC3339)}
	var stats shareImportStats
	err = shareBuildAndPublish(ctx, cfg, name, buildModeImport, func(runID string) (int, error) {
		seq, st, ierr := gitShareImport(ctx, cfg, sub, runID, run)
		stats = st
		return seq, ierr
	})
	if err != nil {
		// A fresh clone whose FIRST import failed (publisher hasn't pushed yet,
		// key not among recipients, …) must not survive: the subscription was
		// never registered, so no CLI verb could ever clean or retry it
		// (review finding). Remove it so re-subscribing starts clean.
		if freshClone {
			_ = os.RemoveAll(shareSubRoot(cfg, name))
		}
		return fmt.Errorf("%w — nothing was registered; fix the cause (has the publisher pushed? is your key among the recipients?) and re-run `mora share subscribe`", err)
	}
	sf.Subscriptions = append(sf.Subscriptions, sub)
	if err := saveShares(cfg, sf); err != nil {
		return err
	}
	owner := stats.Owner
	if owner == "" {
		owner = "(unnamed publisher)"
	}
	fmt.Fprintf(stdout, "subscribed to share %q — %d memories from %s (scope %s), read-only beside your vault.\n", name, stats.Total, owner, stats.Scope)
	fmt.Fprintf(stdout, "shared results appear in search/think attributed as [%s]; your own vault and graph are never modified.\n", name)
	return nil
}

// sharePull fetches and re-imports one or all subscriptions. --ff-only is the
// contract: a publisher history rewrite (share rotation) fails loudly with a
// re-subscribe pointer instead of silently merging.
func sharePull(ctx context.Context, cfg Config, args []string, stdout io.Writer, run execFunc) error {
	name := ""
	if len(args) > 0 {
		if strings.HasPrefix(args[0], "-") || len(args) > 1 {
			return errors.New("usage: mora share pull [<name>]")
		}
		name = args[0]
	}
	sf, err := loadShares(cfg)
	if err != nil {
		return err
	}
	var subs []shareSubscription
	if name != "" {
		for _, s := range sf.Subscriptions {
			if s.Name == name {
				subs = append(subs, s)
			}
		}
		if len(subs) == 0 {
			return fmt.Errorf("no subscription named %q — see `mora share list`", name)
		}
	} else {
		subs = sf.Subscriptions
		if len(subs) == 0 {
			return errors.New("no subscriptions — run `mora share subscribe <name> --remote <URL>` first")
		}
	}
	for _, sub := range subs {
		if bc := bucketOf(sub.Transport); bc != nil {
			if err := sharePullBucket(ctx, cfg, sub, *bc, stdout); err != nil {
				return err
			}
			continue
		}
		if _, err := run(ctx, "", "git", "--version"); err != nil {
			return fmt.Errorf("git is required for sharing but was not found: %w", err)
		}
		repo := shareRepoDir(cfg, sub.Name)
		isRepo, repoErr := vaultRepoState(filepath.Join(repo, ".git"))
		if repoErr != nil {
			return repoErr
		}
		if !isRepo {
			return fmt.Errorf("subscription %q has no local clone — remove and re-subscribe", sub.Name)
		}
		var stats shareImportStats
		if err := shareBuildAndPublish(ctx, cfg, sub.Name, buildModeImport, func(runID string) (int, error) {
			seq, st, ierr := gitShareImport(ctx, cfg, sub, runID, run)
			stats = st
			return seq, ierr
		}); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "share %q: %d new/updated, %d removed, %d total.\n", sub.Name, stats.Imported, stats.Removed, stats.Total)
	}
	return nil
}

// healShareIndex mints a REPAIR generation from the published generation's own
// frozen corpus and commits it via the H2c claim (under the import lease taken by
// the heal chokepoint). Its only meaning: the published generation's index.db
// won't open or fails its integrity digest. It never reads unverified files, so
// it cannot launder a stray/zombie/out-of-band file; if the published corpus is
// itself unreadable it fails closed. It mints a NEW gen (never overwriting the
// corrupt one), keeping it Windows-safe.
func healShareIndex(ctx context.Context, cfg Config, name string) error {
	return shareBuildAndPublish(ctx, cfg, name, buildModeHeal, func(runID string) (int, error) {
		commit, ok, err := resolvePublishedCommit(cfg, name)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, fmt.Errorf("share %q: nothing committed to heal from", name)
		}
		srcCorpus := shareGenCorpusDir(cfg, name, commit.Gen)
		// Verify the source corpus against its committed digest before re-cutting —
		// heal must re-cut only from a verified frozen corpus.
		if d, derr := corpusDigestOf(srcCorpus); derr != nil || d != commit.CorpusDigest {
			return 0, fmt.Errorf("share %q: published corpus is unreadable/corrupt — cannot heal; run `mora share pull %s`", name, name)
		}
		entries, cerr := readFrozenCorpus(srcCorpus)
		if cerr != nil {
			return 0, cerr
		}
		gen := "gen-" + runID
		corpusDigest, indexDigest, berr := buildShareGenerationFromEntries(ctx, cfg, name, gen, entries)
		if berr != nil {
			return 0, berr
		}
		return publishShareGeneration(cfg, shareCommitParams{
			name: name, runID: runID, gen: gen, sourceRev: commit.SourceRev,
			corpusDigest: corpusDigest, indexDigest: indexDigest, count: len(entries),
			builtAt: time.Now(), parentFloor: commit.BucketFloor,
		})
	})
}

// readFrozenCorpus reads a generation's frozen corpus into blob entries for
// re-cutting (heal). The corpus is already digest-verified by the caller.
func readFrozenCorpus(corpusDir string) ([]shareBlobEntry, error) {
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		return nil, err
	}
	var out []shareBlobEntry
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		p := filepath.Join(corpusDir, e.Name())
		body, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil, rerr
		}
		m, perr := parseMemoryBytes(p, body)
		if perr != nil {
			return nil, fmt.Errorf("frozen corpus file %s does not parse: %v", e.Name(), perr)
		}
		out = append(out, shareBlobEntry{mem: m, body: body})
	}
	return out, nil
}

// findSharedMemory resolves an id against every subscription's PUBLISHED
// generation, serving from the frozen corpus and verifying integrity against the
// committed corpus_digest — hashing and serving the SAME bytes it read
// (read-once, no check-to-use re-read). On a digest mismatch, a bit-flip/synced-
// conflict cannot be served: read returns not-found rather than altered bytes.
func findSharedMemory(cfg Config, id string) (Memory, bool) {
	if !shareExportIDRE.MatchString(id) {
		return Memory{}, false
	}
	sf, err := loadShares(cfg)
	if err != nil {
		return Memory{}, false
	}
	for _, sub := range sf.Subscriptions {
		commit, ok, cerr := resolvePublishedCommit(cfg, sub.Name)
		if cerr != nil || !ok {
			continue // no valid artifact to serve; surfaced by doctor/shares_unhealthy
		}
		m, found, ierr := readSharedMemoryFromGen(shareGenCorpusDir(cfg, sub.Name, commit.Gen), id, commit.CorpusDigest)
		if ierr != nil || !found {
			continue
		}
		m.Owner = sub.Name
		return m, true
	}
	return Memory{}, false
}

// readSharedMemoryFromGen reads EVERY corpus file in the resolved generation
// exactly once, retains the target id's bytes in memory, recomputes the
// corpus_digest, and — only if it equals the committed digest — parses and serves
// those RETAINED bytes. Because the bytes hashed are the bytes served (never
// re-read after the check), corruption after the hash cannot be served. Runs on
// every serve with no positive-result cache.
func readSharedMemoryFromGen(corpusDir, id, committedDigest string) (Memory, bool, error) {
	entries, err := os.ReadDir(corpusDir)
	if err != nil {
		return Memory{}, false, err
	}
	var lines []string
	var targetBytes []byte
	found := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(corpusDir, e.Name()))
		if rerr != nil {
			return Memory{}, false, rerr
		}
		sum := sha256.Sum256(b)
		lines = append(lines, hex.EncodeToString(sum[:])+"  "+e.Name())
		if e.Name() == id+".md" {
			targetBytes = b
			found = true
		}
	}
	if committedDigest != "" && manifestDigestOf(lines) != committedDigest {
		return Memory{}, false, nil // corruption: fail closed, never serve altered bytes
	}
	if !found {
		return Memory{}, false, nil
	}
	// Test-only TOCTOU seam: production keeps serving targetBytes retained above.
	// A verify-then-re-read mutation would observe any replacement made here and
	// is therefore caught by TestReadServesTheBytesItHashed.
	if testHookSharedReadAfterHash != nil {
		testHookSharedReadAfterHash(filepath.Join(corpusDir, id+".md"))
	}
	m, perr := parseMemoryBytes(filepath.Join(corpusDir, id+".md"), targetBytes)
	if perr != nil || m.ID != id {
		return Memory{}, false, nil
	}
	return m, true, nil
}

// testHookSharedReadAfterHash is nil in production. Tests use it to replace the
// target file after its bytes have been hashed but before parsing, proving that
// the read path serves the exact retained bytes rather than re-reading a path.
var testHookSharedReadAfterHash func(path string)

// ---------- query-time union ----------

// Fusion of the local ranked list with each subscription's BM25 list, by
// rank-based RRF (score scales are incomparable across corpora). The local arm
// anchors at the weight hybrid fusion gives its strongest arm (fts 1.5), and
// ALL subscriptions together share one arm's worth of vote — so multiple
// shares can never collectively out-vote the user's own vault (codex review).
// k matches defaultFusion. Untuned by eval; revisit with the T2 harness.
const (
	shareFusionLocalWeight  = 1.5
	shareFusionSharedWeight = 1.0
	shareFusionK            = 10.0
)

// ownedTitle renders a result title with its share attribution, if any.
func ownedTitle(m Memory) string {
	if m.Owner == "" {
		return m.Title
	}
	return "[" + m.Owner + "] " + m.Title
}

// searchShareIndex is searchMemories against one subscription's index, with
// every row attributed to the subscription.
func searchShareIndex(ctx context.Context, db *sql.DB, owner, query, scope string, limit int) ([]Memory, error) {
	match := ftsQuery(query)
	if strings.TrimSpace(match) == "" {
		return nil, nil
	}
	q := `SELECT m.id, m.scope, m.type, m.title, m.tags, m.source, m.created_at, m.path, m.text, bm25(memories_fts) AS score
		FROM memories_fts JOIN memories m ON m.id = memories_fts.id WHERE memories_fts MATCH ?`
	args := []any{match}
	if scope != "" {
		q += ` AND m.scope = ?`
		args = append(args, scope)
	}
	q += ` ORDER BY score, m.id LIMIT ?`
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var m Memory
		var tags string
		if err := rows.Scan(&m.ID, &m.Scope, &m.Type, &m.Title, &tags, &m.Source, &m.CreatedAt, &m.Path, &m.Text, &m.Score); err != nil {
			return nil, err
		}
		m.Tags = splitCSV(tags)
		m.Owner = owner
		out = append(out, m)
	}
	return out, rows.Err()
}

// searchSharedCorpora queries every subscription's index and returns one
// ranked list per corpus. A never-pulled subscription (no index yet) is
// silently skipped — that's a normal state, visible in `mora share list`. An
// index that EXISTS but cannot be opened or has the wrong schema fails the
// whole search loudly: honest-failure over silent partial results.
func searchSharedCorpora(ctx context.Context, cfg Config, query, scope string, limit int) ([][]Memory, error) {
	sf, err := loadShares(cfg)
	if err != nil {
		return nil, err
	}
	if len(sf.Subscriptions) == 0 {
		return nil, nil
	}
	var out [][]Memory
	for _, sub := range sf.Subscriptions {
		commit, ok, rerr := resolvePublishedCommit(cfg, sub.Name)
		if rerr != nil || !ok {
			continue // no valid artifact to serve on THIS surface; surfaced by doctor
		}
		db, err := openShareIndexRO(ctx, shareGenIndexPath(cfg, sub.Name, commit.Gen), commit.IndexDigest)
		if err != nil {
			// The published index.db won't open / fails its integrity digest. Heal
			// re-cuts a repair generation from the head's OWN frozen corpus (never
			// unverified files) and commits it; then re-resolve and reopen. If heal
			// cannot re-cut, exclude THIS subscription from search only (per-artifact
			// suppression) — a corrupt share index never takes down local recall.
			if healErr := healShareIndex(ctx, cfg, sub.Name); healErr != nil {
				continue
			}
			commit, ok, rerr = resolvePublishedCommit(cfg, sub.Name)
			if rerr != nil || !ok {
				continue
			}
			if db, err = openShareIndexRO(ctx, shareGenIndexPath(cfg, sub.Name, commit.Gen), commit.IndexDigest); err != nil {
				continue
			}
		}
		res, qerr := searchShareIndex(ctx, db, sub.Name, query, scope, limit)
		_ = db.Close()
		if qerr != nil {
			continue
		}
		if len(res) > 0 {
			out = append(out, res)
		}
	}
	return out, nil
}

// unionSharedResults fuses the local ranked list with every subscription's
// list. With no subscriptions (or no shared hits) the local slice is returned
// UNCHANGED — the zero-share path stays byte-identical for the MCP budget
// gate and every existing caller. Fusion keys are NUL-separated so a shared id
// can never collide with a local id in the score map.
func unionSharedResults(ctx context.Context, cfg Config, local []Memory, query, scope string, limit int) ([]Memory, error) {
	shared, err := searchSharedCorpora(ctx, cfg, query, scope, limit)
	if err != nil {
		return nil, err
	}
	if len(shared) == 0 {
		return local, nil
	}
	byKey := make(map[string]Memory, len(local))
	lists := make([][]string, 0, 1+len(shared))
	weights := make([]float64, 0, 1+len(shared))
	localIDs := make([]string, len(local))
	for i, m := range local {
		localIDs[i] = m.ID
		byKey[m.ID] = m
	}
	lists = append(lists, localIDs)
	weights = append(weights, shareFusionLocalWeight)
	per := shareFusionSharedWeight / float64(len(shared))
	for _, corpus := range shared {
		ids := make([]string, len(corpus))
		for i, m := range corpus {
			key := "share\x00" + m.Owner + "\x00" + m.ID
			ids[i] = key
			byKey[key] = m
		}
		lists = append(lists, ids)
		weights = append(weights, per)
	}
	fused := rrfWeighted(lists, weights, shareFusionK)
	keys := make([]string, 0, len(fused))
	for k := range fused {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if fused[keys[i]] != fused[keys[j]] {
			return fused[keys[i]] > fused[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([]Memory, 0, len(keys))
	for _, k := range keys {
		m := byKey[k]
		m.Score = fused[k]
		out = append(out, m)
	}
	return out, nil
}

// ---------- list / remove / doctor support ----------

// shareStagingClean reports whether every publish staging repo is ciphertext-
// only. Plaintext markdown there would be published by the next `git add -A`;
// *.md.age deliberately does not count. Advisory (doctor): unreadable paths
// read as clean rather than failing doctor.
func shareStagingClean(cfg Config, pubs []sharePublish) bool {
	clean := true
	for _, p := range pubs {
		_ = filepath.WalkDir(shareStagingDir(cfg, p.Name), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".md") {
				clean = false
			}
			return nil
		})
	}
	return clean
}

func shareList(cfg Config, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("share list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("usage: mora share list [--json]")
	}
	sf, err := loadShares(cfg)
	if err != nil {
		return err
	}
	type pubRow struct {
		Name       string `json:"name"`
		Scope      string `json:"scope"`
		Recipients int    `json:"recipients"`
		Remote     string `json:"remote,omitempty"`
	}
	type subRow struct {
		Name     string `json:"name"`
		Remote   string `json:"remote"`
		Memories int    `json:"memories"`
		Pulled   bool   `json:"pulled"`
	}
	pubs := make([]pubRow, 0, len(sf.Publishes))
	for _, p := range sf.Publishes {
		pubs = append(pubs, pubRow{Name: p.Name, Scope: p.Scope, Recipients: len(p.Recipients), Remote: redactCredentials(p.Remote)})
	}
	subs := make([]subRow, 0, len(sf.Subscriptions))
	for _, s := range sf.Subscriptions {
		n := 0
		commit, ok, _ := resolvePublishedCommit(cfg, s.Name)
		if ok {
			n = commit.Count
		}
		subs = append(subs, subRow{Name: s.Name, Remote: redactCredentials(s.Remote), Memories: n, Pulled: ok})
	}
	if *jsonOut {
		b, err := json.MarshalIndent(map[string]any{"publishes": pubs, "subscriptions": subs}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, string(b))
		return nil
	}
	if len(pubs)+len(subs) == 0 {
		fmt.Fprintln(stdout, "no shares — `mora share init` to publish a scope, `mora share subscribe` to receive one.")
		return nil
	}
	sty := newStyler(stdout, false)
	if len(pubs) > 0 {
		fmt.Fprintln(stdout, "publishes:")
		for _, p := range pubs {
			fmt.Fprintf(stdout, "  %s\t%s\t%d recipient key(s)\t%s\n", p.Name, sty.dim(p.Scope), p.Recipients, sty.dim(p.Remote))
		}
	}
	if len(subs) > 0 {
		fmt.Fprintln(stdout, "subscriptions:")
		for _, s := range subs {
			state := fmt.Sprintf("%d memories", s.Memories)
			if !s.Pulled {
				state = "never pulled"
			}
			fmt.Fprintf(stdout, "  %s\t%s\t%s\n", s.Name, state, sty.dim(s.Remote))
		}
	}
	return nil
}

// shareRemove deletes local share state: the grant + staging repo for a
// publish, or the clone + corpus + index for a subscription. The revocation
// message is deliberately honest — git history is durable, so removing a
// publish cannot recall what subscribers already pulled.
func shareRemove(cfg Config, args []string, stdout io.Writer) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: mora share remove <name> --yes")
	}
	name := args[0]
	fs := flag.NewFlagSet("share remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	yes := fs.Bool("yes", false, "confirm removal")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("usage: mora share remove <name> --yes")
	}
	if !*yes {
		return fmt.Errorf("share remove deletes the local share state for %q — re-run with --yes to confirm", name)
	}
	sf, err := loadShares(cfg)
	if err != nil {
		return err
	}
	for i, p := range sf.Publishes {
		if p.Name != name {
			continue
		}
		if err := os.RemoveAll(shareStagingDir(cfg, name)); err != nil {
			return err
		}
		if err := os.Remove(sharePushStatePath(cfg, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		sf.Publishes = append(sf.Publishes[:i], sf.Publishes[i+1:]...)
		if err := saveShares(cfg, sf); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "share %q removed — future pushes stop now.\n", name)
		fmt.Fprintln(stdout, "honest revocation: git history is durable and subscribers keep what they already pulled; to cut future access, rotate to a new repo and new recipient keys.")
		return nil
	}
	for i, s := range sf.Subscriptions {
		if s.Name != name {
			continue
		}
		if err := os.RemoveAll(shareSubRoot(cfg, name)); err != nil {
			return err
		}
		sf.Subscriptions = append(sf.Subscriptions[:i], sf.Subscriptions[i+1:]...)
		if err := saveShares(cfg, sf); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "subscription %q removed — its corpus and index are deleted. Your own vault was never touched.\n", name)
		return nil
	}
	return fmt.Errorf("no share or subscription named %q — see `mora share list`", name)
}

const shareUsage = "usage: mora share <keygen | init <name> --scope <scope> --recipient <age1...> [--remote <url> | --github] | preview [<name>] | push [<name>] [--yes] | subscribe <name> --remote <url> | pull [<name>] | gc [<name>] | storage-limit <bytes> | list [--json] | remove <name> --yes>"

func cmdShare(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) error {
	if len(args) == 0 {
		return errors.New(shareUsage)
	}
	if isHelpFlag(args[0]) || (len(args) > 1 && isHelpFlag(args[1])) {
		_, err := io.WriteString(stdout, shareUsage+"\n")
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if err := shareGuardPaths(cfg); err != nil {
		return err
	}
	switch args[0] {
	case "keygen":
		if len(args) > 1 {
			return errors.New(shareUsage)
		}
		return shareKeygen(cfg, stdout)
	case "init":
		return shareInit(ctx, cfg, args[1:], stdout, realExec)
	case "preview":
		return sharePreview(cfg, args[1:], stdout)
	case "push":
		return sharePush(ctx, cfg, args[1:], stdout, stdin, realExec)
	case "subscribe":
		return shareSubscribe(ctx, cfg, args[1:], stdout, realExec)
	case "pull":
		return sharePull(ctx, cfg, args[1:], stdout, realExec)
	case "list":
		return shareList(cfg, args[1:], stdout)
	case "remove":
		return shareRemove(cfg, args[1:], stdout)
	case "gc":
		return cmdShareGC(cfg, args[1:], stdout, time.Now())
	case "storage-limit":
		return cmdShareStorageLimit(cfg, args[1:], stdout, time.Now())
	default:
		return errors.New(shareUsage)
	}
}

// shareGuardPaths refuses to run any share verb when the share root or the age
// identity would sit inside the vault. data_dir/config locations are
// user-configurable, and a co-located layout would put a subscriber's DECRYPTED
// corpus (or the identity secret) inside the tree that `mora backup` tars and
// vault git-sync pushes — exactly the leak this feature exists to prevent.
func shareGuardPaths(cfg Config) error {
	// Fold case on the platforms whose default filesystems do: a case-variant
	// vault_dir must not slip past the containment check (review finding).
	fold := runtimeGOOS() == "darwin" || runtimeGOOS() == "windows"
	vault := resolveRealDeep(cfg.VaultDir)
	for _, p := range []string{filepath.Join(cfg.DataDir, "share"), filepath.Dir(shareIdentityPath(cfg))} {
		if sharePathsOverlap(resolveRealDeep(p), vault, fold) {
			return fmt.Errorf("share root %s is inside the vault %s — sharing needs data_dir/config outside the vault so decrypted shares never enter vault backups or git-sync", p, cfg.VaultDir)
		}
	}
	return nil
}

// sharePathsOverlap reports whether one cleaned path contains the other (or
// they are equal), optionally case-folded for case-insensitive filesystems.
func sharePathsOverlap(a, b string, foldCase bool) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if foldCase {
		a, b = strings.ToLower(a), strings.ToLower(b)
	}
	sep := string(os.PathSeparator)
	return a == b || strings.HasPrefix(a+sep, b+sep) || strings.HasPrefix(b+sep, a+sep)
}

// resolveRealDeep resolves symlinks through the deepest EXISTING ancestor and
// re-appends the rest, so a not-yet-created path still compares against real
// paths (plain resolveReal falls back to Clean, which on macOS leaves /var vs
// /private/var mismatches and defeats prefix checks).
func resolveRealDeep(p string) string {
	cur := filepath.Clean(p)
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Clean(p)
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}
