package mora

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/leasefile"
	"github.com/pyranthus-hq/mora/internal/rules"
)

// rulesDefaultSigners is where a shared machine keeps the keys it trusts. Only
// root should be able to change this file.
const rulesDefaultSigners = "/etc/mora/allowed_signers"

// rulesMaxGrantDays bounds a grant, so a forgotten one runs out on its own.
const rulesMaxGrantDays = 365

var (
	// rulesSSHKeygen is swapped in tests. ssh-keygen does the signing so Mora
	// stays pure Go and any key an SSH agent holds works, including one behind
	// Touch ID or a hardware token.
	rulesSSHKeygen = "ssh-keygen"
	// rulesSignersTrusted is swapped in tests.
	rulesSignersTrusted = rulesSignersProtected

	rulesGoodSignatureRE = regexp.MustCompile(`key (SHA256:[A-Za-z0-9+/]+)`)
)

// rulesGrant signs one approved rule for a wider scope. It keeps the terminal
// check of rules approve: a key on disk with no touch check would otherwise let
// an agent sign, and Mora cannot tell the two kinds of key apart.
func rulesGrant(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) error {
	const usage = "usage: mora rules grant <id> --to SCOPE --as NAME --key PATH [--days N] [--agent NAME]"
	fs := flag.NewFlagSet("rules grant", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	to := fs.String("to", "", "target scope, such as factory:aeth-forge, team:platform or project:acme")
	as := fs.String("as", "", "your name in the verifier's allowed_signers file")
	key := fs.String("key", "", "SSH key to sign with; a .pub path uses the key an SSH agent holds")
	days := fs.Int("days", 30, "days until the grant expires")
	agent := fs.String("agent", "", "the agent that drafted this rule; recorded as a claim, not proven")
	pos, err := parseRulesArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *to == "" || *as == "" || *key == "" {
		return errors.New(usage)
	}
	if !rules.ValidScope(*to) {
		return fmt.Errorf("scope %q is not valid; use project, team, factory or company, with an optional :name", *to)
	}
	if !rules.ValidPrincipal(*as) {
		return fmt.Errorf("name %q is not valid; use letters, digits and . _ @ + -", *as)
	}
	if *days < 1 || *days > rulesMaxGrantDays {
		return fmt.Errorf("--days must be from 1 to %d", rulesMaxGrantDays)
	}
	id := pos[0]
	if err := requireVerifiedOwnerAction(ownerAction{Kind: "rule.grant", TargetID: id}); err != nil {
		return err
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	approvals, err := rules.LoadApprovals(cfg.StateDir)
	if err != nil {
		return err
	}
	a, ok := approvals[id]
	if !ok {
		return fmt.Errorf("rule %s is not approved here; run mora rules approve first", id)
	}
	r, state := rules.Check(cfg.VaultDir, a)
	if state != rules.StateActive {
		return fmt.Errorf("rule %s is %s here; only an active rule can be granted", id, state)
	}
	fingerprint, err := rulesKeyFingerprint(*key)
	if err != nil {
		return err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	host, _ := os.Hostname()
	now := rulesNow().UTC()
	g := rules.Grant{
		Kind: rules.GrantKind, V: 1,
		Rule:      rules.GrantRule{ID: id, Hash: a.RuleHash, File: string(rules.Render(r))},
		Mode:      a.Mode,
		FromScope: rules.GrantFromScope, ToScope: *to,
		Principal: *as, Key: fingerprint, Machine: host, AgentClaimed: *agent,
		IssuedAt:  now.Format(time.RFC3339),
		ExpiresAt: now.AddDate(0, 0, *days).Format(time.RFC3339),
		Nonce:     hex.EncodeToString(nonce),
	}
	if err := rulesConfirm(stdout, stdin, id, rulesGrantLines(g, r)); err != nil {
		return err
	}
	body, err := rules.RenderGrant(g)
	if err != nil {
		return err
	}
	dir := filepath.Join(cfg.VaultDir, "grants")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, id+"-"+g.Nonce[:8]+".json")
	if err := atomicCreate(path, body, 0o600); err != nil {
		return err
	}
	if err := rulesSign(ctx, *key, path, stdin); err != nil {
		_ = os.Remove(path)
		return err
	}
	fmt.Fprintf(stdout, "signed %s for %s, expires %s\n%s\n%s.sig\n", id, *to, g.ExpiresAt, path, path)
	return nil
}

// rulesVerify checks a grant and prints what it proves. It changes nothing, so
// it needs no terminal.
func rulesVerify(ctx context.Context, args []string, stdout io.Writer) error {
	const usage = "usage: mora rules verify <grant-file> [--signers PATH] [--scope SCOPE]"
	fs := flag.NewFlagSet("rules verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	signers := fs.String("signers", rulesDefaultSigners, "allowed_signers file")
	scope := fs.String("scope", "", "fail unless the grant targets this scope")
	pos, err := parseRulesArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New(usage)
	}
	g, r, _, _, err := rulesVerifyGrant(ctx, pos[0], *signers, *scope)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "verified %s, signed by %s\n", g.Rule.ID, g.Principal)
	for _, l := range rulesGrantLines(g, r) {
		fmt.Fprintf(stdout, "  %s: %s\n", l[0], rulesShow(l[1]))
	}
	return nil
}

// rulesImport applies a verified grant on this machine. A signature replaces
// the typed confirmation only when the signers file is one this account cannot
// change. Otherwise an agent could sign with a key of its own and list it.
func rulesImport(ctx context.Context, args []string, stdout io.Writer, stdin io.Reader) error {
	const usage = "usage: mora rules import <grant-file> --scope SCOPE [--signers PATH]"
	fs := flag.NewFlagSet("rules import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	signers := fs.String("signers", rulesDefaultSigners, "allowed_signers file")
	scope := fs.String("scope", "", "this machine's scope; the grant must target it")
	pos, err := parseRulesArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || *scope == "" {
		return errors.New(usage)
	}
	path := pos[0]
	// Freeze the canonical trust-file identity and trust decision before verification.
	// Never verify through a user-controlled symlink then trust its later target.
	signerPath, err := filepath.EvalSymlinks(*signers)
	if err != nil {
		return err
	}
	signerPath, err = filepath.Abs(signerPath)
	if err != nil {
		return err
	}
	trusted := rulesSignersTrusted(signerPath)
	g, _, body, sig, err := rulesVerifyGrant(ctx, path, signerPath, *scope)
	if err != nil {
		return err
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	if !trusted {
		if err := rulesRequireTerminal(stdin); err != nil {
			return err
		}
		if err := requireVerifiedOwnerAction(ownerAction{Kind: "rule.import", TargetID: g.Rule.ID, Revision: g.Rule.Hash}); err != nil {
			return err
		}
	}
	var set rules.ActiveSet
	err = leasefile.WithGuard(filepath.Join(rules.Dir(cfg.StateDir), "mutation.lock"), func() error {
		if _, err := rules.CheckGrant(g, rulesNow()); err != nil {
			return err
		}
		// Reserve before any approval or vault change. Interrupted attempts consume
		// the grant, rather than allowing a replay to undo a later rules off.
		if err := rules.ReserveGrant(cfg.StateDir, g); err != nil {
			return err
		}
		rulePath := rules.RulePath(cfg.VaultDir, g.Rule.ID)
		if err := os.MkdirAll(filepath.Dir(rulePath), 0o700); err != nil {
			return err
		}
		if err := atomicio.Write(rulePath, []byte(g.Rule.File), 0o600); err != nil {
			return err
		}
		if err := rulesKeepGrant(cfg.VaultDir, body, sig, g); err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		if err := rules.AppendApproval(cfg.StateDir, rules.Approval{
			RuleID: g.Rule.ID, RuleHash: g.Rule.Hash, Mode: g.Mode,
			Via: "grant:" + hex.EncodeToString(sum[:6]), Approver: g.Principal, Expires: g.ExpiresAt,
		}); err != nil {
			return err
		}
		var err error
		set, err = rules.Compile(cfg.VaultDir, cfg.StateDir)
		return err
	})
	if err != nil {
		return fmt.Errorf("grant import failed (if already reserved, request a new signed grant): %w", err)
	}
	fmt.Fprintf(stdout, "imported %s (%s), signed by %s, expires %s. Active rules: %d\n", g.Rule.ID, g.Mode, g.Principal, g.ExpiresAt, len(set.Rules))
	return nil
}

// rulesVerifyGrant reads a grant and its .sig file and checks both. It returns
// the exact bytes that were verified.
func rulesVerifyGrant(ctx context.Context, path, signers, scope string) (rules.Grant, rules.Rule, []byte, []byte, error) {
	fail := func(err error) (rules.Grant, rules.Rule, []byte, []byte, error) {
		return rules.Grant{}, rules.Rule{}, nil, nil, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return fail(err)
	}
	g, err := rules.ParseGrant(body)
	if err != nil {
		return fail(err)
	}
	r, err := rules.CheckGrant(g, rulesNow())
	if err != nil {
		return fail(err)
	}
	if scope != "" && g.ToScope != scope {
		return fail(fmt.Errorf("this grant targets %s, not %s", g.ToScope, scope))
	}
	if _, err := os.Stat(signers); err != nil {
		return fail(fmt.Errorf("no signers file at %s; pass --signers: %w", signers, err))
	}
	sig, err := os.ReadFile(path + ".sig")
	if err != nil {
		return fail(err)
	}
	tmp, err := os.CreateTemp("", "mora-grant-signature-*")
	if err != nil {
		return fail(err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(sig); err != nil {
		_ = tmp.Close()
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	cmd := exec.CommandContext(ctx, rulesSSHKeygen, "-Y", "verify", "-f", signers, "-I", g.Principal,
		"-n", rules.GrantNamespace, "-s", tmp.Name())
	cmd.Stdin = bytes.NewReader(body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fail(fmt.Errorf("signature check failed for %s: %s", g.Principal, strings.TrimSpace(string(out))))
	}
	m := rulesGoodSignatureRE.FindSubmatch(out)
	if m == nil || string(m[1]) != g.Key {
		return fail(errors.New("the grant names a different key from the one that signed it"))
	}
	return g, r, body, sig, nil
}

func rulesSign(ctx context.Context, key, path string, stdin io.Reader) error {
	cmd := exec.CommandContext(ctx, rulesSSHKeygen, "-Y", "sign", "-f", key, "-n", rules.GrantNamespace, path)
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("signing failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// rulesKeyFingerprint reads the public half of key and returns its SHA256
// fingerprint, in the form ssh-keygen prints.
func rulesKeyFingerprint(key string) (string, error) {
	pub := key
	if !strings.HasSuffix(pub, ".pub") {
		pub += ".pub"
	}
	data, err := os.ReadFile(pub)
	if err != nil {
		return "", fmt.Errorf("read public key: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return "", fmt.Errorf("%s is not an SSH public key", pub)
	}
	blob, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return "", fmt.Errorf("%s is not an SSH public key: %w", pub, err)
	}
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), nil
}

// rulesKeepGrant copies an imported grant and its signature into the vault, so
// each rule's trail stays with the rule.
func rulesKeepGrant(vaultDir string, body, sig []byte, g rules.Grant) error {
	dir := filepath.Join(vaultDir, "grants")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	dest := filepath.Join(dir, g.Rule.ID+"-"+g.Nonce[:8]+".json")
	if err := atomicio.Write(dest, body, 0o600); err != nil {
		return err
	}
	return atomicio.Write(dest+".sig", sig, 0o600)
}

func rulesGrantLines(g rules.Grant, r rules.Rule) [][2]string {
	return [][2]string{
		{"rule", r.ID}, {"hash", rules.ShortHash(g.Rule.Hash)},
		{"match", r.Spec.Match}, {"exclude", orNone(r.Spec.Exclude)}, {"unless", orNone(r.Spec.Unless)},
		{"context", orNone(r.Spec.Context)}, {"message", r.Message},
		{"mode", g.Mode}, {"scope", g.FromScope + " to " + g.ToScope},
		{"signer", g.Principal}, {"key", g.Key},
		{"machine, as reported", orNone(g.Machine)}, {"agent, as claimed", orNone(g.AgentClaimed)},
		{"issued", g.IssuedAt}, {"expires", g.ExpiresAt}, {"grant id", g.Nonce[:8]},
	}
}
