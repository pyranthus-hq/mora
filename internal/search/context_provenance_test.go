package search

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestContextMarksAnAgentNoteAsUnchecked(t *testing.T) {
	cfg := config.Config{VaultDir: t.TempDir()}
	item := memory.Memory{ID: "mem_1", Title: "Pricing band", Text: "the band is 2,500 to 5,000", Source: "mcp"}
	got := BuildContext(cfg, []memory.Memory{item}, 4000, true)
	want := `Provenance: agent-written (source "mcp"); not checked against the user's words. Repeating it adds no evidence.`
	if !strings.Contains(got, want) {
		t.Fatalf("missing %q in:\n%s", want, got)
	}
}

func TestContextNamesTheConnectorBehindEvidence(t *testing.T) {
	cfg := config.Config{VaultDir: t.TempDir()}
	item := memory.Memory{ID: "gmail_thread/abc", Provider: "gmail", Title: "Background check", Text: "the check is complete"}
	got := BuildContext(cfg, []memory.Memory{item}, 4000, true)
	if !strings.Contains(got, "Provenance: evidence from gmail.") {
		t.Fatalf("missing the evidence line in:\n%s", got)
	}
}

func TestContextNamesAFileOnDiskWhenThereIsNoProvider(t *testing.T) {
	cfg := config.Config{VaultDir: t.TempDir()}
	item := memory.Memory{
		ID:    "src_1",
		Path:  "/vault/mora/sources/filesystem/knowledge/src_1.md",
		Tags:  []string{"filesystem", "knowledge"},
		Title: "design review",
		Text:  "the reviewer disagreed",
	}
	got := BuildContext(cfg, []memory.Memory{item}, 4000, true)
	if !strings.Contains(got, `Provenance: a file on disk (source "knowledge"). The file exists; what it says is unchecked, and the file may itself be an agent's write-up.`) {
		t.Fatalf("missing the file line in:\n%s", got)
	}
}

// A source label claiming the user confirmed something is quoted to the reader
// as the writer's own wording. Mora does not read that claim and does not grade
// the record differently for making it: guessing at English here would invent
// the user authority this line exists to withhold.
func TestContextQuotesAWriterClaimWithoutActingOnIt(t *testing.T) {
	cfg := config.Config{VaultDir: t.TempDir()}
	item := memory.Memory{ID: "mem_1", Title: "Locked thesis", Text: "the thesis is locked", Source: "user-confirmed 2026-08-12"}
	got := BuildContext(cfg, []memory.Memory{item}, 4000, true)
	want := `Provenance: agent-written (source "user-confirmed 2026-08-12"); not checked against the user's words. Repeating it adds no evidence.`
	if !strings.Contains(got, want) {
		t.Fatalf("missing %q in:\n%s", want, got)
	}
	if strings.Contains(got, "reports user confirmation") {
		t.Fatalf("the surface acted on a free-text label in:\n%s", got)
	}
}

// No field in the vault records that a human accepted a decision, so the line
// may never say one did.
func TestContextNeverClaimsAUserAdoptedADecision(t *testing.T) {
	cfg := config.Config{VaultDir: t.TempDir()}
	decision := memory.Memory{
		ID: "mem_1", Type: "decision", Title: "Ship the rerank lane", Text: "we ship it", Source: "mcp",
		Decision:       &memory.DecisionValidity{AsOf: "2026-01-01T00:00:00Z", Durability: "standing", Complete: true},
		DecisionStatus: "current",
	}
	got := BuildContext(cfg, []memory.Memory{decision}, 4000, true)
	if !strings.Contains(got, "Adopted by user: no record either way") {
		t.Fatalf("missing the adoption line in:\n%s", got)
	}
	claimed := decision
	claimed.Source = "user-confirmed 2026-08-12"
	gotClaimed := BuildContext(cfg, []memory.Memory{claimed}, 4000, true)
	// A label claiming the user confirmed something changes nothing: no field
	// records adoption, so the line reads the same and the label is shown as
	// the writer's own wording on the provenance line.
	if !strings.Contains(gotClaimed, "Adopted by user: no record either way") {
		t.Fatalf("a source label changed the adoption line in:\n%s", gotClaimed)
	}
	if !strings.Contains(gotClaimed, `agent-written (source "user-confirmed 2026-08-12")`) {
		t.Fatalf("the source label was not carried to the reader in:\n%s", gotClaimed)
	}
}

func TestContextOmitsTheAdoptionLineForRecordsThatAreNotDecisions(t *testing.T) {
	cfg := config.Config{VaultDir: t.TempDir()}
	item := memory.Memory{ID: "mem_1", Type: "fact", Title: "A fact", Text: "something happened", Source: "mcp"}
	if got := BuildContext(cfg, []memory.Memory{item}, 4000, true); strings.Contains(got, "Adopted by user:") {
		t.Fatalf("a plain fact should not carry an adoption line:\n%s", got)
	}
}

func TestContextPointsAnOlderRecordAtTheNewerOne(t *testing.T) {
	cfg := config.Config{VaultDir: t.TempDir()}
	item := memory.Memory{
		ID: "mem_old", Title: "Pilot feedback", Text: "the pilot is running", Source: "mcp",
		LaterRelatedEvidence: &memory.LaterRelatedEvidence{ID: "mem_new", Title: "Pilot ended", IndexedAt: "2026-07-30T12:00:00Z"},
	}
	got := BuildContext(cfg, []memory.Memory{item}, 4000, true)
	want := `Later related record: mem_new "Pilot ended" (2026-07-30T12:00:00Z). Read it before relying on this one.`
	if !strings.Contains(got, want) {
		t.Fatalf("missing %q in:\n%s", want, got)
	}
}

