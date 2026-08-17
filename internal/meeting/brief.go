package meeting

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/commitment"
	"github.com/pyranthus-hq/mora/internal/evidence"
	"github.com/pyranthus-hq/mora/internal/governance"
	"github.com/pyranthus-hq/mora/internal/health"
)

var SectionTitles = map[string]string{OpenLoops: "Your open loops and obligations", Unresolved: "Unresolved decisions and threads", Staleness: "Staleness guards", SharedContext: "Material shared context"}
var SectionOrder = []string{OpenLoops, Unresolved, Staleness, SharedContext}

type Citation = evidence.Citation

func NewCitation(memoryID, channel, source, date string) (Citation, error) {
	return evidence.NewCitation(memoryID, channel, source, date)
}

type LineCorrection struct {
	StableAtom     governance.Atom `json:"stable_atom"`
	AttendeeAtom   governance.Atom `json:"attendee_atom"`
	CorrectCommand string          `json:"correct_command"`
	UnlinkCommand  string          `json:"unlink_command"`
}

func NewLineCorrection(stable, attendee governance.Atom) (LineCorrection, error) {
	c := LineCorrection{StableAtom: stable, AttendeeAtom: attendee, CorrectCommand: fmt.Sprintf("mora brief correct --memory-id %s --attendee %s --confirm", stable.Value, attendee.Value), UnlinkCommand: fmt.Sprintf("mora brief correct --memory-id %s --attendee %s --unlink --yes", stable.Value, attendee.Value)}
	if err := c.Validate(); err != nil {
		return LineCorrection{}, err
	}
	return c, nil
}
func (c LineCorrection) Validate() error {
	if c.StableAtom.Kind != governance.AtomStableID || strings.TrimSpace(c.StableAtom.Value) == "" {
		return errors.New("missing stable source atom")
	}
	if c.AttendeeAtom.Kind != governance.AtomHandle && c.AttendeeAtom.Kind != governance.AtomAddress {
		return errors.New("missing attendee atom kind")
	}
	if strings.TrimSpace(c.AttendeeAtom.Value) == "" {
		return errors.New("missing attendee atom value")
	}
	if strings.TrimSpace(c.CorrectCommand) == "" || strings.TrimSpace(c.UnlinkCommand) == "" {
		return errors.New("missing correction commands")
	}
	return nil
}

type CommitmentCitation struct {
	Citation     Citation `json:"citation"`
	CommitmentID string   `json:"commitment_id,omitempty"`
	Role         string   `json:"role"`
	EvidenceRef  string   `json:"evidence_ref,omitempty"`
}
type CitedLine struct {
	Text                string               `json:"text"`
	Attendee            string               `json:"attendee,omitempty"`
	Citation            Citation             `json:"citation"`
	Correction          LineCorrection       `json:"correction"`
	Direction           commitment.Direction `json:"direction,omitempty"`
	Owner               governance.Atom      `json:"owner,omitzero"`
	Counterparty        governance.Atom      `json:"counterparty,omitzero"`
	CounterpartyLabel   string               `json:"counterparty_label,omitempty"`
	CommitmentID        string               `json:"commitment_id,omitempty"`
	Lifecycle           string               `json:"lifecycle,omitempty"`
	ClosureRef          string               `json:"closure_ref,omitempty"`
	DuplicateOf         string               `json:"duplicate_of,omitempty"`
	StateUncertain      bool                 `json:"state_uncertain,omitempty"`
	CommitmentCitations []CommitmentCitation `json:"commitment_citations,omitempty"`
	DueAt               string               `json:"due_at,omitempty"`
}

