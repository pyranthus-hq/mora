package activity

import (
	"reflect"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestAuthoredWriteOptIn(t *testing.T) {
	now := mustTime(t, "2026-09-10T12:00:00Z")
	opts := Options{IncludeAuthoredWrites: true}
	for _, tc := range []struct {
		name string
		m    memory.Memory
		want bool
	}{
		{"native", memory.Memory{ID: "mem_note", Source: "manual", CreatedAt: now.Format(time.RFC3339)}, true},
		{"mirror", memory.Memory{ID: "src_note", Tags: []string{"filesystem", "codex-memory"}, CreatedAt: now.Format(time.RFC3339)}, true},
		{"document", memory.Memory{ID: "src_doc", Tags: []string{"filesystem", "docs"}, CreatedAt: now.Format(time.RFC3339)}, false},
		{"connector missing event", memory.Memory{ID: "gmail_thread/a", Provider: "gmail", CreatedAt: now.Format(time.RFC3339), Provenance: "authored"}, false},
		{"unsupported connector", memory.Memory{ID: "github:issue", Provider: "github", CreatedAt: now.Format(time.RFC3339)}, false},
		{"malformed", memory.Memory{ID: "mem_bad", CreatedAt: "bad"}, false},
		{"zero", memory.Memory{ID: "mem_zero", CreatedAt: time.Time{}.Format(time.RFC3339)}, false},
		{"future", memory.Memory{ID: "mem_future", CreatedAt: now.Add(time.Second).Format(time.RFC3339)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Derive(tc.m, now); got.EventAt != nil || got.Eligible {
				t.Fatalf("default gained authored fallback: %+v", got)
			}
			if got := DeriveWithOptions(tc.m, now, Options{}); !reflect.DeepEqual(got, Derive(tc.m, now)) {
				t.Fatal("zero options changed derivation")
			}
			got := DeriveWithOptions(tc.m, now, opts)
			if got.Eligible != tc.want {
				t.Fatalf("projection=%+v want eligible %v", got, tc.want)
			}
			if tc.want && (got.EventSource != EventSourceAuthoredWrite || got.EventAt.Format(time.RFC3339) != tc.m.CreatedAt || got.Participation != nil || got.Automated != nil) {
				t.Fatalf("invented connector facts: %+v", got)
			}
		})
	}
}

func TestAuthoredWriteRangeAndConnectorEvidence(t *testing.T) {
	now := mustTime(t, "2026-09-10T12:00:00Z")
	from := now.Add(-24 * time.Hour)
	items := []memory.Memory{
		{ID: "mem_old", CreatedAt: from.Add(-time.Second).Format(time.RFC3339)},
		{ID: "mem_b", CreatedAt: from.Format(time.RFC3339)},
		{ID: "mem_a", CreatedAt: from.Format(time.RFC3339)},
		{ID: "calendar_event/one", Provider: "calendar", CreatedAt: now.Format(time.RFC3339), Meta: map[string]any{"occurred_at": now.Add(-time.Hour).Format(time.RFC3339)}},
	}
	got := SelectRangeWithOptions(items, from, now, Options{IncludeAuthoredWrites: true})
	if len(got) != 3 || got[0].Memory.ID != "calendar_event/one" || got[1].Memory.ID != "mem_a" || got[2].Memory.ID != "mem_b" {
		t.Fatalf("range/order=%+v", got)
	}
	if !reflect.DeepEqual(got[0].Projection, Derive(items[3], now)) {
		t.Fatal("connector derivation changed")
	}
	// Authored identity overrides forged connector metadata only in the opt-in path.
	forged := memory.Memory{ID: "mem_forged", Provider: "calendar", CreatedAt: from.Format(time.RFC3339), Meta: items[3].Meta}
	if p := DeriveWithOptions(forged, now, Options{IncludeAuthoredWrites: true}); p.EventSource != EventSourceAuthoredWrite || !p.EventAt.Equal(from) {
		t.Fatalf("forged source won: %+v", p)
	}
}
