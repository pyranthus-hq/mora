package integrations

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// CodexReadOnlyTools are the Mora MCP tools wire pre-approves for Codex.
// Codex's default MCP approval asks before every call, and headless `codex
// exec` runs with approval policy "never", so an unapproved call fails with
// "MCP tool call requires approval, but approval policy is never" (pilot F11).
// Setting mcp_servers.mora.tools.<tool>.approval_mode = "approve" lets these
// reads run unattended. write_memory and delete_memory are left out on
// purpose: mutations keep Codex's own approval policy.
var CodexReadOnlyTools = []string{
	"search_memory", "read_memory", "list_memory", "context_memory", "think",
	"brief", "digest", "calendar_events", "list_entities", "get_entity", "meeting_prep",
}

// Planned file actions.
const (
	ActionCreate    = "create"
	ActionUpdate    = "update"
	ActionDelete    = "delete"
	ActionUnchanged = "unchanged"
)

// Kinds of file wire manages.
const (
	KindMCP          = "mcp"
	KindInstructions = "instructions"
)

// FileChange is one file wire or unwire would touch. Diff covers only the
// Mora-owned part of the file (the mcp entry or the marked block), never any
// other key, so a dry run cannot print someone's tokens or settings.
type FileChange struct {
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Action string `json:"action"`
	Note   string `json:"note,omitempty"`
	Diff   string `json:"diff,omitempty"`
	// Registered and Approved name what this change adds, so the caller can
	// record ownership and unwire can take back exactly that.
	Registered bool     `json:"registered,omitempty"`
	Approved   []string `json:"approved_tools,omitempty"`

	after []byte
	mode  os.FileMode
}

// Plan is the full set of files a wire or unwire run covers, in a stable order.
type Plan struct {
	Changes []FileChange `json:"changes"`
}

// Pending counts the files the plan would actually change.
func (p Plan) Pending() int {
	n := 0
	for _, c := range p.Changes {
		if c.Action != ActionUnchanged {
			n++
		}
	}
	return n
}

// Owned is what wire added to one file: the Mora MCP registration and/or
// Codex read-tool approvals. Without it, unwire removes only the marked block,
// so a registration someone made by hand is never taken away by accident.
type Owned struct {
	Registered bool     `json:"registered,omitempty"`
	Approved   []string `json:"approved_tools,omitempty"`
}

// Owned reports, per file, what the plan adds.
func (p Plan) Owned() map[string]Owned {
	out := map[string]Owned{}
	for _, c := range p.Changes {
		if c.Action != ActionUnchanged && (c.Registered || len(c.Approved) > 0) {
			out[c.Path] = Owned{Registered: c.Registered, Approved: append([]string(nil), c.Approved...)}
		}
	}
	return out
}

// MergeOwned folds a newer wire run's ownership into an older record.
func MergeOwned(old, add map[string]Owned) map[string]Owned {
	out := map[string]Owned{}
	for path, o := range old {
		out[path] = o
	}
	for path, o := range add {
		cur := out[path]
		cur.Registered = cur.Registered || o.Registered
		seen := map[string]bool{}
		for _, t := range cur.Approved {
			seen[t] = true
		}
		for _, t := range o.Approved {
			if !seen[t] {
				cur.Approved = append(cur.Approved, t)
				seen[t] = true
			}
		}
		out[path] = cur
	}
	return out
}

// WireOptions configures PlanWire.
type WireOptions struct {
	// Binary is the command a NEW registration runs. An existing Mora
	// registration is never repointed; `mora integrations connect` does that.
	Binary string
}

type agentHome struct {
	agent string
	home  string
}

func (s Seams) getenv(key string) string {
	if s.Getenv == nil {
		return ""
	}
	return s.Getenv(key)
}

func (s Seams) readDir(path string) ([]os.DirEntry, error) {
	if s.ReadDir == nil {
		return os.ReadDir(path)
	}
	return s.ReadDir(path)
}

func (s Seams) remove(path string) error {
	if s.Remove == nil {
		return os.Remove(path)
	}
	return s.Remove(path)
}

