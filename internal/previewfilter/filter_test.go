package previewfilter

import (
	"github.com/pyranthus-hq/mora/internal/memory"
	"reflect"
	"testing"
	"time"
)

func filterMem(id, scope, from, createdAt string) memory.Memory {
	return memory.Memory{ID: id, Scope: scope, Type: "email", CreatedAt: createdAt,
		Meta: map[string]any{"from": []string{from}, "to": []string{"x@y.com"}}}
}

func idsOf(mems []memory.Memory) []string {
	out := make([]string, 0, len(mems))
	for _, m := range mems {
		out = append(out, m.ID)
	}
	return out
}

func TestFilterByInstance(t *testing.T) {
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	in := map[string][]memory.Memory{"gmail": {
		filterMem("m1", "personal", "riya@a.com", "2026-06-13T00:00:00Z"),
		filterMem("m2", "personal", "bob@z.com", "2026-06-13T00:00:00Z"),
		filterMem("m3", "project:acme", "riya@a.com", "2026-06-13T00:00:00Z"),
		filterMem("m4", "personal", "riya@a.com", "2026-05-01T00:00:00Z"), // 44d old
	}}
	riya := map[string]bool{"person:riya@a.com": true}

	cases := []struct {
		name string
		opts Options
		want []string
	}{
		{"identity (empty)", Options{}, []string{"m1", "m2", "m3", "m4"}},
		{"entity", Options{EntityIDs: riya}, []string{"m1", "m3", "m4"}},
		{"scope", Options{Scope: "project:acme"}, []string{"m3"}},
		{"since-days 7", Options{SinceDays: 7}, []string{"m1", "m2", "m3"}},
		{"entity AND since-days", Options{EntityIDs: riya, SinceDays: 7}, []string{"m1", "m3"}},
		{"P1-D negative since-days is a no-op", Options{SinceDays: -7}, []string{"m1", "m2", "m3", "m4"}},
	}
	for _, c := range cases {
		got := FilterByInstance(in, c.opts, now)
		if ids := idsOf(got["gmail"]); !reflect.DeepEqual(ids, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, ids, c.want)
		}
	}
	// Never mutates input.
	if len(in["gmail"]) != 4 {
		t.Errorf("FilterByInstance mutated its input: %d", len(in["gmail"]))
	}
}

func TestAliasIDSet(t *testing.T) {
	got := AliasIDSet("person:alex.owner+promos@gmail.com",
		[]string{"alex.owner@gmail.com", "alexowner@gmail.com", "alex.owner+promos@gmail.com", "Alex Owner"})
	want := map[string]bool{
		"person:alex.owner+promos@gmail.com": true,
		"person:alex.owner@gmail.com":        true,
		"person:alexowner@gmail.com":         true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AliasIDSet = %v, want %v", got, want)
	}
}

func TestAliasIDSetPhoneHandleAndEmptyAliases(t *testing.T) {
	got := AliasIDSet("person:+15551230000", []string{"+15551230000", "Mom"})
	if want := map[string]bool{"person:+15551230000": true}; !reflect.DeepEqual(got, want) {
		t.Errorf("phone handle: AliasIDSet = %v, want %v", got, want)
	}
	if got := AliasIDSet("person:riya@a.com", nil); !reflect.DeepEqual(got, map[string]bool{"person:riya@a.com": true}) {
		t.Errorf("nil aliases: AliasIDSet = %v, want just the canonical id", got)
	}
}

func TestMemoryMentionsEntity(t *testing.T) {
	// idSet = canonical (riya@a.com) ∪ a merged-away alias (riya@b.com).
	idSet := map[string]bool{"person:riya@a.com": true, "person:riya@b.com": true}
	cases := []struct {
		name string
		m    memory.Memory
		want bool
	}{
		{"sender", memory.Memory{Type: "email", Meta: map[string]any{"from": []string{"riya@a.com"}, "to": []string{"x@y.com"}}}, true},
		{"recipient", memory.Memory{Type: "email", Meta: map[string]any{"from": []string{"x@y.com"}, "to": []string{"riya@a.com"}}}, true},
		{"cc", memory.Memory{Type: "email", Meta: map[string]any{"from": []string{"x@y.com"}, "cc": []string{"riya@a.com"}}}, true},
		{"attendee", memory.Memory{Type: "event", Meta: map[string]any{"attendees": []string{"riya@a.com"}}}, true},
		{"participant", memory.Memory{Type: "imessage", Meta: map[string]any{"participants": []map[string]string{{"handle": "riya@a.com", "name": "Riya"}}}}, true},
		{"merged-away alias (P1-A bug pin)", memory.Memory{Type: "email", Meta: map[string]any{"from": []string{"riya@b.com"}}}, true},
		{"unrelated", memory.Memory{Type: "email", Meta: map[string]any{"from": []string{"bob@z.com"}, "to": []string{"x@y.com"}}}, false},
		{"empty meta", memory.Memory{Type: "email"}, false},
	}
	for _, c := range cases {
		if got := MentionsEntity(c.m, idSet); got != c.want {
			t.Errorf("%s: MentionsEntity = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestMemoryMentionsEntityEmptySetMatchesNothing(t *testing.T) {
	m := memory.Memory{Type: "email", Meta: map[string]any{"from": []string{"riya@a.com"}}}
	if MentionsEntity(m, map[string]bool{}) {
		t.Error("empty idSet should match nothing")
	}
}

func TestClampAndMatches(t *testing.T) {
	if ClampSinceDays(-1) != 0 || ClampSinceDays(3) != 3 {
		t.Fatal("clamp")
	}
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	m := memory.Memory{Scope: "x", CreatedAt: "bad"}
	if Matches(m, Options{Scope: "y"}, now) || Matches(m, Options{SinceDays: 1}, now) || Matches(m, Options{EntityIDs: map[string]bool{"person:none": true}}, now) {
		t.Fatal("mismatch admitted")
	}
	m.CreatedAt = "2026-01-10T00:00:00Z"
	if !Matches(m, Options{}, now) {
		t.Fatal("zero opts")
	}
}
