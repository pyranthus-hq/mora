package companion

import (
	"strings"
	"testing"
)

func TestSharedReadingFieldBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*TodayProjection)
	}{
		{"snippet", func(p *TodayProjection) { p.Items[0].Snippet = strings.Repeat("x", MaxSnippetBytes+1) }},
		{"coverage label", func(p *TodayProjection) { p.Coverage[0].Label = strings.Repeat("x", MaxLabelBytes+1) }},
		{"coverage count", func(p *TodayProjection) { p.Coverage = make([]SourceCoverage, MaxFreshnessSources+1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture, _ := FixtureFor(SchemaToday)
			p := fixture.(*TodayProjection)
			for _, row := range p.Freshness {
				p.Coverage = append(p.Coverage, SourceCoverage{SourceFreshness: row})
			}
			if err := p.Validate(); err != nil {
				t.Fatal(err)
			}
			tc.change(p)
			if err := p.Validate(); err == nil {
				t.Fatal("oversize shared field accepted")
			}
		})
	}
}
