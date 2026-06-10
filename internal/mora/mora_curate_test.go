package mora

import "testing"

func TestCuratedExtIncludesDocsExcludesSource(t *testing.T) {
	for _, ok := range []string{".md", ".txt", ".json", ".yaml", ".toml", ".csv", ".markdown", ".yml", ".rst", ".text", ".MD", ".JSON"} {
		if !curatedAllowedExt(ok) {
			t.Errorf("expected %s allowed in curated mode", ok)
		}
	}
	for _, no := range []string{".ts", ".js", ".go", ".py", ".rs", ".java", ".TS", ".GO", ".jsx", ".tsx"} {
		if curatedAllowedExt(no) {
			t.Errorf("expected %s excluded in curated mode", no)
		}
	}
}

func TestCuratedMetadataFilenames(t *testing.T) {
	for _, name := range []string{"go.mod", "Makefile", "Dockerfile", "CLAUDE.md", "AGENTS.md", "README"} {
		if !curatedMetadataFile(name) {
			t.Errorf("expected metadata filename %s allowed", name)
		}
	}
	for _, name := range []string{"main.go", "README.go", "cmd/README", "dockerfile"} {
		if curatedMetadataFile(name) {
			t.Errorf("expected %s excluded from curated metadata filenames", name)
		}
	}
}
