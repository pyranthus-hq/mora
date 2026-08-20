package mora

import (
	briefartifactpkg "github.com/pyranthus-hq/mora/internal/briefartifact"
	briefstatepkg "github.com/pyranthus-hq/mora/internal/briefstate"
	"os"
	"strings"
	"time"
)

const briefHashSchemaVersion = briefstatepkg.HashSchemaVersion

type briefSnapshot = briefstatepkg.Snapshot
type briefDeltaItem = briefstatepkg.DeltaItem
type briefDelta = briefstatepkg.Delta

func briefPath(cfg Config, key string) string                { return briefstatepkg.Path(cfg, key) }
func loadBriefSnapshot(cfg Config, key string) briefSnapshot { return briefstatepkg.Load(cfg, key) }
func saveBriefSnapshot(cfg Config, s briefSnapshot, now time.Time) error {
	return briefstatepkg.Save(cfg, s, now)
}
func classify(s briefSnapshot, mems []Memory, now time.Time) briefDelta {
	return briefstatepkg.Classify(s, mems, now)
}
func acquireBriefLock(cfg Config) (func(), error) { return briefstatepkg.AcquireLock(cfg) }

// ---------------------------------------------------------------------------
// READ SIDE — the dated brief artifact resolver (Phase 16, D16-1/D16-2)
//
// This is the sibling of artifact.go's WRITE side (briefArtifactPath /
// writeBriefArtifact). artifact.go writes <VaultDir>/briefs/<UTC-date>-brief.md;
// these helpers read the freshest such file (or generate one on demand). The
// resolver is PURE, LOCAL-ONLY, and WATERMARK-SAFE by construction:
//
//   - every date/freshness decision flows from the INJECTED now (never a fresh
//     time.Now() inside a helper), so the tests are deterministic and the UTC
//     scheme matches briefArtifactPath exactly;
//   - it makes ZERO network calls (no net/* import, no connector fetch/sync
//     function) — it only reads the vault from disk + computes (D16-2 / T-16-01);
//   - it NEVER advances or mutates the Phase-12 watermark — every generate-path
//     buildDigest forces advance:false and it never calls saveBriefSnapshot /
//     acquireBriefLock (D16-2 / T-16-02). The resolver is the read half; the
//     watermark store above is the write half, and the two never cross.
// ---------------------------------------------------------------------------

// briefFallbackWindowHours is the fixed, watermark-INDEPENDENT look-back used
// ONLY when the DELTA preview surfaces zero items (e.g. the scheduled --advance
// job already consumed today's delta). Re-building in WINDOW mode over the last
// 24h guarantees a session-start brief is never useless, while passing
// advance:false keeps it strictly read-only (no watermark mutation). It is a
// fixed constant so the fallback choice is fully deterministic and honest
// (T-16-04). 24h mirrors the digest's own digestDefaultHours framing.
const briefFallbackWindowHours = 24

func latestBriefPath(cfg Config, _ time.Time) (string, time.Time, bool) {
	return briefartifactpkg.Latest(cfg.VaultDir)
}
func briefIsFresh(dated, now time.Time) bool { return briefartifactpkg.IsFresh(dated, now) }

// resolveBrief returns the LOCAL brief: the freshest persisted brief read
// VERBATIM when one exists for today's or yesterday's UTC day, otherwise a brief
// GENERATED on demand from the already-ingested local vault. It NEVER syncs,
// NEVER persists, and NEVER advances the watermark — zero egress + read-only
// (D16-1/D16-2). Returns (body, generated, err) where generated reports whether
// the body was freshly built (true) vs read from disk (false).
//
// Read path: if latestBriefPath finds a file AND briefIsFresh, os.ReadFile it and
// return its bytes VERBATIM (no re-render that could drift from what the
// scheduled job persisted — the printed-verbatim trust boundary).
//
// Generate path: build the DELTA digest first (briefOpts{advance:false} — the
// canonical "what changed since the last brief"). If that surfaces ZERO items
// across all sections (the scheduled --advance job already consumed today's
// delta), RE-build in WINDOW mode (a fixed briefFallbackWindowHours look-back,
// watermark-independent) so the session-start brief is never useless yet stays
// honest (T-16-04). BOTH builds force advance:false so neither mutates the
// watermark. The result is renderDigest at the same budget the WRITE side
// persists, so a generated brief is byte-shaped like a read one.
func resolveBrief(cfg Config, now time.Time, opts briefOpts) (string, bool, error) {
	// Only the GLOBAL (unfiltered) brief uses the persisted cache — the disk file is
	// the unfiltered brief, so a filtered request must bypass it and generate fresh
	// (§3), or it would masquerade as "nothing's up".
	if !opts.filtered() && !opts.forceRegen {
		if path, dated, ok := latestBriefPath(cfg, now); ok && briefIsFresh(dated, now) {
			body, err := os.ReadFile(path)
			if err != nil {
				return "", false, err
			}
			return reconcileCachedBriefHealth(cfg, now, string(body)), false, nil
		}
	}

	d, err := filteredBriefDigest(cfg, now, opts)
	if err != nil {
		return "", false, err
	}
	return renderDigest(d, contextDefaultTokens(cfg)*charsPerToken), true, nil
}

