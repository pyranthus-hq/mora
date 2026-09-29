package mora

import (
	"encoding/json"
	"testing"
	"time"
)

// boundsClock carries sub-second precision on purpose: briefClock is time.Now,
// so every real read does too, and a bound formatted with plain RFC3339 would
// silently drop it.
var boundsClock = time.Date(2026, 9, 10, 12, 0, 0, 482913741, time.UTC)

func boundsEvent(id, at string) Memory {
	return Memory{
		ID: id, Scope: "global", Type: "event", Title: id, Text: "fixture",
		Provider: "calendar", Source: "calendar", CreatedAt: at,
		Meta: map[string]any{"occurred_at": at},
	}
}

func pinBoundsClock(t *testing.T) {
	t.Helper()
	old := briefClock
	calls := 0
	briefClock = func() time.Time {
		calls++
		return boundsClock.Add(time.Duration(calls-1) * time.Second)
	}
	t.Cleanup(func() { briefClock = old })
}

// parseBound fails on anything that is not a full-precision instant. Parsing
// with RFC3339Nano and re-formatting catches a bound emitted at second
// precision, which is the defect PYR-83 exists to kill.
func parseBound(t *testing.T, label string, value any) time.Time {
	t.Helper()
	text, ok := value.(string)
	if !ok || text == "" {
		t.Fatalf("%s missing from receipt: %#v", label, value)
	}
	at, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		t.Fatalf("%s=%q is not RFC3339Nano: %v", label, text, err)
	}
	if at.UTC().Format(time.RFC3339Nano) != text {
		t.Fatalf("%s=%q is not the canonical RFC3339Nano form of %s", label, text, at)
	}
	return at
}

// TestListReceiptStatesTheWindowItApplied pins the two facts a consumer needs:
// the bounds are the ones the selection used, and the receipt is usable as the
// closed interval the kernel actually cut.
func TestListReceiptStatesTheWindowItApplied(t *testing.T) {
	for _, argument := range []string{"event_since_hours", "since_hours"} {
		t.Run(argument, func(t *testing.T) {
			// The newest row sits exactly on the inclusive upper bound, with nanos.
			atNow := boundsClock.UTC().Format(time.RFC3339Nano)
			cfg := seedRecencyVault(t,
				boundsEvent("on-the-bound", atNow),
				boundsEvent("inside", boundsClock.Add(-6*time.Hour).UTC().Format(time.RFC3339Nano)),
				boundsEvent("outside", boundsClock.Add(-48*time.Hour).UTC().Format(time.RFC3339Nano)))
			pinBoundsClock(t)

			result, err := mcpListMemory(testCtx(t), cfg, map[string]any{"source": "calendar", argument: 24, "limit": 200})
			if err != nil {
				t.Fatal(err)
			}
			out := result.(map[string]any)
			from := parseBound(t, "window_from", out["window_from"])
			to := parseBound(t, "window_to", out["window_to"])

			if !to.Equal(boundsClock) {
				t.Fatalf("window_to=%s is not the read instant %s", to, boundsClock)
			}
			if to.Sub(from) != 24*time.Hour {
				t.Fatalf("window span %s does not equal the applied event_since_hours", to.Sub(from))
			}
			if from.Nanosecond() != boundsClock.Nanosecond() {
				t.Fatalf("window_from lost sub-second precision: %s", from)
			}

			rows := out["memories"].([]Memory)
			if len(rows) != 2 || rows[0].ID != "on-the-bound" {
				t.Fatalf("unexpected selection: %+v", rows)
			}
			// The receipt must round-trip as a closed interval: every row the kernel
			// kept satisfies window_from <= event_at <= window_to. The row sitting on
			// the bound is the one a truncated window_to would push out.
			for _, row := range rows {
				at, err := time.Parse(time.RFC3339Nano, row.EventAt)
				if err != nil {
					t.Fatalf("row %s event_at=%q: %v", row.ID, row.EventAt, err)
				}
				if at.Before(from) || at.After(to) {
					t.Fatalf("row %s at %s falls outside the stated window [%s, %s]", row.ID, row.EventAt, from, to)
				}
			}
		})
	}
}

