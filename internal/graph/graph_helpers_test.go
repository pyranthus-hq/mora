package graph

import (
	"reflect"
	"testing"
)

func TestGr_PureGraphHelperEdges(t *testing.T) {
	if got := metaStrings([]any{"a", 7, "", "b"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("metaStrings []any = %#v", got)
	}
	if got := metaStrings("solo"); !reflect.DeepEqual(got, []string{"solo"}) {
		t.Fatalf("metaStrings string = %#v", got)
	}
	if got := metaStrings(42); got != nil {
		t.Fatalf("metaStrings unknown = %#v, want nil", got)
	}

	pairs := metaPairs([]any{map[string]any{"handle": "+1555", "name": "Texter"}, map[string]string{"handle": "h2", "name": "Name2"}})
	if len(pairs) != 2 || pairs[0].handle != "+1555" || pairs[1].name != "Name2" {
		t.Fatalf("metaPairs mixed maps = %#v", pairs)
	}
	pairs = metaPairs([]map[string]string{{"handle": "h3", "name": "Name3"}})
	if len(pairs) != 1 || pairs[0].handle != "h3" {
		t.Fatalf("metaPairs typed maps = %#v", pairs)
	}

	parts, senders, recipients, rel := personRefs(Memory{
		Type: "event",
		Meta: map[string]any{
			"organizer":    "host@example.com",
			"attendees":    []any{"guest@example.com", ""},
			"participants": []map[string]string{{"handle": "+15551212", "name": "Phone Friend"}},
			"names":        map[string]any{"host@example.com": "Host Human", "guest@example.com": "Guest Human"},
		},
	})
	if rel != "ATTENDED" || !reflect.DeepEqual(senders, []string{"person:host@example.com"}) || len(recipients) != 0 {
		t.Fatalf("event refs rel/senders/recipients = %s %#v %#v", rel, senders, recipients)
	}
	if len(parts) != 3 || parts[0].id != "person:+15551212" || parts[2].name != "Host Human" {
		t.Fatalf("event participants sorted/resolved = %#v", parts)
	}

	capped := capParticipants([]personRef{
		{id: "person:a"}, {id: "person:b"}, {id: "person:c"},
	}, map[string]bool{"person:a": true, "person:b": true, "person:c": true}, 2)
	if !reflect.DeepEqual(capped, []personRef{{id: "person:a"}, {id: "person:b"}}) {
		t.Fatalf("pathological sender cap = %#v", capped)
	}

	uf := newUnionFind([]string{"a", "b"})
	uf.union("a", "b")
	uf.union("a", "b")
	if uf.find("a") != uf.find("b") {
		t.Fatalf("union should leave a and b in the same set: %#v", uf.parent)
	}

	parts, senders, _, _ = personRefs(Memory{
		Type: "email",
		Meta: map[string]any{
			"from":         []any{"dup@example.com", ""},
			"participants": []map[string]string{{"handle": "", "name": "Ignored"}, {"handle": "dup@example.com", "name": "Duplicate Name"}},
		},
	})
	if !reflect.DeepEqual(senders, []string{"person:dup@example.com"}) || len(parts) != 1 || parts[0].name != "Duplicate Name" {
		t.Fatalf("duplicate participant should backfill missing name: parts=%#v senders=%#v", parts, senders)
	}

	// raw non-@ iMessage handle stays a person and is NOT dropped (precision-first:
	// never lose a real person to an over-broad shape rule).
	parts, _, _, _ = personRefs(Memory{
		Type: "imessage",
		Meta: map[string]any{
			"from": []any{"iMessage;-;weird"},
		},
	})
	if len(parts) != 1 || parts[0].id != "person:imessage;-;weird" {
		t.Fatalf("weird non-@ handle should not be dropped, got: %#v", parts)
	}

	// GitHub notification artifact shapes are structural boilerplate, not entities
	// (issue #70) -> dropped at personRefs so they're never minted as persons.
	parts, _, _, _ = personRefs(Memory{
		Type: "email",
		Meta: map[string]any{
			"from": []any{"Push"},
		},
	})
	if len(parts) != 0 {
		t.Fatalf("artifact shape 'Push' should be dropped from personRefs, got: %#v", parts)
	}

	// repo-slug owner/name is NOT dropped at personRefs — it flows through so it can
	// be TYPED "repo" downstream (issue #70) rather than vanishing from the graph.
	parts, _, _, _ = personRefs(Memory{
		Type: "email",
		Meta: map[string]any{
			"from": []any{"pyranthus-hq/mora"},
		},
	})
	if len(parts) != 1 || parts[0].id != "person:pyranthus-hq/mora" {
		t.Fatalf("repo-slug should flow through personRefs to be typed, got: %#v", parts)
	}
	names := trustedPersonNames(&personAgg{aliases: map[string]bool{"alice@example.com": true, "+15551212": true, "Alice": true, "Alice Example": true}})
	if !reflect.DeepEqual(names, []string{"alice example"}) {
		t.Fatalf("trustedPersonNames=%#v", names)
	}
	uf2 := newUnionFind([]string{"person:a", "person:z"})
	uf2.union("person:z", "person:a")
	if uf2.find("person:a") != uf2.find("person:z") {
		t.Fatalf("reverse union should merge roots: %#v", uf2.parent)
	}
}

func TestGr_BuildGraphGazetteerMentionsAndRewriteEdges(t *testing.T) {
	entities, edges, warnings := buildGraph([]Memory{
		{
			ID:        "m0",
			Type:      "note",
			Title:     "Older mention",
			Text:      "Alice Example was mentioned before metadata arrived.",
			CreatedAt: "2025-12-31T00:00:00Z",
		},
		{
			ID:        "m1",
			Type:      "email",
			Title:     "Intro",
			Text:      "Alice introduced herself.",
			CreatedAt: "2026-01-01T00:00:00Z",
			Meta: map[string]any{
				"from":  []string{"alice@example.com"},
				"names": map[string]string{"alice@example.com": "Alice Example"},
			},
		},
		{
			ID:        "m2",
			Type:      "note",
			Title:     "Mention",
			Text:      "Follow up with Alice Example next week.",
			CreatedAt: "2026-01-02T00:00:00Z",
		},
	})
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}
	var alice graphEntity
	for _, e := range entities {
		if e.ID == "person:alice@example.com" {
			alice = e
		}
	}
	if alice.MentionCount != 3 || alice.FirstSeen != "2025-12-31T00:00:00Z" || alice.LastSeen != "2026-01-02T00:00:00Z" {
		t.Fatalf("gazetteer mention should extend Alice evidence bounds: %+v", alice)
	}
	grHasEdge := false
	for _, e := range edges {
		if e.Src == "memory:m2" && e.Rel == "MENTIONS" && e.Dst == "person:alice@example.com" && e.EvidenceID == "m2" {
			grHasEdge = true
		}
	}
	if !grHasEdge {
		t.Fatalf("body mention edge missing from edges: %#v", edges)
	}

	rewritten := rewritePersonEdges([]graphEdge{
		{Src: "person:a", Rel: "EMAILED", Dst: "person:b", EvidenceID: "m"},
		{Src: "person:a", Rel: "EMAILED", Dst: "person:b", EvidenceID: "m"},
		{Src: "person:b", Rel: "MENTIONS", Dst: "person:c", EvidenceID: "m2"},
		{Src: "person:b", Rel: "MENTIONS", Dst: "person:c", EvidenceID: "m2"},
	}, map[string]string{"person:b": "person:a", "person:c": "person:z"})
	if len(rewritten) != 1 {
		t.Fatalf("rewrite should drop self-loop and dedupe, got %#v", rewritten)
	}
	if rewritten[0].Src != "person:a" || rewritten[0].Dst != "person:z" {
		t.Fatalf("rewrite should map source and destination endpoints: %#v", rewritten)
	}
}
