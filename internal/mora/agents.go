package mora

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

// Agent profiles (#539) bind one MCP client to a policy Mora enforces, so the
// model is outside the trust boundary: which authored scopes it may read,
// whether connector evidence is visible at all, how its writes land, and which
// tools exist for it. A profile is selected by the transport, never by the
// caller: `mora mcp serve --profile <name>` for stdio, and the bearer token for
// `mora mcp serve-http`. A request cannot widen its own profile.
//
// The store is <ConfigDir>/agents.json (0600). Tokens are shown once at
// creation and only their SHA-256 is stored.

const (
	agentScopesAll    = "all"
	agentTokenPrefix  = "mora_agent_"
	agentsStoreSchema = 1
	agentQueryReceipt = 200
)

// agentScopeSafeTools are the tools whose results can be filtered row by row
// against a profile. Every other tool renders text or aggregates across the
// whole vault, so it is only available to a profile that may already read
// everything, raw sources included. delete_memory is in that second group: it
// acts on any id it is given, so a scoped profile could delete a record it
// cannot read.
var agentScopeSafeTools = map[string]bool{
	"search_memory": true,
	"read_memory":   true,
	"list_memory":   true,
	"write_memory":  true,
}

var agentDefaultTools = []string{"search_memory", "read_memory", "list_memory", "write_memory"}

var agentNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