// envUnderHome returns an environment path only when it lies inside home.
// A run against a temp HOME (tests, sandboxes) must never reach a real Codex
// or Orca home through an inherited variable; real setups keep both under ~.
func envUnderHome(seams Seams, key string) string {
	v := seams.getenv(key)
	if v == "" || !filepath.IsAbs(v) {
		return ""
	}
	rel, err := filepath.Rel(filepath.Clean(seams.Home), filepath.Clean(v))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return v
}

// codexHomes lists every Codex home to wire: ~/.codex, $CODEX_HOME, and each
// Orca-managed account home (<orca data>/codex-accounts/<id>/home), which
// Codex under Orca reads instead of ~/.codex (pilot F10). Duplicates collapse.
func codexHomes(seams Seams) []string {
	var candidates []string
	candidates = append(candidates, filepath.Join(seams.Home, ".codex"))
	if env := envUnderHome(seams, "CODEX_HOME"); env != "" {
		candidates = append(candidates, env)
	}
	bases := []string{
		envUnderHome(seams, "ORCA_USER_DATA_PATH"),
		filepath.Join(seams.Home, ".config", "orca"),
		filepath.Join(seams.Home, "Library", "Application Support", "orca"),
	}
	for _, base := range bases {
		if base == "" {
			continue
		}
		accounts := filepath.Join(base, "codex-accounts")
		entries, err := seams.readDir(accounts)
		if err != nil {
			continue
		}
		var found []string
		for _, e := range entries {
			if e.IsDir() {
				found = append(found, filepath.Join(accounts, e.Name(), "home"))
			}
		}
		sort.Strings(found)
		candidates = append(candidates, found...)
	}
	var out []string
	seen := map[string]bool{}
	for _, c := range candidates {
		if !isDir(seams, c) {
			continue
		}
		key := filepath.Clean(c)
		if resolved, err := filepath.EvalSymlinks(key); err == nil {
			key = resolved
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, filepath.Clean(c))
	}
	return out
}

func wireTargets(seams Seams) []agentHome {
	var out []agentHome
	if isDir(seams, filepath.Join(seams.Home, ".claude")) {
		out = append(out, agentHome{"claude", seams.Home})
	}
	for _, h := range codexHomes(seams) {
		out = append(out, agentHome{"codex", h})
	}
	if isDir(seams, filepath.Join(seams.Home, ".pi", "agent")) {
		out = append(out, agentHome{"pi", filepath.Join(seams.Home, ".pi", "agent")})
	}
	return out
}

// readOptional returns a file's bytes and whether it exists.
func readOptional(seams Seams, path string) ([]byte, bool, error) {
	body, err := seams.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return body, true, nil
}

func newChange(seams Seams, agent, kind, path string, before []byte, existed bool, after []byte, remove bool, fragment func([]byte) string, defaultMode os.FileMode) FileChange {
	c := FileChange{Agent: agent, Kind: kind, Path: path, after: after, mode: defaultMode}
	if existed {
		c.mode = fileMode(seams, path)
	}
	switch {
	case remove && existed:
		c.Action = ActionDelete
	case !existed && after != nil:
		c.Action = ActionCreate
	case existed && string(before) != string(after):
		c.Action = ActionUpdate
	default:
		c.Action = ActionUnchanged
		return c
	}
	var afterFragment string
	if !remove {
		afterFragment = fragment(after)
	}
	c.Diff = lineDiff(path, fragment(before), afterFragment)
	return c
}

// PlanWire computes, without writing, every change that wires the detected
// agents to Mora: an MCP registration where the agent has MCP, read-tool
// approval for Codex, and the managed instruction block.
func PlanWire(seams Seams, opts WireOptions) (Plan, error) {
	if seams.Home == "" {
		return Plan{}, errors.New("integrations home must not be empty")
	}
	var plan Plan
	for _, t := range wireTargets(seams) {
		switch t.agent {
		case "claude":
			c, err := planClaudeMCP(seams, opts)
			if err != nil {
				return Plan{}, err
			}
			plan.Changes = append(plan.Changes, c)
			c, err = planBlock(seams, t.agent, filepath.Join(t.home, ".claude", "CLAUDE.md"), mcpBlock())
			if err != nil {
				return Plan{}, err
			}
			plan.Changes = append(plan.Changes, c)
		case "codex":
			c, err := planCodexMCP(seams, t.home, opts)
			if err != nil {
				return Plan{}, err
			}
			plan.Changes = append(plan.Changes, c)
			c, err = planBlock(seams, t.agent, filepath.Join(t.home, "AGENTS.md"), mcpBlock())
			if err != nil {
				return Plan{}, err
			}
			plan.Changes = append(plan.Changes, c)
		case "pi":
			c, err := planBlock(seams, t.agent, filepath.Join(t.home, "AGENTS.md"), cliBlock())
			if err != nil {
				return Plan{}, err
			}
			plan.Changes = append(plan.Changes, c)
		}
	}
	return plan, nil
}