func NewCitedLine(text, attendee string, citation Citation, correction LineCorrection, asOf time.Time) (CitedLine, error) {
	line := CitedLine{Attendee: strings.TrimSpace(attendee), Citation: citation, Correction: correction}
	raw := OneLine(text)
	if raw == "" {
		return CitedLine{}, errors.New("empty cited line")
	}
	if err := line.Citation.Validate(); err != nil {
		return CitedLine{}, err
	}
	if err := line.Correction.Validate(); err != nil {
		return CitedLine{}, err
	}
	line.Text = HistoricalText(asOf, line.Citation.Date(), line.Attendee, raw)
	return line, nil
}
func (l CitedLine) Validate() error {
	if strings.TrimSpace(l.Text) == "" {
		return errors.New("empty text")
	}
	if err := l.Citation.Validate(); err != nil {
		return err
	}
	if err := l.Correction.Validate(); err != nil {
		return err
	}
	if l.Direction == "" {
		return nil
	}
	if l.Direction != commitment.OwedBySelf && l.Direction != commitment.OwedByCounterparty {
		return fmt.Errorf("invalid commitment direction %q", l.Direction)
	}
	if !atomPresent(l.Owner) || !atomPresent(l.Counterparty) {
		return errors.New("typed commitment line is missing owner or counterparty")
	}
	if (l.Direction == commitment.OwedByCounterparty) != atomEqual(l.Owner, l.Counterparty) {
		return errors.New("commitment owner and direction disagree")
	}
	switch l.DueAt {
	case commitment.DueNone, commitment.DueRelative:
	case "":
		return errors.New("typed commitment line is missing due classification")
	default:
		if _, err := time.Parse("2006-01-02", l.DueAt); err != nil {
			return fmt.Errorf("invalid commitment due value %q", l.DueAt)
		}
	}
	return nil
}
func (l CitedLine) ValidateHistorical(asOf time.Time) error {
	prefix := HistoricalPrefix(asOf, l.Citation.Date(), l.Attendee)
	if prefix == "" || !strings.HasPrefix(l.Text, prefix) || !strings.HasSuffix(l.Text, "”") {
		return errors.New("evidence must be rendered as a dated, past-tense cited record")
	}
	return nil
}
func atomPresent(a governance.Atom) bool {
	return strings.TrimSpace(a.Kind) != "" && strings.TrimSpace(a.Value) != ""
}
func atomEqual(a, b governance.Atom) bool {
	return strings.EqualFold(strings.TrimSpace(a.Provider), strings.TrimSpace(b.Provider)) && strings.EqualFold(strings.TrimSpace(a.Kind), strings.TrimSpace(b.Kind)) && strings.EqualFold(strings.TrimSpace(a.Value), strings.TrimSpace(b.Value))
}

type CitedEvent struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	StartsAt  string   `json:"starts_at"`
	Attendees []string `json:"attendees"`
	Citation  Citation `json:"citation"`
}

func (e CitedEvent) Validate() error {
	if strings.TrimSpace(e.ID) == "" || strings.TrimSpace(e.Title) == "" || strings.TrimSpace(e.StartsAt) == "" {
		return errors.New("meeting event is incomplete")
	}
	if _, err := time.Parse(time.RFC3339, e.StartsAt); err != nil {
		return fmt.Errorf("invalid event starts_at %q: %w", e.StartsAt, err)
	}
	return e.Citation.Validate()
}

type BriefSection struct {
	Kind  string      `json:"kind"`
	Title string      `json:"title"`
	Lines []CitedLine `json:"lines"`
}
type Brief struct {
	AsOf           string            `json:"as_of"`
	Event          *CitedEvent       `json:"event"`
	Sections       []BriefSection    `json:"sections"`
	Gaps           []string          `json:"gaps,omitempty"`
	SelfUnresolved bool              `json:"self_unresolved,omitempty"`
	NameFallback   bool              `json:"name_fallback,omitempty"`
	EgressCalls    int               `json:"egress_calls"`
	SourceHealth   []health.Source   `json:"source_health,omitempty"`
	IndexHealth    health.Index      `json:"-"`
	ProducerHealth []health.Producer `json:"-"`
	Health         health.Compact    `json:"health"`
}

func (b Brief) Validate() error {
	asOf, err := time.Parse(time.RFC3339, b.AsOf)
	if err != nil {
		return fmt.Errorf("invalid as_of %q: %w", b.AsOf, err)
	}
	if b.EgressCalls != 0 {
		return fmt.Errorf("meeting brief egress meter is %d, want 0", b.EgressCalls)
	}
	if b.Event == nil {
		if len(b.Sections) != 0 {
			return errors.New("meeting brief has sections without an event")
		}
		return nil
	}
	if err := b.Event.Validate(); err != nil {
		return fmt.Errorf("event citation: %w", err)
	}
	for _, section := range b.Sections {
		title, known := SectionTitles[section.Kind]
		if !known || title != section.Title {
			return fmt.Errorf("unknown meeting brief section %q", section.Kind)
		}
		if len(section.Lines) == 0 {
			return fmt.Errorf("meeting brief section %q is empty", section.Kind)
		}
		for i, line := range section.Lines {
			if err := line.Validate(); err != nil {
				return fmt.Errorf("%s line %d is uncited: %w", section.Kind, i, err)
			}
			if err := line.ValidateHistorical(asOf); err != nil {
				return fmt.Errorf("%s line %d violates the dated-historical rail: %w", section.Kind, i, err)
			}
		}
	}
	return nil
}
