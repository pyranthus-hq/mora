package genericutil

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPtr(t *testing.T) {
	if got := Ptr(true); got == nil || *got != true {
		t.Fatalf("Ptr(true) = %v, want pointer to true", got)
	}
	if got := Ptr(false); got == nil || *got != false {
		t.Fatalf("Ptr(false) = %v, want pointer to false", got)
	}
}

func TestIsInteractive(t *testing.T) {
	// A non-*os.File reader is never interactive.
	if IsInteractive(strings.NewReader("")) {
		t.Error("strings.Reader must not be interactive")
	}
	// A real regular file is an *os.File but not a char device => false.
	f, err := os.CreateTemp(t.TempDir(), "notty")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if IsInteractive(f) {
		t.Error("regular file must not be interactive")
	}
	// A closed *os.File makes Stat fail => the Stat-error branch returns false.
	f2, err := os.CreateTemp(t.TempDir(), "closed")
	if err != nil {
		t.Fatal(err)
	}
	f2.Close()
	if IsInteractive(f2) {
		t.Error("closed file (Stat error) must not be interactive")
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := TruncateRunes("abc", 0); got != "" {
		t.Fatalf("max<=0 should yield empty, got %q", got)
	}
	if got := TruncateRunes("abc", -3); got != "" {
		t.Fatalf("negative max should yield empty, got %q", got)
	}
	if got := TruncateRunes("hello", 10); got != "hello" {
		t.Fatalf("short string should be unchanged, got %q", got)
	}
	if got := TruncateRunes("hello", 5); got != "hello" {
		t.Fatalf("exact length should be unchanged, got %q", got)
	}
	if got := TruncateRunes("hello world", 5); got != "hello" {
		t.Fatalf("expected ASCII clip to 5, got %q", got)
	}
	// Multibyte: "hé" is bytes h(1)+é(2). max=2 lands inside é, so it must back
	// up to a rune boundary rather than split the rune.
	s := "héllo"
	got := TruncateRunes(s, 2)
	if got != "h" {
		t.Fatalf("expected rune-safe backup to %q, got %q", "h", got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("TruncateRunes produced invalid UTF-8: %q", got)
	}
	// A whole multibyte string under the limit is returned intact.
	if got := TruncateRunes(s, 100); got != s {
		t.Fatalf("multibyte string within limit should be unchanged, got %q", got)
	}
}

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	present := dir + "/present.txt"
	if err := os.WriteFile(present, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !FileExists(present) {
		t.Fatalf("FileExists(%q) = false, want true", present)
	}
	if FileExists(dir + "/nope.txt") {
		t.Fatal("FileExists(missing) = true, want false")
	}
}

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{" a , b ,, c ", []string{"a", "b", "c"}},
		{",,", nil},
	}
	for _, tc := range cases {
		got := SplitCSV(tc.in)
		if len(got) != len(tc.want) {
			t.Fatalf("SplitCSV(%q) = %v, want %v", tc.in, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("SplitCSV(%q) = %v, want %v", tc.in, got, tc.want)
			}
		}
	}
}

func TestExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	if got := ExpandHome("~/x/y"); got != home+"/x/y" && got != home+`\x\y` {
		t.Fatalf("ExpandHome(~/x/y) = %q, want under %q", got, home)
	}
	if got := ExpandHome("~/x/y"); !strings.HasPrefix(got, home) {
		t.Fatalf("expanded path %q must be rooted at HOME %q", got, home)
	}
	// Unchanged: absolute, relative, bare ~, and ~user (no "~/" prefix).
	for _, p := range []string{"/abs/path", "rel/path", "~", "~otheruser", "./x", "a~/b"} {
		if got := ExpandHome(p); got != p {
			t.Errorf("ExpandHome(%q) = %q, want unchanged", p, got)
		}
	}
}

func TestIsHelpFlag(t *testing.T) {
	for _, s := range []string{"--help", "-h", "help"} {
		if !IsHelpFlag(s) {
			t.Errorf("IsHelpFlag(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "-help", "--h", "helper", "-y"} {
		if IsHelpFlag(s) {
			t.Errorf("IsHelpFlag(%q) = true, want false", s)
		}
	}
}
