package mora

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/mora/exam"
)

var update = flag.Bool("update", false, "update exam corpus goldens")

const examFixtureRoot = "eval/obligations-v1"

type examEventFixture struct {
	AsOf    string `json:"as_of"`
	EventID string `json:"event_id"`
}

func loadExamLedger(t *testing.T) exam.Ledger {
	t.Helper()
	l, err := exam.Load(filepath.Join(examFixtureRoot, "ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func loadExamEvent(t *testing.T) (examEventFixture, time.Time) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(examFixtureRoot, "events.json"))
	if err != nil {
		t.Fatal(err)
	}
	var event examEventFixture
	if err := json.Unmarshal(b, &event); err != nil {
		t.Fatal(err)
	}
	at, err := time.Parse(time.RFC3339, event.AsOf)
	if err != nil {
		t.Fatal(err)
	}
	return event, at
}

func TestExamCorpusMatchesLedger(t *testing.T) {
	l := loadExamLedger(t)
	got, err := exam.Render(l)
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		writeExamCorpus(t, got)
		writeExamHashes(t, l, got)
		return
	}
	for rel, want := range got {
		path := filepath.Join(examFixtureRoot, filepath.FromSlash(rel))
		committed, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read committed corpus %s: %v", rel, err)
		}
		if !bytes.Equal(committed, want) {
			t.Errorf("corpus drift at %s; run go test ./internal/mora -run TestExamCorpusMatchesLedger -update", rel)
		}
	}
	assertNoUnexpectedCorpusFiles(t, got)
}