// healthBannerLinePrefix is the fixed prefix healthBannerLine/healthBannerFrom
// always emit — the marker reconcileCachedBriefHealth uses to find (and
// remove) an EMBEDDED banner line without re-parsing the whole render.
const healthBannerLinePrefix = "🔴 MORA HEALTH:"
const healthBannerYellowLinePrefix = "🟡 MORA HEALTH:"

func isHealthBannerLine(s string) bool {
	return strings.HasPrefix(s, healthBannerLinePrefix) || strings.HasPrefix(s, healthBannerYellowLinePrefix)
}

// reconcileCachedBriefHealth closes the cached-brief hole (Packet C2, the live
// HEALTH-02 failure): resolveBrief's cache-read path returns a persisted file
// VERBATIM, but the file may be hours or days old — a source that died AFTER
// it was written must still redden THIS session's brief, and a source that
// RECOVERED since must not keep showing yesterday's red line forever. Fixed at
// the READ path, never the write path (the persisted file itself stays
// byte-stable — the "printed-verbatim trust boundary" is deliberate): re-derive
// the CURRENT banner from cfg/now and prepend or strip it on top of the cached
// body's existing (possibly stale, possibly absent) banner line.
//
// A no-op (returns body unchanged) whenever the current banner and the
// embedded one already agree — including the common "both empty" case — so a
// healthy fixture's cached brief stays byte-identical (the T0 budget fixture
// and every existing byte-stability test depend on this).
func reconcileCachedBriefHealth(cfg Config, now time.Time, body string) string {
	banner := healthBannerFrom(healthOf(cfg, now))

	header := body
	remainder := ""
	if idx := strings.IndexByte(body, '\n'); idx >= 0 {
		header, remainder = body[:idx], body[idx+1:]
	}

	embedded := ""
	rest := remainder
	if isHealthBannerLine(remainder) {
		if idx := strings.IndexByte(remainder, '\n'); idx >= 0 {
			embedded, rest = remainder[:idx], remainder[idx+1:]
		} else {
			embedded, rest = remainder, ""
		}
	}

	if banner == embedded {
		return body // already current — including the common healthy/no-banner case.
	}
	if banner == "" {
		return header + "\n" + rest // health recovered since the file was written: strip it.
	}
	return header + "\n" + banner + "\n" + rest
}

// filteredBriefDigest factors resolveBrief's generate path: a DELTA preview with a
// fixed 24h WINDOW fallback when the delta is empty, forwarding the full filter set
// and forcing advance:false on both builds. Shared by resolveBrief (human + --json),
// the `mora brief --envelope` cited-items prompt, and the MCP `brief` tool, so all
// three cite the SAME items. Read-only; never mutates the Phase-12 watermark.
func filteredBriefDigest(cfg Config, now time.Time, opts briefOpts) (Digest, error) {
	d, err := buildDigest(cfg, now, briefOpts{
		advance: false, perSourceCap: opts.perSourceCap,
		source: opts.source, entityIDSet: opts.entityIDSet, scope: opts.scope, sinceDays: opts.sinceDays,
	})
	if err != nil {
		return Digest{}, err
	}
	if briefSurfacedItemCount(d) == 0 {
		fallback, fallbackErr := buildDigest(cfg, now, briefOpts{
			advance: false, sinceHours: briefFallbackWindowHours, perSourceCap: opts.perSourceCap,
			source: opts.source, entityIDSet: opts.entityIDSet, scope: opts.scope, sinceDays: opts.sinceDays,
		})
		if fallbackErr != nil {
			return Digest{}, fallbackErr
		}
		d = preserveBriefFallbackEmptyExplanation(d, fallback)
	}
	return d, nil
}

// preserveBriefFallbackEmptyExplanation keeps the reason from the first DELTA
// pass when the brief's internal 24-hour WINDOW fallback is also empty. The
// fallback is not a caller-requested since_hours mode, so its window-specific
// reason must not replace a true steady-state "no changes since last brief."
func preserveBriefFallbackEmptyExplanation(delta, fallback Digest) Digest {
	if briefSurfacedItemCount(fallback) == 0 && delta.EmptyExplanation != "" {
		fallback.EmptyExplanation = delta.EmptyExplanation
	}
	return fallback
}

// briefSurfacedItemCount sums len(section.Items) across a digest — the
// "is the delta empty" predicate resolveBrief uses to decide whether to fall back
// to the 24h window. A digest with zero surfaced items everywhere is the
// post-advance common case the fallback exists to rescue (T-16-04).
func briefSurfacedItemCount(d Digest) int {
	// The Urgent shelf counts too (issue #62): its items are lifted OUT of the sections,
	// so ignoring them would treat an urgent-only delta as empty and fall back to the
	// 24h window — dropping the very shelf the delta produced.
	n := len(d.Urgent)
	for _, s := range d.Sections {
		n += len(s.Items)
	}
	return n
}
