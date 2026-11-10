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

const projectUsage = "usage: mora project bind [--scope project:NAME] [DIR] | unbind [DIR] | list [--json] | which [DIR] [--json]"

// projectBinding ties one resolved directory to one project scope.
type projectBinding struct {
	Dir   string `json:"dir"`
	Scope string `json:"scope"`
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
		if b.Dir == resolved {
			replaced = b.Scope
			continue
		}
		out = append(out, b)
	}
	out = append(out, projectBinding{Dir: resolved, Scope: scope})
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return resolved, replaced, saveProjectBindings(cfg, out)
}

// lookupProject finds the longest bound directory that equals dir or contains
// it. It never guesses from a folder name.
func lookupProject(cfg Config, dir string) (scope, root string, found bool, err error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", false, err
	}
	if r, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		abs = r
	}
	bindings, err := loadProjectBindings(cfg)
	if err != nil {
		return "", "", false, err
	}
	for _, b := range bindings {
		if (abs == b.Dir || strings.HasPrefix(abs, strings.TrimSuffix(b.Dir, string(filepath.Separator))+string(filepath.Separator))) && len(b.Dir) > len(root) {
			scope, root, found = b.Scope, b.Dir, true
		}
	}
	return scope, root, found, nil
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
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 1 || (*scope != "" && args[0] != "bind") || (*jsonOut && args[0] != "list" && args[0] != "which") {
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
		resolved, replaced, err := bindProjectDir(ctx, dir, s)
		if err != nil {
			return err
		}
		if replaced != "" && replaced != s {
			fmt.Fprintf(stdout, "%s -> %s (replaced %s)\n", resolved, s, replaced)
			return nil
		}
		fmt.Fprintf(stdout, "%s -> %s\n", resolved, s)
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
			if b.Dir != resolved {
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
			fmt.Fprintf(stdout, "%s\t%s\n", b.Dir, b.Scope)
		}
		return nil
	case "which":
		s, root, found, err := lookupProject(cfg, dir)
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
			return json.NewEncoder(stdout).Encode(projectBinding{Dir: root, Scope: s})
		}
		fmt.Fprintf(stdout, "%s\t%s\n", s, root)
		return nil
	}
	return errors.New(projectUsage)
}