func writeExamCorpus(t *testing.T, files map[string][]byte) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(examFixtureRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func assertNoUnexpectedCorpusFiles(t *testing.T, rendered map[string][]byte) {
	t.Helper()
	root := filepath.Join(examFixtureRoot, "vault")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		rel, err := filepath.Rel(examFixtureRoot, path)
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

func TestExamCorpusRoundTripsThroughProduction(t *testing.T) {
	root := filepath.Join(examFixtureRoot, "vault")
	var count int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		count++
		before, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		m, err := parseMemory(path)
		if err != nil {
			return err
		}
		after, err := renderMemory(m)
		if err != nil {
			return err
		}
		if !bytes.Equal(before, after) {
			return fmt.Errorf("production round-trip changed %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("exam corpus has no memories")
	}
}

func TestExamCorpusMatchesConnectorShape(t *testing.T) {
	l := loadExamLedger(t)
	for _, a := range l.Artifacts {
		var path string
		switch a.Channel {
		case "calendar":
			path = filepath.Join(examFixtureRoot, "vault", "sources", "calendar", strings.ReplaceAll(a.MemoryID, "/", "_")+".md")
		case "notes":
			path = filepath.Join(examFixtureRoot, "vault", "memories", "exam", strings.ReplaceAll(a.MemoryID, "/", "_")+".md")
		default:
			continue
		}
		m, err := parseMemory(path)
		if err != nil {
			t.Fatal(err)
		}
		if a.Channel == "calendar" && (m.Type != "event" || m.Provider != "calendar") {
			t.Errorf("calendar %s shape = type %q provider %q", a.ID, m.Type, m.Provider)
		}
		if a.Channel == "notes" && (m.Provider != "" || !strings.Contains(filepath.ToSlash(path), "/memories/")) {
			t.Errorf("notes %s was not rendered as a user memory", a.ID)
		}
	}
}

func TestLedgerQuotesAreRenderable(t *testing.T) {
	l := loadExamLedger(t)
	check := func(id string, span exam.Span) {
		t.Helper()
		if span.MessageID == "" {
			clean := stripNoiseTokens(oneLine(span.Quote))
			if clean != span.Quote {
				t.Errorf("%s subject quote is not renderable: got %q want %q", id, clean, span.Quote)
			}
			return
		}
		cleaned := senderAuthoredBody(span.Quote)
		segments := meetingBriefEvidenceSegments(cleaned)
		if len(segments) != 1 {
			t.Errorf("%s quote split into %d segments: %q", id, len(segments), segments)
			return
		}
		got := stripNoiseTokens(stripSpeakerPrefix(segments[0]))
		if got != span.Quote {
			t.Errorf("%s quote is not renderable: got %q want %q", id, got, span.Quote)
		}
	}
	for _, c := range l.Commitments {
		check(c.ID, c.OpenedBy)
		for _, tr := range c.Transitions {
			check(c.ID+" transition", tr.Evidence)
		}
	}
}

func TestExamCorpusNoRealIdentities(t *testing.T) {
	files := map[string][]byte{}
	err := filepath.WalkDir(filepath.Join(examFixtureRoot, "vault"), func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[path] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := exam.LintCorpus(files); err != nil {
		t.Fatal(err)
	}
}

func TestExamFlywheelArtifactsShareNoIdentityBytes(t *testing.T) {
	l := loadExamLedger(t)
	rendered, err := exam.Render(l)
	if err != nil {
		t.Fatal(err)
	}
	const handle = "+15550100137"
	const email = "dana@example.net"
	var flywheel []byte
	var danaGmail [][]byte
	for _, a := range l.Artifacts {
		if a.ID == "a/imessage-flywheel" {
			flywheel = rendered["vault/sources/imessage/"+strings.ReplaceAll(a.MemoryID, "/", "_")+".md"]
		}
		if a.Channel != "gmail" {
			continue
		}
		fromDana := false
		for _, m := range a.Messages {
			fromDana = fromDana || m.From == "p/dana"
		}
		if fromDana {
			danaGmail = append(danaGmail, rendered["vault/sources/gmail/"+strings.ReplaceAll(a.MemoryID, "/", "_")+".md"])
		}
	}
	if len(flywheel) == 0 || !bytes.Contains(flywheel, []byte(handle)) || bytes.Contains(flywheel, []byte(email)) {
		t.Fatalf("flywheel iMessage must expose only the bare handle")
	}
	if len(danaGmail) == 0 {
		t.Fatal("flywheel ledger has no Gmail arm")
	}
	for _, body := range danaGmail {
		if !bytes.Contains(body, []byte(email)) || bytes.Contains(body, []byte(handle)) {
			t.Fatal("flywheel Gmail must expose only the email identity")
		}
	}
}

func seedExamHome(t *testing.T) (Config, examEventFixture, time.Time) {
	t.Helper()
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	event, at := loadExamEvent(t)
	if err := saveSources(cfg, []Source{{Name: "gmail", Type: "gmail", Email: "alex@example.com", Enabled: ptr(true), CreatedAt: "2026-07-01T00:00:00Z"}}); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(examFixtureRoot, "vault")
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(cfg.VaultDir, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuild exam index: %v", err)
	}
	return cfg, event, at
}

func TestExamCorpusProducesANonEmptyBrief(t *testing.T) {
	cfg, event, at := seedExamHome(t)
	brief, err := buildEventMeetingBrief(context.Background(), cfg, event.EventID, at, 0, 25)
	if err != nil {
		t.Fatal(err)
	}
	if brief.SelfUnresolved || len(brief.Gaps) != 0 {
		t.Fatalf("exam brief gapped: self_unresolved=%v gaps=%v", brief.SelfUnresolved, brief.Gaps)
	}
	lines := 0
	attendees := map[string]bool{}
	for _, section := range brief.Sections {
		for _, line := range section.Lines {
			lines++
			attendees[strings.ToLower(line.Attendee)] = true
		}
	}
	if lines == 0 {
		t.Fatal("exam corpus produced an empty brief")
	}
	for _, expected := range []string{"sam rivera", "dana@example.net"} {
		if !attendees[expected] {
			t.Errorf("exam brief has no line for expected attendee %q; got %v", expected, attendees)
		}
	}
}

func TestExamCorpusProducesANonEmptyDigest(t *testing.T) {
	cfg, _, at := seedExamHome(t)
	digest, err := buildDigest(cfg, at, briefOpts{sinceHours: 24 * 30, perSourceCap: 50})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, item := range digest.Urgent {
		ids[item.ID] = true
	}
	for _, section := range digest.Sections {
		for _, item := range section.Items {
			ids[item.ID] = true
		}
	}
	if len(ids) == 0 {
		t.Fatal("exam corpus produced an empty window digest")
	}
	l := loadExamLedger(t)
	for _, c := range l.Commitments {
		if !containsString(c.ExpectedIn, "daily") {
			continue
		}
		for _, a := range l.Artifacts {
			if a.ID == c.OpenedBy.ArtifactID && !ids[a.MemoryID] {
				t.Errorf("daily corpus did not surface expected artifact %s", a.MemoryID)
			}
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestExamCorpusHashesMatch(t *testing.T) {
	l := loadExamLedger(t)
	rendered, err := exam.Render(l)
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		writeExamCorpus(t, rendered)
		writeExamHashes(t, l, rendered)
		return
	}
	manifestPath := filepath.Join(examFixtureRoot, "CORPUS.sha256")
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("ERR_CORPUS_HASH_MISSING: %v", err)
	}
	entries, err := parseExamHashes(b, l.Version)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(entries))
	for rel := range entries {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	for _, rel := range paths {
		wantHash := entries[rel]
		if strings.HasPrefix(rel, "vault/") {
			generated, ok := rendered[rel]
			if !ok {
				t.Fatalf("ERR_LEDGER_DRIFT: manifest names corpus file not rendered by ledger: %s", rel)
			}
			if hashBytes(generated) != wantHash {
				t.Fatalf("ERR_LEDGER_DRIFT: rendered %s hash differs from manifest", rel)
			}
			committed, err := os.ReadFile(filepath.Join(examFixtureRoot, filepath.FromSlash(rel)))
			if err != nil || hashBytes(committed) != wantHash {
				t.Fatalf("ERR_CORPUS_TAMPERED: checked-out %s differs from manifest", rel)
			}
			continue
		}
		source, err := os.ReadFile(filepath.Join(examFixtureRoot, filepath.FromSlash(rel)))
		if err != nil || hashBytes(source) != wantHash {
			t.Fatalf("ERR_LEDGER_DRIFT: source artifact %s differs from manifest", rel)
		}
	}
	for rel := range rendered {
		if _, ok := entries[rel]; !ok {
			t.Fatalf("ERR_LEDGER_DRIFT: rendered file %s is missing from manifest", rel)
		}
	}
}

func writeExamHashes(t *testing.T, l exam.Ledger, rendered map[string][]byte) {
	t.Helper()
	entries := map[string][]byte{}
	for rel, body := range rendered {
		entries[rel] = body
	}
	for _, name := range []string{"ledger.json", "events.json"} {
		b, err := os.ReadFile(filepath.Join(examFixtureRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		entries[name] = b
	}
	extra, err := filepath.Glob(filepath.Join(examFixtureRoot, "*-ledger.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range extra {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		entries[filepath.Base(path)] = b
	}
	paths := make([]string, 0, len(entries))
	for path := range entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var out strings.Builder
	fmt.Fprintf(&out, "# renderer_version=%s ledger_schema=%d\n", exam.RendererVersion, l.Version)
	for _, path := range paths {
		fmt.Fprintf(&out, "%s  %s\n", hashBytes(entries[path]), path)
	}
	if err := os.WriteFile(filepath.Join(examFixtureRoot, "CORPUS.sha256"), []byte(out.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func parseExamHashes(b []byte, schema int) (map[string]string, error) {
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	wantHeader := fmt.Sprintf("# renderer_version=%s ledger_schema=%d", exam.RendererVersion, schema)
	if len(lines) < 2 || lines[0] != wantHeader {
		return nil, fmt.Errorf("ERR_CORPUS_HASH_VERSION: header = %q, want %q", lines[0], wantHeader)
	}
	entries := map[string]string{}
	prior := ""
	for _, line := range lines[1:] {
		parts := strings.SplitN(line, "  ", 2)
		if len(parts) != 2 || len(parts[0]) != sha256.Size*2 || parts[1] <= prior {
			return nil, fmt.Errorf("ERR_CORPUS_HASH_FORMAT: malformed or unsorted line %q", line)
		}
		if _, err := hex.DecodeString(parts[0]); err != nil {
			return nil, fmt.Errorf("ERR_CORPUS_HASH_FORMAT: %w", err)
		}
		entries[parts[1]] = parts[0]
		prior = parts[1]
	}
	return entries, nil
}

func hashBytes(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
