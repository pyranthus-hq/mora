package mora

import (
	"fmt"
	"time"
)

// health_banner.go — the ONE-line aggregate banner (Gate 2, B3). Gate 1's banner
// covered only sources; this covers all three arms (sources, index, producers) but
// stays EXACTLY one line and the FIRST content line — four tests and the digest
// budget frame pin that (Landmine 2). healthBannerFrom replaces
// healthBannerFromSources at the render sites; the old function stays a thin
// sources-only adapter for callers that have not yet been given the index arm.

// healthBannerLineCap bounds the rendered banner to one line that fits even the
// tightest MCP token ceiling (delete_memory 500, double-counted). The JSON health
// payload keeps the raw, uncapped detail; only the rendered banner caps.
const healthBannerLineCap = 240

// bannerRank orders every arm's states into ONE worst-first ordering (B3):
// failed > never > dirty > degraded > stale. Lower is worse.
func bannerRank(state string) int {
	switch state {
	case healthFailed: // == idxFailed / prodFailed
		return 0
	case healthNever: // == idxNever / prodNever
		return 1
	case idxDirty:
		return 2
	case idxDegraded:
		return 3
	case healthStale: // == prodStale
		return 4
	default:
		return 5
	}
}

// healthBannerFrom renders the single worst arm across sources, index and
// producers as one capped line, or "" when everything is fresh. Pure over the
// snapshotted Health (no cfg/now), so a render path never calls time.Now().
func healthBannerFrom(h Health) string {
	bestRank := 6
	bestAge := -1
	best := ""

	consider := func(rank, age int, line string) {
		if line == "" {
			return
		}
		if rank < bestRank || (rank == bestRank && age > bestAge) {
			bestRank, bestAge, best = rank, age, line
		}
	}

	if w := worstSource(h.Sources); w != nil {
		consider(bannerRank(w.State), w.AgeHours, healthBannerLine(*w))
	}
	if line := indexBannerLine(h.Index); line != "" {
		consider(bannerRank(h.Index.State), h.Index.PendingOps, line)
	}
	if w := worstProducer(h.Producers); w != nil {
		consider(bannerRank(w.State), w.AgeHours, producerBannerLine(*w))
	}

	return capBannerLine(best)
}

// indexBannerLine renders the index arm's alarm line, or "" when fresh.
func indexBannerLine(idx indexHealth) string {
	switch idx.State {
	case idxFresh, "":
		return ""
	case idxNever:
		return "🔴 MORA HEALTH: search index has never been built. Run: mora index rebuild"
	case idxFailed:
		if idx.Blocked {
			return "🔴 MORA HEALTH: search index rebuild is BLOCKED (vault identity mismatch). Run: mora doctor"
		}
		detail := "search index FAILED"
		if idx.LastError != "" {
			detail += " — " + sanitizeHealthError(idx.LastError)
		}
		return "🔴 MORA HEALTH: " + detail + ". Run: mora doctor"
	case idxDegraded:
		return fmt.Sprintf("🔴 MORA HEALTH: search index is DEGRADED — built with %s, config requests %s. Run: mora doctor",
			idx.Embedder.Model, idx.Embedder.Configured)
	case idxDirty:
		if idx.PendingOps > 0 {
			since := bannerClockOf(idx.DirtySince)
			return fmt.Sprintf("🔴 MORA HEALTH: search index is DIRTY — %d vault %s not indexed%s. Run: mora index rebuild",
				idx.PendingOps, plural(idx.PendingOps, "write"), since)
		}
		return "🔴 MORA HEALTH: search index is DIRTY — graph projection is lagging a write. Run: mora index rebuild"
	default:
		return "🔴 MORA HEALTH: search index is " + idx.State + ". Run: mora doctor"
	}
}

// bannerClockOf formats a stored RFC3339 DirtySince as " since HH:MM" (UTC, so the
// render is deterministic and clock-free), or "" if unparseable.
func bannerClockOf(dirtySince string) string {
	if dirtySince == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, dirtySince)
	if err != nil {
		return ""
	}
	return " since " + t.UTC().Format("15:04")
}

// worstProducer / producerBannerLine are the producer arm's half — always
// empty in PR 1 (no producer ledger yet), populated by Packet E / PR 4.
func worstProducer(ps []producerHealth) *producerHealth {
	var worst *producerHealth
	worstRank := 0
	for i := range ps {
		p := &ps[i]
		if p.State == prodFresh {
			continue
		}
		rank := bannerRank(p.State)
		if worst == nil || rank < worstRank || (rank == worstRank && p.AgeHours > worst.AgeHours) {
			worst = p
			worstRank = rank
		}
	}
	return worst
}

func producerBannerLine(p producerHealth) string {
	if p.State == prodNever {
		return fmt.Sprintf("🔴 MORA HEALTH: %s has never been produced. Run: mora doctor", p.Name)
	}
	return fmt.Sprintf("🔴 MORA HEALTH: %s has not been produced for %dh. Run: mora doctor", p.Name, p.AgeHours)
}

// capBannerLine enforces the one-line byte budget (a raw driver error or a long
// path could otherwise blow the MCP ceiling / the digest budget frame).
func capBannerLine(s string) string {
	if r := []rune(s); len(r) > healthBannerLineCap {
		return string(r[:healthBannerLineCap-1]) + "…"
	}
	return s
}