var agentProjectScopePattern = regexp.MustCompile(`^project:[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// validScope accepts the scope vocabulary an agent profile may be granted.
func validScope(s string) bool {
	return s == "personal" || s == "global" || agentProjectScopePattern.MatchString(s)
}

type agentProfile struct {
	Name        string   `json:"name"`
	TokenSHA256 string   `json:"token_sha256"`
	ReadScopes  []string `json:"read_scopes"`
	RawSources  bool     `json:"raw_sources"`
	Write       string   `json:"write"`
	Tools       []string `json:"tools"`
	CreatedAt   string   `json:"created_at"`
	RevokedAt   string   `json:"revoked_at,omitempty"`
}

type agentsStore struct {
	Schema   int            `json:"schema"`
	Profiles []agentProfile `json:"profiles"`
}

func agentsStorePath(cfg Config) string { return filepath.Join(cfg.ConfigDir, "agents.json") }

func agentReceiptsPath(cfg Config, name string) string {
	return filepath.Join(cfg.StateDir, "agents", name+".jsonl")
}

func loadAgentsStore(cfg Config) (agentsStore, error) {
	b, err := os.ReadFile(agentsStorePath(cfg))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return agentsStore{Schema: agentsStoreSchema}, nil
		}
		return agentsStore{}, err
	}
	var s agentsStore
	if err := json.Unmarshal(b, &s); err != nil {
		return agentsStore{}, fmt.Errorf("parse %s: %w", agentsStorePath(cfg), err)
	}
	if s.Schema != agentsStoreSchema {
		return agentsStore{}, fmt.Errorf("%s has schema %d, want %d", agentsStorePath(cfg), s.Schema, agentsStoreSchema)
	}
	for _, p := range s.Profiles {
		if err := p.validate(); err != nil {
			return agentsStore{}, fmt.Errorf("%s: %w", agentsStorePath(cfg), err)
		}
	}
	return s, nil
}

func saveAgentsStore(cfg Config, s agentsStore) error {
	s.Schema = agentsStoreSchema
	if err := os.MkdirAll(cfg.ConfigDir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.Write(agentsStorePath(cfg), append(b, '\n'), 0o600)
}

func (s agentsStore) find(name string) (int, bool) {
	for i, p := range s.Profiles {
		if p.Name == name {
			return i, true
		}
	}
	return -1, false
}

// active returns the unrevoked profile with this name.
func (s agentsStore) active(name string) (agentProfile, error) {
	i, ok := s.find(name)
	if !ok {
		return agentProfile{}, fmt.Errorf("no agent profile %q (create one with `mora agents add`)", name)
	}
	if s.Profiles[i].RevokedAt != "" {
		return agentProfile{}, fmt.Errorf("agent profile %q was revoked at %s", name, s.Profiles[i].RevokedAt)
	}
	return s.Profiles[i], nil
}

// matchToken maps a presented bearer token to exactly one active profile. Every
// profile is compared, in constant time per comparison, so the answer does not
// depend on where in the list the match sits.
func (s agentsStore) matchToken(token string) (agentProfile, bool) {
	if !strings.HasPrefix(token, agentTokenPrefix) {
		return agentProfile{}, false
	}
	sum := sha256.Sum256([]byte(token))
	presented := []byte(hex.EncodeToString(sum[:]))
	var found agentProfile
	ok := false
	for _, p := range s.Profiles {
		if subtle.ConstantTimeCompare(presented, []byte(p.TokenSHA256)) == 1 && p.RevokedAt == "" {
			found, ok = p, true
		}
	}
	return found, ok
}

func (s agentsStore) activeCount() int {
	n := 0
	for _, p := range s.Profiles {
		if p.RevokedAt == "" {
			n++
		}
	}
	return n
}

func (p agentProfile) validate() error {
	if !agentNamePattern.MatchString(p.Name) {
		return fmt.Errorf("agent profile name %q must match %s", p.Name, agentNamePattern)
	}
	switch p.Write {
	case mcpWritePolicyOpen, mcpWritePolicyPropose, mcpWritePolicyReadonly:
	default:
		return fmt.Errorf("agent profile %q has invalid write policy %q (open|propose|readonly)", p.Name, p.Write)
	}
	if len(p.ReadScopes) == 0 {
		return fmt.Errorf("agent profile %q has no read scopes", p.Name)
	}
	for _, sc := range p.ReadScopes {
		if sc == agentScopesAll {
			if len(p.ReadScopes) != 1 {
				return fmt.Errorf("agent profile %q: %q cannot be combined with other scopes", p.Name, agentScopesAll)
			}
			continue
		}
		if !validScope(sc) {
			return fmt.Errorf("agent profile %q has invalid scope %q", p.Name, sc)
		}
	}
	for _, t := range p.Tools {
		if _, ok := mcpToolIndex[t]; !ok {
			return fmt.Errorf("agent profile %q names unknown tool %q", p.Name, t)
		}
		if t == "delete_memory" && p.Write != mcpWritePolicyOpen {
			return fmt.Errorf("agent profile %q: delete_memory requires write=open", p.Name)
		}
		if !agentScopeSafeTools[t] && !p.unrestricted() {
			return fmt.Errorf("agent profile %q: %s cannot be filtered per row, so it needs --read-scopes all --raw-sources", p.Name, t)
		}
	}
	return nil
}

// unrestricted reports whether the profile may already see the whole vault,
// raw sources included; only then do non-filterable tools become available.
func (p agentProfile) unrestricted() bool {
	return p.RawSources && len(p.ReadScopes) == 1 && p.ReadScopes[0] == agentScopesAll
}

func (p agentProfile) allowsTool(name string) bool {
	for _, t := range p.Tools {
		if t == name {
			return true
		}
	}
	return false
}

func (p agentProfile) allowsScope(scope string) bool {
	for _, sc := range p.ReadScopes {
		if sc == agentScopesAll || sc == scope {
			return true
		}
	}
	return false
}

func (p agentProfile) singleScope() string {
	if len(p.ReadScopes) == 1 && p.ReadScopes[0] != agentScopesAll {
		return p.ReadScopes[0]
	}
	return ""
}

// allowsRow is the per-record read decision: the scope must be granted, and
// with raw_sources off the record must be authored (no connector, no copied
// document). Shared-corpus rows carry their publisher's scope and no provider,
// so a granted scope admits them exactly like a local authored memory.
func (p agentProfile) allowsRow(row map[string]any) bool {
	scope, _ := row["scope"].(string)
	if scope == "" || !p.allowsScope(scope) {
		return false
	}
	if p.RawSources {
		return true
	}
	if provider, _ := row["provider"].(string); provider != "" {
		return false
	}
	switch prov, _ := row["provenance"].(string); prov {
	case "evidence", "document":
		return false
	}
	return true
}

type agentProfileKey struct{}

func withAgentProfile(ctx context.Context, p agentProfile) context.Context {
	return context.WithValue(ctx, agentProfileKey{}, p)
}

func agentProfileFrom(ctx context.Context) (agentProfile, bool) {
	p, ok := ctx.Value(agentProfileKey{}).(agentProfile)
	return p, ok
}

// admitAgentCall decides, before any handler runs, whether this profile may
// make this call, and returns the arguments the handler will actually see:
// scope is pinned or checked, the connector filter is refused when raw sources
// are hidden, and writes are stamped with the agent's own source label so an
// approved memory never reads as hand-written.
func admitAgentCall(p agentProfile, name string, args map[string]any) (map[string]any, error) {
	if !p.allowsTool(name) {
		return nil, fmt.Errorf("tool %q is not available to agent profile %q", name, p.Name)
	}
	shaped := make(map[string]any, len(args)+2)
	for k, v := range args {
		shaped[k] = v
	}
	if !p.RawSources {
		if src, _ := shaped["source"].(string); src != "" && name != "write_memory" {
			return nil, fmt.Errorf("agent profile %q cannot filter by connector source: raw sources are not visible to it", p.Name)
		}
	}
	scope, _ := shaped["scope"].(string)
	switch name {
	case "write_memory":
		if scope == "" {
			scope = p.singleScope()
			if scope == "" {
				scope = "global"
			}
			shaped["scope"] = scope
		}
		if !p.allowsScope(scope) {
			return nil, fmt.Errorf("agent profile %q may not write to scope %q", p.Name, scope)
		}
		shaped["source"] = "agent:" + p.Name
	case "search_memory", "list_memory":
		if scope == "" {
			if s := p.singleScope(); s != "" {
				shaped["scope"] = s
			}
		} else if !p.allowsScope(scope) {
			return nil, fmt.Errorf("scope %q is not readable by agent profile %q", scope, p.Name)
		}
	}
	return shaped, nil
}

// agentRowScrubKeys name nested fields that point at OTHER records (titles,
// snippets, ids), plus the local file path. A kept row must not carry a pointer to a record the profile
// could not read, so these are removed wholesale rather than filtered.
var agentRowScrubKeys = []string{"path", "corroborating", "later_related_evidence", "collapsed_evidence_ids", "evidence", "participation"}

// filterAgentResult projects a handler's native value down to what the profile
// may see. It works on the JSON shape the client would receive, so it cannot be
// bypassed by a Go type the filter does not know about: every array of rows is
// filtered by allowsRow, every receipt array keyed by evidence_id keeps only
// ids that survived, and a read of a record the profile cannot see answers
// exactly like a missing id.
func filterAgentResult(p agentProfile, name string, args map[string]any, value any) (any, []agentRowRef, error) {
	if value == nil {
		return nil, nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, nil, fmt.Errorf("agent profile %q: %s returned a shape the profile filter cannot inspect", p.Name, name)
	}
	var kept []agentRowRef
	if mem, ok := out["memory"].(map[string]any); ok {
		if !p.allowsRow(mem) {
			if name == "write_memory" {
				return nil, nil, fmt.Errorf("agent profile %q: write result is outside the profile", p.Name)
			}
			return nil, nil, fmt.Errorf("memory %q not found", strArg(args, "id", ""))
		}
		scrubAgentRow(mem)
		kept = append(kept, rowRef(mem))
	}
	keptIDs := map[string]bool{}
	for k, v := range out {
		rows, ok := v.([]any)
		if !ok || len(rows) == 0 {
			continue
		}
		first, ok := rows[0].(map[string]any)
		if !ok {
			continue
		}
		if _, isRow := first["scope"]; !isRow {
			continue
		}
		filtered := make([]any, 0, len(rows))
		for _, r := range rows {
			row, ok := r.(map[string]any)
			if !ok || !p.allowsRow(row) {
				continue
			}
			scrubAgentRow(row)
			filtered = append(filtered, row)
			ref := rowRef(row)
			kept = append(kept, ref)
			keptIDs[ref.ID] = true
		}
		out[k] = filtered
	}
	for k, v := range out {
		rows, ok := v.([]any)
		if !ok || len(rows) == 0 {
			continue
		}
		first, ok := rows[0].(map[string]any)
		if !ok {
			continue
		}
		if _, isReceipt := first["evidence_id"]; !isReceipt {
			continue
		}
		filtered := make([]any, 0, len(rows))
		for _, r := range rows {
			row, ok := r.(map[string]any)
			if !ok {
				continue
			}
			id, _ := row["evidence_id"].(string)
			base, _, _ := strings.Cut(id, "#")
			if keptIDs[id] || keptIDs[base] {
				scrubAgentRow(row)
				filtered = append(filtered, row)
			}
		}
		// A gap in the rank positions would show that a hidden row ranked
		// above a kept one, so the kept rows are numbered again.
		for i, r := range filtered {
			if row, ok := r.(map[string]any); ok {
				if _, has := row["position"]; has {
					row["position"] = i + 1
				}
			}
		}
		out[k] = filtered
	}
	if !p.RawSources {
		delete(out, "freshness")
		delete(out, "excluded_by_filter")
	}
	if !p.unrestricted() {
		// The count of rows cut for size includes rows this profile cannot
		// see, so it would show that they exist.
		delete(out, "results_truncated")
	}
	return out, kept, nil
}

func scrubAgentRow(row map[string]any) {
	for _, k := range agentRowScrubKeys {
		delete(row, k)
	}
}

type agentRowRef struct {
	ID    string `json:"id"`
	Scope string `json:"scope"`
}

func rowRef(row map[string]any) agentRowRef {
	id, _ := row["id"].(string)
	scope, _ := row["scope"].(string)
	return agentRowRef{ID: id, Scope: scope}
}

// agentReceipt is one line of a profile's read/write log. It records what the
// agent asked for and which records it was handed, never any memory text.
type agentReceipt struct {
	At         string        `json:"at"`
	Tool       string        `json:"tool"`
	Action     string        `json:"action"`
	Query      string        `json:"query,omitempty"`
	Rows       []agentRowRef `json:"rows,omitempty"`
	ProposalID string        `json:"proposal_id,omitempty"`
	Error      string        `json:"error,omitempty"`
}

// appendAgentReceipt adds one line to the profile's receipt log. It reports
// every failure, including a failed close, because a lost line is a read the
// owner can no longer account for.
func appendAgentReceipt(cfg Config, profile string, r agentReceipt) error {
	path := agentReceiptsPath(cfg, profile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(append(b, '\n'))
	return errors.Join(writeErr, f.Close())
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func newAgentToken() (string, string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	token := agentTokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}

// ---------- CLI ----------

const agentsUsage = `usage: mora agents <add|update|list|revoke|rotate|log>
  mora agents add <name> --read-scopes <scope,...|all> [--write propose|readonly|open] [--raw-sources] [--tools t1,t2]
  mora agents update <name> [--read-scopes ...] [--write ...] [--raw-sources=true|false] [--tools ...]
  mora agents list [--json]
  mora agents revoke <name>
  mora agents rotate <name>
  mora agents log <name> [--since 24h] [--json]`

const (
	agentsUpdateUsage = "usage: mora agents update <name> [--read-scopes ...] [--write ...] [--raw-sources=true|false] [--tools ...]"
	agentsListUsage   = "usage: mora agents list [--json]"
	agentsLogUsage    = "usage: mora agents log <name> [--since 24h] [--json]"
)

func cmdAgents(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(agentsUsage)
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	switch args[0] {
	case "add":
		return agentsAdd(cfg, args[1:], stdout)
	case "update":
		return agentsUpdate(cfg, args[1:], stdout)
	case "list":
		return agentsList(cfg, args[1:], stdout)
	case "revoke":
		return agentsRevoke(cfg, args[1:], stdout)
	case "rotate":
		return agentsRotate(cfg, args[1:], stdout)
	case "log":
		return agentsLog(cfg, args[1:], stdout)
	default:
		return errors.New(agentsUsage)
	}
}

// splitNameFirst lets `add <name> --flags` and `add --flags <name>` both parse.
func splitNameFirst(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func agentsAdd(cfg Config, args []string, stdout io.Writer) error {
	name, rest := splitNameFirst(args)
	fs := flag.NewFlagSet("agents add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	scopes := fs.String("read-scopes", "", "comma-separated scopes, or all")
	write := fs.String("write", mcpWritePolicyPropose, "open|propose|readonly")
	rawSources := fs.Bool("raw-sources", false, "expose connector evidence (mail, messages, calendar, files)")
	tools := fs.String("tools", strings.Join(agentDefaultTools, ","), "comma-separated tool allowlist")
	if err := fs.Parse(rest); err != nil {
		return fmt.Errorf("%v\n%s", err, agentsUsage)
	}
	if name == "" && fs.NArg() == 1 {
		name = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return errors.New(agentsUsage)
	}
	s, err := loadAgentsStore(cfg)
	if err != nil {
		return err
	}
	if _, exists := s.find(name); exists {
		return fmt.Errorf("agent profile %q already exists (use `mora agents rotate %s` for a new token)", name, name)
	}
	token, hash, err := newAgentToken()
	if err != nil {
		return err
	}
	p := agentProfile{
		Name: name, TokenSHA256: hash, ReadScopes: splitList(*scopes), RawSources: *rawSources,
		Write: *write, Tools: splitList(*tools), CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := p.validate(); err != nil {
		return err
	}
	s.Profiles = append(s.Profiles, p)
	if err := saveAgentsStore(cfg, s); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Created agent profile %q: read %s, raw sources %s, writes %s, tools %s.\n",
		p.Name, strings.Join(p.ReadScopes, ","), onOff(p.RawSources), p.Write, strings.Join(p.Tools, ","))
	fmt.Fprintf(stdout, "Token (shown once; store it in the agent's secret field, never in chat):\n%s\n", token)
	return nil
}

// agentsUpdate changes a profile's policy in place and keeps its token, so a
// connected agent picks up the new boundary on its next call. Only the flags
// given are changed; the result is validated exactly like `agents add`.
func agentsUpdate(cfg Config, args []string, stdout io.Writer) error {
	name, rest := splitNameFirst(args)
	fs := flag.NewFlagSet("agents update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	scopes := fs.String("read-scopes", "", "comma-separated scopes, or all")
	write := fs.String("write", "", "open|propose|readonly")
	rawSources := fs.Bool("raw-sources", false, "expose connector evidence")
	tools := fs.String("tools", "", "comma-separated tool allowlist")
	if err := fs.Parse(rest); err != nil {
		return fmt.Errorf("%v\n%s", err, agentsUpdateUsage)
	}
	if name == "" || fs.NArg() != 0 {
		return errors.New(agentsUpdateUsage)
	}
	s, err := loadAgentsStore(cfg)
	if err != nil {
		return err
	}
	i, ok := s.find(name)
	if !ok {
		return fmt.Errorf("no agent profile %q", name)
	}
	p := s.Profiles[i]
	changed := false
	fs.Visit(func(f *flag.Flag) {
		changed = true
		switch f.Name {
		case "read-scopes":
			p.ReadScopes = splitList(*scopes)
		case "write":
			p.Write = *write
		case "raw-sources":
			p.RawSources = *rawSources
		case "tools":
			p.Tools = splitList(*tools)
		}
	})
	if !changed {
		return errors.New("agents update: give at least one of --read-scopes, --write, --raw-sources, --tools")
	}
	if err := p.validate(); err != nil {
		return err
	}
	s.Profiles[i] = p
	if err := saveAgentsStore(cfg, s); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Updated agent profile %q: read %s, raw sources %s, writes %s, tools %s. Its token is unchanged.\n",
		p.Name, strings.Join(p.ReadScopes, ","), onOff(p.RawSources), p.Write, strings.Join(p.Tools, ","))
	return nil
}

func onOff(b bool) string {
	if b {
		return "visible"
	}
	return "hidden"
}

func agentsList(cfg Config, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("agents list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%v\n%s", err, agentsListUsage)
	}
	if fs.NArg() != 0 {
		return errors.New(agentsListUsage)
	}
	s, err := loadAgentsStore(cfg)
	if err != nil {
		return err
	}
	type row struct {
		Name       string   `json:"name"`
		ReadScopes []string `json:"read_scopes"`
		RawSources bool     `json:"raw_sources"`
		Write      string   `json:"write"`
		Tools      []string `json:"tools"`
		CreatedAt  string   `json:"created_at"`
		RevokedAt  string   `json:"revoked_at,omitempty"`
	}
	rows := make([]row, 0, len(s.Profiles))
	for _, p := range s.Profiles {
		rows = append(rows, row{p.Name, p.ReadScopes, p.RawSources, p.Write, p.Tools, p.CreatedAt, p.RevokedAt})
	}
	if *jsonOut {
		return emitReceipt(stdout, "mora.agents", 1, map[string]any{"profiles": rows})
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "No agent profiles. Create one with `mora agents add <name> --read-scopes <scope>`.")
		return nil
	}
	for _, r := range rows {
		state := "active"
		if r.RevokedAt != "" {
			state = "revoked " + r.RevokedAt
		}
		fmt.Fprintf(stdout, "%s  read=%s  raw_sources=%t  write=%s  tools=%s  (%s)\n",
			r.Name, strings.Join(r.ReadScopes, ","), r.RawSources, r.Write, strings.Join(r.Tools, ","), state)
	}
	return nil
}

func agentsRevoke(cfg Config, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New(agentsUsage)
	}
	s, err := loadAgentsStore(cfg)
	if err != nil {
		return err
	}
	i, ok := s.find(args[0])
	if !ok {
		return fmt.Errorf("no agent profile %q", args[0])
	}
	if s.Profiles[i].RevokedAt == "" {
		s.Profiles[i].RevokedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if err := saveAgentsStore(cfg, s); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Revoked agent profile %q; its token stops working on the next request.\n", args[0])
	return nil
}

func agentsRotate(cfg Config, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New(agentsUsage)
	}
	s, err := loadAgentsStore(cfg)
	if err != nil {
		return err
	}
	i, ok := s.find(args[0])
	if !ok {
		return fmt.Errorf("no agent profile %q", args[0])
	}
	token, hash, err := newAgentToken()
	if err != nil {
		return err
	}
	s.Profiles[i].TokenSHA256 = hash
	s.Profiles[i].RevokedAt = ""
	if err := saveAgentsStore(cfg, s); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "New token for %q (the old one stops working now; shown once):\n%s\n", args[0], token)
	return nil
}

func agentsLog(cfg Config, args []string, stdout io.Writer) error {
	name, rest := splitNameFirst(args)
	fs := flag.NewFlagSet("agents log", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	since := fs.Duration("since", 24*time.Hour, "look-back window")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(rest); err != nil {
		return fmt.Errorf("%v\n%s", err, agentsLogUsage)
	}
	if name == "" || fs.NArg() != 0 {
		return errors.New(agentsLogUsage)
	}
	s, err := loadAgentsStore(cfg)
	if err != nil {
		return err
	}
	if _, ok := s.find(name); !ok {
		return fmt.Errorf("no agent profile %q", name)
	}
	b, err := os.ReadFile(agentReceiptsPath(cfg, name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cutoff := time.Now().Add(-*since)
	receipts := []agentReceipt{}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r agentReceipt
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		if at, perr := time.Parse(time.RFC3339, r.At); perr == nil && at.Before(cutoff) {
			continue
		}
		receipts = append(receipts, r)
	}
	sort.SliceStable(receipts, func(i, j int) bool { return receipts[i].At < receipts[j].At })
	if *jsonOut {
		return emitReceipt(stdout, "mora.agents.log", 1, map[string]any{"profile": name, "receipts": receipts})
	}
	if len(receipts) == 0 {
		fmt.Fprintf(stdout, "No calls from %q in the last %s.\n", name, *since)
		return nil
	}
	for _, r := range receipts {
		ids := make([]string, 0, len(r.Rows))
		for _, row := range r.Rows {
			ids = append(ids, row.Scope+"/"+row.ID)
		}
		line := fmt.Sprintf("%s  %-14s %-9s", r.At, r.Tool, r.Action)
		if r.Query != "" {
			line += fmt.Sprintf("  q=%q", r.Query)
		}
		if r.ProposalID != "" {
			line += "  proposal=" + r.ProposalID
		}
		if len(ids) > 0 {
			line += "  rows=" + strings.Join(ids, ",")
		}
		if r.Error != "" {
			line += "  error=" + r.Error
		}
		fmt.Fprintln(stdout, line)
	}
	return nil
}

// agentToolDefs is the tools/list catalog a profile sees: only its tools, in
// the canonical order.
func agentToolDefs(p agentProfile) []mcppkg.ToolDefinition {
	all := mcppkg.ToolCatalog()
	out := make([]mcppkg.ToolDefinition, 0, len(p.Tools))
	for _, def := range all {
		if p.allowsTool(def.Name) {
			out = append(out, def)
		}
	}
	return out
}

func agentNow() string { return time.Now().UTC().Format(time.RFC3339) }

// agentQueryOf records what the agent asked for, bounded. It is the agent's
// own request, not memory content.
func agentQueryOf(args map[string]any) string {
	for _, k := range []string{"query", "id", "title"} {
		if s, _ := args[k].(string); s != "" {
			return truncateRunes(s, agentQueryReceipt)
		}
	}
	return ""
}

func proposalIDOf(v any) string {
	wrapped, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	p, ok := wrapped["proposal"].(map[string]any)
	if !ok {
		return ""
	}
	id, _ := p["id"].(string)
	return id
}

// Remote defaults for agent profiles (#539): hide raw sources, propose writes,
// and deny deletion. A profile that loosens any of these must be visible in
// Doctor so an operator can see the explicit relaxation without reading the
// store by hand.
func (p agentProfile) remoteDefaultRelaxations() []string {
	var out []string
	if p.RawSources {
		out = append(out, "raw_sources=on")
	}
	if p.Write == mcpWritePolicyOpen {
		out = append(out, "write=open")
	}
	if p.allowsTool("delete_memory") {
		out = append(out, "delete_memory=allowed")
	}
	return out
}

// agentRemoteRelaxation is one active profile that loosens a remote default.
// Doctor publishes these in --json and prints them in the human report.
type agentRemoteRelaxation struct {
	Name         string   `json:"name"`
	RawSources   bool     `json:"raw_sources"`
	Write        string   `json:"write"`
	DeleteMemory bool     `json:"delete_memory"`
	Relaxations  []string `json:"relaxations"`
}

// collectAgentRemoteRelaxations returns active profiles that loosen remote
// defaults, sorted by name. A missing store is an empty list (no profiles
// yet). A corrupt store is returned as an error so Doctor can surface it.
func collectAgentRemoteRelaxations(cfg Config) ([]agentRemoteRelaxation, error) {
	store, err := loadAgentsStore(cfg)
	if err != nil {
		return nil, err
	}
	out := []agentRemoteRelaxation{}
	for _, p := range store.Profiles {
		if p.RevokedAt != "" {
			continue
		}
		relax := p.remoteDefaultRelaxations()
		if len(relax) == 0 {
			continue
		}
		out = append(out, agentRemoteRelaxation{
			Name:         p.Name,
			RawSources:   p.RawSources,
			Write:        p.Write,
			DeleteMemory: p.allowsTool("delete_memory"),
			Relaxations:  relax,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
