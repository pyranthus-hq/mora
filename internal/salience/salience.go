package salience

// Package-private salience model (Phase 14, D14-1..D14-4). This file is the SINGLE
// source of truth for person ranking: the math kernel that BOTH the entity-graph
// person ranking (14-03 buildGraph) and the digest ordering (14-05) consume, so the
// model lives in exactly one place and rebuilds stay byte-identical.
//
// Determinism is load-bearing. The frozen output (salience_micros, an int64) feeds a
// byte-identical-rebuild invariant, so NOTHING here may consult a wall clock
// (time.Now), depend on map-iteration order in any returned value, or produce
// NaN/Inf. Recency is VAULT-RELATIVE (decayed against the vault's max lastSeen, passed
// in as an argument) precisely so the score has zero clock dependence. Every function
// here is pure: same inputs -> same output, no I/O, no globals mutated.
//
// Model (D14-1, verbatim from the 2026-06-04 design memo):
//
//	S(p)  = HumanGate(p) × Recency(p) × Core(p)
//	Core  = 0.70·Volume + 0.15·Reciprocity + 0.10·ChannelAffinity + 0.05·Breadth
//
// HumanGate is person→1, service→0 (a service scores exactly 0 micros and is excluded
// from the People overview while staying searchable). Reciprocity contributes 0 in v1
// (no `self` config — one wrong self-guess poisons every directed edge) but the 0.15
// term is kept LITERAL (weights are NOT renormalized) so a future `self` slots in
// cleanly. With every component in [0,1], Core is bounded by 0.70+0.15+0.10+0.05 = 1.0,
// so S ∈ [0,1] and salience_micros ∈ [0, 1_000_000].

