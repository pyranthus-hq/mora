package briefartifact

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)

func TestPathFixedUTCDate(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "vault")
	want := filepath.Join(vault, "briefs", "2026-06-08-brief.md")
	if got := Path(vault, fixedNow); got != want {
		t.Fatalf("Path=%q want %q", got, want)
	}
}

func TestPathCanonicalizesToUTC(t *testing.T) {
	zone := time.FixedZone("EDT", -4*60*60)
	late := time.Date(2026, 6, 8, 23, 30, 0, 0, zone)
	want := filepath.Join("vault", "briefs", "2026-06-09-brief.md")
	if got := Path("vault", late); got != want {
		t.Fatalf("Path=%q want %q", got, want)
	}
}

func TestPathLivesUnderVault(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "vault")
	got := Path(vault, fixedNow)
	if filepath.Dir(got) != filepath.Join(vault, "briefs") {
		t.Fatalf("path outside vault: %q", got)
	}
}

func TestWriteReturnsPathAndReplacesSameDay(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "vault")
	path, err := Write(vault, fixedNow, []byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	if path != Path(vault, fixedNow) {
		t.Fatalf("path=%q", path)
	}
	path2, err := Write(vault, fixedNow, []byte("second"))
	if err != nil {
		t.Fatal(err)
	}
	if path2 != path {
		t.Fatalf("same day paths differ: %q %q", path, path2)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Fatalf("body=%q", got)
	}
	files, err := filepath.Glob(filepath.Join(vault, "briefs", "*-brief.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files=%v", files)
	}
}

func TestWriteDistinctDays(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "vault")
	if _, err := Write(vault, fixedNow, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(vault, fixedNow.Add(24*time.Hour), []byte("two")); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(vault, "briefs", "*-brief.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files=%v", files)
	}
}

func TestWriteUsesHumanReadableMode(t *testing.T) {
	path, err := Write(t.TempDir(), fixedNow, []byte("body"))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != Mode.Perm() {
		t.Fatalf("mode=%#o want %#o", info.Mode().Perm(), Mode.Perm())
	}
}

func TestWriteSurfacesPersistenceFailure(t *testing.T) {
	parent := t.TempDir()
	vault := filepath.Join(parent, "not-a-directory")
	if err := os.WriteFile(vault, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if path, err := Write(vault, fixedNow, []byte("body")); err == nil || path != "" {
		t.Fatalf("path=%q err=%v", path, err)
	}
}
