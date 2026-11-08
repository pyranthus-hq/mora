package sessions

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func relHomes(home string, dirs []string) []string {
	out := []string{}
	for _, dir := range dirs {
		rel, _ := filepath.Rel(home, dir)
		out = append(out, filepath.ToSlash(rel))
	}
	sort.Strings(out)
	return out
}

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need a privilege on Windows")
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func envOf(env map[string]string) func(string) string {
	return func(key string) string { return env[key] }
}

func TestCodexHomes(t *testing.T) {
	home := t.TempDir()
	orcaEnv := filepath.Join(home, "orca-data")
	want := []string{
		".codex",
		".config/orca/codex-accounts/acct-1/home",
		".config/orca/codex-accounts/acct-2/home",
		".config/orca/codex-runtime-home/home",
		".local/share/orca/codex-accounts/acct-3/home",
		".local/share/orca/codex-runtime-home/home",
		"Library/Application Support/orca/codex-accounts/acct-4/home",
		"custom-codex",
		"orca-data/codex-accounts/acct-5/home",
	}
	for _, dir := range want {
		mkdirs(t, filepath.Join(home, filepath.FromSlash(dir)))
	}
	mkdirs(t, filepath.Join(home, ".config/orca/codex-accounts/no-home"))
	var st Stats
	o := Options{Home: home, Getenv: envOf(map[string]string{
		"CODEX_HOME":          filepath.Join(home, "custom-codex"),
		"ORCA_USER_DATA_PATH": orcaEnv,
	})}
	if got := relHomes(home, codexHomes(o, &st)); !reflect.DeepEqual(got, want) {
		t.Fatalf("homes\n got %q\nwant %q", got, want)
	}

	t.Run("duplicates collapse", func(t *testing.T) {
		o := Options{Home: home, Getenv: envOf(map[string]string{"CODEX_HOME": filepath.Join(home, ".codex")})}
		got := relHomes(home, codexHomes(o, &st))
		count := 0
		for _, dir := range got {
			if dir == ".codex" {
				count++
			}
		}
		if count != 1 || len(got) != len(want)-2 {
			t.Fatalf("homes %q", got)
		}
		link := filepath.Join(home, "codex-link")
		symlink(t, filepath.Join(home, ".codex"), link)
		o = Options{Home: home, Getenv: envOf(map[string]string{"CODEX_HOME": link})}
		if again := relHomes(home, codexHomes(o, &st)); !reflect.DeepEqual(again, got) {
			t.Fatalf("a link to a home was not collapsed: %q", again)
		}
	})

	t.Run("env outside home is ignored", func(t *testing.T) {
		outside := t.TempDir()
		mkdirs(t, filepath.Join(outside, "codex-accounts", "a", "home"))
		o := Options{Home: home, Getenv: envOf(map[string]string{"CODEX_HOME": outside, "ORCA_USER_DATA_PATH": outside})}
		for _, dir := range codexHomes(o, &st) {
			if strings.HasPrefix(dir, outside) {
				t.Fatalf("reached %s", dir)
			}
		}
		o = Options{Home: home, Getenv: envOf(map[string]string{"CODEX_HOME": "relative/codex"})}
		if got := relHomes(home, codexHomes(o, &st)); strings.Contains(strings.Join(got, " "), "relative") {
			t.Fatalf("homes %q", got)
		}
	})
}