// TestListReceiptStatesBoundsForAnEmptyWindow: the bounds describe the read,
// not the rows, so an empty successful window still states them.
func TestListReceiptStatesBoundsForAnEmptyWindow(t *testing.T) {
	cfg := seedRecencyVault(t, boundsEvent("outside", boundsClock.Add(-48*time.Hour).UTC().Format(time.RFC3339Nano)))
	pinBoundsClock(t)

	result, err := mcpListMemory(testCtx(t), cfg, map[string]any{"source": "calendar", "event_since_hours": 1, "limit": 200})
	if err != nil {
		t.Fatal(err)
	}
	out := result.(map[string]any)
	if rows := out["memories"].([]Memory); len(rows) != 0 {
		t.Fatalf("expected an empty window: %+v", rows)
	}
	from := parseBound(t, "window_from", out["window_from"])
	to := parseBound(t, "window_to", out["window_to"])
	if to.Sub(from) != time.Hour || !to.Equal(boundsClock) {
		t.Fatalf("empty read stated the wrong window [%s, %s]", from, to)
	}
}

// TestListWithoutEventWindowStatesNoBounds: a read that applied no window must
// not gain keys. The bounds are present only when event_since_hours was.
func TestListWithoutEventWindowStatesNoBounds(t *testing.T) {
	cfg := seedRecencyVault(t, boundsEvent("inside", boundsClock.Add(-6*time.Hour).UTC().Format(time.RFC3339Nano)))
	pinBoundsClock(t)

	result, err := mcpListMemory(testCtx(t), cfg, map[string]any{"source": "calendar"})
	if err != nil {
		t.Fatal(err)
	}
	out := result.(map[string]any)
	for _, key := range []string{"window_from", "window_to"} {
		if _, ok := out[key]; ok {
			t.Fatalf("%s appeared on a read that applied no window", key)
		}
	}
}

// TestListCLIReceiptStatesOneWindowForEveryRow covers the published CLI
// receipt and the single clock read: two rows selected in one call cannot be
// measured against different bounds, because there is only one pair to state.
func TestListCLIReceiptStatesOneWindowForEveryRow(t *testing.T) {
	seedRecencyVault(t,
		boundsEvent("on-the-bound", boundsClock.UTC().Format(time.RFC3339Nano)),
		boundsEvent("inside", boundsClock.Add(-6*time.Hour).UTC().Format(time.RFC3339Nano)))
	pinBoundsClock(t)

	var receipt struct {
		Memories        []Memory `json:"memories"`
		EventSinceHours int      `json:"event_since_hours"`
		Order           string   `json:"order"`
		WindowFrom      string   `json:"window_from"`
		WindowTo        string   `json:"window_to"`
	}
	out := run(t, "list", "--source", "calendar", "--event-since-hours", "24", "--limit", "200", "--json")
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatalf("receipt=%q err=%v", out, err)
	}
	if receipt.EventSinceHours != 24 || receipt.Order != "source-event" {
		t.Fatalf("event window not echoed: %q", out)
	}
	from := parseBound(t, "window_from", receipt.WindowFrom)
	to := parseBound(t, "window_to", receipt.WindowTo)
	if !to.Equal(boundsClock) || to.Sub(from) != 24*time.Hour {
		t.Fatalf("CLI stated the wrong window [%s, %s]", from, to)
	}
	if len(receipt.Memories) != 2 {
		t.Fatalf("expected both rows: %q", out)
	}
	for _, row := range receipt.Memories {
		at, err := time.Parse(time.RFC3339Nano, row.EventAt)
		if err != nil {
			t.Fatalf("row %s event_at=%q: %v", row.ID, row.EventAt, err)
		}
		if at.Before(from) || at.After(to) {
			t.Fatalf("row %s at %s falls outside the stated window [%s, %s]", row.ID, row.EventAt, from, to)
		}
	}

	var plain map[string]any
	if err := json.Unmarshal([]byte(run(t, "list", "--source", "calendar", "--json")), &plain); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"window_from", "window_to"} {
		if _, ok := plain[key]; ok {
			t.Fatalf("%s appeared on the source-only CLI receipt", key)
		}
	}
}
