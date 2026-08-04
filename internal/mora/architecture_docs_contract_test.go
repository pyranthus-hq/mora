package mora

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestArchitectureOverviewContractMatchesSource(t *testing.T) {
	root := filepath.Join("..", "..")
	overviewPath := filepath.Join(root, "docs", "architecture", "00-overview.md")
	overviewBytes, err := os.ReadFile(overviewPath)
	if err != nil {
		t.Fatal(err)
	}
	overview := string(overviewBytes)

	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	module := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(string(goMod), "\n", 2)[0], "module "))

	contractRE := regexp.MustCompile(`<!-- generated-contract: module=([^ ]+) mcp-tools=([0-9]+) connectors=([0-9]+) rrf-k=([0-9]+) segment-k=([0-9]+) -->`)
	match := contractRE.FindStringSubmatch(overview)
	if match == nil {
		t.Fatal("architecture overview is missing its generated-contract marker")
	}
	want := []string{
		module,
		strconv.Itoa(len(mcpToolRegistry)),
		strconv.Itoa(len(connectorCatalog)),
		strconv.FormatFloat(defaultFusion.k, 'f', -1, 64),
		strconv.Itoa(gmailSegmentFusionK),
	}
	for i, got := range match[1:] {
		if got != want[i] {
			t.Fatalf("architecture contract field %d = %q, want %q; update the overview with the source change", i, got, want[i])
		}
	}

	docs, err := filepath.Glob(filepath.Join(root, "docs", "architecture", "[0-9][0-9]-*.md"))
	if err != nil {
		t.Fatal(err)
	}
	indexStart := strings.Index(overview, "## Document index")
	indexEnd := strings.Index(overview, "## Glossary")
	if indexStart < 0 || indexEnd <= indexStart {
		t.Fatal("architecture overview is missing its document index boundaries")
	}
	documentIndex := overview[indexStart:indexEnd]
	for _, doc := range docs {
		name := filepath.Base(doc)
		if name == "00-overview.md" {
			continue
		}
		link := fmt.Sprintf("(./%s)", name)
		if got := strings.Count(documentIndex, link); got != 1 {
			t.Errorf("overview contains %d links to %s, want exactly 1", got, name)
		}
	}
}