func TestCodexTreesRead(t *testing.T) {
	home := t.TempDir()
	body := codexMetaLine + customExec("c1", `tools.exec_command({cmd: "ls"})`)
	writeFile(t, filepath.Join(home, ".codex", "sessions", "2026", "rollout-a.jsonl"), body)
	writeFile(t, filepath.Join(home, ".codex", "sessions", "2026", "other.jsonl"), body)
	writeFile(t, filepath.Join(home, ".codex", "archived_sessions", "any-name.jsonl"), body)
	writeFile(t, filepath.Join(home, ".local", "share", "orca", "codex-runtime-home", "home", "sessions", "rollout-b.jsonl"), body)
	_, st := scanHome(t, home)
	if st.FilesRead != 3 || st.CodexSessions != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestEnvOutsideHomeIgnored(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(outside, "projects", "p", "s1.jsonl"), bash("t1", "ls"))
	rec := &recorder{}
	st, err := Scan(t.Context(), Options{Home: home, Getenv: envOf(map[string]string{"CLAUDE_CONFIG_DIR": outside})}, rec)
	if err != nil {
		t.Fatal(err)
	}
	if st.FilesRead != 0 || len(rec.calls) != 0 {
		t.Fatalf("calls=%d stats=%+v", len(rec.calls), st)
	}
}

func TestClaudeConfigDirUnderHome(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, "alt-claude")
	writeFile(t, filepath.Join(config, "projects", "p", "s2.jsonl"), bash("t2", "pwd"))
	writeFile(t, claudeFile(home, "s1.jsonl"), bash("t1", "ls"))
	for name, dir := range map[string]string{"second root": config, "same root": filepath.Join(home, ".claude")} {
		t.Run(name, func(t *testing.T) {
			rec := &recorder{}
			st, err := Scan(t.Context(), Options{Home: home, Getenv: envOf(map[string]string{"CLAUDE_CONFIG_DIR": dir})}, rec)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if dir == config {
				want = 2
			}
			if st.FilesRead != want || len(rec.calls) != want {
				t.Fatalf("calls=%d stats=%+v", len(rec.calls), st)
			}
		})
	}
}

// A symlinked ~/.claude/projects must not hide the real root it points at.
func TestClaudeRootLinkDoesNotHideConfigDir(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, "alt-claude")
	writeFile(t, filepath.Join(config, "projects", "p", "s1.jsonl"), bash("t1", "ls"))
	mkdirs(t, filepath.Join(home, ".claude"))
	symlink(t, filepath.Join(config, "projects"), filepath.Join(home, ".claude", "projects"))
	rec := &recorder{}
	st, err := Scan(t.Context(), Options{Home: home, Getenv: envOf(map[string]string{"CLAUDE_CONFIG_DIR": config})}, rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.calls) != 1 || st.SymlinksSkipped != 1 {
		t.Fatalf("calls=%d stats=%+v", len(rec.calls), st)
	}
}

func TestNeverOpenOtherCodexFilesOrFollowSymlink(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	forbidden := codexMetaLine + customExec("forbidden", `tools.exec_command({cmd: "cat secret"})`)
	for _, name := range []string{"private-file.jsonl", "history.jsonl", "auth.json", "rollout-top.jsonl"} {
		writeFile(t, filepath.Join(home, ".codex", name), forbidden)
	}
	writeFile(t, codexFile(home, "rollout-x.jsonl"), codexMetaLine+customExec("c1", `tools.exec_command({cmd: "ls"})`))
	secret := writeFile(t, filepath.Join(outside, "outside.jsonl"), forbidden)
	symlink(t, secret, codexFile(home, "rollout-symlink.jsonl"))
	rec, st := scanHome(t, home)
	if !reflect.DeepEqual(rec.commands(), []string{"ls"}) || st.FilesRead != 1 || st.SymlinksSkipped != 1 {
		t.Fatalf("commands=%q stats=%+v", rec.commands(), st)
	}
}

func TestSymlinkedCodexSessionsTreeIsNotRead(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(outside, "rollout-private.jsonl"), codexMetaLine+customExec("c1", `tools.exec_command({cmd: "ls"})`))
	mkdirs(t, filepath.Join(home, ".codex"))
	symlink(t, outside, filepath.Join(home, ".codex", "sessions"))
	rec, st := scanHome(t, home)
	if len(rec.calls) != 0 || st.FilesRead != 0 || st.SymlinksSkipped != 1 {
		t.Fatalf("calls=%d stats=%+v", len(rec.calls), st)
	}
}

