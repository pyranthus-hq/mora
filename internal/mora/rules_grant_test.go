package mora

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/rules"
)

const grantTestName = "adit@example.com"

// grantTestKey makes a real key and a signers file that names it.
func grantTestKey(t *testing.T, name string) (key, signers string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("ssh-keygen refuses a key whose file permissions it cannot read on Windows")
	}
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen is not installed")
	}
	dir := t.TempDir()
	key = filepath.Join(dir, "id_test")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	pub, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	signers = filepath.Join(dir, "allowed_signers")
	line := fmt.Sprintf("%s namespaces=%q %s", name, rules.GrantNamespace, pub)
	if err := os.WriteFile(signers, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return key, signers
}

// grantTestSetup approves one rule and signs a grant for it, and returns the
// grant's path, copied out of the vault as a person would copy it.
func grantTestSetup(t *testing.T) (grant, key, signers string) {
	t.Helper()
	key, signers = grantTestKey(t, grantTestName)
	withTempHome(t)
	run(t, "rules", "add", "--id", "no-sudo", "--match", `^\s*sudo\b`, "--message", "Do not use sudo here.")
	stubRulesTerminal(t, "no-sudo")
	run(t, "rules", "approve", "no-sudo", "--mode", "warn")
	run(t, "rules", "grant", "no-sudo", "--to", "factory:aeth-forge", "--as", grantTestName, "--key", key, "--agent", "claude-code")
	cfg := mustConfig(t)
	found, err := filepath.Glob(filepath.Join(cfg.VaultDir, "grants", "no-sudo-*.json"))
	if err != nil || len(found) != 1 {
		t.Fatalf("grants = %v, err = %v", found, err)
	}
	dir := t.TempDir()
	grant = filepath.Join(dir, filepath.Base(found[0]))
	for _, ext := range []string{"", ".sig"} {
		body, err := os.ReadFile(found[0] + ext)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(grant+ext, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return grant, key, signers
}

// stubSignersTrusted makes the signers file count as root-owned, or not.
func stubSignersTrusted(t *testing.T, trusted bool) {
	t.Helper()
	prev := rulesSignersTrusted
	rulesSignersTrusted = func(string) bool { return trusted }
	t.Cleanup(func() { rulesSignersTrusted = prev })
}

func TestRulesGrantVerifyImportRoundTrip(t *testing.T) {
	grant, _, signers := grantTestSetup(t)
	out := run(t, "rules", "verify", grant, "--signers", signers, "--scope", "factory:aeth-forge")
	for _, want := range []string{"verified no-sudo, signed by " + grantTestName, "scope: person to factory:aeth-forge", "agent, as claimed: claude-code"} {
		if !strings.Contains(out, want) {
			t.Fatalf("verify output lacks %q:\n%s", want, out)
		}
	}

	// The second machine has never seen this rule.
	withTempHome(t)
	cfg := mustConfig(t)
	stubSignersTrusted(t, true)
	clearAgentEnv(t)
	prevInteractive := rulesInteractive
	rulesInteractive = func(io.Reader) bool { return false }
	t.Cleanup(func() { rulesInteractive = prevInteractive })
	out = run(t, "rules", "import", grant, "--signers", signers, "--scope", "factory:aeth-forge")
	if !strings.Contains(out, "imported no-sudo (warn), signed by "+grantTestName) {
		t.Fatalf("import output = %q", out)
	}
	if ids := rulesActiveIDs(t, cfg); strings.Join(ids, ",") != "no-sudo" {
		t.Fatalf("active = %v", ids)
	}
	approvals, err := rules.LoadApprovals(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	a := approvals["no-sudo"]
	if a.Approver != grantTestName || !strings.HasPrefix(a.Via, "grant:") || a.Expires == "" {
		t.Fatalf("approval = %+v", a)
	}
	kept, _ := filepath.Glob(filepath.Join(cfg.VaultDir, "grants", "no-sudo-*"))
	if len(kept) != 2 {
		t.Fatalf("kept = %v", kept)
	}
	got := decodeHookOutput(t, runPreTool(t, `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"sudo ls"}}`))
	if !strings.Contains(got.HookSpecificOutput.AdditionalContext, "Do not use sudo here.") {
		t.Fatalf("hook output = %+v", got)
	}
}

func TestRulesGrantRefusesInsideAgentSession(t *testing.T) {
	key, _ := grantTestKey(t, grantTestName)
	withTempHome(t)
	run(t, "rules", "add", "--id", "no-sudo", "--match", `^\s*sudo\b`, "--message", "Do not use sudo here.")
	stubRulesTerminal(t, "no-sudo")
	run(t, "rules", "approve", "no-sudo", "--mode", "warn")
	t.Setenv("CLAUDECODE", "1")
	_, err := runErr(t, "rules", "grant", "no-sudo", "--to", "team:platform", "--as", grantTestName, "--key", key)
	if err == nil || !strings.Contains(err.Error(), "CLAUDECODE") {
		t.Fatalf("err = %v", err)
	}
	if found, _ := filepath.Glob(filepath.Join(mustConfig(t).VaultDir, "grants", "*")); len(found) != 0 {
		t.Fatalf("files written: %v", found)
	}
}

func TestRulesGrantRefusesARuleNotActiveHere(t *testing.T) {
	key, _ := grantTestKey(t, grantTestName)
	withTempHome(t)
	run(t, "rules", "add", "--id", "no-sudo", "--match", `^\s*sudo\b`, "--message", "Do not use sudo here.")
	stubRulesTerminal(t, "no-sudo")
	_, err := runErr(t, "rules", "grant", "no-sudo", "--to", "team:platform", "--as", grantTestName, "--key", key)
	if err == nil || !strings.Contains(err.Error(), "not approved here") {
		t.Fatalf("not approved: err = %v", err)
	}
	run(t, "rules", "approve", "no-sudo", "--mode", "warn")
	run(t, "rules", "off", "no-sudo")
	_, err = runErr(t, "rules", "grant", "no-sudo", "--to", "team:platform", "--as", grantTestName, "--key", key)
	if err == nil || !strings.Contains(err.Error(), "only an active rule") {
		t.Fatalf("off: err = %v", err)
	}
}

func TestRulesGrantBadArguments(t *testing.T) {
	withTempHome(t)
	stubRulesTerminal(t, "no-sudo")
	cases := map[string][]string{
		"person scope": {"no-sudo", "--to", "person", "--as", "a", "--key", "k"},
		"bad name":     {"no-sudo", "--to", "team", "--as", "-Ofoo", "--key", "k"},
		"zero days":    {"no-sudo", "--to", "team", "--as", "a", "--key", "k", "--days", "0"},
		"long days":    {"no-sudo", "--to", "team", "--as", "a", "--key", "k", "--days", "366"},
		"no key":       {"no-sudo", "--to", "team", "--as", "a"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := runErr(t, append([]string{"rules", "grant"}, args...)...); err == nil {
				t.Fatal("passed")
			}
		})
	}
}

func TestRulesVerifyRefusesTamperedGrant(t *testing.T) {
	grant, _, signers := grantTestSetup(t)
	body, err := os.ReadFile(grant)
	if err != nil {
		t.Fatal(err)
	}
	// Still a valid grant by every field check. Only the signature can tell.
	changed := strings.Replace(string(body), "factory:aeth-forge", "factory:other", 1)
	if changed == string(body) {
		t.Fatal("nothing to tamper with")
	}
	if err := os.WriteFile(grant, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = runErr(t, "rules", "verify", grant, "--signers", signers)
	if err == nil || !strings.Contains(err.Error(), "signature check failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestRulesVerifyRefusesAnUnlistedSigner(t *testing.T) {
	grant, _, _ := grantTestSetup(t)
	_, stranger := grantTestKey(t, grantTestName)
	_, err := runErr(t, "rules", "verify", grant, "--signers", stranger)
	if err == nil || !strings.Contains(err.Error(), "signature check failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestRulesVerifyRefusesTheWrongScope(t *testing.T) {
	grant, _, signers := grantTestSetup(t)
	_, err := runErr(t, "rules", "verify", grant, "--signers", signers, "--scope", "team:platform")
	if err == nil || !strings.Contains(err.Error(), "targets factory:aeth-forge") {
		t.Fatalf("err = %v", err)
	}
}

func TestRulesVerifyRefusesAnExpiredGrant(t *testing.T) {
	grant, _, signers := grantTestSetup(t)
	prev := rulesNow
	rulesNow = func() time.Time { return time.Now().AddDate(0, 0, 31) }
	t.Cleanup(func() { rulesNow = prev })
	_, err := runErr(t, "rules", "verify", grant, "--signers", signers)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err = %v", err)
	}
}

// A signer may not name another key in the grant, because the log would then
// credit the wrong key.
func TestRulesVerifyRefusesAGrantThatNamesAnotherKey(t *testing.T) {
	grant, key, signers := grantTestSetup(t)
	body, err := os.ReadFile(grant)
	if err != nil {
		t.Fatal(err)
	}
	g, err := rules.ParseGrant(body)
	if err != nil {
		t.Fatal(err)
	}
	g.Key = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	forged, err := rules.RenderGrant(g)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(grant, forged, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(grant + ".sig"); err != nil {
		t.Fatal(err)
	}
	if err := rulesSign(context.Background(), key, grant, strings.NewReader("")); err != nil {
		t.Fatal(err)
	}
	_, err = runErr(t, "rules", "verify", grant, "--signers", signers)
	if err == nil || !strings.Contains(err.Error(), "different key") {
		t.Fatalf("err = %v", err)
	}
}

func TestRulesImportNeedsAPersonWhenSignersAreWritable(t *testing.T) {
	grant, _, signers := grantTestSetup(t)
	withTempHome(t)
	cfg := mustConfig(t)
	stubSignersTrusted(t, false)
	if err := os.Setenv("CLAUDECODE", "1"); err != nil {
		t.Fatal(err)
	}
	_, err := runErr(t, "rules", "import", grant, "--signers", signers, "--scope", "factory:aeth-forge")
	os.Unsetenv("CLAUDECODE")
	if err == nil || !strings.Contains(err.Error(), "inside an agent session") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(rules.RulePath(cfg.VaultDir, "no-sudo")); !os.IsNotExist(err) {
		t.Fatalf("rule file written: %v", err)
	}
	withTestVerifiedOwnerAction(t)
	stubRulesTerminal(t, "no-sudo")
	run(t, "rules", "import", grant, "--signers", signers, "--scope", "factory:aeth-forge")
	if ids := rulesActiveIDs(t, cfg); strings.Join(ids, ",") != "no-sudo" {
		t.Fatalf("active = %v", ids)
	}
}

func TestRulesImportRefusesReplayAfterOff(t *testing.T) {
	grant, _, signers := grantTestSetup(t)
	withTempHome(t)
	cfg := mustConfig(t)
	stubSignersTrusted(t, true)
	stubRulesTerminal(t, "no-sudo")
	run(t, "rules", "import", grant, "--signers", signers, "--scope", "factory:aeth-forge")
	run(t, "rules", "off", "no-sudo")
	_, err := runErr(t, "rules", "import", grant, "--signers", signers, "--scope", "factory:aeth-forge")
	if err == nil || !strings.Contains(err.Error(), "already imported") {
		t.Fatalf("err = %v", err)
	}
	if ids := rulesActiveIDs(t, cfg); len(ids) != 0 {
		t.Fatalf("active = %v", ids)
	}
}

func TestRulesImportNeedsAScope(t *testing.T) {
	grant, _, signers := grantTestSetup(t)
	if _, err := runErr(t, "rules", "import", grant, "--signers", signers); err == nil {
		t.Fatal("import without a scope passed")
	}
}

func TestRulesKeyFingerprintMatchesSSHKeygen(t *testing.T) {
	key, _ := grantTestKey(t, grantTestName)
	got, err := rulesKeyFingerprint(key)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("ssh-keygen", "-l", "-E", "sha256", "-f", key+".pub").CombinedOutput()
	if err != nil {
		t.Fatalf("ssh-keygen: %v: %s", err, out)
	}
	if !strings.Contains(string(out), got) {
		t.Fatalf("fingerprint %s not in %s", got, out)
	}
}