// PlanUnwire computes the reverse of PlanWire. It always removes the marked
// block, and removes only the registrations and approvals owned records as
// wire-added; all removes Mora's MCP entry (with every Codex tool table)
// wherever it is. A file left holding nothing is deleted.
func PlanUnwire(seams Seams, owned map[string]Owned, all bool) (Plan, error) {
	if seams.Home == "" {
		return Plan{}, errors.New("integrations home must not be empty")
	}
	var plan Plan
	add := func(c FileChange, ok bool, err error) error {
		if err != nil {
			return err
		}
		if ok {
			plan.Changes = append(plan.Changes, c)
		}
		return nil
	}
	for _, t := range wireTargets(seams) {
		var err error
		switch t.agent {
		case "claude":
			if err = add(unplanClaudeMCP(seams, owned, all)); err == nil {
				err = add(unplanBlock(seams, t.agent, filepath.Join(t.home, ".claude", "CLAUDE.md")))
			}
		case "codex":
			if err = add(unplanCodexMCP(seams, t.home, owned, all)); err == nil {
				err = add(unplanBlock(seams, t.agent, filepath.Join(t.home, "AGENTS.md")))
			}
		case "pi":
			err = add(unplanBlock(seams, t.agent, filepath.Join(t.home, "AGENTS.md")))
		}
		if err != nil {
			return Plan{}, err
		}
	}
	return plan, nil
}

// Apply writes a plan computed by PlanWire or PlanUnwire.
func Apply(seams Seams, plan Plan) error {
	for _, c := range plan.Changes {
		var err error
		switch c.Action {
		case ActionCreate, ActionUpdate:
			err = seams.WriteFile(c.Path, c.after, c.mode)
		case ActionDelete:
			err = seams.remove(c.Path)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", c.Path, err)
		}
	}
	return nil
}

func planBlock(seams Seams, agent, path, block string) (FileChange, error) {
	before, existed, err := readOptional(seams, path)
	if err != nil {
		return FileChange{}, err
	}
	after, err := upsertBlock(before, block)
	if err != nil {
		return FileChange{}, fmt.Errorf("%s: %w", path, err)
	}
	return newChange(seams, agent, KindInstructions, path, before, existed, after, false, blockText, 0o644), nil
}

func unplanBlock(seams Seams, agent, path string) (FileChange, bool, error) {
	before, existed, err := readOptional(seams, path)
	if err != nil || !existed {
		return FileChange{}, false, err
	}
	after, changed, err := removeBlock(before)
	if err != nil {
		return FileChange{}, false, fmt.Errorf("%s: %w", path, err)
	}
	if !changed {
		return FileChange{}, false, nil
	}
	empty := strings.TrimSpace(string(after)) == ""
	return newChange(seams, agent, KindInstructions, path, before, true, after, empty, blockText, 0o644), true, nil
}

func requireBinary(opts WireOptions, path string) error {
	if opts.Binary == "" || !filepath.IsAbs(opts.Binary) {
		return fmt.Errorf("%s: registering Mora needs an absolute --binary path", path)
	}
	return nil
}

