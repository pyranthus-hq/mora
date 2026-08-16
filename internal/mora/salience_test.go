package mora

import (
	"strconv"
)

// ---- Task 1: sat / channelScale / recencyDecay / salienceMicros ----

// ---- Task 2: scoreSalience / aggregatePersonSalience / metaMessageCount ----

// imsgMemory builds a one-conversation iMessage memory between the given handle and
// a fixed "me" handle, carrying a message_count string as the real connectors emit it.
func imsgMemory(id, handle, name, occurred string, msgCount int) Memory {
	return Memory{
		ID:        id,
		Type:      "imessage",
		CreatedAt: occurred,
		Meta: map[string]any{
			"occurred_at":   occurred,
			"message_count": strconv.Itoa(msgCount),
			"participants": []any{
				map[string]any{"handle": handle, "name": name},
				map[string]any{"handle": "+10000000000", "name": "Me"},
			},
		},
	}
}

// emailMemory builds a single email thread from->to with a message_count string.
func emailMemory(id, from, to, occurred string, msgCount int) Memory {
	return Memory{
		ID:        id,
		Type:      "email",
		CreatedAt: occurred,
		Meta: map[string]any{
			"occurred_at":   occurred,
			"message_count": strconv.Itoa(msgCount),
			"from":          []any{from},
			"to":            []any{to},
		},
	}
}

// eventMemory builds a calendar event with an organizer + one attendee.
func eventMemory(id, organizer, attendee, occurred string) Memory {
	return Memory{
		ID:        id,
		Type:      "event",
		CreatedAt: occurred,
		Meta: map[string]any{
			"occurred_at": occurred,
			"organizer":   organizer,
			"attendees":   []any{attendee},
		},
	}
}

// TestSalienceAggregateSaturationCapsRunaway is the core property this phase relies on:
// per-channel saturation BOUNDS a high-volume single channel so it cannot run away. A
// 50,000-message texter must NOT outscore a 5,000-message texter (both saturate the
// imsg channel to ~1.0) — proving the volume signal is capped, not linear. (Pre-
// saturation, naive message-count would make 50k score 10× the 5k contact.)

// TestSalienceAggregateMultiChannelWins is the inversion fix's positive case: once
// per-channel volumes saturate, a MULTI-CHANNEL human outranks a single-channel one of
// equal saturated volume, because Breadth (0.05) + the clamped multi-channel Volume
// break the tie in the human's favor. This is what stops a one-channel noise source
// from sitting above a real, multi-surface relationship.

// TestSalienceAggregateMessageCountVsFallback proves Volume reads the real count when
// present and degrades to the 1-per-memory fallback when absent — a 100-message thread
// outranks a count-less thread for the same channel/recency.

// TestSalienceAggregateGarbageMeta feeds tampered/garbage Meta and asserts a finite,
// defined micros (no NaN/Inf/panic) — the T-14-01 mitigation.
