package mora

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/memoryfile"
)

const projectUsage = "usage: mora project bind [--scope project:NAME] [--owner-only] [DIR] | unbind [DIR] | list [--json] | which [DIR] [--json]"

// projectBinding ties one resolved directory to one project scope.
type projectBinding struct {
	Dir   string `json:"dir"`
	Scope string `json:"scope"`
	// OwnerOnly lets personal notes appear on their own in this folder's
	// sessions. Absent in an old file, so it loads as false.
	OwnerOnly bool `json:"owner_only,omitempty"`
}

type projectBindingFile struct {
	Bindings []projectBinding `json:"bindings"`
}

func projectBindingsPath(cfg Config) string { return filepath.Join(cfg.ConfigDir, "projects.json") }

func loadProjectBindings(cfg Config) ([]projectBinding, error) {
	body, err := os.ReadFile(projectBindingsPath(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f projectBindingFile
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("read %s: %w", projectBindingsPath(cfg), err)
	}
	for _, b := range f.Bindings {
		if !filepath.IsAbs(b.Dir) || filepath.Clean(b.Dir) != b.Dir || validProjectScope(b.Scope) != nil {
			return nil, fmt.Errorf("read %s: invalid binding %q -> %q", projectBindingsPath(cfg), b.Dir, b.Scope)
		}
	}
	sort.Slice(f.Bindings, func(i, j int) bool { return f.Bindings[i].Dir < f.Bindings[j].Dir })
	return f.Bindings, nil
}

// saveProjectBindings has no lock: two simultaneous binds can lose one update.
// shortcut: single-owner CLI, add a lease if bindings ever get a daemon writer.
func saveProjectBindings(cfg Config, bindings []projectBinding) error {
	body, err := json.MarshalIndent(projectBindingFile{Bindings: bindings}, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.Write(projectBindingsPath(cfg), append(body, '\n'), 0o600)
}

func validProjectScope(scope string) error {
	name := strings.TrimPrefix(scope, "project:")
	if name == scope || name == "" || memoryfile.ValidateWriteScope(scope) != nil {
		return fmt.Errorf("invalid project scope %q: use project:NAME", scope)
	}
	return nil
}

func resolveProjectDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// bindProjectDir stores dir -> scope and reports the scope it replaced, if any.
func bindProjectDir(ctx context.Context, dir, scope string) (resolved, replaced string, err error) {
	return bindProjectDirOwner(ctx, dir, scope, false)
}

// bindProjectDirOwner is bindProjectDir with the owner_only flag. A rebind
// replaces the whole record, so a bind without the flag clears it.
func bindProjectDirOwner(ctx context.Context, dir, scope string, ownerOnly bool) (resolved, replaced string, err error) {
	if err := validProjectScope(scope); err != nil {
		return "", "", err
	}
	if resolved, err = resolveProjectDir(dir); err != nil {
		return "", "", fmt.Errorf("folder %q: %w", dir, err)
	}
	if info, serr := os.Stat(resolved); serr != nil || !info.IsDir() {
		return "", "", fmt.Errorf("%q is not a folder", dir)
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return "", "", err
	}
	bindings, err := loadProjectBindings(cfg)
	if err != nil {
		return "", "", err
	}
	out := bindings[:0:0]
	for _, b := range bindings {
		if sameProjectDir(b.Dir, resolved) {
			replaced = b.Scope
			continue
		}
		out = append(out, b)
	}
	out = append(out, projectBinding{Dir: resolved, Scope: scope, OwnerOnly: ownerOnly})
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return resolved, replaced, saveProjectBindings(cfg, out)
}

// sameProjectDir compares by file identity, so letter case on a
// case-insensitive volume cannot make one folder look like two. A path that
// no longer exists matches only by exact string.
func sameProjectDir(a, b string) bool {
	if a == b {
		return true
	}
	ai, aerr := os.Stat(a)
	bi, berr := os.Stat(b)
	return aerr == nil && berr == nil && os.SameFile(ai, bi)
}

// resolveExisting resolves symlinks in the deepest existing ancestor of path
// and appends the missing rest. It reports false when no ancestor resolves.
func resolveExisting(path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	rest := ""
	for cur := abs; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest), true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// lookupProject finds the longest bound directory that is dir or one of its
// parents, walking up from the deepest existing ancestor and comparing by file
// identity. It never guesses from a folder name, and it fails closed: an
// unreadable or invalid store gives found=false with an error.
func lookupProject(cfg Config, dir string) (scope, root string, found bool, err error) {
	b, found, err := lookupProjectBinding(cfg, dir)
	return b.Scope, b.Dir, found, err
}

// lookupProjectBinding is lookupProject returning the whole binding record.
func lookupProjectBinding(cfg Config, dir string) (projectBinding, bool, error) {
	bindings, err := loadProjectBindings(cfg)
	if err != nil {
		return projectBinding{}, false, err
	}
	cur, ok := resolveExisting(dir)
	if !ok {
		return projectBinding{}, false, nil
	}
	for {
		if info, serr := os.Stat(cur); serr == nil {
			for _, b := range bindings {
				if bi, berr := os.Stat(b.Dir); berr == nil && os.SameFile(info, bi) {
					return b, true, nil
				}
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return projectBinding{}, false, nil
		}
		cur = parent
	}
}

func cmdProject(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(projectUsage)
	}
	switch args[0] {
	case "bind", "unbind", "list", "which":
	default:
		return fmt.Errorf("unknown project command %q; %s", args[0], projectUsage)
	}
	fs := flag.NewFlagSet("mora project "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	scope := fs.String("scope", "", "project scope, project:NAME")
	jsonOut := fs.Bool("json", false, "emit JSON")
	ownerOnly := fs.Bool("owner-only", false, "let personal notes appear on their own in this folder")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 1 || (*scope != "" && args[0] != "bind") || (*ownerOnly && args[0] != "bind") || (*jsonOut && args[0] != "list" && args[0] != "which") {
		return errors.New(projectUsage)
	}
	dir := "."
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}
	if args[0] == "list" && fs.NArg() != 0 {
		return errors.New(projectUsage)
	}
	cfg, err := loadConfigFor(ctx)
	if err != nil {
		return err
	}
	switch args[0] {
	case "bind":
		s := *scope
		if s == "" {
			resolved, err := resolveProjectDir(dir)
			if err != nil {
				return fmt.Errorf("folder %q: %w", dir, err)
			}
			s = "project:" + strings.ToLower(filepath.Base(resolved))
			if validProjectScope(s) != nil {
				return fmt.Errorf("cannot derive a scope from folder name %q: pass --scope project:NAME", filepath.Base(resolved))
			}
		}
		resolved, replaced, err := bindProjectDirOwner(ctx, dir, s, *ownerOnly)
		if err != nil {
			return err
		}
		note := ""
		if *ownerOnly {
			note = " (owner-only)"
		}
		if replaced != "" && replaced != s {
			fmt.Fprintf(stdout, "%s -> %s%s (replaced %s)\n", resolved, s, note, replaced)
			return nil
		}
		fmt.Fprintf(stdout, "%s -> %s%s\n", resolved, s, note)
		return nil
	case "unbind":
		resolved, err := resolveProjectDir(dir)
		if err != nil {
			return fmt.Errorf("folder %q: %w", dir, err)
		}
		bindings, err := loadProjectBindings(cfg)
		if err != nil {
			return err
		}
		out := bindings[:0:0]
		for _, b := range bindings {
			if !sameProjectDir(b.Dir, resolved) {
				out = append(out, b)
			}
		}
		if len(out) == len(bindings) {
			return fmt.Errorf("%s is not bound to a project", resolved)
		}
		if err := saveProjectBindings(cfg, out); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "unbound %s\n", resolved)
		return nil
	case "list":
		bindings, err := loadProjectBindings(cfg)
		if err != nil {
			return err
		}
		if *jsonOut {
			if bindings == nil {
				bindings = []projectBinding{}
			}
			return json.NewEncoder(stdout).Encode(bindings)
		}
		for _, b := range bindings {
			fmt.Fprintf(stdout, "%s\t%s%s\n", b.Dir, b.Scope, ownerOnlyColumn(b))
		}
		return nil
	case "which":
		b, found, err := lookupProjectBinding(cfg, dir)
		if err != nil {
			return err
		}
		if !found {
			if *jsonOut {
				fmt.Fprintln(stdout, "null")
			}
			return errors.New("no project")
		}
		if *jsonOut {
			return json.NewEncoder(stdout).Encode(b)
		}
		fmt.Fprintf(stdout, "%s\t%s%s\n", b.Scope, b.Dir, ownerOnlyColumn(b))
		return nil
	}
	return errors.New(projectUsage)
}

// ownerOnlyColumn is the extra text column list and which print for an
// owner-only binding. Other bindings print exactly as before.
func ownerOnlyColumn(b projectBinding) string {
	if b.OwnerOnly {
		return "\towner-only"
	}
	return ""
}
