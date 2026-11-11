package mora

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// wslHome sets up a simulated WSL machine: a distro name, a mount root with a
// mounted C drive holding a Windows profile, and returns the Windows hooks file.
func wslHome(t *testing.T, distro string) (hooksFile string) {
	t.Helper()
	mnt := t.TempDir()
	cfgDir := filepath.Join(mnt, "c", "Users", "adit", ".gemini", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	withWSLEnv(t, "linux", mnt)
	withWSLDistro(t, distro)
	return filepath.Join(cfgDir, "hooks.json")
}

func installAntigravity(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := cmdHook(testCtx(t), append([]string{"install", "--client", "antigravity"}, args...), &out, testStderr, nil)
	return out.String(), err
}

func moraCommands(t *testing.T, file string) (start, recall string) {
	t.Helper()
	m := readHooksJSON(t, file)["mora"].(map[string]any)
	cmd := func(ev string) string { return m[ev].([]any)[0].(map[string]any)["command"].(string) }
	return cmd("SessionStart"), cmd("PreInvocation")
}

func TestAntigravityUnderWSLWritesWslExeCommandsForWindowsFile(t *testing.T) {
	home := withTempHookHome(t)
	file := wslHome(t, "Ubuntu")
	if _, err := installAntigravity(t, "--hooks-file", file, "--threshold", "-1"); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(home, "bin", "mora")
	start, recall := moraCommands(t, file)
	if want := "wsl.exe -d Ubuntu -e " + exe + " hook session-start --client antigravity"; start != want {
		t.Fatalf("session start command = %q, want %q", start, want)
	}
	if want := "wsl.exe -d Ubuntu -e " + exe + " hook recall --client antigravity --threshold -1"; recall != want {
		t.Fatalf("recall command = %q, want %q", recall, want)
	}
	if strings.Contains(start, "'") {
		t.Fatal("cmd /c cannot run a POSIX-quoted command")
	}
}

func TestAntigravityUnderWSLQuotesDistroAndPathWithSpaces(t *testing.T) {
	if got := antigravityHookCommand("/opt/my tools/mora", "recall", "Ubuntu 24", "--threshold", "1"); got !=
		`wsl.exe -d "Ubuntu 24" -e "/opt/my tools/mora" hook recall --client antigravity --threshold 1` {
		t.Fatalf("command = %s", got)
	}
}

func TestAntigravityUnderWSLRefusesWithoutHooksFile(t *testing.T) {
	home := withTempHookHome(t)
	wslHome(t, "Ubuntu")
	for _, sub := range [][]string{{"install"}, {"uninstall"}, {"status"}} {
		var out bytes.Buffer
		err := cmdHook(testCtx(t), append(sub, "--client", "antigravity"), &out, testStderr, nil)
		if err == nil || !strings.Contains(err.Error(), "--hooks-file") || !strings.Contains(err.Error(), "/mnt/c/Users/<name>/.gemini/config/hooks.json") {
			t.Fatalf("%v: want a refusal naming the flag and the /mnt/c form, got %v", sub, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".gemini")); err == nil {
		t.Fatal("nothing may be written to the Linux home under WSL")
	}
}

func TestAntigravityUnderWSLLinuxFileKeepsShellQuoting(t *testing.T) {
	home := withTempHookHome(t)
	wslHome(t, "Ubuntu")
	file := filepath.Join(t.TempDir(), "hooks.json")
	if _, err := installAntigravity(t, "--hooks-file", file); err != nil {
		t.Fatal(err)
	}
	start, _ := moraCommands(t, file)
	if want := "'" + filepath.Join(home, "bin", "mora") + "' hook session-start --client antigravity"; start != want {
		t.Fatalf("a Linux hooks file keeps the sh form: %q, want %q", start, want)
	}
}

func TestAntigravityNativeLinuxUnchanged(t *testing.T) {
	home := withTempHookHome(t) // no WSL distro
	if _, err := installAntigravity(t); err != nil {
		t.Fatal(err)
	}
	start, _ := moraCommands(t, filepath.Join(home, ".gemini", "config", "hooks.json"))
	if !strings.HasPrefix(start, "'") || strings.Contains(start, "wsl.exe") {
		t.Fatalf("native install must keep the sh form, got %q", start)
	}
}

func TestAntigravityStatusPrintsInspectedFile(t *testing.T) {
	withTempHookHome(t)
	file := wslHome(t, "Ubuntu")
	if _, err := installAntigravity(t, "--hooks-file", file); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := cmdHook(testCtx(t), []string{"status", "--client", "antigravity", "--hooks-file", file}, &out, testStderr, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "File: "+file) || !strings.Contains(out.String(), "SessionStart: installed") {
		t.Fatalf("status = %q", out.String())
	}
}

func TestAntigravityStatusJSONKeepsV1Fields(t *testing.T) {
	withTempHookHome(t)
	path := filepath.Join(t.TempDir(), "hooks.json")
	if _, err := installAntigravity(t, "--hooks-file", path); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := cmdHook(testCtx(t), []string{"status", "--client", "antigravity", "--json", "--hooks-file", path}, &out, testStderr, nil); err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"installed", "path", "harnesses", "pre_tool_use", "global", "bound_scope"} {
		if _, ok := got[key]; !ok {
			t.Errorf("mora.hook.status v1 field %q is missing: %s", key, out.String())
		}
	}
	var global hookLevelStatus
	_ = json.Unmarshal(got["global"], &global)
	if global.SessionStart != "installed" || global.UserPromptSubmit != "installed" || global.SessionEnd != "not installed" {
		t.Fatalf("global = %+v", global)
	}
	if string(got["pre_tool_use"]) != `"not installed"` {
		t.Fatalf("pre_tool_use = %s", got["pre_tool_use"])
	}
}

func TestAntigravityWriteFollowsSymlinkKeepsModeAndDoesNotEscape(t *testing.T) {
	withTempHookHome(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "real-hooks.json")
	const foreign = `{"orca":{"PreInvocation":[{"type":"command","command":"a.cmd && b.cmd > <out>"}]}}`
	if err := os.WriteFile(target, []byte(foreign), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "hooks.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := installAntigravity(t, "--hooks-file", link); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced by a regular file: %v", err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"mora"`) {
		t.Fatalf("the symlink target was not updated:\n%s", body)
	}
	if !strings.Contains(string(body), `a.cmd && b.cmd > <out>`) || strings.Contains(string(body), `\u0026`) || strings.Contains(string(body), `\u003c`) {
		t.Fatalf("another hook's command was re-encoded:\n%s", body)
	}
	if info, _ := os.Stat(target); info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %v, want the existing 0640 kept", info.Mode().Perm())
	}
	if _, err := installAntigravity(t, "--hooks-file", filepath.Join(dir, "new.json")); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(filepath.Join(dir, "new.json")); info.Mode().Perm() != 0o600 {
		t.Fatalf("a new file must be 0600, got %v", info.Mode().Perm())
	}
}

func TestNormalizeWSLNetworkPaths(t *testing.T) {
	mnt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(mnt, "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	withWSLEnv(t, "linux", mnt)
	withWSLDistro(t, "Ubuntu")
	cases := map[string]string{
		`\\wsl.localhost\Ubuntu\home\adit\app`: "/home/adit/app",
		`\\wsl$\Ubuntu\home\adit\app\`:         "/home/adit/app",
		`//wsl.localhost/Ubuntu/home/adit/app`: "/home/adit/app",
		`//wsl$/ubuntu/home/adit/../adit/app`:  "/home/adit/app",
		`\\wsl.localhost\Debian\home\adit\app`: `\\wsl.localhost\Debian\home\adit\app`,
		`\\wsl.localhost\Ubuntu`:               "/",
		`\\wsl.localhost\`:                     `\\wsl.localhost\`,
		`C:\..\..\etc`:                         `C:\..\..\etc`,
		`C:\a\..\..\etc`:                       `C:\a\..\..\etc`,
		`C:\a\..\b\.\c`:                        filepath.Join(mnt, "c", "b", "c"),
		`c:/Users/../Users/adit`:               filepath.Join(mnt, "c", "Users", "adit"),
	}
	for in, want := range cases {
		if got := normalizeWSLWindowsPath(in); got != want {
			t.Errorf("normalizeWSLWindowsPath(%q) = %q, want %q", in, got, want)
		}
	}
	withWSLDistro(t, "")
	if got := normalizeWSLWindowsPath(`\\wsl.localhost\Ubuntu\home\adit`); got != `\\wsl.localhost\Ubuntu\home\adit` {
		t.Fatalf("outside WSL a network path must be left alone, got %q", got)
	}
	withWSLEnv(t, "windows", mnt)
	withWSLDistro(t, "Ubuntu")
	if got := normalizeWSLWindowsPath(`\\wsl.localhost\Ubuntu\home\adit`); got != `\\wsl.localhost\Ubuntu\home\adit` {
		t.Fatalf("on Windows nothing is rewritten, got %q", got)
	}
}

func TestMCPServeAcceptsWindowsProjectDirUnderWSL(t *testing.T) {
	cfg := mcpProjectSetup(t)
	_ = cfg
	mnt := t.TempDir()
	dir := filepath.Join(mnt, "c", "Users", "adit", "app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	withWSLEnv(t, "linux", mnt)
	if _, _, err := bindProjectDir(testCtx(t), dir, "project:wsl"); err != nil {
		t.Fatal(err)
	}
	replies, stderr := mcpProjectServe(t, []string{"--project-dir", `C:\Users\adit\app`})
	if strings.Contains(stderr, "not bound") || !strings.Contains(replies[0].Result.Instructions, `"project:wsl"`) {
		t.Fatalf("a Windows --project-dir must bind the server under WSL; stderr=%q instructions=%q", stderr, replies[0].Result.Instructions)
	}
}

func TestReadEscapesTerminalControlSequencesButJSONKeepsData(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	body := "hello \x1b]52;c;Zm9v\x07 world \x1b[2J gone"
	pbSeed(t, cfg, "mem_esc", "global", "note", "Title \x1b[31mred", body, 2)
	got := run(t, "read", "mem_esc")
	if strings.ContainsRune(got, 0x1b) || strings.ContainsRune(got, 0x07) {
		t.Fatalf("a control character reached the terminal: %q", got)
	}
	if !strings.Contains(got, "hello") || !strings.Contains(got, "gone") {
		t.Fatalf("the note text must still be shown: %q", got)
	}
	var data struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(run(t, "read", "mem_esc", "--json")), &data); err != nil || data.Text != body {
		t.Fatalf("--json must carry the unescaped text, got %q (%v)", data.Text, err)
	}
}

type endlessReader struct{ n int }

func (r *endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	r.n += len(p)
	return len(p), nil
}

func TestSessionEndStdinIsBounded(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	r := &endlessReader{}
	done := make(chan error, 1)
	go func() { done <- runHookSessionEnd(testCtx(t), nil, io.Discard, r) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runHookSessionEnd kept reading an endless stdin")
	}
	if r.n > handoffInputBytes+(64<<10) {
		t.Fatalf("read %d bytes, want at most about %d", r.n, handoffInputBytes)
	}
}
