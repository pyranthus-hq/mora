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

var updateV3 = flag.Bool("updatev3", false, "update obligations-v3 corpus goldens")

const examFixtureV3Root = "eval/obligations-v3"

func TestExamCorpusV3(t *testing.T) {
	l, err := exam.Load(filepath.Join(examFixtureV3Root, "ledger.json"))
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
	rerendered, err := exam.Render(l)
	if err != nil {
		t.Fatal(err)
	}
	assertRenderedCorpusV3Equal(t, rendered, rerendered)

	if *updateV3 {
		writeExamCorpusV3(t, rendered)
		writeExamHashesV3(t, l, rendered)
		return
	}
	if err := verifyExamHashes(examFixtureV3Root, l, rendered); err != nil {
		t.Fatal(err)
	}
	assertNoUnexpectedCorpusFilesV3(t, rendered)
	for rel, want := range rendered {
		got, err := os.ReadFile(filepath.Join(examFixtureV3Root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read checked-out corpus %s: %v", rel, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("checked-out corpus differs from render at %s", rel)
		}
	}
}

func TestExamCorpusV3RejectsTamper(t *testing.T) {
	l, err := exam.Load(filepath.Join(examFixtureV3Root, "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := exam.Render(l)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	entries := make(map[string][]byte, len(rendered)+2)
	for rel, body := range rendered {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		entries[rel] = body
	}
	for _, rel := range []string{"events.json", "ledger.json"} {
		body, err := os.ReadFile(filepath.Join(examFixtureV3Root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), body, 0o644); err != nil {
			t.Fatal(err)
		}
		entries[rel] = body
	}
	writeTestExamHashManifest(t, root, l.Version, entries)

	paths := make([]string, 0, len(rendered))
	for rel := range rendered {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	target := filepath.Join(root, filepath.FromSlash(paths[0]))
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyExamHashes(root, l, rendered); err == nil || !strings.Contains(err.Error(), "ERR_CORPUS_TAMPERED") {
		t.Fatalf("tamper verification error = %v, want ERR_CORPUS_TAMPERED", err)
	}
}

func assertRenderedCorpusV3Equal(t *testing.T, first, second map[string][]byte) {
	t.Helper()
	if len(first) != len(second) {
		t.Fatalf("deterministic re-render file count = %d, want %d", len(second), len(first))
	}
	for rel, want := range first {
		if got, ok := second[rel]; !ok || !bytes.Equal(got, want) {
			t.Fatalf("deterministic re-render differs at %s", rel)
		}
	}
}

func writeExamCorpusV3(t *testing.T, files map[string][]byte) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(examFixtureV3Root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func writeExamHashesV3(t *testing.T, l exam.Ledger, rendered map[string][]byte) {
	t.Helper()
	entries := make(map[string][]byte, len(rendered)+2)
	for rel, body := range rendered {
		entries[rel] = body
	}
	sources, err := examSourceArtifactNames(examFixtureV3Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range sources {
		body, err := os.ReadFile(filepath.Join(examFixtureV3Root, rel))
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
	if err := os.WriteFile(filepath.Join(examFixtureV3Root, "CORPUS.sha256"), []byte(out.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertNoUnexpectedCorpusFilesV3(t *testing.T, rendered map[string][]byte) {
	t.Helper()
	err := filepath.WalkDir(filepath.Join(examFixtureV3Root, "vault"), func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(examFixtureV3Root, path)
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
