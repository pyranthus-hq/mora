package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

const sabotageGenuineObligation = "Can you send the signed pilot agreement before Friday?"

type sabotageEventFixture struct {
	AsOf    string `json:"as_of"`
	EventID string `json:"event_id"`
}

type sabotageMatch struct {
	DefectClass string
	Fixture     string
	Line        string
}

func sabotageFixtureDir(parts ...string) string {
	all := append([]string{"eval", "sabotage", "gibberish-2026-07"}, parts...)
	return filepath.Join(all...)
}

func loadSabotageEvent(t *testing.T) sabotageEventFixture {
	t.Helper()
	b, err := os.ReadFile(sabotageFixtureDir("events.json"))
	if err != nil {
		t.Fatal(err)
	}
	var event sabotageEventFixture
	if err := json.Unmarshal(b, &event); err != nil {
		t.Fatalf("decode sabotage events.json: %v", err)
	}
	if event.AsOf == "" || event.EventID == "" {
		t.Fatalf("incomplete sabotage events.json: %+v", event)
	}
	return event
}

func sabotageAsOf(t *testing.T, event sabotageEventFixture) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, event.AsOf)
	if err != nil {
		t.Fatalf("parse sabotage as_of: %v", err)
	}
	return at
}

// copySabotageVault replays committed INPUT bytes rather than constructing
// Memory values in the test. onlyBase, when non-empty, selects fixture basenames.
func copySabotageVault(t *testing.T, cfg Config, onlyBase map[string]bool) {
	t.Helper()
	srcRoot := sabotageFixtureDir("vault")
	err := filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(cfg.VaultDir, rel), 0o755)
		}
		if len(onlyBase) > 0 && !onlyBase[filepath.Base(path)] {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		dst := filepath.Join(cfg.VaultDir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatalf("copy sabotage fixture vault: %v", err)
	}
}

func seedSabotageHome(t *testing.T, onlyBase map[string]bool) (Config, sabotageEventFixture, time.Time) {
	t.Helper()
	withTempHome(t)
	pinBriefClock(t)
	run(t, "init")
	cfg := mustConfig(t)
	event := loadSabotageEvent(t)
	at := sabotageAsOf(t, event)
	if err := saveSources(cfg, []Source{{
		Name: "gmail", Type: "gmail", Email: "adit@example.com",
		Enabled: ptr(true), CreatedAt: at.Add(-30 * 24 * time.Hour).Format(time.RFC3339),
	}}); err != nil {
		t.Fatal(err)
	}
	copySabotageVault(t, cfg, onlyBase)
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatalf("rebuild sabotage fixture index: %v", err)
	}
	return cfg, event, at
}

func sabotageBriefLines(brief MeetingBrief) []string {
	var lines []string
	if brief.Event != nil {
		lines = append(lines, brief.Event.Title)
	}
	for _, section := range brief.Sections {
		for _, line := range section.Lines {
			lines = append(lines, line.Text)
		}
	}
	return lines
}

func scanSabotageJunk(lines []string) []sabotageMatch {
	var matches []sabotageMatch
	for _, line := range lines {
		for _, junk := range sabotageJunkPatterns {
			if regexp.MustCompile(junk.pattern).FindStringIndex(strings.TrimSpace(line)) != nil {
				matches = append(matches, sabotageMatch{
					DefectClass: junk.defectClass,
					Fixture:     junk.sourceFixture,
					Line:        line,
				})
			}
		}
	}
	return matches
}

func assertSabotageBriefPasses(t *testing.T, surface string, brief MeetingBrief) {
	t.Helper()
	lines := sabotageBriefLines(brief)
	if matches := scanSabotageJunk(lines); len(matches) > 0 {
		t.Fatalf("%s regenerated frozen gibberish: %+v\nlines: %q", surface, matches, lines)
	}
}

