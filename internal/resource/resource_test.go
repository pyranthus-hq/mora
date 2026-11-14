package resource

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func skill(id, source, text string) Candidate {
	return Candidate{StableID: id, Kind: KindSkill, CanonicalSource: source, Purpose: "safe deployment helper", Applicability: []string{"deployment"}, EffectiveScope: "project:alpha", Source: SourcePasted, Pasted: []byte(text)}
}

func TestResourceStoreIdenticalImportReusesRecord(t *testing.T) {
	root := t.TempDir()
	r1, v1, first, err := Import(root, skill("demo-skill", "test:demo", "# Demo\n"))
	if err != nil || !first.CreatedRecord || !first.CreatedRevision {
		t.Fatalf("first import: %#v, %v", first, err)
	}
	r2, v2, second, err := Import(root, skill("demo-skill", "test:demo", "# Demo\n"))
	if err != nil || !second.Duplicate || second.CreatedRecord || second.CreatedRevision {
		t.Fatalf("duplicate import: %#v, %v", second, err)
	}
	if r1.StableID != r2.StableID || v1.RevisionDigest != v2.RevisionDigest {
		t.Fatal("identical import changed immutable identity")
	}
	if _, err := os.Stat(filepath.Join(root, "resources", r1.StableID, "revisions", v1.RevisionDigest, "tree", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}

func TestResourceStoreChangedExplicitIDCreatesImmutableRevision(t *testing.T) {
	root := t.TempDir()
	_, first, _, err := Import(root, skill("demo-skill", "test:demo", "# One\n"))
	if err != nil {
		t.Fatal(err)
	}
	changed := skill("", "test:demo", "# Two\n")
	if _, _, _, err := Import(root, changed); !errors.Is(err, ErrPriorStableIDRequired) {
		t.Fatalf("changed import without ID = %v", err)
	}
	_, second, outcome, err := Import(root, skill("demo-skill", "test:demo", "# Two\n"))
	if err != nil || !outcome.CreatedRevision {
		t.Fatalf("changed import: %#v, %v", outcome, err)
	}
	if first.RevisionDigest == second.RevisionDigest {
		t.Fatal("changed content reused revision")
	}
	_, old, err := ReadRevision(root, "demo-skill", first.RevisionDigest)
	if err != nil || old.TreeDigest == second.TreeDigest {
		t.Fatalf("old revision was not retained: %v", err)
	}
}

func TestResourceStoreRepoIdentityPinsCommitAndTree(t *testing.T) {
	root, source := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "guide.md"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Candidate{Kind: KindRepository, Purpose: "runbook", EffectiveScope: "project:alpha", Source: SourceFolder, Folder: source, Reference: Reference{URL: "https://example.test/repo", Version: "abc123"}}
	record, first, _, err := Import(root, c)
	if err != nil {
		t.Fatal(err)
	}
	if first.Reference.Version != "abc123" || first.TreeDigest == "" {
		t.Fatalf("repo revision not pinned: %#v", first)
	}
	if err := os.WriteFile(filepath.Join(source, "guide.md"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Import(root, c); !errors.Is(err, ErrPriorStableIDRequired) {
		t.Fatalf("changed repo without ID = %v", err)
	}
	c.StableID = record.StableID
	_, second, _, err := Import(root, c)
	if err != nil || second.TreeDigest == first.TreeDigest {
		t.Fatalf("changed repo: %v", err)
	}
}

func TestResourceIntakeRefusesUnsafeTreesWithoutOutput(t *testing.T) {
	root, source := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(source, ".env"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Candidate{StableID: "unsafe-skill", Kind: KindSkill, CanonicalSource: "test:unsafe", Purpose: "unsafe", EffectiveScope: "project:alpha", Source: SourceFolder, Folder: source}
	if _, _, _, err := Import(root, c); !errors.Is(err, ErrUnsafeSource) {
		t.Fatalf("secret-like file = %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unsafe intake wrote output: %v", entries)
	}
	bad := skill("../escape", "test:bad", "# x\n")
	if _, _, _, err := Import(root, bad); !errors.Is(err, ErrInvalidCandidate) {
		t.Fatalf("caller ID = %v", err)
	}
}

func TestResourceIntakeCapsAndIncompleteFolderWithoutOutput(t *testing.T) {
	root, source := t.TempDir(), t.TempDir()
	oversized := filepath.Join(source, "large.md")
	f, err := os.OpenFile(oversized, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	c := Candidate{StableID: "large-tool", Kind: KindTool, CanonicalSource: "test:large", Purpose: "large", EffectiveScope: "project:alpha", Source: SourceFolder, Folder: source}
	if _, _, _, err := Import(root, c); !errors.Is(err, ErrUnsafeSource) {
		t.Fatalf("oversized tree = %v", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("oversized intake wrote output: %v, %v", entries, err)
	}

	incompleteRoot := t.TempDir()
	incomplete := Candidate{StableID: "incomplete-skill", Kind: KindSkill, CanonicalSource: "test:incomplete", Purpose: "incomplete", EffectiveScope: "project:alpha", Source: SourceFolder, Folder: incompleteRoot}
	if _, _, _, err := Import(root, incomplete); !errors.Is(err, ErrInvalidCandidate) {
		t.Fatalf("incomplete skill folder = %v", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("incomplete intake wrote output: %v, %v", entries, err)
	}
}

func TestResourceStoreConcurrentPublication(t *testing.T) {
	root := t.TempDir()
	c := skill("concurrent-skill", "test:concurrent", "# Concurrent\n")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); _, _, _, err := Import(root, c); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestResourceReferenceIsVisibleButIncomplete(t *testing.T) {
	root := t.TempDir()
	c := Candidate{StableID: "repo-lead", Kind: KindRepository, CanonicalSource: "https://example.test/repo", Purpose: "research lead", EffectiveScope: "project:alpha", Source: SourceReference, Reference: Reference{URL: "https://example.test/repo", Version: "deadbeef"}}
	_, revision, _, err := Import(root, c)
	if err != nil {
		t.Fatal(err)
	}
	if revision.Complete || revision.Availability != AvailabilityUnfetched || revision.TreeDigest != "" {
		t.Fatalf("reference became content: %#v", revision)
	}
}

func TestDiscoverResourcesProjectOverrideBeforeRank(t *testing.T) {
	root := t.TempDir()
	_, global, _, err := Import(root, Candidate{StableID: "shared", Kind: KindBestPractice, CanonicalSource: "test:shared", Purpose: "database deployment", EffectiveScope: "global", Source: SourceReference, Reference: Reference{URL: "https://example.test/global"}})
	if err != nil {
		t.Fatal(err)
	}
	_, project, _, err := Import(root, Candidate{StableID: "shared", Kind: KindBestPractice, CanonicalSource: "test:shared", Purpose: "alpha deployment", EffectiveScope: "project:alpha", Source: SourceReference, Reference: Reference{URL: "https://example.test/project"}})
	if err != nil {
		t.Fatal(err)
	}
	results, err := Discover(root, Query{ProjectScope: "project:alpha", Task: "database deployment", Bindings: []Binding{{ResourceID: "shared", RevisionDigest: global.RevisionDigest, Scope: "global", Eligible: true}, {ResourceID: "shared", RevisionDigest: project.RevisionDigest, Scope: "project:alpha", Eligible: true}}})
	if err != nil || len(results) != 1 || results[0].RevisionDigest != project.RevisionDigest || results[0].Scope != "project:alpha" {
		t.Fatalf("override: %#v, %v", results, err)
	}
}

func TestDiscoverResourcesMaxFiveAndNoSideEffects(t *testing.T) {
	root := t.TempDir()
	bindings := make([]Binding, 0, 6)
	for i := 0; i < 6; i++ {
		id := "lead-" + string(rune('a'+i))
		_, revision, _, err := Import(root, Candidate{StableID: id, Kind: KindTool, CanonicalSource: "test:" + id, Purpose: "deployment tool", EffectiveScope: "project:alpha", Source: SourceReference, Reference: Reference{URL: "https://example.test/" + id}})
		if err != nil {
			t.Fatal(err)
		}
		bindings = append(bindings, Binding{ResourceID: id, RevisionDigest: revision.RevisionDigest, Scope: "project:alpha", Eligible: true})
	}
	results, err := Discover(root, Query{ProjectScope: "project:alpha", Task: "deployment", Bindings: bindings})
	if err != nil || len(results) != 5 {
		t.Fatalf("max five: %d, %v", len(results), err)
	}
	if _, err := os.Stat(filepath.Join(root, "resources", "events")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discover wrote event state: %v", err)
	}
}

func TestResourceEventsSeparateRecommendationLoadOutcome(t *testing.T) {
	state := t.TempDir()
	for _, kind := range []EventType{EventRecommended, EventLoaded, EventOutcome} {
		if err := AppendEvent(state, Event{Type: kind, ResourceID: "event-skill", RevisionDigest: strings.Repeat("a", 64), ProjectScope: "project:alpha", Evidence: "receipt"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := AppendEvent(state, Event{Type: EventLoaded, ResourceID: "event-skill", RevisionDigest: strings.Repeat("a", 64), TrackingDisabled: true}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(state, "resources/events/events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(body), "\n"); got != 3 {
		t.Fatalf("event records = %d", got)
	}
}