func TestClaudeSymlinksAreNotFollowed(t *testing.T) {
	cases := map[string]func(t *testing.T, home, outside string){
		"main file": func(t *testing.T, home, outside string) {
			mkdirs(t, filepath.Dir(claudeFile(home, "x")))
			symlink(t, filepath.Join(outside, "p", "s9.jsonl"), claudeFile(home, "link.jsonl"))
		},
		"project folder": func(t *testing.T, home, outside string) {
			mkdirs(t, filepath.Join(home, ".claude", "projects"))
			symlink(t, filepath.Join(outside, "p"), filepath.Join(home, ".claude", "projects", "link"))
		},
		"projects root": func(t *testing.T, home, outside string) {
			mkdirs(t, filepath.Join(home, ".claude"))
			symlink(t, outside, filepath.Join(home, ".claude", "projects"))
		},
		"subagents folder": func(t *testing.T, home, outside string) {
			mkdirs(t, claudeFile(home, "s1"))
			symlink(t, filepath.Join(outside, "p"), claudeFile(home, "s1/subagents"))
		},
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			home, outside := t.TempDir(), t.TempDir()
			writeFile(t, filepath.Join(outside, "p", "s9.jsonl"), bash("t1", "ls"))
			writeFile(t, filepath.Join(outside, "p", "agent-x.jsonl"), bash("t2", "ls"))
			plant(t, home, outside)
			rec, st := scanHome(t, home)
			if len(rec.calls) != 0 || st.SymlinksSkipped != 1 {
				t.Fatalf("calls=%d stats=%+v", len(rec.calls), st)
			}
		})
	}
}

// A symlink under Home must not take the scan out of Home, by any route.
func TestSymlinkOutOfHomeIsNotRead(t *testing.T) {
	cases := []struct {
		name string
		link string // relative to Home, and it points at the outside folder
		env  string
	}{
		{"CLAUDE_CONFIG_DIR", "link", "CLAUDE_CONFIG_DIR"},
		{"CODEX_HOME", "link", "CODEX_HOME"},
		{"ORCA_USER_DATA_PATH", "link", "ORCA_USER_DATA_PATH"},
		{"claude folder", ".claude", ""},
		{"codex folder", ".codex", ""},
		{"orca folder", ".config/orca", ""},
		{"account home", ".config/orca/codex-accounts/acct/home", ""},
		{"runtime home", ".config/orca/codex-runtime-home/home", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home, outside := t.TempDir(), t.TempDir()
			claude := bash("t1", "ls")
			codex := codexMetaLine + customExec("c1", `tools.exec_command({cmd: "ls"})`)
			writeFile(t, filepath.Join(outside, "projects", "p", "s1.jsonl"), claude)
			writeFile(t, filepath.Join(outside, "sessions", "rollout-a.jsonl"), codex)
			writeFile(t, filepath.Join(outside, "codex-runtime-home", "home", "sessions", "rollout-b.jsonl"), codex)
			link := filepath.Join(home, filepath.FromSlash(c.link))
			mkdirs(t, filepath.Dir(link))
			symlink(t, outside, link)
			env := map[string]string{}
			if c.env != "" {
				env[c.env] = link
			}
			rec := &recorder{}
			st, err := Scan(t.Context(), Options{Home: home, Getenv: envOf(env)}, rec)
			if err != nil {
				t.Fatal(err)
			}
			if len(rec.calls) != 0 || st.FilesRead != 0 || st.SymlinksSkipped == 0 {
				t.Fatalf("calls=%d stats=%+v", len(rec.calls), st)
			}
		})
	}
}

