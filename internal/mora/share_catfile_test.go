package mora

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// catFileRepo commits files (name → bytes) into a fresh repo and returns it.
func catFileRepo(t *testing.T, files map[string][]byte) string {
	t.Helper()
	repo := t.TempDir()
	mustGit(t, repo, "init", "-q")
	mustGit(t, repo, "config", "user.email", "t@t")
	mustGit(t, repo, "config", "user.name", "t")
	for name, body := range files {
		p := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustGit(t, repo, "add", "-A")
	mustGit(t, repo, "commit", "-q", "-m", "fixture")
	return repo
}

func TestGitCatFileBatchReadsBlobsInOrder(t *testing.T) {
	files := map[string][]byte{
		"a":         []byte("alpha\n"),
		"empty":     {},
		"dir/b c":   []byte("line one\nline two\n\nno trailing newline"),
		"binary":    {0, '\n', 0xff, '\n', '\n'},
		"untouched": []byte("never requested"),
	}
	repo := catFileRepo(t, files)
	names := []string{"binary", "a", "empty", "dir/b c"}
	var objects []string
	for _, n := range names {
		objects = append(objects, "HEAD:"+n)
	}
	var got []string
	err := realGitCatFileBatch(context.Background(), repo, objects, 1<<20, func(i int, body []byte) error {
		if !bytes.Equal(body, files[names[i]]) {
			t.Errorf("object %d (%s) = %q; want %q", i, names[i], body, files[names[i]])
		}
		got = append(got, names[i])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != strings.Join(names, ",") {
		t.Fatalf("answers came back as %v; want %v", got, names)
	}
	if err := realGitCatFileBatch(context.Background(), repo, nil, 1, func(int, []byte) error {
		t.Fatal("callback ran for an empty request")
		return nil
	}); err != nil {
		t.Fatalf("empty request = %v", err)
	}
}

func TestGitCatFileBatchRefusesWhatCatFileBlobRefused(t *testing.T) {
	repo := catFileRepo(t, map[string][]byte{"a": []byte("alpha"), "dir/b": []byte("bravo"), "big": bytes.Repeat([]byte{'x'}, 100)})
	never := func(int, []byte) error { return nil }
	for _, tc := range []struct {
		name    string
		objects []string
		want    string
	}{
		{"missing path", []string{"HEAD:a", "HEAD:nope"}, "not readable"},
		{"missing ref", []string{"refs/mora/import/gone:a"}, "not readable"},
		{"tree", []string{"HEAD:dir"}, "not a blob"},
		{"commit", []string{"HEAD"}, "not a blob"},
		{"newline in name", []string{"HEAD:a\nHEAD:dir/b"}, "one line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := realGitCatFileBatch(context.Background(), repo, tc.objects, 1<<20, never)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v; want %q", err, tc.want)
			}
		})
	}

	t.Run("oversize stops before the body", func(t *testing.T) {
		calls := 0
		err := realGitCatFileBatch(context.Background(), repo, []string{"HEAD:a", "HEAD:big", "HEAD:dir/b"}, 99, func(int, []byte) error {
			calls++
			return nil
		})
		o, ok := asGitBatchOversize(err)
		if !ok || o.index != 1 || o.size != 100 {
			t.Fatalf("err = %v; want oversize report for object 1 (100 bytes)", err)
		}
		if calls != 1 {
			t.Fatalf("callback ran %d times; want 1 (reading stops at the oversize blob)", calls)
		}
	})

	t.Run("caller error is returned unchanged and stops the read", func(t *testing.T) {
		stop := errors.New("refusing entry")
		calls := 0
		err := realGitCatFileBatch(context.Background(), repo, []string{"HEAD:a", "HEAD:dir/b", "HEAD:big"}, 1<<20, func(int, []byte) error {
			calls++
			return stop
		})
		if err != stop {
			t.Fatalf("err = %v; want the caller's own error, unwrapped", err)
		}
		if calls != 1 {
			t.Fatalf("callback ran %d times after it refused", calls)
		}
	})

	t.Run("not a repository", func(t *testing.T) {
		err := realGitCatFileBatch(context.Background(), t.TempDir(), []string{"HEAD:a"}, 1<<20, never)
		if err == nil {
			t.Fatal("cat-file outside a repository succeeded")
		}
	})
}

