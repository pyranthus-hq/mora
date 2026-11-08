package sessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openFile(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestEachLine(t *testing.T) {
	cases := map[string]struct {
		body    string
		lines   []string
		partial int
	}{
		"plain":            {"a\nb\n", []string{"a", "b"}, 0},
		"blank lines":      {"a\n\n  \r\nb\n", []string{"a", "b"}, 0},
		"crlf":             {"a\r\nb\r\n", []string{"a", "b"}, 0},
		"no last newline":  {"a\n{\"b\":1}", []string{"a"}, 1},
		"only partial":     {"a", nil, 1},
		"longer than read": {strings.Repeat("x", 3<<20) + "\nb\n", []string{strings.Repeat("x", 3<<20), "b"}, 0},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := openFile(t, writeFile(t, filepath.Join(t.TempDir(), "f.jsonl"), tc.body))
			var st Stats
			var got []string
			if err := eachLine(f, &st, func(line []byte) { got = append(got, string(line)) }); err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, "|") != strings.Join(tc.lines, "|") {
				t.Fatalf("got %d lines, want %d", len(got), len(tc.lines))
			}
			if st.Lines != len(tc.lines) || st.PartialLastLines != tc.partial {
				t.Fatalf("stats %+v", st)
			}
		})
	}
}

// A writer that keeps up with the reader must not keep the read going.
func TestReadStopsAtSizeTakenAtOpen(t *testing.T) {
	path := writeFile(t, filepath.Join(t.TempDir(), "f.jsonl"), "a\nb\nc\n")
	f := openFile(t, path)
	w, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var st Stats
	seen := 0
	err = eachLine(f, &st, func([]byte) {
		if seen++; seen > 100 {
			t.Fatal("the read did not stop")
		}
		if _, err := w.WriteString("more\n"); err != nil {
			t.Error(err)
		}
	})
	if err != nil || seen != 3 {
		t.Fatalf("seen=%d err=%v", seen, err)
	}
}

func TestFileShrinksDuringRead(t *testing.T) {
	t.Run("between open and read", func(t *testing.T) {
		path := writeFile(t, filepath.Join(t.TempDir(), "f.jsonl"), "a\nb\n")
		f := openFile(t, path)
		if err := os.Truncate(path, 0); err != nil {
			t.Fatal(err)
		}
		var st Stats
		if err := eachLine(f, &st, func([]byte) { t.Error("read a line from an empty file") }); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("while reading", func(t *testing.T) {
		// Larger than the reader's buffer, so a later read finds the file short.
		line := strings.Repeat("x", 1<<16) + "\n"
		path := writeFile(t, filepath.Join(t.TempDir(), "f.jsonl"), strings.Repeat(line, 64))
		f := openFile(t, path)
		var st Stats
		seen := 0
		err := eachLine(f, &st, func([]byte) {
			if seen++; seen == 1 {
				if err := os.Truncate(path, 10); err != nil {
					t.Error(err)
				}
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		if seen > 16 {
			t.Fatalf("read %d lines past the truncate", seen)
		}
	})
}
