package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testGrant(t *testing.T, now time.Time) Grant {
	t.Helper()
	r := Rule{ID: "no-sudo", Message: "Do not use sudo here.", Spec: Spec{Version: 1, Tool: "Bash", Match: `^\s*sudo\b`}}
	return Grant{
		Kind: GrantKind, V: 1,
		Rule: GrantRule{ID: r.ID, Hash: Hash(r), File: string(Render(r))},
		Mode: ModeWarn, FromScope: GrantFromScope, ToScope: "factory:aeth-forge",
		Principal: "adit@example.com", Key: "SHA256:abc", Machine: "laptop",
		IssuedAt:  now.Format(time.RFC3339),
		ExpiresAt: now.Add(24 * time.Hour).Format(time.RFC3339),
		Nonce:     strings.Repeat("a", 32),
	}
}

func TestCheckGrantAcceptsAValidGrant(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	r, err := CheckGrant(testGrant(t, now), now)
	if err != nil || r.ID != "no-sudo" {
		t.Fatalf("rule = %+v, err = %v", r, err)
	}
}

func TestCheckGrantRefuses(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	cases := map[string]struct {
		change func(*Grant)
		at     time.Time
		want   string
	}{
		"kind":        {func(g *Grant) { g.Kind = "other" }, now, "not a version 1"},
		"version":     {func(g *Grant) { g.V = 2 }, now, "not a version 1"},
		"off mode":    {func(g *Grant) { g.Mode = ModeOff }, now, "mode must be"},
		"block mode":  {func(g *Grant) { g.Mode = "block" }, now, "mode must be"},
		"from scope":  {func(g *Grant) { g.FromScope = "team" }, now, "scope"},
		"to person":   {func(g *Grant) { g.ToScope = "person" }, now, "scope"},
		"flag name":   {func(g *Grant) { g.Principal = "-Ofoo" }, now, "principal"},
		"short nonce": {func(g *Grant) { g.Nonce = "abc" }, now, "nonce"},
		"bad key":     {func(g *Grant) { g.Key = "MD5:abc" }, now, "fingerprint"},
		"expired":     {func(*Grant) {}, now.Add(25 * time.Hour), "expired"},
		"at expiry":   {func(*Grant) {}, now.Add(24 * time.Hour), "expired"},
		"future":      {func(*Grant) {}, now.Add(-time.Hour), "future"},
		"backward": {func(g *Grant) {
			g.ExpiresAt = now.Add(-time.Minute).Format(time.RFC3339)
		}, now, "before it was issued"},
		"swapped text": {func(g *Grant) {
			g.Rule.File = strings.Replace(g.Rule.File, "Do not use sudo here.", "Use sudo freely.", 1)
		}, now, "does not match its hash"},
		"other id": {func(g *Grant) { g.Rule.ID = "other" }, now, "names rule"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			g := testGrant(t, now)
			c.change(&g)
			if _, err := CheckGrant(g, c.at); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestParseGrantRefusesExtraData(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	body, err := RenderGrant(testGrant(t, now))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseGrant(body); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseGrant([]byte(strings.Replace(string(body), `"v": 1,`, `"v": 1, "extra": true,`, 1))); err == nil {
		t.Fatal("an unknown field passed")
	}
	if _, err := ParseGrant(append(body, []byte("{}")...)); err == nil {
		t.Fatal("trailing data passed")
	}
	if _, err := ParseGrant(make([]byte, MaxGrantBytes+1)); err == nil {
		t.Fatal("an oversize file passed")
	}
}

func TestCheckGrantFreshRefusesReplayAndRollback(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	g := testGrant(t, now)
	if err := CheckGrantFresh(dir, g); err != nil {
		t.Fatalf("empty log: %v", err)
	}
	if err := AppendGrantSeen(dir, GrantSeen{Nonce: g.Nonce, RuleID: g.Rule.ID, IssuedAt: g.IssuedAt}); err != nil {
		t.Fatal(err)
	}
	if err := CheckGrantFresh(dir, g); err == nil || !strings.Contains(err.Error(), "already imported") {
		t.Fatalf("replay: %v", err)
	}
	older := testGrant(t, now.Add(-time.Hour))
	older.Nonce = strings.Repeat("b", 32)
	if err := CheckGrantFresh(dir, older); err == nil || !strings.Contains(err.Error(), "newer grant") {
		t.Fatalf("rollback: %v", err)
	}
	newer := testGrant(t, now.Add(time.Hour))
	newer.Nonce = strings.Repeat("c", 32)
	if err := CheckGrantFresh(dir, newer); err != nil {
		t.Fatalf("newer grant: %v", err)
	}
	other := testGrant(t, now.Add(-time.Hour))
	other.Nonce = strings.Repeat("d", 32)
	other.Rule.ID = "other"
	if err := CheckGrantFresh(dir, other); err != nil {
		t.Fatalf("other rule: %v", err)
	}
}

func TestExpiredApprovalLeavesTheActiveSet(t *testing.T) {
	vault, state := t.TempDir(), t.TempDir()
	r := Rule{ID: "no-sudo", Message: "Do not use sudo here.", Spec: Spec{Version: 1, Tool: "Bash", Match: `^\s*sudo\b`}}
	if err := writeRuleForTest(vault, r); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if err := AppendApproval(state, Approval{RuleID: r.ID, RuleHash: Hash(r), Mode: ModeWarn, Via: "grant:x", Expires: past}); err != nil {
		t.Fatal(err)
	}
	if _, st := Check(vault, Approval{RuleID: r.ID, RuleHash: Hash(r), Mode: ModeWarn, Expires: past}); st != StateExpired {
		t.Fatalf("state = %q", st)
	}
	set, err := Compile(vault, state)
	if err != nil || len(set.Rules) != 0 {
		t.Fatalf("set = %+v, err = %v", set, err)
	}
}

func TestActiveRuleExpired(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	if (ActiveRule{}).Expired(now) {
		t.Fatal("no expiry counted as expired")
	}
	if (ActiveRule{Expires: now.Add(time.Hour).Format(time.RFC3339)}).Expired(now) {
		t.Fatal("a future expiry counted as expired")
	}
	if !(ActiveRule{Expires: now.Format(time.RFC3339)}).Expired(now) {
		t.Fatal("the expiry moment did not count")
	}
	if !(ActiveRule{Expires: "soon"}).Expired(now) {
		t.Fatal("a bad expiry did not count as expired")
	}
}

func writeRuleForTest(vault string, r Rule) error {
	path := RulePath(vault, r.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, Render(r), 0o600)
}
