package mora

import (
	"fmt"
	"github.com/pyranthus-hq/mora/internal/companion"
	"strings"
	"testing"
)

func TestCompanionTodayReadingSnippets(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	for _, tc := range []struct{ kind, body, want string }{
		{"imessage", "Me: [attachment]\nMe: [attachment]\nAbhi: Gym start now?", "Gym start now?"},
		{"whatsapp", "Me: [attachment]\nAbhi: Gym start now?\nAbhi: [attachment]\nMe: See you", "Gym start now?"},
		{"gmail", "[image: tracking pixel]\n\nhttps://example.test/track?id=123\n\n12345\n\nCan we meet tomorrow?\n\nSecond paragraph.", "Can we meet tomorrow?"},
		{"imessage", "Me: Only mine\nAbhi: [attachment] [attachment]", ""},
		{"gmail", "… ... " + strings.Repeat("界", 221), strings.Repeat("界", 220)},
	} {
		t.Run(tc.kind+fmt.Sprint(len(tc.body)), func(t *testing.T) {
			m := Memory{ID: "snippet-" + tc.kind, Type: tc.kind, Provider: tc.kind, Text: tc.body, Title: "Synthetic conversation", CreatedAt: "2026-09-11T10:00:00Z"}
			if tc.kind != "gmail" {
				var rows []map[string]any
				offset := 0
				for i, line := range strings.Split(tc.body, "\n") {
					sender, text, _ := strings.Cut(line, ": ")
					start := offset + len(sender) + 2
					rows = append(rows, map[string]any{"evidence_ref": fmt.Sprintf("%s#%d", m.ID, i), "at": m.CreatedAt, "from_me": sender == "Me", "sender": sender, "block_start": start, "block_end": start + len(text)})
					offset += len(line) + 1
				}
				m.Meta = map[string]any{"message_evidence": rows}
			}
			if err := writeMemory(cfg, m); err != nil {
				t.Fatal(err)
			}
			d := Digest{Sections: []DigestSection{{Items: []DigestItem{{ID: m.ID, Source: tc.kind, Title: m.Title, Snippet: tc.body}}}}}
			items := companionTodayCandidates(d, cfg)
			if len(items) != 1 {
				t.Fatalf("items = %+v", items)
			}
			if got := companionReadingSnippet(m, tc.body); got != tc.want {
				t.Fatalf("reading snippet = %q, want %q", got, tc.want)
			}
			wantEvidence := companionText(tc.want, companion.MaxSnippetBytes)
			if got := items[0].Evidence[0].Snippet; got != wantEvidence {
				t.Fatalf("snippet = %q, want %q", got, wantEvidence)
			}
		})
	}
}

func TestCompanionTodayAndHealthSourceLabels(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	enabled := true
	if err := saveSources(cfg, []Source{{Name: "gmail", Type: "gmail", Email: "alex@example.test", Enabled: &enabled}, {Name: "imessage", Type: "imessage", Enabled: &enabled}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ verb, field string }{{"today", "coverage"}, {"health", "sources"}} {
		doc := decodeWire(t, run(t, "companion", tc.verb, "--json"), "mora.companion."+tc.verb)
		rows := doc[tc.field].([]any)
		labels := map[string]string{}
		for _, raw := range rows {
			row := raw.(map[string]any)
			labels[row["key"].(string)] = row["label"].(string)
		}
		if labels["gmail"] != "Gmail · alex@example.test" || labels["imessage"] != "Messages" {
			t.Fatalf("%s labels = %v", tc.verb, labels)
		}
		if tc.verb == "health" {
			if _, ok := doc["reads_in_flight"].([]any); !ok {
				t.Fatal("reads_in_flight missing")
			}
		}
	}
	projection := companion.NewTodayProjection()
	projection.Items = []companion.TodayItem{{ID: "itm_test", Evidence: []companion.Evidence{{Snippet: "Gym start now?"}}}}
	doc := companionTodayDocument(cfg, projection)
	if len(doc.Items) != 1 || doc.Items[0].Snippet != "Gym start now?" {
		t.Fatalf("desktop items = %+v", doc.Items)
	}
}

func TestCompanionMultibyteSnippetValidatesSharedSchema(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{strings.Repeat("界", 221), strings.Repeat("界", 170)},
		{strings.Repeat("é", 221), strings.Repeat("é", 220)},
		{"a" + strings.Repeat("🙂", 221), "a" + strings.Repeat("🙂", 127)},
	} {
		snippet := companionReadingSnippet(Memory{Provider: "gmail", Text: tc.text}, "")
		item, ok := companionTodayItem(DigestItem{ID: "multibyte", Source: "gmail", Title: "Message", Snippet: snippet}, companion.ItemChanged)
		if !ok {
			t.Fatal("missing item")
		}
		p := companion.NewTodayProjection()
		p.GeneratedAt = "2026-09-11T10:00:00Z"
		p.Health = companion.HealthSummary{State: companion.HealthHealthy, Policy: companion.PolicyReadonly}
		p.Items = []companion.TodayItem{item}
		if item.Snippet != tc.want || len(item.Snippet) > companion.MaxSnippetBytes || item.Snippet != item.Evidence[0].Snippet {
			t.Fatalf("unbounded or divergent snippet: %q", item.Snippet)
		}
		if _, err := companion.Marshal(&p); err != nil {
			t.Fatal(err)
		}
	}
}