import (
	"math"

	"github.com/pyranthus-hq/mora/internal/memory"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Core weights (D14-1) — LITERAL, never renormalized. wReciprocity is kept even
// though Reciprocity evaluates to 0 in v1, so a future `self` config slots in without
// rebalancing the other terms.
const (
	wVolume          = 0.70
	wReciprocity     = 0.15
	wChannelAffinity = 0.10
	wBreadth         = 0.05
)

// Per-channel saturation scales (D14-2). A channel's volume is saturated against its
// own scale so a high-volume texter does not re-invert the ranking: imessage carries
// far more messages per relationship than email or calendar, so its scale is larger.
const (
	imsgSatScale  = 250.0
	emailSatScale = 12.0
	eventSatScale = 6.0
)

// breadthSatScale saturates the count of DISTINCT channels a person spans (D14-1
// Breadth term). A 3-channel scale means 1 channel → low, 3 channels → ~1.
const breadthSatScale = 3.0

// Recency decay (D14-3): vault-relative half-life and floor. A person seen at the
// vault's most-recent instant scores 1.0; the score halves every recencyHalfLifeDays
// and never drops below recencyFloor (so a long-dormant-but-real contact is not
// zeroed out of the ranking entirely).
const (
	recencyHalfLifeDays = 180.0
	recencyFloor        = 0.40
)

// salienceMicrosScale freezes the [0,1] score into an integer sort key (D14-4):
// salience_micros = round(S · 1e6). The integer is what makes rebuilds byte-identical.
const salienceMicrosScale = 1e6

// sat applies the per-channel saturation curve sat(x, scale) = min(1, log1p(x)/log1p(scale)).
// It is monotonic non-decreasing in x, equals 0 at x==0, reaches 1 at x==scale, and
// clamps to 1 for x>scale. scale<=0 is guarded to 0 (avoids a 0/0 from log1p(0)).
func sat(x, scale float64) float64 {
	if scale <= 0 {
		return 0
	}
	v := math.Log1p(x) / math.Log1p(scale)
	if v > 1 {
		return 1
	}
	if v < 0 {
		return 0
	}
	return v
}

// channelScale maps a memory's Type to its saturation scale (D14-2). Any unknown type
// falls back to the email scale (12) — the conservative middle between iMessage's
// high-volume relationships and calendar's sparse ones.
func channelScale(memType string) float64 {
	switch memType {
	case "imessage":
		return imsgSatScale
	case "email":
		return emailSatScale
	case "event":
		return eventSatScale
	default:
		return emailSatScale
	}
}

// recencyDecay computes the vault-RELATIVE recency multiplier for a person's lastSeen
// against the vault's max lastSeen (D14-3). Both instants are RFC3339 strings passed
// in as arguments — by construction there is NO time.Now() here, so the frozen score
// has zero wall-clock dependence and is byte-identical across rebuilds.
//
// decay = max(recencyFloor, 2^(-Δdays/recencyHalfLifeDays)), where Δdays is the gap
// from lastSeen to vaultMax. An empty or unparseable lastSeen/vaultMax degrades to the
// floor (a defined value, no panic) — the documented edge for memories that never
// carried a parseable timestamp.
func recencyDecay(lastSeen, vaultMax string) float64 {
	ls, err1 := time.Parse(time.RFC3339, lastSeen)
	vm, err2 := time.Parse(time.RFC3339, vaultMax)
	if err1 != nil || err2 != nil {
		return recencyFloor
	}
	deltaDays := vm.Sub(ls).Hours() / 24
	if deltaDays <= 0 {
		// lastSeen at or after vaultMax -> no decay.
		return 1
	}
	decay := math.Exp2(-deltaDays / recencyHalfLifeDays)
	if decay < recencyFloor {
		return recencyFloor
	}
	if decay > 1 {
		return 1
	}
	return decay
}

// salienceMicros freezes a [0,1] score into the integer sort key (D14-4):
// round(s · 1e6) as int64, via math.Round (round-half-away-from-zero). For s ∈ [0,1]
// the result is in [0, 1_000_000].
func salienceMicros(s float64) int64 {
	return int64(math.Round(s * salienceMicrosScale))
}

// salienceInput is the accumulated, per-person evidence the score is computed from.
// perChannelVolume holds the raw fanout-weighted message volume per channel (Type);
// the per-channel saturation is applied INSIDE scoreSalience so the raw sums stay
// addable during accumulation. channels is the set of distinct channels the person
// appears on (drives Breadth). lastSeen is the person's most-recent validFrom instant.
type salienceInput struct {
	kind             string // "person" | "service" (HumanGate input)
	perChannelVolume map[string]float64
	channels         map[string]bool
	lastSeen         string
}

// scoreSalience assembles S = HumanGate × Recency × Core and freezes it to micros.
//
//	HumanGate       = 1 for a person, 0 for a service (a service scores exactly 0).
//	Volume          = min(1, Σ_ch sat(perChannelVolume[ch], channelScale(ch))).
//	                  Per-channel saturation defeats the high-volume-texter inversion;
//	                  summing across channels (then clamping to 1) lets a multi-channel
//	                  person rank at/above a single-channel one while keeping Volume∈[0,1].
//	ChannelAffinity = max_ch sat(perChannelVolume[ch], scale) — the person's strongest channel.
//	Breadth         = sat(distinctChannels, 3) — 1 channel low, 3 channels ~1.
//	Reciprocity     = 0 in v1 (the 0.15 term is kept literal; see D14-1).
//
// All three volume-derived components are in [0,1], so Core ≤ 1.0 and S ∈ [0,1].
// Deterministic: iterates the channel SET in sorted order (no map-iteration leak),
// though the math (sum/max) is order-independent regardless.
func scoreSalience(in salienceInput, vaultMax string) int64 {
	humanGate := 1.0
	if in.kind != "person" {
		humanGate = 0
	}
	if humanGate == 0 {
		return 0 // short-circuit: a service is exactly 0 regardless of volume/recency.
	}

	// Sorted channel keys -> determinism by construction (sum/max are commutative, but
	// we never want a returned value to depend on map-iteration order).
	chans := make([]string, 0, len(in.perChannelVolume))
	for ch := range in.perChannelVolume {
		chans = append(chans, ch)
	}
	sort.Strings(chans)

	volumeSum := 0.0
	channelAffinity := 0.0
	for _, ch := range chans {
		s := sat(in.perChannelVolume[ch], channelScale(ch))
		volumeSum += s
		if s > channelAffinity {
			channelAffinity = s
		}
	}
	volume := volumeSum
	if volume > 1 {
		volume = 1 // clamp Σ per-channel sats to keep Volume ∈ [0,1].
	}

	breadth := sat(float64(len(in.channels)), breadthSatScale)
	reciprocity := 0.0 // v1: no `self` config (D14-1); term kept literal.

	core := wVolume*volume + wReciprocity*reciprocity + wChannelAffinity*channelAffinity + wBreadth*breadth
	recency := recencyDecay(in.lastSeen, vaultMax)
	s := humanGate * recency * core
	return salienceMicros(s)
}

// metaMessageCount reads a per-memory message count from Meta["message_count"]. Both
// connectors emit it as a quoted JSON STRING (gmail.go:92 fmt.Sprintf, imessage/map.go:141
// strconv.Itoa, both committed in a8269ef), so after the JSON round-trip the value is a
// Go string. A missing/empty/unparseable value — or one < 1 — falls back to 1. The
// fallback is the documented edge for OLDER filesystem memories that predate the
// connector capture; today's gmail/imessage memories carry a real count.
func metaMessageCount(m memory.Memory) int {
	if m.Meta == nil {
		return 1
	}
	s, ok := m.Meta["message_count"].(string)
	if !ok {
		return 1
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// Input is the public accumulated evidence shape for the pure salience scorer.
type Input struct {
	Kind             string
	PerChannelVolume map[string]float64
	Channels         map[string]bool
	LastSeen         string
}

func Saturate(x, scale float64) float64              { return sat(x, scale) }
func ChannelScale(memoryType string) float64         { return channelScale(memoryType) }
func RecencyDecay(lastSeen, vaultMax string) float64 { return recencyDecay(lastSeen, vaultMax) }
func Micros(score float64) int64                     { return salienceMicros(score) }
func Score(in Input, vaultMax string) int64 {
	return scoreSalience(salienceInput{kind: in.Kind, perChannelVolume: in.PerChannelVolume, channels: in.Channels, lastSeen: in.LastSeen}, vaultMax)
}
func MessageCount(m memory.Memory) int { return metaMessageCount(m) }