func planClaudeMCP(seams Seams, opts WireOptions) (FileChange, error) {
	path := ConfigPath(seams.Home, Claude)
	before, existed, err := readOptional(seams, path)
	if err != nil {
		return FileChange{}, err
	}
	after := before
	var e *entry
	if existed {
		if e, err = readJSONEntry(before); err != nil {
			return FileChange{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	if e == nil {
		if err := requireBinary(opts, path); err != nil {
			return FileChange{}, err
		}
		if after, _, err = upsertJSONEntry(before, Claude, opts.Binary); err != nil {
			return FileChange{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	c := newChange(seams, "claude", KindMCP, path, before, existed, after, false, claudeMoraText, 0o600)
	if e != nil {
		c.Note = "already registered: " + e.Command
	}
	c.Registered = e == nil
	return c, nil
}

func unplanClaudeMCP(seams Seams, owned map[string]Owned, all bool) (FileChange, bool, error) {
	path := ConfigPath(seams.Home, Claude)
	if !all && !owned[path].Registered {
		return FileChange{}, false, nil
	}
	before, existed, err := readOptional(seams, path)
	if err != nil || !existed {
		return FileChange{}, false, err
	}
	after, changed, err := removeJSONEntry(before)
	if err != nil {
		return FileChange{}, false, fmt.Errorf("%s: %w", path, err)
	}
	if !changed {
		return FileChange{}, false, nil
	}
	return newChange(seams, "claude", KindMCP, path, before, true, after, onlyEmptyServers(after), claudeMoraText, 0o600), true, nil
}

// onlyEmptyServers reports a config that holds nothing but an empty
// mcpServers object: the file wire created, which unwire deletes again.
func onlyEmptyServers(body []byte) bool {
	doc := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &doc); err != nil || len(doc) != 1 {
		return false
	}
	servers := map[string]json.RawMessage{}
	return json.Unmarshal(doc["mcpServers"], &servers) == nil && len(servers) == 0
}

// claudeMoraText renders only mcpServers.mora for a diff.
func claudeMoraText(body []byte) string {
	doc := map[string]json.RawMessage{}
	if json.Unmarshal(body, &doc) != nil {
		return ""
	}
	servers := map[string]json.RawMessage{}
	if json.Unmarshal(doc["mcpServers"], &servers) != nil {
		return ""
	}
	raw, ok := servers["mora"]
	if !ok {
		return ""
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	pretty, err := json.MarshalIndent(map[string]any{"mora": v}, "", "  ")
	if err != nil {
		return ""
	}
	return string(pretty)
}

func planCodexMCP(seams Seams, home string, opts WireOptions) (FileChange, error) {
	path := filepath.Join(home, "config.toml")
	before, existed, err := readOptional(seams, path)
	if err != nil {
		return FileChange{}, err
	}
	e, err := readCodexEntry(before)
	if err != nil {
		return FileChange{}, fmt.Errorf("%s: %w", path, err)
	}
	after := before
	if e == nil {
		if err := requireBinary(opts, path); err != nil {
			return FileChange{}, err
		}
		after = upsertCodexEntry(before, opts.Binary)
	}
	var approved []string
	if after, approved, err = ensureCodexToolApprovals(after, CodexReadOnlyTools); err != nil {
		return FileChange{}, fmt.Errorf("%s: %w", path, err)
	}
	c := newChange(seams, "codex", KindMCP, path, before, existed, after, false, codexMoraText, 0o600)
	if e != nil {
		c.Note = "already registered: " + e.Command
	}
	c.Registered = e == nil
	c.Approved = approved
	return c, nil
}

func unplanCodexMCP(seams Seams, home string, owned map[string]Owned, all bool) (FileChange, bool, error) {
	path := filepath.Join(home, "config.toml")
	mine := owned[path]
	if !all && !mine.Registered && len(mine.Approved) == 0 {
		return FileChange{}, false, nil
	}
	before, existed, err := readOptional(seams, path)
	if err != nil || !existed {
		return FileChange{}, false, err
	}
	if codexInline(before) {
		return FileChange{}, false, fmt.Errorf("%s: %w", path, errCodexInline)
	}
	var after []byte
	var changed bool
	if all || mine.Registered {
		after, changed = removeCodexEntry(before)
	} else {
		after, changed = removeCodexToolApprovals(before, mine.Approved)
	}
	if !changed {
		return FileChange{}, false, nil
	}
	empty := strings.TrimSpace(string(after)) == ""
	return newChange(seams, "codex", KindMCP, path, before, true, after, empty, codexMoraText, 0o600), true, nil
}

func isMoraHeader(h string) bool {
	return h == "mcp_servers.mora" || strings.HasPrefix(h, "mcp_servers.mora.")
}

// codexMoraText renders only the [mcp_servers.mora] family for a diff.
func codexMoraText(body []byte) string {
	var out []string
	in := false
	for _, line := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			in = isMoraHeader(tomlHeader(line))
		}
		if in {
			out = append(out, line)
		}
	}
	return strings.TrimRight(strings.Join(out, "\n"), "\n")
}

var errCodexToolsTable = errors.New("codex config sets Mora's tools as a table or inline key; add read-tool approvals by hand")

// ensureCodexToolApprovals adds an [mcp_servers.mora.tools.<tool>] table with
// approval_mode = "approve" for each tool that has no table yet, directly
// after the mora tables, and returns the tools it added. A tool the user
// already configured keeps its value.
func ensureCodexToolApprovals(body []byte, tools []string) ([]byte, []string, error) {
	ending := codexLineEnding(body)
	lines := strings.Split(string(body), ending)
	have := map[string]bool{}
	last := -1
	in := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			h := tomlHeader(line)
			in = isMoraHeader(h)
			if h == "mcp_servers.mora.tools" {
				return nil, nil, errCodexToolsTable
			}
			if strings.HasPrefix(h, "mcp_servers.mora.tools.") {
				have[strings.TrimPrefix(h, "mcp_servers.mora.tools.")] = true
			}
		} else if in {
			if key := tomlKey(line); key == "tools" || strings.HasPrefix(key, "tools.") {
				return nil, nil, errCodexToolsTable
			}
		}
		if in {
			last = i + 1
		}
	}
	if last < 0 {
		return body, nil, nil
	}
	var add, added []string
	for _, tool := range tools {
		if !have[tool] {
			add = append(add, "", "[mcp_servers.mora.tools."+tool+"]", codexApproveLine)
			added = append(added, tool)
		}
	}
	if len(add) == 0 {
		return body, nil, nil
	}
	insert := last
	for insert > 0 && strings.TrimSpace(lines[insert-1]) == "" {
		insert--
	}
	out := append([]string{}, lines[:insert]...)
	out = append(out, add...)
	out = append(out, lines[insert:]...)
	return []byte(strings.Join(out, ending)), added, nil
}

const codexApproveLine = `approval_mode = "approve"`

// removeCodexToolApprovals deletes the tool tables wire added, each with the
// blank line that follows it, but only while the table still holds exactly
// wire's approval line: a table the user edited since is theirs.
func removeCodexToolApprovals(body []byte, tools []string) ([]byte, bool) {
	ending := codexLineEnding(body)
	lines := strings.Split(string(body), ending)
	want := map[string]bool{}
	for _, t := range tools {
		want[t] = true
	}
	var out []string
	changed := false
	for i := 0; i < len(lines); {
		h := ""
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "[") {
			h = tomlHeader(lines[i])
		}
		end := i + 1
		for end < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[end]), "[") {
			end++
		}
		if h != "" && want[strings.TrimPrefix(h, "mcp_servers.mora.tools.")] && strings.HasPrefix(h, "mcp_servers.mora.tools.") {
			var content []string
			for _, l := range lines[i+1 : end] {
				if strings.TrimSpace(l) != "" {
					content = append(content, strings.TrimSpace(l))
				}
			}
			if len(content) == 1 && content[0] == codexApproveLine {
				changed = true
				i = end
				continue
			}
		}
		if h == "" {
			end = i + 1
		}
		out = append(out, lines[i:end]...)
		i = end
	}
	if !changed {
		return body, false
	}
	return []byte(strings.Join(out, ending)), true
}

// lineDiff renders a small unified-style diff of two fragments: every line,
// marked " ", "-" or "+". Fragments are a few dozen lines, so a plain LCS
// table is enough.
func lineDiff(path, before, after string) string {
	if before == after {
		return ""
	}
	a, b := splitLines(before), splitLines(after)
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", path, path)
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			sb.WriteString(" " + a[i] + "\n")
			i++
			j++
		case j < len(b) && (i == len(a) || lcs[i][j+1] >= lcs[i+1][j]):
			sb.WriteString("+" + b[j] + "\n")
			j++
		default:
			sb.WriteString("-" + a[i] + "\n")
			i++
		}
	}
	return sb.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
