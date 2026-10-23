package httpservice

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestScrubLegacyTokenLog(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n", ""} {
		t.Run("ending-"+ending, func(t *testing.T) {
			d := t.TempDir()
			p := filepath.Join(d, "serve-http.out.log")
			keep := "starting\n  token: diagnostic\nother token: " + strings.Repeat("b", 64) + "\n"
			body := "  token: " + strings.Repeat("a", 64) + "\n" + keep + "  token: " + strings.Repeat("c", 64) + ending
			if err := os.WriteFile(p, []byte(body), 0640); err != nil {
				t.Fatal(err)
			}
			if err := ScrubLegacyTokenLog(d); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != keep {
				t.Fatalf("remaining=%q", b)
			}
			info, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0640 {
				t.Fatalf("mode=%v", info.Mode().Perm())
			}
		})
	}
	d := t.TempDir()
	if err := ScrubLegacyTokenLog(d); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d, "serve-http.out.log")); !os.IsNotExist(err) {
		t.Fatalf("missing log created: %v", err)
	}
}
