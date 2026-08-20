package health

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pyranthus-hq/mora/internal/operation"
)

// health_banner.go — the ONE-line aggregate banner (Gate 2, B3). Gate 1's banner
// covered only sources; this covers all three arms (sources, index, producers) but
// stays EXACTLY one line and the FIRST content line — four tests and the digest
// budget frame pin that (Landmine 2). BannerAll replaces
// healthBannerFromSources at the render sites; the old function stays a thin
// sources-only adapter for callers that have not yet been given the index arm.

// BannerLineCap bounds the rendered banner to one line that fits even the
// tightest MCP token ceiling (delete_memory 500, double-counted). The JSON health
// payload keeps the raw, uncapped detail; only the rendered banner caps.
const BannerLineCap = 240

// sourceBannerRank orders source states into worst-first ordering.
// Red alarms (source/index issues or corrupt producer ledger) outrank yellow producer warnings.
// Lower is worse.
func sourceBannerRank(state string) int {
	switch state {
	case Failed:
		return 0
	case Never:
		return 1
	case Stale:
		return 4
	default:
		return 10
	}
}

func indexBannerRank(state string) int {
	switch state {
	case IndexFailed:
		return 0
	case IndexNever:
		return 1
	case IndexDirty:
		return 2
	case IndexDegraded:
		return 3
	default:
		return 10
	}
}

func producerBannerRank(p Producer) int {
	if p.Subject == ProducerSubjectLedger {
		return 0 // corrupt producer ledger is an uncomputable RED state
	}
	switch p.State {
	case ProducerFailed:
		return 5
	case ProducerNever:
		return 6
	case ProducerStale:
		return 7
	default:
		return 10
	}
}

// BannerAll renders the single worst arm across sources, index and
// producers as one capped line, or "" when everything is fresh. Pure over the
// snapshotted Health (no cfg/now), so a render path never calls time.Now().
func BannerAll(h Health) string {
	bestRank := 99
	bestAge := -1
	best := ""

	consider := func(rank, age int, line string) {
		if rank < bestRank || (rank == bestRank && age > bestAge) {
			bestRank, bestAge, best = rank, age, line
		}
	}

	if w := Worst(h.Sources); w != nil {
		consider(sourceBannerRank(w.State), w.AgeHours, BannerLine(*w))
	}
	if line := indexBannerLineWithActivity(h.Index, h.Activities); line != "" {
		consider(indexBannerRank(h.Index.State), h.Index.PendingOps, line)
	}
	if w := worstShareIndex(h.Index.Shares); w != nil {
		consider(indexBannerRank(w.State), w.PendingOps, shareIndexBannerLine(*w))
	}
	if w := worstProducer(h.Producers); w != nil {
		consider(producerBannerRank(*w), w.AgeHours, producerBannerLine(*w))
	}
	for _, a := range h.Activities {
		switch a.State {
		case operation.Failed:
			consider(0, 0, fmt.Sprintf("🔴 MORA HEALTH: %s operation FAILED (%s). Run: mora doctor", strings.ReplaceAll(string(a.Kind), "_", " "), a.FailureCode))
		case operation.Stalled:
			consider(1, 0, fmt.Sprintf("🔴 MORA HEALTH: %s operation STALLED (phase %s). Run: mora doctor", strings.ReplaceAll(string(a.Kind), "_", " "), operation.SanitizePhase(a.Phase)))
		}
	}

	return CapBannerLine(best)
}

// worstShareIndex returns the worst non-fresh subscription index arm in h.Index.Shares.
func worstShareIndex(shares []Index) *Index {
	var worst *Index
	worstRank := 99
	for i := range shares {
		s := &shares[i]
		if s.State == IndexFresh {
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
func shareIndexBannerLine(idx Index) string {
	switch idx.State {
	case IndexFresh, "":
		return ""
	case IndexNever:
		return "🔴 MORA HEALTH: subscription index has never been built. Run: mora doctor"
	case IndexFailed:
		detail := "subscription index FAILED"
		if idx.LastError != "" {
			detail += " — " + SanitizeError(idx.LastError)
		}
		return fmt.Sprintf("🔴 MORA HEALTH: %s. Run: mora doctor", detail)
	case IndexDegraded:
		return "🔴 MORA HEALTH: subscription index is DEGRADED. Run: mora doctor"
	case IndexDirty:
		return "🔴 MORA HEALTH: subscription index is DIRTY. Run: mora doctor"
	default:
		return fmt.Sprintf("🔴 MORA HEALTH: subscription index is %s. Run: mora doctor", idx.State)
	}
}

// indexBannerLineWithActivity renders the index arm's alarm line, or "" when fresh.
func indexBannerLineWithActivity(idx Index, activities []operation.Activity) string {
	switch idx.State {
	case IndexFresh, "":
		return ""
	case IndexNever:
		return "🔴 MORA HEALTH: search index has never been built. Run: mora index rebuild"
	case IndexFailed:
		if idx.Blocked {
			return "🔴 MORA HEALTH: search index rebuild is BLOCKED (vault identity mismatch). Run: mora doctor"
		}
		detail := "search index FAILED"
		if idx.LastError != "" {
			detail += " — " + SanitizeError(idx.LastError)
		}
		return "🔴 MORA HEALTH: " + detail + ". Run: mora doctor"
	case IndexDegraded:
		return fmt.Sprintf("🔴 MORA HEALTH: search index is DEGRADED — built with %s, config requests %s. Run: mora doctor",
			idx.Embedder.Model, idx.Embedder.Configured)
	case IndexDirty:
		for i := len(activities) - 1; i >= 0; i-- {
			a := activities[i]
			if a.State != operation.Running {
				continue
			}
			kind := strings.ReplaceAll(string(a.Kind), "_", " ")
			snapshot := "the last committed snapshot"
			if idx.IndexedAt != "" {
				snapshot = "the last committed snapshot from " + idx.IndexedAt
			}
			return fmt.Sprintf("🔴 MORA HEALTH: refresh in progress (%s, phase %s); serving %s. Current index remains DIRTY. Run: mora doctor",
				kind, operation.SanitizePhase(a.Phase), snapshot)
		}
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
func worstProducer(ps []Producer) *Producer {
	var worst *Producer
	worstRank := 99
	for i := range ps {
		p := &ps[i]
		if p.State == ProducerFresh {
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

func producerBannerLine(p Producer) string {
	if p.Subject == ProducerSubjectLedger {
		detail := "producer ledger unreadable"
		if p.LastError != "" {
			detail += " — " + SanitizeError(p.LastError)
		}
		return fmt.Sprintf("🔴 MORA HEALTH: %s. Run: mora doctor", detail)
	}
	if p.State == ProducerNever {
		return fmt.Sprintf("🟡 MORA HEALTH: %s has never been produced. Run: mora doctor", p.Name)
	}
	return fmt.Sprintf("🟡 MORA HEALTH: %s has not been produced for %dh. Run: mora doctor", p.Name, p.AgeHours)
}

// CapBannerLine enforces the one-line byte budget (a raw driver error or a long
// path could otherwise blow the MCP ceiling / the digest budget frame). Output
// is guaranteed to be valid UTF-8 and <= BannerLineCap bytes.
func CapBannerLine(s string) string {
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
	if len(s) <= BannerLineCap {
		return s
	}
	ellipsis := "…"
	maxBytes := BannerLineCap - len(ellipsis)
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

func plural(n int, unit string) string {
	if n == 1 {
		return unit
	}
	return unit + "s"
}
