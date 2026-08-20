package meeting

import (
	"fmt"
	"io"
	"strings"
)

type SectionCandidate struct {
	Kind string
	Line CitedLine
}

func WithCandidates(brief Brief, candidates []SectionCandidate) Brief {
	brief.Sections = brief.Sections[:0]
	for _, kind := range SectionOrder {
		lines := []CitedLine{}
		for _, candidate := range candidates {
			if candidate.Kind == kind {
				lines = append(lines, candidate.Line)
			}
		}
		if len(lines) > 0 {
			brief.Sections = append(brief.Sections, BriefSection{Kind: kind, Title: SectionTitles[kind], Lines: lines})
		}
	}
	return brief
}
func LineCount(brief Brief) int {
	count := 0
	for _, section := range brief.Sections {
		count += len(section.Lines)
	}
	return count
}

type AttributionAssociation struct {
	DecisionKey, PersonID string
	AttendeeSender        bool
}

// ResolveAttribution returns the selected association index, or -1 when attribution is ambiguous.
func ResolveAttribution(associations []AttributionAssociation, decisions map[string]string, confirm, reject string) (int, bool) {
	filtered := make([]int, 0, len(associations))
	confirmed := make([]int, 0, len(associations))
	for i, candidate := range associations {
		switch decisions[candidate.DecisionKey] {
		case reject:
			continue
		case confirm:
			confirmed = append(confirmed, i)
		}
		filtered = append(filtered, i)
	}
	if len(confirmed) == 1 {
		return confirmed[0], true
	}
	if len(confirmed) > 1 || len(filtered) == 0 {
		return -1, false
	}
	byPerson := map[string]int{}
	for _, i := range filtered {
		candidate := associations[i]
		current, exists := byPerson[candidate.PersonID]
		if !exists || candidate.AttendeeSender {
			byPerson[candidate.PersonID] = i
		} else {
			byPerson[candidate.PersonID] = current
		}
	}
	only := -1
	senders := []int{}
	for _, i := range byPerson {
		only = i
		if associations[i].AttendeeSender {
			senders = append(senders, i)
		}
	}
	if len(senders) == 1 {
		return senders[0], true
	}
	if len(senders) == 0 && len(byPerson) == 1 {
		return only, true
	}
	return -1, false
}
func CitationText(c Citation) string {
	return fmt.Sprintf("{memory-id: %s, channel: %s, source: %s, date: %s}", c.MemoryID(), c.Channel(), c.Source(), c.Date())
}
func Render(w io.Writer, brief Brief, banner string) error {
	if err := brief.Validate(); err != nil {
		return fmt.Errorf("refusing to render uncited meeting brief: %w", err)
	}
	if banner != "" {
		fmt.Fprintln(w, banner)
		fmt.Fprintln(w)
	}
	if brief.Event == nil {
		return nil
	}
	fmt.Fprintln(w, "# Meeting brief")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "- %s — %s", brief.Event.Title, brief.Event.StartsAt)
	if len(brief.Event.Attendees) > 0 {
		fmt.Fprintf(w, " — attendees: %s", strings.Join(brief.Event.Attendees, ", "))
	}
	fmt.Fprintf(w, " %s\n", CitationText(brief.Event.Citation))
	for _, section := range brief.Sections {
		fmt.Fprintf(w, "\n## %s\n", section.Title)
		for _, line := range section.Lines {
			fmt.Fprintf(w, "- %s %s\n", line.Text, CitationText(line.Citation))
			fmt.Fprintf(w, "  actions: correct=`%s` unlink=`%s`\n", line.Correction.CorrectCommand, line.Correction.UnlinkCommand)
		}
	}
	return nil
}
