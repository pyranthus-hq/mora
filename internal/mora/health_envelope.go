package mora

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"
)

// health_envelope.go — Packet C1 (Gate 2 PR 2): the BOUNDED per-call health
// envelope every hot MCP/CLI/HTTP surface carries. The rich arrays (Health.Sources,
// Health.Index, Health.Producers) stay where the byte budget already allows them —
// doctor --json, the digest payload, and MeetingBrief — per Finding 8's budget
// constraint. Every other surface gets THIS small typed object instead: the
// aggregate state, the two arms individually (so a caller can tell "a source is
// stale" from "the index is dirty" without re-deriving it from the rich arrays),
// and the one already-capped banner line (healthBannerLineCap, health_banner.go).

const (
	compactSourceCap      = 3
	compactSourceBytesCap = 80
)

// compactHealth is the bounded envelope: worst-of-3 aggregate state plus the
// source/index arms as plain state strings (never the full arrays) plus the
// one-line banner. Producers are folded into State already (aggregateHealthState).
// Includes a token-cheap, bounded per-source map projection (PerSource, SourcesOmitted).
type compactHealth struct {
	State          string            `json:"state"`                     // healthy | degraded | unhealthy
	Sources        string            `json:"sources"`                   // worst source state: fresh|stale|failed|never
	PerSource      map[string]string `json:"per_source,omitempty"`      // fixed-cap bounded per-source state map
	SourcesOmitted int               `json:"sources_omitted,omitempty"` // count of omitted sources when capped
	Index          string            `json:"index"`                     // index state: fresh|dirty|degraded|failed|never
	Banner         string            `json:"banner,omitempty"`          // capped one-line alarm, "" when healthy
}

// compactHealthFrom projects the rich Health kernel down to the bounded
// envelope. Pure (no cfg/now): a render path never calls time.Now(), and a
// caller that already built a Health (digest/meetingbrief build time) can reuse
// it here without recomputing sourceHealthAll/indexHealthOf.
func compactHealthFrom(h Health) compactHealth {
	sources := healthFresh
	if w := worstSource(h.Sources); w != nil {
		sources = w.State
	}

	var perSource map[string]string
	var omitted int
	if len(h.Sources) > 0 {
		srcs := make([]sourceHealth, len(h.Sources))
		copy(srcs, h.Sources)
		sort.Slice(srcs, func(i, j int) bool {
			ri, rj := healthStateRank(srcs[i].State), healthStateRank(srcs[j].State)
			if ri != rj {
				return ri < rj
			}
			if srcs[i].AgeHours != srcs[j].AgeHours {
				return srcs[i].AgeHours > srcs[j].AgeHours
			}
			return srcs[i].Key < srcs[j].Key
		})

		candidate := make(map[string]string)
		for _, s := range srcs {
			if len(candidate) >= compactSourceCap {
				omitted++
				continue
			}
			candidate[s.Key] = s.State
			b, err := json.Marshal(candidate)
			if err != nil || len(b) > compactSourceBytesCap {
				delete(candidate, s.Key)
				omitted++
				continue
			}
		}
		if len(candidate) > 0 {
			perSource = candidate
		}
	}

	return compactHealth{
		State:          h.State,
		Sources:        sources,
		PerSource:      perSource,
		SourcesOmitted: omitted,
		Index:          h.Index.State,
		Banner:         healthBannerFrom(h),
	}
}

// healthFromParts assembles a Health from arms a caller already computed at
// build time (digest/meetingbrief pin Sources/Index/Producers once, at buildDigest/
// buildMeetingBriefFromEvent time) and derives State the SAME way healthOf
// does — aggregateHealthState, never hand-set — so a caller that skips this
// and builds a bare Health{Sources:…, Index:…} literal doesn't silently ship
// an empty .State (compactHealthFrom trusts .State is already aggregated).
//
// PR 4 threads the producer arm through: with it hardcoded empty, the compact
// envelope every MCP/meeting_prep caller reads would be blind to a dead producer
// while the CLI banner (which reads the arms directly) reported it — the two
// surfaces would disagree about the same vault.
func healthFromParts(sources []sourceHealth, idx indexHealth, producers []producerHealth) Health {
	h := Health{Sources: sources, Index: idx, Producers: producers}
	h.State = aggregateHealthState(h)
	return h
}

// compactHealthOf computes the compact envelope directly from cfg/now — the
// convenience entry point for MCP/CLI/HTTP call sites that have no already-built
// Health to reuse (mirrors healthBanner's role for the aggregate banner).
func compactHealthOf(cfg Config, now time.Time) compactHealth {
	return compactHealthFrom(healthOf(cfg, now))
}

// printHealthBannerLine writes the current banner as its own leading line, or
// nothing when the vault is healthy — the CLI TEXT-render half of C3's surface
// contract ("exposes typed health in payload OR renders the banner"). Scoped to
// plain-text output; `--json` CLI output is out of scope for this packet (only
// the MCP transport takes the {payload,health} envelope break, C4/Open Q1) —
// each call site is responsible for calling this ONLY on its non-JSON branch.
func printHealthBannerLine(w io.Writer, cfg Config, now time.Time) {
	if banner := healthBanner(cfg, now); banner != "" {
		fmt.Fprintln(w, banner)
	}
}
