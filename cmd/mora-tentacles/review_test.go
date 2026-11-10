package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeMora writes a stand-in for mora that logs each call to a file and runs body.
func fakeMora(t *testing.T, body string) (log string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fake")
	}
	dir := t.TempDir()
	log = filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$*\" >> '" + log + "'\n" + body
	bin := filepath.Join(dir, "mora")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MORA_BIN", bin)
	return log
}

func calls(t *testing.T, log string) []string {
	b, err := os.ReadFile(log)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// capture runs fn with stdout and stderr redirected and returns both.
func capture(t *testing.T, fn func() error) (out, errOut string, err error) {
	t.Helper()
	ro, wo, _ := os.Pipe()
	re, we, _ := os.Pipe()
	so, se := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = wo, we
	err = fn()
	os.Stdout, os.Stderr = so, se
	_ = wo.Close()
	_ = we.Close()
	a, _ := io.ReadAll(ro)
	b, _ := io.ReadAll(re)
	return string(a), string(b), err
}

func TestReviewListMergesTextsCutsAndDropsBadRows(t *testing.T) {
	var rows []string
	rows = append(rows, `{"id":"mem_1","kind":"saved","scope":"global","type":"insight","title":"`+strings.Repeat("t", 300)+`"}`)
	rows = append(rows, `{"id":"p_2","kind":"held","scope":"project:x","type":"fact","title":"Held"}`)
	rows = append(rows, `{"id":"bad id","kind":"saved","scope":"g","type":"t","title":"x"}`)
	rows = append(rows, `{"id":"-x","kind":"saved","scope":"g","type":"t","title":"x"}`)
	rows = append(rows, `{"id":"mem_3","kind":"other","scope":"g","type":"t","title":"x"}`)
	for i := 0; i < 52; i++ {
		rows = append(rows, `{"id":"mem_n`+strings.Repeat("1", i%5)+string(rune('a'+i%26))+string(rune('a'+i/26))+`","kind":"saved","scope":"g","type":"t","title":"n"}`)
	}
	list := `{"notes":[` + strings.Join(rows, ",") + `]}`
	long := strings.Repeat("é", 5000)
	log := fakeMora(t, `case "$1 $2" in
"review --list") cat <<'EOF'
`+list+`
EOF
;;
"read mem_1") echo '{"text":"`+long+`"}';;
"mcp proposals") echo '{"inspection":{"arguments":{"text":"held text"}}}';;
*) echo '{"text":"saved text"}';;
esac
`)
	out, stderr, err := capture(t, reviewList)
	if err != nil || stderr != "" {
		t.Fatalf("err=%v stderr=%q", err, stderr)
	}
	var got ReviewList
	if json.Unmarshal([]byte(out), &got) != nil {
		t.Fatalf("bad json %q", out)
	}
	if len(got.Notes) != 50 || got.More != 4 { // 2 good + 52 numbered = 54 valid
		t.Fatalf("notes=%d more=%d", len(got.Notes), got.More)
	}
	if n := got.Notes[0]; n.ID != "mem_1" || len([]rune(n.Title)) != 200 || len([]rune(n.Text)) != 4000 {
		t.Fatalf("first note not cut: %d %d", len([]rune(n.Title)), len([]rune(n.Text)))
	}
	if n := got.Notes[1]; n.Kind != "held" || n.Text != "held text" {
		t.Fatalf("held note: %+v", n)
	}
	if got.Notes[2].Text != "saved text" {
		t.Fatalf("saved text: %+v", got.Notes[2])
	}
	for _, c := range calls(t, log) {
		if strings.Contains(c, "bad id") || strings.Contains(c, "-x") || strings.Contains(c, "mem_3") {
			t.Fatalf("dropped row was fetched: %s", c)
		}
	}
}

func TestReviewListFailureGivesFixedError(t *testing.T) {
	fakeMora(t, "echo 'secret path /Users/x leaked' >&2\necho 'leaked stdout'\nexit 3\n")
	out, stderr, err := capture(t, reviewList)
	if err == nil {
		t.Fatal("want non-zero exit")
	}
	var got map[string]string
	if json.Unmarshal([]byte(out), &got) != nil || got["error"] == "" {
		t.Fatalf("out=%q", out)
	}
	if strings.Contains(out+stderr, "leaked") || strings.Contains(out+stderr, "secret") {
		t.Fatalf("mora text passed through: %q %q", out, stderr)
	}
}

func TestReviewActRunsExactlyOneCommand(t *testing.T) {
	want := map[string]string{
		"keep saved":    "confirm mem_1",
		"discard saved": "undo mem_1",
		"keep held":     "mcp proposals approve mem_1",
		"discard held":  "mcp proposals reject mem_1",
	}
	for k, w := range want {
		log := fakeMora(t, "exit 0\n")
		f := strings.Fields(k)
		out, _, err := capture(t, func() error { return reviewAct([]string{f[0], f[1], "mem_1"}) })
		if err != nil || strings.TrimSpace(out) != `{"ok":true}` {
			t.Fatalf("%s: err=%v out=%q", k, err, out)
		}
		if c := calls(t, log); len(c) != 1 || c[0] != w {
			t.Fatalf("%s: calls=%q want %q", k, c, w)
		}
	}
}

func TestReviewActRefusesBadInputBeforeRunning(t *testing.T) {
	log := fakeMora(t, "exit 0\n")
	for _, a := range [][]string{
		{"keep", "saved", "x; rm -rf /"}, {"keep", "saved", "--force"}, {"discard", "held", "a b"},
		{"keep", "saved", ""}, {"keep", "saved", strings.Repeat("a", 81)},
		{"approve", "saved", "mem_1"}, {"keep", "mine", "mem_1"}, {"keep", "saved"}, {"keep", "saved", "mem_1", "--mine"},
	} {
		out, _, err := capture(t, func() error { return reviewAct(a) })
		if err == nil || !strings.Contains(out, `"ok":false`) {
			t.Fatalf("%q not refused: out=%q err=%v", a, out, err)
		}
	}
	if c := calls(t, log); len(c) != 0 {
		t.Fatalf("a refused request ran mora: %q", c)
	}
}

func TestReviewActFailureGivesFixedText(t *testing.T) {
	fakeMora(t, "echo 'raw failure detail' >&2\necho 'raw stdout'\nexit 1\n")
	out, stderr, err := capture(t, func() error { return reviewAct([]string{"discard", "saved", "mem_1"}) })
	if err == nil || !strings.Contains(out, `"ok":false`) || !strings.Contains(out, `"error":"Mora could not complete the action."`) {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if strings.Contains(out+stderr, "raw") {
		t.Fatalf("mora text passed through: %q %q", out, stderr)
	}
}
