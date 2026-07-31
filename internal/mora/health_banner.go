package mora

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
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

// sourceBannerRank orders source states into worst-first ordering.
// Red alarms (source/index issues or corrupt producer ledger) outrank yellow producer warnings.
// Lower is worse.
func sourceBannerRank(state string) int {
	switch state {
	case healthFailed:
		return 0
	case healthNever:
		return 1
	case healthStale:
		return 4
	default:
		return 10
	}
}

func indexBannerRank(state string) int {
	switch state {
	case idxFailed:
		return 0
	case idxNever:
		return 1
	case idxDirty:
		return 2
	case idxDegraded:
		return 3
	default:
		return 10
	}
}

func producerBannerRank(p producerHealth) int {
	if p.Subject == producerHealthSubjectLedger {
		return 0 // corrupt producer ledger is an uncomputable RED state
	}
	switch p.State {
	case prodFailed:
		return 5
	case prodNever:
		return 6
	case prodStale:
		return 7
	default:
		return 10
	}
}

// healthBannerFrom renders the single worst arm across sources, index and
// producers as one capped line, or "" when everything is fresh. Pure over the
// snapshotted Health (no cfg/now), so a render path never calls time.Now().
func healthBannerFrom(h Health) string {
	bestRank := 99
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
		consider(sourceBannerRank(w.State), w.AgeHours, healthBannerLine(*w))
	}
	if line := indexBannerLine(h.Index); line != "" {
		consider(indexBannerRank(h.Index.State), h.Index.PendingOps, line)
	}
	if w := worstShareIndex(h.Index.Shares); w != nil {
		consider(indexBannerRank(w.State), w.PendingOps, shareIndexBannerLine(*w))
	}
	if w := worstProducer(h.Producers); w != nil {
		consider(producerBannerRank(*w), w.AgeHours, producerBannerLine(*w))
	}

	return capBannerLine(best)
}

// worstShareIndex returns the worst non-fresh subscription index arm in h.Index.Shares.
func worstShareIndex(shares []indexHealth) *indexHealth {
	var worst *indexHealth
	worstRank := 99
	for i := range shares {
		s := &shares[i]
		if s.State == idxFresh {
			continue
		}
		rank := indexBannerRank(s.State)
		if worst == nil || rank < worstRank {
			worst = s
			worstRank = rank
		}
	}
	return worst
}

// shareIndexBannerLine renders a subscription index alarm line.
func shareIndexBannerLine(idx indexHealth) string {
	switch idx.State {
	case idxFresh, "":
		return ""
	case idxNever:
		return "🔴 MORA HEALTH: subscription index has never been built. Run: mora doctor"
	case idxFailed:
		detail := "subscription index FAILED"
		if idx.LastError != "" {
			detail += " — " + sanitizeHealthError(idx.LastError)
		}
		return fmt.Sprintf("🔴 MORA HEALTH: %s. Run: mora doctor", detail)
	case idxDegraded:
		return "🔴 MORA HEALTH: subscription index is DEGRADED. Run: mora doctor"
	case idxDirty:
		return "🔴 MORA HEALTH: subscription index is DIRTY. Run: mora doctor"
	default:
		return fmt.Sprintf("🔴 MORA HEALTH: subscription index is %s. Run: mora doctor", idx.State)
	}
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

// worstProducer / producerBannerLine are the producer arm's half.
func worstProducer(ps []producerHealth) *producerHealth {
	var worst *producerHealth
	worstRank := 99
	for i := range ps {
		p := &ps[i]
		if p.State == prodFresh {
			continue
		}
		rank := producerBannerRank(*p)
		if worst == nil || rank < worstRank || (rank == worstRank && p.AgeHours > worst.AgeHours) {
			worst = p
			worstRank = rank
		}
	}
	return worst
}

func producerBannerLine(p producerHealth) string {
	if p.Subject == producerHealthSubjectLedger {
		detail := "producer ledger unreadable"
		if p.LastError != "" {
			detail += " — " + sanitizeHealthError(p.LastError)
		}
		return fmt.Sprintf("🔴 MORA HEALTH: %s. Run: mora doctor", detail)
	}
	if p.State == prodNever {
		return fmt.Sprintf("🟡 MORA HEALTH: %s has never been produced. Run: mora doctor", p.Name)
	}
	return fmt.Sprintf("🟡 MORA HEALTH: %s has not been produced for %dh. Run: mora doctor", p.Name, p.AgeHours)
}

// capBannerLine enforces the one-line byte budget (a raw driver error or a long
// path could otherwise blow the MCP ceiling / the digest budget frame). Output
// is guaranteed to be valid UTF-8 and <= healthBannerLineCap bytes.
func capBannerLine(s string) string {
	// Producer identities and driver errors are durable external input. Replace
	// malformed byte sequences before applying the cap so even a short corrupt
	// value cannot leak invalid UTF-8 onto MCP/JSON or terminal surfaces.
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		default:
			return r
		}
	}, strings.ToValidUTF8(s, "�"))
	if len(s) <= healthBannerLineCap {
		return s
	}
	ellipsis := "…"
	maxBytes := healthBannerLineCap - len(ellipsis)
	end := 0
	for end < len(s) {
		_, size := utf8.DecodeRuneInString(s[end:])
		if end+size > maxBytes {
			break
		}
		end += size
	}
	return s[:end] + ellipsis
}
