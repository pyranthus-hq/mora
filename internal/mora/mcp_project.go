package mora

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	mcppkg "github.com/pyranthus-hq/mora/internal/mcp"
)

// mcpProject is the project a stdio server was started for with --project-dir.
// It is resolved once and held for the life of the server.
type mcpProject struct{ scope, root string }

type mcpProjectKey struct{}

func mcpProjectFrom(ctx context.Context) (mcpProject, bool) {
	p, ok := ctx.Value(mcpProjectKey{}).(mcpProject)
	return p, ok
}

type mcpServeArgs struct {
	profile, dir       string
	hasProfile, hasDir bool
}

// parseMCPServeArgs reads `serve` and its flags: --profile NAME and
// --project-dir DIR, each at most once, in either order.
func parseMCPServeArgs(args []string) (a mcpServeArgs, ok bool) {
	if len(args) == 0 || args[0] != "serve" {
		return a, false
	}
	for i := 1; i < len(args); i += 2 {
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
	scope, root, found, err := lookupProject(cfg, dir)
	if err != nil {
		return unbound("bindings unreadable")
	}
	if !found {
		return unbound("no binding")
	}
	return context.WithValue(ctx, mcpProjectKey{}, mcpProject{scope: scope, root: root})
}

const (
	projectBriefFrameBegin = "\n\n<MORA:BEGIN project brief>\n"
	projectBriefFrameEnd   = "\n<MORA:END project brief>"
)

// withProjectInstruction appends the project sentence and, when the frozen
// binding remains valid, its rendered brief to the handshake instructions.
func withProjectInstruction(ctx context.Context, result map[string]any, cfg Config, configOK bool, now time.Time, profile *agentProfile) map[string]any {
	project, ok := mcpProjectFrom(ctx)
	instr, isStr := result["instructions"].(string)
	if !ok || !isStr {
		return result
	}
	instr += " " + mcppkg.ProjectInstruction(project.scope)
	if profile != nil && (!profile.allowsTool("brief") || !profile.unrestricted()) {
		result["instructions"] = instr
		return result
	}
	if !configOK {
		result["instructions"] = instr + projectBriefUnavailable(project.scope)
		return result
	}
	scope, root, found, err := lookupProject(cfg, project.root)
	if err != nil || !found || scope != project.scope || root != project.root {
		result["instructions"] = instr + projectBriefUnavailable(project.scope)
		return result
	}
	text, ids, err := buildProjectBrief(cfg, project.scope, project.root, now)
	if err != nil {
		result["instructions"] = instr + projectBriefUnavailable(project.scope)
		return result
	}
	result["instructions"] = instr + projectBriefFrameBegin + text + projectBriefFrameEnd
	if len(ids) != 0 {
		_ = appendGiven(cfg, givenLine{
			Surface: "mcp:initialize", Kind: "project-brief", Caller: agentCallerFrom(ctx),
			Dir: project.root, Scope: givenScope(project.scope),
		}, ids, len(ids))
	}
	return result
}

func projectBriefUnavailable(scope string) string {
	if validProjectScope(scope) != nil {
		scope = "this project"
	}
	return "\n\nMora project brief unavailable for " + scope + "."
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