// TestGitPullReadsEveryBlobThroughOnePinnedBatch pins the subprocess count: a
// real-change pull reads its N memories through ONE cat-file --batch process,
// and every object it names goes through the run-private pin ref (H1).
func TestGitPullReadsEveryBlobThroughOnePinnedBatch(t *testing.T) {
	var mems []Memory
	for i := 0; i < 3; i++ {
		mems = append(mems, fixtureMemory(fmt.Sprintf("mem_20260601_00000%d_aaaaaaaa", i), fmt.Sprintf("M%d", i), "first"))
	}
	cfg, id, remote := subscribeGitShare(t, mems)
	for i := 3; i < 7; i++ {
		mems = append(mems, fixtureMemory(fmt.Sprintf("mem_20260601_00000%d_aaaaaaaa", i), fmt.Sprintf("M%d", i), "second"))
	}
	commitShareRemote(t, remote, id.Recipient(), mems)

	var batches [][]string
	orig := gitCatFileBatchFn
	gitCatFileBatchFn = func(ctx context.Context, dir string, objects []string, maxSize int64, each func(int, []byte) error) error {
		batches = append(batches, append([]string(nil), objects...))
		return orig(ctx, dir, objects, maxSize, each)
	}
	t.Cleanup(func() { gitCatFileBatchFn = orig })
	catFileRuns := 0
	counting := func(ctx context.Context, dir, name string, args ...string) (string, error) {
		if name == "git" && len(args) > 0 && args[0] == "cat-file" {
			catFileRuns++
		}
		return realExec(ctx, dir, name, args...)
	}
	var out bytes.Buffer
	if err := sharePull(context.Background(), cfg, []string{"neil"}, &out, counting); err != nil {
		t.Fatalf("pull: %v\n%s", err, out.String())
	}
	if len(batches) != 1 || len(batches[0]) != len(mems) {
		t.Fatalf("batches = %d (sizes %v); want one batch of %d", len(batches), batches, len(mems))
	}
	for _, o := range batches[0] {
		if !strings.HasPrefix(o, "refs/mora/import/run_") || !strings.Contains(o, ":memories/") {
			t.Fatalf("batch named %q; every object must come from the run-private pin ref", o)
		}
	}
	if catFileRuns != 1 {
		t.Fatalf("single-object cat-file ran %d times; want 1 (share.json only)", catFileRuns)
	}
	if head := mustHead(t, cfg, "neil"); head.Count != len(mems) {
		t.Fatalf("head count = %d; want %d", head.Count, len(mems))
	}
}

func TestGitPullRefusesOversizedBlobFromGitSizeHeader(t *testing.T) {
	m := fixtureMemory("mem_20260601_000000_aaaaaaaa", "A", "first")
	cfg, _, remote := subscribeGitShare(t, []Memory{m})
	big := filepath.Join(remote, "memories", "mem_20260601_000001_bbbbbbbb.md.age")
	if err := os.WriteFile(big, bytes.Repeat([]byte{'x'}, shareMaxMemoryBytes+(1<<20)+1), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, remote, "add", "-A")
	mustGit(t, remote, "commit", "-q", "-m", "oversized")
	before := mustHead(t, cfg, "neil")
	var out bytes.Buffer
	err := sharePull(context.Background(), cfg, []string{"neil"}, &out, realExec)
	want := fmt.Sprintf("entry mem_20260601_000001_bbbbbbbb exceeds the %d-byte per-memory cap", shareMaxMemoryBytes)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("pull = %v; want %q", err, want)
	}
	if after := mustHead(t, cfg, "neil"); after.Seq != before.Seq {
		t.Fatalf("a refused pull published seq %d over %d", after.Seq, before.Seq)
	}
}

// TestShareBuildRefusesGrowthTheLedgerCannotSee: the build counts its own
// writes instead of re-walking per file, so bytes another writer adds mid-build
// are invisible to the per-write checks. The closing whole-product walk must
// still refuse the generation before any commit can publish it.
func TestShareBuildRefusesGrowthTheLedgerCannotSee(t *testing.T) {
	cfg := packetHConfig(t)
	packetHSetLimitHeadroom(t, cfg, 256<<10)
	var entries []shareBlobEntry
	for i := 0; i < 3; i++ {
		m := fixtureMemory(fmt.Sprintf("mem_20260716_00000%d_aaaaaaaa", i), "Bounded", "body")
		entries = append(entries, shareBlobEntry{mem: m, body: bytes.Repeat([]byte{'m'}, 1024)})
	}
	orig := testHookPostFirstGenCorpusWrite
	testHookPostFirstGenCorpusWrite = func() {
		late := filepath.Join(cfg.VaultDir, "late-connector-write")
		if err := os.WriteFile(late, bytes.Repeat([]byte{'x'}, 300<<10), 0o600); err != nil {
			t.Errorf("late write: %v", err)
		}
	}
	t.Cleanup(func() { testHookPostFirstGenCorpusWrite = orig })
	_, _, err := buildShareGenerationFromEntries(context.Background(), cfg, "acme", "gen-late", entries)
	if err == nil || !strings.Contains(err.Error(), "storage-limit") {
		t.Fatalf("build after a late 300 KiB write = %v; want the closing walk to refuse it", err)
	}
	if _, ok, _ := resolvePublishedCommit(cfg, "acme"); ok {
		t.Fatal("a refused build left a commit")
	}
}
