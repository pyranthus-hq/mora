package sessions

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// transcript is one file to read. session is the file stem, or the folder
// above subagents for a subagent file.
type transcript struct {
	path, harness, session, agent string
	subagent                      bool
}

type discovery struct {
	o     Options
	st    *Stats
	files []transcript
}

// discover lists the transcripts under Home. Only the two known layouts are
// walked, so auth.json, history.jsonl and the like are never opened.
func discover(o Options, st *Stats) []transcript {
	d := &discovery{o: o, st: st}
	for _, root := range claudeRoots(o, st) {
		d.walk(root, func(path string, e fs.DirEntry) error { return d.claudeEntry(root, path, e) })
	}
	for _, home := range codexHomes(o, st) {
		d.codexTree(filepath.Join(home, "sessions"), "rollout-*.jsonl")
		d.codexTree(filepath.Join(home, "archived_sessions"), "*.jsonl")
	}
	sort.Slice(d.files, func(i, j int) bool { return d.files[i].path < d.files[j].path })
	return d.files
}

// walk visits everything under root except symlinks, which it counts. A
// symlinked root is not entered at all.
func (d *discovery) walk(root string, visit func(string, fs.DirEntry) error) {
	info, err := os.Lstat(root)
	if err != nil {
		return
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		d.st.SymlinksSkipped++
		return
	}
	_ = filepath.WalkDir(root, func(path string, e fs.DirEntry, err error) error {
		switch {
		case err != nil:
			d.st.UnreadableFiles++
			return nil
		case e.Type()&fs.ModeSymlink != 0:
			d.st.SymlinksSkipped++
			return nil
		case path == root:
			return nil
		}
		return visit(path, e)
	})
}

// claudeEntry keeps <project>/<session>.jsonl and agent-*.jsonl anywhere under
// <project>/<session>/subagents/. Other session folders are not entered.
func (d *discovery) claudeEntry(root, path string, e fs.DirEntry) error {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	inSubagents := len(parts) >= 3 && parts[2] == "subagents"
	if e.IsDir() {
		if len(parts) >= 3 && !inSubagents {
			return fs.SkipDir
		}
		return nil
	}
	name := e.Name()
	if len(parts) == 2 && strings.HasSuffix(name, ".jsonl") {
		d.add(e, transcript{path: path, harness: HarnessClaude, session: strings.TrimSuffix(name, ".jsonl")})
	} else if ok, _ := filepath.Match("agent-*.jsonl", name); ok && inSubagents {
		agent := strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl")
		d.add(e, transcript{path: path, harness: HarnessClaude, session: parts[1], agent: agent, subagent: true})
	}
	return nil
}

func (d *discovery) codexTree(root, pattern string) {
	d.walk(root, func(path string, e fs.DirEntry) error {
		if ok, _ := filepath.Match(pattern, e.Name()); ok && !e.IsDir() {
			stem := strings.TrimSuffix(e.Name(), ".jsonl")
			d.add(e, transcript{path: path, harness: HarnessCodex, session: stem})
		}
		return nil
	})
}

// add applies the checks that need no open: age, size and file type.
func (d *discovery) add(e fs.DirEntry, t transcript) {
	info, err := e.Info()
	switch {
	case err != nil || !info.Mode().IsRegular():
	case !d.o.Since.IsZero() && info.ModTime().Before(d.o.Since):
		d.st.FilesSkippedOld++
	case info.Size() == 0:
		d.st.EmptyFiles++
	default:
		d.files = append(d.files, t)
	}
}

// claudeRoots lists <Home>/.claude/projects and $CLAUDE_CONFIG_DIR/projects.
func claudeRoots(o Options, st *Stats) []string {
	roots := []string{filepath.Join(o.Home, ".claude", "projects")}
	if env := envUnderHome(o, "CLAUDE_CONFIG_DIR"); env != "" {
		roots = append(roots, filepath.Join(env, "projects"))
	}
	var keep []string
	for _, root := range roots {
		if info, err := os.Lstat(root); err == nil && info.Mode()&fs.ModeSymlink != 0 {
			st.SymlinksSkipped++
			continue
		}
		keep = append(keep, root)
	}
	return uniqueDirs(keep)
}

// envUnderHome returns an environment path only when it lies inside Home, so a
// temp HOME can never reach a real transcript through an inherited variable.
func envUnderHome(o Options, key string) string {
	v := o.getenv(key)
	if v == "" || !filepath.IsAbs(v) {
		return ""
	}
	rel, err := filepath.Rel(filepath.Clean(o.Home), filepath.Clean(v))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return v
}

// codexHomes lists ~/.codex, $CODEX_HOME and every Orca-managed home:
// <base>/codex-accounts/<id>/home and <base>/codex-runtime-home/home.
func codexHomes(o Options, st *Stats) []string {
	homes := []string{filepath.Join(o.Home, ".codex")}
	if env := envUnderHome(o, "CODEX_HOME"); env != "" {
		homes = append(homes, env)
	}
	bases := []string{
		envUnderHome(o, "ORCA_USER_DATA_PATH"),
		filepath.Join(o.Home, ".config", "orca"),
		filepath.Join(o.Home, "Library", "Application Support", "orca"),
		filepath.Join(o.Home, ".local", "share", "orca"),
	}
	for _, base := range bases {
		if base == "" {
			continue
		}
		homes = append(homes, filepath.Join(base, "codex-runtime-home", "home"))
		accounts := filepath.Join(base, "codex-accounts")
		entries, err := os.ReadDir(accounts)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.Type()&fs.ModeSymlink != 0 {
				st.SymlinksSkipped++
			} else if e.IsDir() {
				homes = append(homes, filepath.Join(accounts, e.Name(), "home"))
			}
		}
	}
	return uniqueDirs(homes)
}

// uniqueDirs keeps the directories that exist, once each by resolved path.
func uniqueDirs(dirs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, dir := range dirs {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		key := filepath.Clean(dir)
		if resolved, err := filepath.EvalSymlinks(key); err == nil {
			key = resolved
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, filepath.Clean(dir))
		}
	}
	return out
}
