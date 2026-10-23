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

func TestScrubLegacyTokenLogPreservesOpenAppendFile(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		name := "regular"
		if symlink {
			name = "symlink"
		}
		t.Run(name, func(t *testing.T) {
			d := t.TempDir()
			path := filepath.Join(d, "serve-http.out.log")
			target := path
			if symlink {
				target = filepath.Join(d, "target.log")
			}
			if err := os.WriteFile(target, []byte("  token: "+strings.Repeat("a", 64)+"\nkeep\n"), 0640); err != nil {
				t.Fatal(err)
			}
			if symlink {
				if err := os.Symlink(target, path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			before, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if err := ScrubLegacyTokenLog(d); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatal("scrub changed inode or mode")
			}
			if symlink {
				info, err := os.Lstat(path)
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("scrub replaced symlink", err)
				}
			}
			if _, err := f.WriteString("after scrub\n"); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != "keep\nafter scrub\n" {
				t.Fatalf("log after append = %q", b)
			}
		})
	}
}
