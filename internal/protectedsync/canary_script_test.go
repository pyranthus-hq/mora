package protectedsync

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Exercise only synthetic executables. Never read real protected stores or
// change a host's app installation / FDA grant.
func TestFDACanaryHostFailureAndPrivacy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Bash canary targets Darwin/Linux")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	script, err := filepath.Abs("../../scripts/regress/fda-continuity-canary.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"", "doctor", "imessage", "applecalendar", "version", "stapler"} {
		t.Run("failure_"+failure, func(t *testing.T) {
			root := t.TempDir()
			bin := filepath.Join(root, "bin")
			app := filepath.Join(root, "Mora.app")
			evidence := filepath.Join(root, "evidence")
			for _, dir := range []string{bin, filepath.Join(app, "Contents", "MacOS")} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			write := func(path, body string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("#!/bin/bash\n"+body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(bin, "uname"), "echo Darwin\n")
			write(filepath.Join(bin, "codesign"), "echo synthetic-signature\n")
			write(filepath.Join(bin, "stapler"), "[ \"$FAIL_COMMAND\" != stapler ]\n")
			write(filepath.Join(app, "Contents", "MacOS", "mora"), `echo "$*" >> "$CALL_LOG"
key="$1"; [ "$1" != sync ] || key="$2"
echo synthetic-output
[ "$FAIL_COMMAND" != "$key" ]
`)
			cmd := exec.Command(bash, script, "HOST")
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "APP="+app, "EVIDENCE_DIR="+evidence, "LIVE=1", "FAIL_COMMAND="+failure, "CALL_LOG="+filepath.Join(root, "calls"))
			out, err := cmd.CombinedOutput()
			if (err != nil) != (failure != "") {
				t.Fatalf("failure=%q err=%v output=%s", failure, err, out)
			}
			if !strings.Contains(string(out), "live_canary: HAND_OFF") || !strings.Contains(string(out), "RAW and PRIVATE") {
				t.Fatalf("missing boundary: %s", out)
			}
			calls, err := os.ReadFile(filepath.Join(root, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"doctor --json", "sync imessage --json", "sync applecalendar --json"} {
				if !strings.Contains(string(calls), want) {
					t.Fatalf("did not collect remaining evidence: %s", calls)
				}
			}
			for _, p := range []string{evidence, filepath.Join(evidence, "live-imessage.json")} {
				st, err := os.Stat(p)
				if err != nil {
					t.Fatal(err)
				}
				if st.Mode().Perm()&0077 != 0 {
					t.Fatalf("public permissions on %s: %v", p, st.Mode())
				}
			}
		})
	}
}

func TestFDACanaryPinRejectsUnpinnedUpgradeTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Bash canary targets Darwin/Linux")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable")
	}
	script, err := filepath.Abs("../../scripts/regress/fda-continuity-canary.sh")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/bash\n"+body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write("gh", `case "$*" in
 *releases/latest*) echo v9.9.9 ;;
 *object.type*) echo commit ;;
 *) echo 0000000000000000000000000000000000000000 ;;
esac
`)
	write("curl", `out="${@: -1}"
for version in 0.15.0 0.15.1; do
 for arch in amd64 arm64; do
  printf '%064d  mora_%s_darwin_%s_app.zip\n' 0 "$version" "$arch"
 done
done > "$out"
`)
	env := append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "EVIDENCE_DIR="+filepath.Join(root, "evidence"), "N_TAG=v0.15.0", "N1_TAG=v0.15.1")
	cmd := exec.Command(bash, script, "PIN")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("PIN: %v %s", err, out)
	}
	text := string(out)
	for _, want := range []string{"mora upgrade has no version selector", "STOP: wrong baseline version", "STOP: upgrade did not install pinned N1_TAG", "No automatic sanitization", "live_canary: HAND_OFF"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	start := strings.Index(text, "# Run these steps in Bash")
	if start < 0 {
		t.Fatalf("missing handoff block: %s", text)
	}
	end := strings.Index(text[start:], "\n=== live_canary")
	if end < 0 {
		t.Fatalf("missing handoff block: %s", text)
	}
	write("curl", `out="${@: -1}"; : > "$out"`)
	missing := exec.Command(bash, script, "PIN")
	missing.Env = env
	missingOut, missingErr := missing.CombinedOutput()
	if missingErr == nil || strings.Contains(string(missingOut), "# Run these steps in Bash") {
		t.Fatalf("missing checksums accepted: %v %s", missingErr, missingOut)
	}
	handoff := text[start : start+end]
	cmd = exec.Command(bash, "-c", handoff)
	cmd.Env = env
	out, err = cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "STOP: latest release does not match pinned N1_TAG") {
		t.Fatalf("unguarded upgrade: %v %s", err, out)
	}
}