// A linked codex-accounts is not listed at all when it leaves Home, so it counts
// once and not once per account. One that stays inside Home still reads.
func TestLinkedCodexAccounts(t *testing.T) {
	codex := codexMetaLine + customExec("c1", `tools.exec_command({cmd: "ls"})`)
	fill := func(accounts string) {
		for _, id := range []string{"one", "two"} {
			writeFile(t, filepath.Join(accounts, id, "home", "sessions", "rollout-"+id+".jsonl"), codex)
		}
	}
	home, outside := t.TempDir(), t.TempDir()
	fill(outside)
	link := filepath.Join(home, ".config", "orca", "codex-accounts")
	mkdirs(t, filepath.Dir(link))
	symlink(t, outside, link)
	if rec, st := scanHome(t, home); len(rec.calls) != 0 || st.SymlinksSkipped != 1 {
		t.Fatalf("outside Home: calls=%d stats=%+v", len(rec.calls), st)
	}
	home = t.TempDir()
	fill(filepath.Join(home, "moved"))
	link = filepath.Join(home, ".config", "orca", "codex-accounts")
	mkdirs(t, filepath.Dir(link))
	symlink(t, filepath.Join(home, "moved"), link)
	if rec, st := scanHome(t, home); st.FilesRead != 2 || st.SymlinksSkipped != 0 || len(rec.calls) != 2 {
		t.Fatalf("inside Home: calls=%d stats=%+v", len(rec.calls), st)
	}
}

// A link that stays inside Home is how a home can be moved, so it still reads.
func TestSymlinkInsideHomeIsRead(t *testing.T) {
	home := t.TempDir()
	real := filepath.Join(home, "elsewhere", "codex")
	writeFile(t, filepath.Join(real, "sessions", "rollout-a.jsonl"), codexMetaLine+customExec("c1", `tools.exec_command({cmd: "ls"})`))
	symlink(t, real, filepath.Join(home, ".codex"))
	rec, st := scanHome(t, home)
	if len(rec.calls) != 1 || st.SymlinksSkipped != 0 {
		t.Fatalf("calls=%d stats=%+v", len(rec.calls), st)
	}
}

// Home itself can be a link, as /home often is. Its transcripts are inside it.
func TestLinkedHomeIsRead(t *testing.T) {
	real, parent := t.TempDir(), t.TempDir()
	writeFile(t, claudeFile(real, "s1.jsonl"), bash("t1", "ls"))
	home := filepath.Join(parent, "home")
	symlink(t, real, home)
	rec, st := scanHome(t, home)
	if len(rec.calls) != 1 || st.SymlinksSkipped != 0 {
		t.Fatalf("calls=%d stats=%+v", len(rec.calls), st)
	}
}

func TestSinceSkipsOldFiles(t *testing.T) {
	home := t.TempDir()
	old := writeFile(t, claudeFile(home, "old.jsonl"), bash("t1", "ls"))
	writeFile(t, claudeFile(home, "new.jsonl"), bash("t2", "pwd"))
	oldCodex := writeFile(t, codexFile(home, "rollout-old.jsonl"), codexMetaLine)
	now := time.Now()
	for _, path := range []string{old, oldCodex} {
		if err := os.Chtimes(path, now.Add(-48*time.Hour), now.Add(-48*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	rec := &recorder{}
	st, err := Scan(t.Context(), Options{Home: home, Getenv: noEnv, Since: now.Add(-24 * time.Hour)}, rec)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rec.commands(), []string{"pwd"}) || st.FilesSkippedOld != 2 || st.FilesRead != 1 {
		t.Fatalf("commands=%q stats=%+v", rec.commands(), st)
	}
}

func TestEmptyFileIsNotASession(t *testing.T) {
	home := t.TempDir()
	writeFile(t, claudeFile(home, "empty.jsonl"), "")
	writeFile(t, codexFile(home, "rollout-empty.jsonl"), "")
	_, st := scanHome(t, home)
	if st != (Stats{EmptyFiles: 2}) {
		t.Fatalf("stats %+v", st)
	}
}
