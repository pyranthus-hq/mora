package mora

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	hookspkg "github.com/pyranthus-hq/mora/internal/hooks"
	"github.com/pyranthus-hq/mora/internal/rules"
)

// preToolSubprocessEnv is the parent environment without anything that could
// point the binary at a real home, config or agent session.
func preToolSubprocessEnv(home string, extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if key == "HOME" || key == "USERPROFILE" || key == "CLAUDECODE" || strings.HasPrefix(key, "MORA_") || strings.HasPrefix(key, "XDG_") {
			continue
		}
		env = append(env, kv)
	}
	return append(append(env, "HOME="+home, "USERPROFILE="+home), extra...)
}

type preToolExit struct {
	code           int
	stdout, stderr string
}

func runPreToolProcess(t *testing.T, name string, args []string, env []string, stdin string) preToolExit {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	res := preToolExit{stdout: out.String(), stderr: errb.String()}
	var exitErr *exec.ExitError
	switch {
	case errors.As(err, &exitErr):
		res.code = exitErr.ExitCode()
	case err != nil:
		t.Fatalf("run %s: %v", name, err)
	}
	return res
}

// TestHookPreToolExitCodes builds the real binary and proves the fail-open
// contract at the process and shell level.
func TestHookPreToolExitCodes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the installed command is a POSIX sh line")
	}
	if testing.Short() {
		t.Skip("builds the mora binary")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "mora")
	build := exec.Command(goBin, "build", "-o", bin, "./cmd/mora")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	golden, err := os.ReadFile(filepath.Join("testdata", "hooks", "pretool_warn.golden"))
	if err != nil {
		t.Fatal(err)
	}

	home := filepath.Join(dir, "home")
	stateDir := filepath.Join(home, ".local", "state", "mora")
	if err := os.MkdirAll(rules.Dir(stateDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := rules.WriteActive(rules.ActivePath(stateDir), preToolActiveFixture(t)); err != nil {
		t.Fatal(err)
	}

	subRun(t, "warned call prints the golden line", func(t *testing.T) {
		res := runPreToolProcess(t, bin, []string{"hook", "pre-tool-use"}, preToolSubprocessEnv(home), preToolInputS1)
		if res.code != 0 || res.stderr != "" || res.stdout != string(golden) {
			t.Fatalf("got %+v", res)
		}
	})
	subRun(t, "garbage stdin", func(t *testing.T) {
		res := runPreToolProcess(t, bin, []string{"hook", "pre-tool-use"}, preToolSubprocessEnv(home), "garbage\n")
		if res.code != 0 || res.stderr != "" || res.stdout != "" {
			t.Fatalf("got %+v", res)
		}
	})
	subRun(t, "worker panic is recovered", func(t *testing.T) {
		env := preToolSubprocessEnv(home, "MORA_TEST=1", "MORA_TEST_PRETOOL_PANIC=1")
		res := runPreToolProcess(t, bin, []string{"hook", "pre-tool-use"}, env, preToolInputS1)
		if res.code != 0 || res.stderr != "" || res.stdout != "" {
			t.Fatalf("got %+v", res)
		}
	})
	subRun(t, "shell guard hides exit 2", func(t *testing.T) {
		crash := filepath.Join(dir, "crashing-mora")
		script := "#!/bin/sh\necho 'fatal error: all goroutines are asleep' >&2\nexit 2\n"
		if err := os.WriteFile(crash, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		direct := runPreToolProcess(t, crash, []string{"hook", "pre-tool-use"}, preToolSubprocessEnv(home), preToolInputS1)
		if direct.code != 2 || direct.stderr == "" {
			t.Fatalf("the fake binary should fail loudly on its own, got %+v", direct)
		}
		res := runPreToolProcess(t, "sh", []string{"-c", hookspkg.PreToolCommand(crash)}, preToolSubprocessEnv(home), preToolInputS1)
		if res.code != 0 || res.stderr != "" || res.stdout != "" {
			t.Fatalf("installed command leaked the failure: %+v", res)
		}
	})
	subRun(t, "installed command passes the warning through", func(t *testing.T) {
		res := runPreToolProcess(t, "sh", []string{"-c", hookspkg.PreToolCommand(bin)}, preToolSubprocessEnv(home), preToolInputS1)
		if res.code != 0 || res.stderr != "" || res.stdout != string(golden) {
			t.Fatalf("got %+v", res)
		}
	})
}
