package sessions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// recorder keeps what Scan reports. Scan serializes the calls.
type recorder struct {
	calls   []Call
	denials []Denial
}

func (r *recorder) Call(c Call)     { r.calls = append(r.calls, c) }
func (r *recorder) Denial(d Denial) { r.denials = append(r.denials, d) }

func (r *recorder) commands() []string {
	out := []string{}
	for _, c := range r.calls {
		out = append(out, c.Command)
	}
	return out
}

func noEnv(string) string { return "" }

func scanHome(t *testing.T, home string) (*recorder, Stats) {
	t.Helper()
	rec := &recorder{}
	st, err := Scan(context.Background(), Options{Home: home, Getenv: noEnv}, rec)
	if err != nil {
		t.Fatal(err)
	}
	return rec, st
}

func writeFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// copyFixture puts the fixture where the harnesses keep their files. The repo
// holds it as claude and codex, because .gitignore ignores .claude.
func copyFixture(t *testing.T, home string) {
	t.Helper()
	for from, to := range map[string]string{"claude": ".claude", "codex": ".codex"} {
		if err := os.CopyFS(filepath.Join(home, to), os.DirFS(filepath.Join("testdata", "home", from))); err != nil {
			t.Fatal(err)
		}
	}
}

func claudeFile(home, name string) string {
	return filepath.Join(home, ".claude", "projects", "-fake-work", filepath.FromSlash(name))
}

func codexFile(home, name string) string {
	return filepath.Join(home, ".codex", "sessions", "2026", "10", "03", name)
}

const (
	ts1 = "2026-10-01T10:00:00.000Z"
	ts2 = "2026-10-02T10:00:00.000Z"
)

func toolUse(id, name, command string) string {
	return fmt.Sprintf(`{"message":{"id":"m-%s","role":"assistant","content":[{"type":"tool_use","id":"%s","name":"%s","input":{"command":%s}}]},"type":"assistant","timestamp":"%s","sessionId":"s1"}`+"\n",
		id, id, name, strconv.Quote(command), ts1)
}

func bash(id, command string) string { return toolUse(id, "Bash", command) }

// toolResult is a user record for one tool. extra adds top-level fields.
func toolResult(id, extra string) string {
	return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"text","is_error":true,"tool_use_id":"%s"}]},"timestamp":"%s","sessionId":"s1"%s}`+"\n",
		id, ts2, extra)
}

func prompt(extra string) string {
	return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"invented"},"timestamp":"%s","sessionId":"s1"%s}`+"\n", ts2, extra)
}

const codexMetaLine = `{"timestamp":"2026-10-03T12:00:00.000Z","type":"session_meta","payload":{"id":"thread-1","timestamp":"2026-10-03T12:00:00.000Z"}}` + "\n"

func codexResponse(payload string) string {
	return `{"timestamp":"2026-10-03T12:00:05.000Z","type":"response_item","payload":` + payload + "}\n"
}
