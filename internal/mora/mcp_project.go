package mora

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

var errMCPWorkspaceUnavailable = fmt.Errorf("MCP workspace is unavailable")

// mcpProject is the project selected for one MCP request. --project-dir is an
// explicit session binding; --workspace resolves this value from the current
// client roots immediately before the request runs.
type mcpProject struct {
	scope, root string
	ownerOnly   bool
}

type mcpProjectKey struct{}
type mcpWorkspaceKey struct{}

func mcpProjectFrom(ctx context.Context) (mcpProject, bool) {
	p, ok := ctx.Value(mcpProjectKey{}).(mcpProject)
	return p, ok
}

type mcpServeArgs struct {
	profile, dir                  string
	hasProfile, hasDir, workspace bool
}

// parseMCPServeArgs reads `serve` and its flags: --profile NAME,
// --project-dir DIR and --workspace. The latter puts stdio into managed
// workspace mode: roots select context but never confer authority.
func parseMCPServeArgs(args []string) (a mcpServeArgs, ok bool) {
	if len(args) == 0 || args[0] != "serve" {
		return a, false
	}
	for i := 1; i < len(args); {
		if args[i] == "--workspace" && !a.workspace {
			a.workspace = true
			i++
			continue
		}
		if i+1 >= len(args) {
			return a, false
		}
		// A value that starts with "-" is a flag, so the value is missing.
		if strings.HasPrefix(args[i+1], "-") {
			return a, false
		}
		switch {
		case args[i] == "--profile" && !a.hasProfile:
			a.profile, a.hasProfile = args[i+1], true
		case args[i] == "--project-dir" && !a.hasDir:
			a.dir, a.hasDir = args[i+1], true
		default:
			return a, false
		}
		i += 2
	}
	return a, true
}

// withMCPServeProject binds the server to DIR when DIR is an existing absolute
// directory with a project binding. Every other case serves as if no flag was given and says
// so on stderr; stdout is the protocol stream. The working directory never counts.
func withMCPServeProject(ctx context.Context, dir string, stderr io.Writer) context.Context {
	dir = normalizeWSLWindowsPath(dir)
	unbound := func(why string) context.Context {
		fmt.Fprintf(stderr, "mora mcp serve: folder %q is not bound to a project (%s); serving without a project\n", dir, why)
		return ctx
	}
	if !filepath.IsAbs(dir) {
		return unbound("the path is not absolute")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return unbound("the path is not an existing directory")
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return unbound("config unavailable")
	}
	binding, found, err := lookupProjectBinding(cfg, dir)
	if err != nil {
		return unbound("bindings unreadable")
	}
	if !found {
		return unbound("no binding")
	}
	return context.WithValue(ctx, mcpProjectKey{}, mcpProject{scope: binding.Scope, root: binding.Dir, ownerOnly: binding.OwnerOnly})
}

func withMCPWorkspace(ctx context.Context) context.Context {
	return context.WithValue(ctx, mcpWorkspaceKey{}, true)
}

// withMCPWorkspaceProject snapshots the client roots once for the current
// request. A configured --project-dir is deliberate administrator authority
// and always wins. Managed sessions fail closed when roots cannot identify one
// local project; callers intentionally receive no broader fallback.
func withMCPWorkspaceProject(ctx context.Context) (context.Context, error) {
	managed, _ := ctx.Value(mcpWorkspaceKey{}).(bool)
	if !managed || hasMCPProject(ctx) {
		return ctx, nil
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return ctx, err
	}
	roots, err := mcppkg.ListClientRoots(ctx)
	if err != nil {
		return ctx, err
	}
	p, err := resolveMCPProjectRoots(cfg, roots)
	if err != nil {
		return ctx, err
	}
	if binding, found, err := lookupProjectBinding(cfg, p.root); err != nil || !found {
		if err != nil {
			return ctx, err
		}
		return ctx, errMCPProjectRootsUnbound
	} else {
		p.ownerOnly = binding.OwnerOnly
	}
	return context.WithValue(ctx, mcpProjectKey{}, p), nil
}

func hasMCPProject(ctx context.Context) bool {
	_, ok := mcpProjectFrom(ctx)
	return ok
}

func mcpWorkspaceManaged(ctx context.Context) bool {
	managed, _ := ctx.Value(mcpWorkspaceKey{}).(bool)
	return managed
}

// mcpProjectAllowsScope is the session's immutable read policy. Client roots
// chose p; they never widen this list. A profile can only narrow it further at
// the existing agent admission/filter boundary.
func mcpProjectAllowsScope(p mcpProject, scope string) bool {
	return scope == p.scope || scope == "global" || (p.ownerOnly && scope == "personal")
}

func mcpProjectFilter(ctx context.Context, requested string, f searchFilters) (searchFilters, error) {
	p, ok := mcpProjectFrom(ctx)
	if !ok || !mcpWorkspaceManaged(ctx) {
		return f, nil
	}
	if requested != "" {
		if !mcpProjectAllowsScope(p, requested) {
			return f, errMCPWorkspaceUnavailable
		}
		f.Scopes = []string{requested}
		f.AuthoredOnly = true
		return f, nil
	}
	f.Scopes = []string{p.scope, "global"}
	if p.ownerOnly {
		f.Scopes = append(f.Scopes, "personal")
	}
	f.AuthoredOnly = true
	return f, nil
}

func mcpProjectAllowsMemory(ctx context.Context, m Memory) bool {
	p, ok := mcpProjectFrom(ctx)
	if !ok || !mcpWorkspaceManaged(ctx) {
		return true
	}
	// Connector rows are raw source material, never project-session context.
	return mcpProjectAllowsScope(p, m.Scope) && m.Provider == ""
}

var mcpProjectSafeTools = map[string]bool{
	"search_memory":  true,
	"read_memory":    true,
	"list_memory":    true,
	"context_memory": true,
	"write_memory":   true,
	"brief":          true,
}

func mcpProjectAllowsTool(ctx context.Context, name string) bool {
	if _, ok := mcpProjectFrom(ctx); !ok || !mcpWorkspaceManaged(ctx) {
		return true
	}
	return mcpProjectSafeTools[name]
}

// withProjectInstruction appends the project sentence to the handshake
// instructions. Only the scope can appear, and only when it is a plain name.
func withProjectInstruction(ctx context.Context, result map[string]any, configOK bool) map[string]any {
	p, ok := mcpProjectFrom(ctx)
	instr, isStr := result["instructions"].(string)
	if !ok || !configOK || !isStr {
		return result
	}
	result["instructions"] = instr + " " + mcppkg.ProjectInstruction(p.scope)
	return result
}

// withProjectScope gives a write_memory call whose arguments have no scope key
// the server's project scope. Any scope key, even empty, null or not a string,
// is the client's and is left alone.
func withProjectScope(ctx context.Context, name string, args map[string]any) map[string]any {
	p, ok := mcpProjectFrom(ctx)
	if _, has := args["scope"]; !ok || name != "write_memory" || has {
		return args
	}
	shaped := make(map[string]any, len(args)+1)
	for k, v := range args {
		shaped[k] = v
	}
	shaped["scope"] = p.scope
	return shaped
}
