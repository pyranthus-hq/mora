package meeting

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/commitment"
	"github.com/pyranthus-hq/mora/internal/governance"
)

var briefTestNow = time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)

func validBriefParts(t *testing.T) (Citation, LineCorrection, CitedLine, CitedEvent, Brief) {
	t.Helper()
	c, err := NewCitation("gmail_thread/t1", "gmail", "gmail:me@example.com", "2026-07-10T13:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	cor, err := NewLineCorrection(governance.Atom{Kind: governance.AtomStableID, Value: "gmail_thread/t1"}, governance.Atom{Kind: governance.AtomAddress, Value: "other@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	line, err := NewCitedLine("I will send it", "Other", c, cor, briefTestNow)
	if err != nil {
		t.Fatal(err)
	}
	event := CitedEvent{ID: "event/1", Title: "Sync", StartsAt: "2026-07-11T10:00:00Z", Attendees: []string{"other@example.com"}, Citation: c}
	brief := Brief{AsOf: briefTestNow.Format(time.RFC3339), Event: &event, Sections: []BriefSection{{Kind: OpenLoops, Title: SectionTitles[OpenLoops], Lines: []CitedLine{line}}}}
	return c, cor, line, event, brief
}

func TestBriefCitationJSONAndValidation(t *testing.T) {
	c, _, _, _, _ := validBriefParts(t)
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"memory_id":"gmail_thread/t1","channel":"gmail","source":"gmail:me@example.com","date":"2026-07-10T13:00:00Z"}`
	if string(body) != want {
		t.Fatalf("json=%s", body)
	}
	var round Citation
	if err := json.Unmarshal(body, &round); err != nil {
		t.Fatal(err)
	}
	if round.MemoryID() != "gmail_thread/t1" || round.Channel() != "gmail" || round.Source() != "gmail:me@example.com" || round.Date() != "2026-07-10T13:00:00Z" {
		t.Fatalf("round=%+v", round)
	}
	for _, raw := range []string{`{"channel":"x","source":"x","date":"2026-01-01T00:00:00Z"}`, `{"memory_id":"x","source":"x","date":"2026-01-01T00:00:00Z"}`, `{"memory_id":"x","channel":"x","date":"2026-01-01T00:00:00Z"}`, `{"memory_id":"x","channel":"x","source":"x"}`, `{"memory_id":"x","channel":"x","source":"x","date":"bad"}`, `{`} {
		var got Citation
		if json.Unmarshal([]byte(raw), &got) == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	var malformed Citation
	if malformed.UnmarshalJSON([]byte("{")) == nil {
		t.Fatal("malformed JSON accepted")
	}
}

func TestBriefLineAndCorrectionFailClosed(t *testing.T) {
	c, cor, line, _, _ := validBriefParts(t)
	if !strings.Contains(cor.CorrectCommand, "--confirm") || !strings.Contains(cor.UnlinkCommand, "--unlink --yes") {
		t.Fatalf("commands=%+v", cor)
	}
	for _, tc := range []struct {
		name             string
		stable, attendee governance.Atom
	}{
		{"missing stable", governance.Atom{}, governance.Atom{Kind: governance.AtomAddress, Value: "a"}},
		{"bad attendee", governance.Atom{Kind: governance.AtomStableID, Value: "m"}, governance.Atom{Kind: "bad", Value: "a"}},
		{"empty attendee", governance.Atom{Kind: governance.AtomStableID, Value: "m"}, governance.Atom{Kind: governance.AtomHandle}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewLineCorrection(tc.stable, tc.attendee); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	badCommands := cor
	badCommands.CorrectCommand = ""
	if badCommands.Validate() == nil {
		t.Fatal("empty command accepted")
	}
	if _, err := NewCitedLine(" ", "", c, cor, briefTestNow); err == nil {
		t.Fatal("empty line accepted")
	}
	badCitation := Citation{}
	if _, err := NewCitedLine("x", "", badCitation, cor, briefTestNow); err == nil {
		t.Fatal("bad citation accepted")
	}
	if _, err := NewCitedLine("x", "", c, badCommands, briefTestNow); err == nil {
		t.Fatal("bad correction accepted")
	}
	bad := line
	bad.Text = ""
	if bad.Validate() == nil {
		t.Fatal("empty text accepted")
	}
	bad = line
	bad.Citation = Citation{}
	if bad.Validate() == nil {
		t.Fatal("bad citation accepted")
	}
	bad = line
	bad.Correction = badCommands
	if bad.Validate() == nil {
		t.Fatal("bad correction accepted")
	}
	bad = line
	bad.Direction = "weird"
	if bad.Validate() == nil {
		t.Fatal("bad direction accepted")
	}
	bad = line
	bad.Direction = commitment.OwedBySelf
	bad.Owner = governance.Atom{}
	bad.Counterparty = governance.Atom{Kind: governance.AtomAddress, Value: "a"}
	if bad.Validate() == nil {
		t.Fatal("missing owner accepted")
	}
	self := governance.Atom{Kind: governance.AtomAddress, Value: "self"}
	other := governance.Atom{Kind: governance.AtomAddress, Value: "other"}
	bad = line
	bad.Direction = commitment.OwedByCounterparty
	bad.Owner = self
	bad.Counterparty = other
	bad.DueAt = commitment.DueNone
	if bad.Validate() == nil {
		t.Fatal("direction mismatch accepted")
	}
	for _, due := range []string{"", "not-a-date"} {
		bad = line
		bad.Direction = commitment.OwedBySelf
		bad.Owner = self
		bad.Counterparty = other
		bad.DueAt = due
		if bad.Validate() == nil {
			t.Errorf("due %q accepted", due)
		}
	}
	for _, due := range []string{commitment.DueNone, commitment.DueRelative, "2026-07-11"} {
		good := line
		good.Direction = commitment.OwedBySelf
		good.Owner = self
		good.Counterparty = other
		good.DueAt = due
		if err := good.Validate(); err != nil {
			t.Errorf("due %q: %v", due, err)
		}
	}
	if line.ValidateHistorical(briefTestNow) != nil {
		t.Fatal("valid historical rejected")
	}
	bad = line
	bad.Text = "present tense"
	if bad.ValidateHistorical(briefTestNow) == nil {
		t.Fatal("missing prefix accepted")
	}
	bad = line
	bad.Text = HistoricalPrefix(briefTestNow, c.Date(), line.Attendee) + "“is waiting”"
	if bad.ValidateHistorical(briefTestNow) == nil {
		t.Fatal("present tense accepted")
	}
}

func TestCitedEventAndBriefValidation(t *testing.T) {
	c, _, line, event, brief := validBriefParts(t)
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := brief.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, e := range []CitedEvent{{}, {ID: "x", Title: "x", StartsAt: "bad", Citation: c}, {ID: "x", Title: "x", StartsAt: "2026-01-01T00:00:00Z"}} {
		if e.Validate() == nil {
			t.Errorf("event accepted %+v", e)
		}
	}
	empty := Brief{AsOf: briefTestNow.Format(time.RFC3339)}
	if err := empty.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []Brief{
		{AsOf: "bad"},
		{AsOf: briefTestNow.Format(time.RFC3339), EgressCalls: 1},
		{AsOf: briefTestNow.Format(time.RFC3339), Sections: []BriefSection{{Kind: OpenLoops}}},
		{AsOf: briefTestNow.Format(time.RFC3339), Event: &CitedEvent{}},
		{AsOf: briefTestNow.Format(time.RFC3339), Event: &event, Sections: []BriefSection{{Kind: "bad", Title: "bad", Lines: []CitedLine{line}}}},
		{AsOf: briefTestNow.Format(time.RFC3339), Event: &event, Sections: []BriefSection{{Kind: OpenLoops, Title: SectionTitles[OpenLoops]}}},
	}
	for i, b := range cases {
		if b.Validate() == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	badLine := line
	badLine.Text = ""
	b := brief
	b.Sections[0].Lines = []CitedLine{badLine}
	if b.Validate() == nil {
		t.Fatal("uncited line accepted")
	}
	badLine = line
	badLine.Text = HistoricalPrefix(briefTestNow, c.Date(), line.Attendee) + "“needs work”"
	b = brief
	b.Sections[0].Lines = []CitedLine{badLine}
	if b.Validate() == nil {
		t.Fatal("present tense line accepted")
	}
}