func TestSabotageGibberishNeverRenders(t *testing.T) {
	// Load-bearing mutation map for the four production gates that the frozen
	// incident depends on. Each fixture is deliberately clean with respect to the
	// other three gates, so disabling any ONE gate makes its named junk render:
	//
	//   assignedToThirdParty            -> wrapped-third-party.md
	//   meetingBriefIsTwoPartyExchange  -> ambiguous-group-ask.md
	//   isMeetingNotification           -> rsvp-meet-url.md / teams-footer.md
	//   stripURLs                       -> meet-url-action.md
	//
	// Keep this isolation intact: coupling two defects in one memory recreates the
	// false-green failure mode this sabotage gate exists to prevent.
	cfg, event, at := seedSabotageHome(t, nil)
	ctx := context.Background()

	direct, err := buildEventMeetingBrief(ctx, cfg, event.EventID, at, 0, 8)
	if err != nil {
		t.Fatalf("buildEventMeetingBrief: %v", err)
	}
	var rendered bytes.Buffer
	if err := renderMeetingBrief(&rendered, direct); err != nil {
		t.Fatalf("renderMeetingBrief: %v", err)
	}
	assertSabotageBriefPasses(t, "buildEventMeetingBrief/renderMeetingBrief", direct)
	if matches := scanSabotageJunk(strings.Split(rendered.String(), "\n")); len(matches) > 0 {
		t.Fatalf("human renderer regenerated frozen gibberish: %+v\n%s", matches, rendered.String())
	}
	if strings.Contains(rendered.String(), sabotageGenuineObligation) {
		t.Fatalf("unrelated commitment crossed the event relevance gate:\n%s", rendered.String())
	}

	mcpValue, err := callMCPTool(ctx, "meeting_prep", map[string]any{
		"event_id": event.EventID,
		"at":       event.AsOf,
	})
	if err != nil {
		t.Fatalf("MCP meeting_prep: %v", err)
	}
	mcpBrief, ok := mcpValue.(MeetingBrief)
	if !ok {
		t.Fatalf("MCP meeting_prep returned %T, want MeetingBrief", mcpValue)
	}
	assertSabotageBriefPasses(t, "MCP meeting_prep", mcpBrief)
}

func TestSabotageJunkPatternWhitespaceVariants(t *testing.T) {
	variants := []struct {
		line        string
		defectClass string
	}{
		{"Need\thelp ?", "invite-footer"},
		{"Was this   helpful ?", "generic-footer-question"},
		{"Open to see how the loop  works ?", "forwarded-cta"},
		{"Cost varies depending  on   usage", "substring-pending"},
		{"Declined:Sync up meeting", "rsvp-notification"},
		{"Fwd:Google  Ads Account Audit", "forwarded-subject"},
	}
	for _, variant := range variants {
		t.Run(variant.defectClass, func(t *testing.T) {
			matches := scanSabotageJunk([]string{variant.line})
			if len(matches) == 0 || matches[0].DefectClass != variant.defectClass {
				t.Fatalf("junk scorer missed whitespace/punctuation variant %q: %+v", variant.line, matches)
			}
		})
	}
}

func questionExtractionFromSabotageVault(t *testing.T) string {
	t.Helper()
	var questions []string
	err := filepath.WalkDir(sabotageFixtureDir("vault"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasSuffix(line, "?") {
				questions = append(questions, line)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("extract question sentences: %v", err)
	}
	sort.Strings(questions)
	return strings.Join(questions, "\n")
}

func sabotageScorerPasses(artifact string) bool {
	return len(scanSabotageJunk(strings.Split(artifact, "\n"))) == 0 &&
		strings.Contains(artifact, sabotageGenuineObligation)
}

func TestSabotageScorerSelfCheck(t *testing.T) {
	frozen, err := os.ReadFile(sabotageFixtureDir("frozen", "rendered-2026-07.md"))
	if err != nil {
		t.Fatal(err)
	}
	degenerate := map[string]string{
		"committed frozen rendered gibberish": string(frozen),
		"always-empty brief":                  "",
		"every sentence ending in question":   questionExtractionFromSabotageVault(t),
	}
	for name, artifact := range degenerate {
		t.Run(name, func(t *testing.T) {
			if sabotageScorerPasses(artifact) {
				t.Fatalf("EVAL_BROKEN: sabotage scorer accepted degenerate artifact %q:\n%s", name, artifact)
			}
		})
	}

	// The control proves that the non-emptiness rail is satisfiable rather than an
	// unconditional failure disguised as an eval.
	if !sabotageScorerPasses(sabotageGenuineObligation) {
		t.Fatal(fmt.Errorf("EVAL_BROKEN: scorer rejected its clean positive control"))
	}
}
