package mora

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"testing"
)

// readAliases returns the aliases JSON array for an entity id.
func readAliases(t *testing.T, cfg Config, id string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath(cfg)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var aj string
	if err := db.QueryRow(`SELECT aliases FROM entities WHERE id = ?`, id).Scan(&aj); err != nil {
		t.Fatalf("entity %q not found: %v", id, err)
	}
	var out []string
	_ = json.Unmarshal([]byte(aj), &out)
	sort.Strings(out)
	return out
}

func hasEdge(edges map[string]edgeVals, key string) bool {
	_, ok := edges[key]
	return ok
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestGraphPersonEdgesFromGmail proves email participants become person entities
// with PARTICIPATED_IN edges, senders get EMAILED edges to each recipient, and
// display-name aliases accrete.
func TestGraphPersonEdgesFromGmail(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/t1", Scope: "personal", Type: "email", Title: "Re: demo",
		CreatedAt: "2026-05-01T00:00:00Z", LastSynced: "2026-05-02T00:00:00Z", Text: "hi",
		Meta: map[string]any{
			"from":  []string{"neil@example.com"},
			"to":    []string{"adit@x.com", "bob@y.com"},
			"names": map[string]string{"neil@example.com": "Neil Patel"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	ents := readEntities(t, cfg)
	for _, id := range []string{"person:neil@example.com", "person:adit@x.com", "person:bob@y.com"} {
		e, ok := ents[id]
		if !ok {
			t.Fatalf("missing person entity %q (have %d entities)", id, len(ents))
		}
		if e.kind != "person" {
			t.Fatalf("%s kind = %q, want person", id, e.kind)
		}
	}
	if d := ents["person:neil@example.com"].display; d != "Neil Patel" {
		t.Fatalf("display_name = %q, want resolved name", d)
	}
	if al := readAliases(t, cfg, "person:neil@example.com"); !contains(al, "Neil Patel") || !contains(al, "neil@example.com") {
		t.Fatalf("aliases = %v, want both address and name", al)
	}

	edges := readEdges(t, cfg)
	hub := "memory:gmail_thread/t1"
	if !hasEdge(edges, hub+"|PARTICIPATED_IN|person:neil@example.com|gmail_thread/t1") {
		t.Fatal("missing PARTICIPATED_IN edge for sender")
	}
	if !hasEdge(edges, hub+"|PARTICIPATED_IN|person:adit@x.com|gmail_thread/t1") {
		t.Fatal("missing PARTICIPATED_IN edge for recipient")
	}
	if !hasEdge(edges, "person:neil@example.com|EMAILED|person:adit@x.com|gmail_thread/t1") {
		t.Fatal("missing EMAILED edge sender->recipient (adit)")
	}
	if !hasEdge(edges, "person:neil@example.com|EMAILED|person:bob@y.com|gmail_thread/t1") {
		t.Fatal("missing EMAILED edge sender->recipient (bob)")
	}
}

// TestGraphCalendarAttended proves calendar attendees/organizer use ATTENDED, not
// PARTICIPATED_IN.
func TestGraphCalendarAttended(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	if err := writeMemory(cfg, Memory{
		ID: "calendar_event/e1", Scope: "personal", Type: "event", Title: "Standup",
		CreatedAt: "2026-05-01T09:00:00Z", Text: "x",
		Meta: map[string]any{
			"attendees": []string{"adit@x.com"},
			"organizer": "boss@corp.com",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	edges := readEdges(t, cfg)
	hub := "memory:calendar_event/e1"
	if !hasEdge(edges, hub+"|ATTENDED|person:adit@x.com|calendar_event/e1") {
		t.Fatal("missing ATTENDED edge for attendee")
	}
	if !hasEdge(edges, hub+"|ATTENDED|person:boss@corp.com|calendar_event/e1") {
		t.Fatal("missing ATTENDED edge for organizer")
	}
}

// TestGraphIMessagePeople proves iMessage handle↔name pairs become person
// entities keyed by handle with the resolved name as alias/display.
func TestGraphIMessagePeople(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	if err := writeMemory(cfg, Memory{
		ID: "imessage_chat/c1", Scope: "personal", Type: "imessage", Title: "chat",
		CreatedAt: "2026-05-01T00:00:00Z", Text: "x",
		Meta: map[string]any{
			"participants": []map[string]string{{"handle": "+15551234567", "name": "Neil Patel"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	ents := readEntities(t, cfg)
	e, ok := ents["person:+15551234567"]
	if !ok {
		t.Fatalf("missing iMessage person entity (have %d)", len(ents))
	}
	if e.kind != "person" || e.display != "Neil Patel" {
		t.Fatalf("entity = %+v, want person/Neil Patel", e)
	}
	edges := readEdges(t, cfg)
	if !hasEdge(edges, "memory:imessage_chat/c1|PARTICIPATED_IN|person:+15551234567|imessage_chat/c1") {
		t.Fatal("missing PARTICIPATED_IN edge for iMessage participant")
	}
}

// TestIssue219UnnamedPhoneIsNotAPerson keeps a source-native phone node as a
// structural person while preventing the number itself from entering public People.
func TestIssue219UnnamedPhoneIsNotAPerson(t *testing.T) {
	if got := publicEntityKind("person:+15551234567", "person", "+15551234567"); got != "artifact" {
		t.Fatalf("unnamed public phone kind = %q, want artifact", got)
	}
	if got := publicEntityKind("person:+15551234567", "person", "Neil Patel"); got != "person" {
		t.Fatalf("named public phone kind = %q, want person", got)
	}
}

// TestGraphPersonSelfMerge proves the same address across two memories collapses to
// one canonical person row with accreted aliases and a 2-memory mention_count.
func TestGraphPersonSelfMerge(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	// First memory: address only, no display name.
	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/a", Scope: "personal", Type: "email", Title: "A",
		CreatedAt: "2026-05-01T00:00:00Z", Text: "x",
		Meta: map[string]any{"from": []string{"neil@example.com"}, "to": []string{"adit@x.com"}},
	}); err != nil {
		t.Fatal(err)
	}
	// Second memory: same address WITH a display name.
	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/b", Scope: "personal", Type: "email", Title: "B",
		CreatedAt: "2026-05-03T00:00:00Z", Text: "y",
		Meta: map[string]any{
			"from": []string{"neil@example.com"}, "to": []string{"adit@x.com"},
			"names": map[string]string{"neil@example.com": "Neil Patel"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}

	ents := readEntities(t, cfg)
	e, ok := ents["person:neil@example.com"]
	if !ok {
		t.Fatal("merged person entity missing")
	}
	if e.mentionCount != 2 {
		t.Fatalf("mention_count = %d, want 2 (one row, two memories)", e.mentionCount)
	}
	if al := readAliases(t, cfg, "person:neil@example.com"); !contains(al, "Neil Patel") {
		t.Fatalf("aliases = %v, want the name accreted from the second memory", al)
	}
}

// TestGraphPersonTombstone proves a tombstoned memory's person edges are
// invalidated and the person drops out of the live entity list when it has no
// other live evidence.
func TestGraphPersonTombstone(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/dead", Scope: "personal", Type: "email", Title: "gone",
		CreatedAt: "2026-04-01T00:00:00Z", DeletedAt: "2026-04-05T00:00:00Z", Text: "x",
		Meta: map[string]any{"from": []string{"ghost@example.com"}, "to": []string{"adit@x.com"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	edges := readEdges(t, cfg)
	ev, ok := edges["memory:gmail_thread/dead|PARTICIPATED_IN|person:ghost@example.com|gmail_thread/dead"]
	if !ok {
		t.Fatal("expected the (invalidated) edge to still exist as provenance")
	}
	if !ev.invalidatedAt.Valid || ev.invalidatedAt.String != "2026-04-05T00:00:00Z" {
		t.Fatalf("invalidated_at = %+v, want deleted_at", ev.invalidatedAt)
	}
	// ghost has no live evidence -> not in the live list.
	res, err := graphListEntities(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res {
		if e.Name == "ghost@example.com" || e.Kind == "person" && contains([]string{e.Name}, "ghost@example.com") {
			t.Fatalf("tombstoned-only person leaked into live list: %+v", e)
		}
	}
}

// TestGraphFanoutCap proves a memory with more than the fan-out cap participants
// caps its person edges and emits a warning (no silent truncation).
func TestGraphFanoutCap(t *testing.T) {
	var to []string
	for i := 0; i < maxParticipantFanout+20; i++ {
		to = append(to, fmt.Sprintf("p%03d@x.com", i))
	}
	m := Memory{
		ID: "gmail_thread/big", Type: "email", Title: "blast", CreatedAt: "2026-05-01T00:00:00Z",
		Meta: map[string]any{"from": []any{"sender@x.com"}, "to": toAnySlice(to)},
	}
	ents, edges, warnings := buildGraph([]Memory{m})

	// Count PARTICIPATED_IN person edges from the hub.
	n := 0
	for _, e := range edges {
		if e.Rel == "PARTICIPATED_IN" {
			n++
		}
	}
	if n > maxParticipantFanout {
		t.Fatalf("fan-out not capped: %d PARTICIPATED_IN edges > cap %d", n, maxParticipantFanout)
	}
	if len(warnings) == 0 {
		t.Fatal("expected a fan-out cap warning (honesty: no silent truncation)")
	}
	_ = ents
}

// TestPersonCoOccurrence proves the query-time self-join surfaces people who
// shared a thread/event, and excludes those who never co-occurred.
func TestPersonCoOccurrence(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	// neil + adit + bob share thread t1; carol is only in an unrelated thread t2.
	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/t1", Scope: "personal", Type: "email", Title: "T1",
		CreatedAt: "2026-05-01T00:00:00Z", Text: "x",
		Meta: map[string]any{"from": []string{"neil@example.com"}, "to": []string{"adit@x.com", "bob@y.com"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeMemory(cfg, Memory{
		ID: "gmail_thread/t2", Scope: "personal", Type: "email", Title: "T2",
		CreatedAt: "2026-05-02T00:00:00Z", Text: "y",
		Meta: map[string]any{"from": []string{"carol@z.com"}, "to": []string{"adit@x.com"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath(cfg)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	co, err := coOccurringPeople(ctx, db, "person:neil@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(co, "person:adit@x.com") || !contains(co, "person:bob@y.com") {
		t.Fatalf("neil should co-occur with adit and bob: %v", co)
	}
	if contains(co, "person:carol@z.com") {
		t.Fatalf("neil never shared a thread with carol: %v", co)
	}
	if contains(co, "person:neil@example.com") {
		t.Fatalf("co-occurrence must exclude the person itself: %v", co)
	}
}

func toAnySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
