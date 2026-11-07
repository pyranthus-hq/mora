package rules

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// A grant is a person's signed statement that a rule may apply outside their
// own machine. The signature covers the exact bytes of the grant file, so
// nothing here canonicalizes. Parsing and checking stay in this package;
// signing and verifying the signature run ssh-keygen and live in internal/mora.
const (
	GrantKind      = "mora.rule.grant"
	GrantNamespace = "mora-rule-grant"
	GrantFromScope = "person"
	MaxGrantBytes  = 96 << 10

	// grantClockSkew allows a signer's clock to run ahead of the verifier's.
	grantClockSkew = 5 * time.Minute
)

var (
	grantScopeRE     = regexp.MustCompile(`^(project|team|factory|company)(:[a-z0-9][a-z0-9._-]{0,63})?$`)
	grantPrincipalRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]{0,127}$`)
	grantNonceRE     = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// GrantRule carries the whole rule file, so a verifier needs nothing else.
type GrantRule struct {
	ID   string `json:"id"`
	Hash string `json:"hash"`
	File string `json:"file"`
}

// Grant is the signed record. Principal is who signed, and the verifier's
// allowed_signers file decides whether that name owns the key. Machine and
// AgentClaimed are what the signer's tool reported. Neither is proven.
type Grant struct {
	Kind         string    `json:"kind"`
	V            int       `json:"v"`
	Rule         GrantRule `json:"rule"`
	Mode         string    `json:"mode"`
	FromScope    string    `json:"from_scope"`
	ToScope      string    `json:"to_scope"`
	Principal    string    `json:"principal"`
	Key          string    `json:"key"`
	Machine      string    `json:"machine"`
	AgentClaimed string    `json:"agent_claimed,omitempty"`
	IssuedAt     string    `json:"issued_at"`
	ExpiresAt    string    `json:"expires_at"`
	Nonce        string    `json:"nonce"`
}

// ValidScope reports whether s can be a grant's target. A grant always moves a
// rule up from a person, so "person" is not a target.
func ValidScope(s string) bool { return grantScopeRE.MatchString(s) }

// ValidPrincipal reports whether s is safe to pass to ssh-keygen as a name.
func ValidPrincipal(s string) bool { return grantPrincipalRE.MatchString(s) }

// RenderGrant is the file form of a grant.
func RenderGrant(g Grant) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(g); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ParseGrant decodes a grant file strictly. An unknown field is an error, so a
// field a verifier would ignore cannot ride along under the signature.
func ParseGrant(data []byte) (Grant, error) {
	if len(data) > MaxGrantBytes {
		return Grant{}, fmt.Errorf("grant file is larger than %d bytes", MaxGrantBytes)
	}
	var g Grant
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&g); err != nil {
		return Grant{}, fmt.Errorf("grant file: %w", err)
	}
	if dec.More() {
		return Grant{}, errors.New("grant file has trailing data")
	}
	return g, nil
}

// CheckGrant checks everything but the signature and returns the rule inside.
func CheckGrant(g Grant, now time.Time) (Rule, error) {
	if g.Kind != GrantKind || g.V != 1 {
		return Rule{}, fmt.Errorf("not a version 1 %s", GrantKind)
	}
	if g.Mode != ModeWarn && g.Mode != ModeShadow {
		return Rule{}, fmt.Errorf("grant mode must be warn or shadow, got %q", g.Mode)
	}
	if g.FromScope != GrantFromScope || !ValidScope(g.ToScope) {
		return Rule{}, fmt.Errorf("grant scope %q to %q is not valid", g.FromScope, g.ToScope)
	}
	if !ValidPrincipal(g.Principal) {
		return Rule{}, fmt.Errorf("grant principal %q is not valid", g.Principal)
	}
	if !strings.HasPrefix(g.Key, "SHA256:") {
		return Rule{}, errors.New("grant key is not a SHA256 fingerprint")
	}
	if !grantNonceRE.MatchString(g.Nonce) {
		return Rule{}, errors.New("grant nonce is not valid")
	}
	issued, err := time.Parse(time.RFC3339, g.IssuedAt)
	if err != nil {
		return Rule{}, fmt.Errorf("grant issued_at: %w", err)
	}
	expires, err := time.Parse(time.RFC3339, g.ExpiresAt)
	if err != nil {
		return Rule{}, fmt.Errorf("grant expires_at: %w", err)
	}
	if !expires.After(issued) {
		return Rule{}, errors.New("grant expires before it was issued")
	}
	if issued.After(now.Add(grantClockSkew)) {
		return Rule{}, errors.New("grant was issued in the future")
	}
	if !now.Before(expires) {
		return Rule{}, fmt.Errorf("grant expired at %s", g.ExpiresAt)
	}
	r, err := Parse([]byte(g.Rule.File))
	if err != nil {
		return Rule{}, fmt.Errorf("grant rule: %w", err)
	}
	if err := Validate(r); err != nil {
		return Rule{}, fmt.Errorf("grant rule: %w", err)
	}
	if r.ID != g.Rule.ID {
		return Rule{}, fmt.Errorf("grant names rule %q but its file is %q", g.Rule.ID, r.ID)
	}
	if Hash(r) != g.Rule.Hash {
		return Rule{}, errors.New("grant rule file does not match its hash")
	}
	return r, nil
}

// GrantSeen is one row of the imported-grants log. The log is what stops an
// old grant from turning a rule back on after someone turned it off.
type GrantSeen struct {
	V        int    `json:"v"`
	TS       string `json:"ts"`
	Nonce    string `json:"nonce"`
	RuleID   string `json:"rule_id"`
	IssuedAt string `json:"issued_at"`
}

// GrantsSeenPath is <state>/rules/grants.jsonl.
func GrantsSeenPath(stateDir string) string { return filepath.Join(Dir(stateDir), "grants.jsonl") }

// AppendGrantSeen records an imported grant with a single O_APPEND write.
func AppendGrantSeen(stateDir string, row GrantSeen) error {
	row.V = 1
	if row.TS == "" {
		row.TS = time.Now().UTC().Format(TSLayout)
	}
	body, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return appendLine(GrantsSeenPath(stateDir), append(body, '\n'))
}

// CheckGrantFresh refuses a grant that was imported before, or one older than
// the newest grant already imported for the same rule.
func CheckGrantFresh(stateDir string, g Grant) error {
	data, err := os.ReadFile(GrantsSeenPath(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	issued, err := time.Parse(time.RFC3339, g.IssuedAt)
	if err != nil {
		return fmt.Errorf("grant issued_at: %w", err)
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		var row GrantSeen
		if json.Unmarshal(line, &row) != nil || row.V != 1 {
			continue
		}
		if row.Nonce == g.Nonce {
			return errors.New("this grant was already imported here")
		}
		if row.RuleID != g.Rule.ID {
			continue
		}
		if seen, err := time.Parse(time.RFC3339, row.IssuedAt); err == nil && issued.Before(seen) {
			return fmt.Errorf("a newer grant for %s was already imported here", g.Rule.ID)
		}
	}
	return nil
}