func TestContextMarksTheVaultControlFiles(t *testing.T) {
	cfg := contextCfg(t)
	got := BuildContext(cfg, nil, 4000, false)
	if !strings.Contains(got, "Provenance: a vault control file, not a memory; who wrote it is unchecked.") {
		t.Fatalf("missing the control-file line in:\n%s", got)
	}
}

// A long source label is a free-text field; one in the real vault runs to a
// full sentence. It must not be able to swallow the budget.
func TestContextClipsALongSourceLabel(t *testing.T) {
	cfg := config.Config{VaultDir: t.TempDir()}
	item := memory.Memory{ID: "mem_1", Title: "Long label", Text: "body", Source: strings.Repeat("a very long provenance sentence ", 20)}
	got := BuildContext(cfg, []memory.Memory{item}, 4000, true)
	line := ""
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "Provenance: ") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatal("no provenance line")
	}
	if utf8.RuneCountInString(line) > 200 {
		t.Fatalf("the provenance line is %d runes long: %q", utf8.RuneCountInString(line), line)
	}
	if !strings.Contains(line, "…") {
		t.Fatalf("a clipped label should show it was clipped: %q", line)
	}
}

// The warning is the point of the line, so a budget that cannot hold the whole
// header drops the record rather than printing a bare title.
func TestContextNeverPrintsATitleWithoutItsProvenanceLine(t *testing.T) {
	cfg := config.Config{VaultDir: t.TempDir()}
	item := memory.Memory{ID: "mem_1", Title: "Widget pilot", Text: strings.Repeat("claim ", 40), Source: "mcp"}
	full := BuildContext(cfg, []memory.Memory{item}, 4000, true)
	for budget := 1; budget <= utf8.RuneCountInString(full)+5; budget++ {
		got := BuildContext(cfg, []memory.Memory{item}, budget, true)
		if utf8.RuneCountInString(got) > budget || !utf8.ValidString(got) {
			t.Fatalf("budget %d produced %d runes, valid=%v", budget, utf8.RuneCountInString(got), utf8.ValidString(got))
		}
		if strings.Contains(got, "# Widget pilot") && !strings.Contains(got, "Provenance: agent-written") {
			t.Fatalf("budget %d printed a bare title:\n%s", budget, got)
		}
	}
}

// One ordinary agent note costs this much header. The number is here so a
// future change to the wording has to look at what it costs the budget.
func TestContextHeaderCostIsVisible(t *testing.T) {
	cfg := config.Config{VaultDir: t.TempDir()}
	item := memory.Memory{ID: "mem_1", Title: "Pricing band", Text: "body", Source: "mcp"}
	got := BuildContext(cfg, []memory.Memory{item}, 4000, true)
	header := got[:strings.Index(got, "body")]
	t.Logf("an agent note costs %d runes of header, of which %d are the title line",
		utf8.RuneCountInString(header), utf8.RuneCountInString("\n# Pricing band\n"))
	if utf8.RuneCountInString(header) > 250 {
		t.Fatalf("the header grew to %d runes: %q", utf8.RuneCountInString(header), header)
	}
}

// Every hedge in a provenance warning sits at the end of it, so half a warning
// reads as more confident than the whole one. At any budget the surface either
// prints a complete warning or prints nothing at all.
func TestContextNeverPrintsHalfAWarning(t *testing.T) {
	cfg := config.Config{VaultDir: t.TempDir()}
	item := memory.Memory{ID: "mem_1", Title: "Locked thesis", Text: "body", Source: "mcp"}
	full := BuildContext(cfg, []memory.Memory{item}, 4000, true)
	for budget := 1; budget <= len(full); budget++ {
		got := BuildContext(cfg, []memory.Memory{item}, budget, true)
		for _, line := range strings.Split(got, "\n") {
			if !strings.HasPrefix(line, "Provenance:") {
				continue
			}
			if !strings.HasSuffix(line, ".") {
				t.Fatalf("budget %d printed a cut-off warning: %q", budget, line)
			}
		}
	}
}

// A staging path is shortened to the file name so it cannot spend the budget
// repeating a directory layout on every row.
func TestContextShortensAStagingPathToItsFileName(t *testing.T) {
	cfg := config.Config{VaultDir: t.TempDir()}
	item := memory.Memory{
		ID:     "src_1",
		Tags:   []string{"filesystem", "claude-project-memory"},
		Path:   "/vault/mora/sources/filesystem/claude-project-memory/src_1.md",
		Title:  "a mirrored note",
		Text:   "body",
		Source: "/home/a/.config/mora/staging/claude-project-memory/-home-a-project/some-note.md",
	}
	got := BuildContext(cfg, []memory.Memory{item}, 4000, true)
	if !strings.Contains(got, `(source "-home-a-project/some-note.md")`) {
		t.Fatalf("the label was not shortened to its last two segments in:\n%s", got)
	}
	if strings.Contains(got, "/staging/") {
		t.Fatalf("the staging path reached the reader in:\n%s", got)
	}
	// Two notes of the same name under different projects must still read
	// differently, or the line invites a reader to treat them as one source.
	other := item
	other.Source = "/home/a/.config/mora/staging/claude-project-memory/-home-a-other/some-note.md"
	gotOther := BuildContext(cfg, []memory.Memory{other}, 4000, true)
	if !strings.Contains(gotOther, `(source "-home-a-other/some-note.md")`) {
		t.Fatalf("two projects collapsed to one source in:\n%s", gotOther)
	}
	// A label that is not a file path is left exactly as the writer typed it.
	notAPath := item
	notAPath.Source = "/review/approved"
	gotLabel := BuildContext(cfg, []memory.Memory{notAPath}, 4000, true)
	if !strings.Contains(gotLabel, `(source "/review/approved")`) {
		t.Fatalf("a non-path label was mangled in:\n%s", gotLabel)
	}
}
