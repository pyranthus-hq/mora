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
		// The id becomes a filename in the share repo and in every subscriber's
		// corpus — a hand-edited id with separators or dot-tricks must never
		// travel (codex review P1). Loud, not skipped: the file IS in the shared
		// scope, so silently dropping it would falsify the preview.
		if !shareExportIDRE.MatchString(m.ID) {
			return fmt.Errorf("memory %s has an id (%q) unsafe for export — rename it to letters/digits/._- before sharing", path, m.ID)
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

const shareInitUsage = "usage: mora share init <name> --scope <scope> --recipient <age1...> [--recipient ...] (--remote <URL> | --github [--repo <name>]) [--owner <label>]"

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

	sf.Publishes = append(sf.Publishes, sharePublish{
		Name: name, Scope: *scope, Recipients: recipients, Remote: *remote,
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

	if _, err := run(ctx, "", "git", "--version"); err != nil {
		return fmt.Errorf("git is required for sharing but was not found: %w", err)
	}
	staging := shareStagingDir(cfg, pub.Name)
	isRepo, repoErr := vaultRepoState(filepath.Join(staging, ".git"))
	if repoErr != nil {
		return repoErr
	}
	if !isRepo {
		return fmt.Errorf("share %q has no staging repo — run `mora share init %s …` first", pub.Name, pub.Name)
	}
	origin, err := run(ctx, staging, "git", "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("share %q has no usable origin remote (%v) — re-run `mora share init`", pub.Name, err)
	}
	// The staging repo's origin must be the remote the grant was created for —
	// a swapped origin would publish to somewhere the user never approved.
	if pub.Remote != "" && strings.TrimSpace(origin) != pub.Remote {
		return fmt.Errorf("staging repo origin (%s) does not match the share's configured remote (%s) — refusing to publish to an unapproved destination", redactCredentials(strings.TrimSpace(origin)), redactCredentials(pub.Remote))
	}
	if _, err := run(ctx, staging, "git", "symbolic-ref", "-q", "HEAD"); err != nil {
		return fmt.Errorf("share staging repo is in detached HEAD — check out a branch before publishing: %w", err)
	}

	memDir := filepath.Join(staging, "memories")
	for id, plain := range ch.Plain {
		ct, err := encryptShareBytes(recipients, plain)
		if err != nil {
			return fmt.Errorf("encrypting %s: %w", id, err)
		}
		if err := atomicWrite(filepath.Join(memDir, id+".md.age"), ct, 0o644); err != nil {
			return err
		}
	}
	for _, f := range ch.RemoveFiles {
		if err := os.Remove(filepath.Join(memDir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}

	if _, err := run(ctx, staging, "git", "add", "-A"); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	// Hard stop, mirroring sync git: .gitignore shields only untracked files.
	// In a share repo the sensitive class is PLAINTEXT (*.md) plus any stray
	// index/token/identity material.
	tracked, lsErr := run(ctx, staging, "git", "ls-files", "--",
		"*.md", "*.db", "*.db-shm", "*.db-wal", "*.token", "tokens", "identity*")
	if lsErr != nil {
		return fmt.Errorf("git ls-files: %w", lsErr)
	}
	if t := strings.TrimSpace(tracked); t != "" {
		return fmt.Errorf("refusing to publish: plaintext or sensitive files are git-TRACKED in the share repo:\n%s\nuntrack them first (the working copy is kept): git -C %s rm -r --cached <path>", t, staging)
	}
	status, err := run(ctx, staging, "git", "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("git status: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		commitArgs := append(commitIdentityArgs(ctx, staging, run), "commit", "-m",
			fmt.Sprintf("mora share push %s %s", pub.Name, time.Now().Format(time.RFC3339)))
		if _, err := run(ctx, staging, "git", commitArgs...); err != nil {
			return fmt.Errorf("git commit: %w", err)
		}
	}
	// Always push, even with no content changes — the remote may be behind
	// after an earlier failed push. Never --force.
	if _, err := run(ctx, staging, "git", "push", "origin", "HEAD"); err != nil {
		return fmt.Errorf("git push failed (the share was NOT published): %w", err)
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
	if err := shareGuardPaths(cfg); err != nil {
		return err
	}
	switch args[0] {
	case "keygen":
		return shareKeygen(cfg, stdout)
	case "init":
		return shareInit(ctx, cfg, args[1:], stdout, realExec)
	case "preview":
		return sharePreview(cfg, args[1:], stdout)
	case "push":
		return sharePush(ctx, cfg, args[1:], stdout, stdin, realExec)
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
	vault := resolveRealDeep(cfg.VaultDir)
	for _, p := range []string{filepath.Join(cfg.DataDir, "share"), filepath.Dir(shareIdentityPath(cfg))} {
		rp := resolveRealDeep(p)
		if rp == vault || strings.HasPrefix(rp+string(os.PathSeparator), vault+string(os.PathSeparator)) ||
			strings.HasPrefix(vault+string(os.PathSeparator), rp+string(os.PathSeparator)) {
			return fmt.Errorf("share root %s is inside the vault %s — sharing needs data_dir/config outside the vault so decrypted shares never enter vault backups or git-sync", p, cfg.VaultDir)
		}
	}
	return nil
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
