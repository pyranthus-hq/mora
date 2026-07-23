package mora

import (
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/pyranthus-hq/mora/internal/mora/exam"
)

var updateV2 = flag.Bool("updatev2", false, "update obligations-v2 corpus goldens")

const examFixtureV2Root = "eval/obligations-v2"

func TestExamCorpusV2(t *testing.T) {
	l, err := exam.Load(filepath.Join(examFixtureV2Root, "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, check := range map[string]func(exam.Ledger) error{
		"validate":          exam.Validate,
		"identity lint":     exam.Lint,
		"leakage lint":      exam.LintLeakage,
		"date fingerprint":  exam.LintDateFingerprint,
		"title fingerprint": exam.LintTitleFingerprint,
	} {
		if err := check(l); err != nil {
			t.Fatalf("%s failed: %v", name, err)
		}
	}

	rendered, err := exam.Render(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := exam.LintCorpus(rendered); err != nil {
		t.Fatal(err)
	}
	if *updateV2 {
		writeExamCorpusV2(t, rendered)
		writeExamHashesV2(t, l, rendered)
		return
	}
	if err := verifyExamHashes(examFixtureV2Root, l, rendered); err != nil {
		t.Fatal(err)
	}
	assertNoUnexpectedCorpusFilesV2(t, rendered)
	for rel, want := range rendered {
		got, err := os.ReadFile(filepath.Join(examFixtureV2Root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read checked-out corpus %s: %v", rel, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("checked-out corpus differs from render at %s", rel)
		}
	}
}

func writeExamCorpusV2(t *testing.T, files map[string][]byte) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(examFixtureV2Root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func writeExamHashesV2(t *testing.T, l exam.Ledger, rendered map[string][]byte) {
	t.Helper()
	entries := make(map[string][]byte, len(rendered)+2)
	for rel, body := range rendered {
		entries[rel] = body
	}
	sources, err := examSourceArtifactNames(examFixtureV2Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range sources {
		body, err := os.ReadFile(filepath.Join(examFixtureV2Root, rel))
		if err != nil {
			t.Fatal(err)
		}
		entries[rel] = body
	}
	paths := make([]string, 0, len(entries))
	for rel := range entries {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	var out strings.Builder
	fmt.Fprintf(&out, "# renderer_version=%s ledger_schema=%d\n", exam.RendererVersionFor(l.Version), l.Version)
	for _, rel := range paths {
		fmt.Fprintf(&out, "%s  %s\n", hashBytes(entries[rel]), rel)
	}
	if err := os.WriteFile(filepath.Join(examFixtureV2Root, "CORPUS.sha256"), []byte(out.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertNoUnexpectedCorpusFilesV2(t *testing.T, rendered map[string][]byte) {
	t.Helper()
	err := filepath.WalkDir(filepath.Join(examFixtureV2Root, "vault"), func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(examFixtureV2Root, path)
		if err != nil {
			return err
		}
		if _, ok := rendered[filepath.ToSlash(rel)]; !ok {
			return fmt.Errorf("unexpected hand-authored corpus file %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
