package sharing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeCommitFixture(t *testing.T, s GenerationStore, name, file string, c Commit) {
	t.Helper()
	if err := os.MkdirAll(s.CommitsDir(name), 0700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.CommitsDir(name), file), body, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestGenerationStoreExactLayoutAndRunID(t *testing.T) {
	s := GenerationStore{DataDir: t.TempDir()}
	root := SubscriptionRoot(s.DataDir, "team")
	cases := map[string]string{s.GensDir("team"): filepath.Join(root, "gens"), s.GenDir("team", "gen-r"): filepath.Join(root, "gens", "gen-r"), s.CorpusDir("team", "gen-r"): filepath.Join(root, "gens", "gen-r", "corpus"), s.IndexPath("team", "gen-r"): filepath.Join(root, "gens", "gen-r", "index.db"), s.CommitsDir("team"): filepath.Join(root, "commits"), s.CommitPath("team", 42): filepath.Join(root, "commits", "0000000042"), s.AttemptPath("team"): filepath.Join(root, "attempt.json"), s.ImportLockPath("team"): filepath.Join(root, "import.lock"), s.MigratedLatchPath("team"): filepath.Join(root, "migrated"), s.FetchDir("team", "r"): filepath.Join(root, "fetch-r")}
	for got, want := range cases {
		if got != want {
			t.Fatalf("path=%q want=%q", got, want)
		}
	}
	if RunID("gen-run-1") != "run-1" || RunID("legacy") != "legacy" {
		t.Fatal("run id")
	}
}
func TestGenerationStoreResolveHighestAndFailClosed(t *testing.T) {
	s := GenerationStore{DataDir: t.TempDir()}
	if got, ok, err := s.Resolve("none"); err != nil || ok || got.Seq != 0 {
		t.Fatalf("%+v %v %v", got, ok, err)
	}
	if err := os.MkdirAll(s.CommitsDir("empty"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.CommitsDir("empty"), "debris"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.Resolve("empty"); err != nil || ok || got.Seq != 0 {
		t.Fatalf("%+v %v %v", got, ok, err)
	}
	writeCommitFixture(t, s, "team", "0000000002", Commit{Seq: 2, Gen: "gen-two"})
	writeCommitFixture(t, s, "team", "0000000010", Commit{Seq: 10, Gen: "gen-ten"})
	writeCommitFixture(t, s, "team", "0000000009", Commit{Seq: 9, Gen: "gen-nine"})
	if err := os.Mkdir(filepath.Join(s.CommitsDir("team"), "0000000099"), 0700); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Resolve("team")
	if err != nil || !ok || got.Seq != 10 || got.Gen != "gen-ten" {
		t.Fatalf("%+v %v %v", got, ok, err)
	}
	if err := os.WriteFile(s.CommitPath("team", 11), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Resolve("team"); err == nil || !strings.Contains(err.Error(), `share "team": commit record 0000000011 is corrupt`) {
		t.Fatal(err)
	}
}
func TestGenerationStoreReadAllSkipsDebrisAndCorruptClaims(t *testing.T) {
	s := GenerationStore{DataDir: t.TempDir()}
	if got, err := s.ReadAll("none"); err != nil || got != nil {
		t.Fatalf("%+v %v", got, err)
	}
	writeCommitFixture(t, s, "team", "0000000002", Commit{Seq: 2})
	writeCommitFixture(t, s, "team", "0000000001", Commit{Seq: 1})
	if err := os.WriteFile(filepath.Join(s.CommitsDir("team"), "0000000003"), []byte("claim"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.CommitsDir("team"), "debris"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(s.CommitsDir("team"), "0000000004"), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadAll("team")
	if err != nil || !reflect.DeepEqual(got, []Commit{{Seq: 1}, {Seq: 2}}) {
		t.Fatalf("%+v %v", got, err)
	}
}
func TestGenerationStoreInjectedReadErrors(t *testing.T) {
	boom := errors.New("boom")
	s := GenerationStore{DataDir: t.TempDir(), ReadDir: func(string) ([]os.DirEntry, error) { return nil, boom }}
	if _, _, err := s.Resolve("x"); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, err := s.ReadAll("x"); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	base := GenerationStore{DataDir: t.TempDir()}
	writeCommitFixture(t, base, "x", "0000000001", Commit{Seq: 1})
	base.ReadFile = func(string) ([]byte, error) { return nil, boom }
	if _, _, err := base.Resolve("x"); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, err := base.ReadAll("x"); !errors.Is(err, boom) {
		t.Fatal(err)
	}
}
func TestGenerationDigestsAreExactAndDeterministic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "b.md"), []byte("B"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("A"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "ignored.md"), 0700); err != nil {
		t.Fatal(err)
	}
	a := sha256.Sum256([]byte("A"))
	b := sha256.Sum256([]byte("B"))
	rows := hex.EncodeToString(a[:]) + "  a.md\n" + hex.EncodeToString(b[:]) + "  b.md"
	want := sha256.Sum256([]byte(rows))
	got, err := CorpusDigest(dir)
	if err != nil || got != hex.EncodeToString(want[:]) {
		t.Fatalf("digest=%s err=%v", got, err)
	}
	file := filepath.Join(dir, "ignored.txt")
	sum := sha256.Sum256([]byte("x"))
	if got, err := FileDigest(file); err != nil || got != hex.EncodeToString(sum[:]) {
		t.Fatalf("digest=%s err=%v", got, err)
	}
	if _, err := CorpusDigest(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing corpus accepted")
	}
	if _, err := FileDigest(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
}
