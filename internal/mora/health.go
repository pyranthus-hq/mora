package mora

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/pyranthus-hq/mora/internal/memory"
)

// health.go — Gate 1 (HEALTH-01..05): per-source freshness as a first-class
// doctor/brief signal, not just a digest heading nobody reads. sourceHealthAll
// is the single source of truth both `mora doctor` (checks + JSON `sources`)
// and the red banner (healthBanner / healthBannerFromSources) are built from —
// classifyState (digest.go) keeps its own three-state logic for the human
// digest headings, but the CRITICAL alarm threshold now lives here.

// Three-state-plus-never health classification, worst-first below. never and
// failed are deliberately distinct (unlike classifyState's merged
// "unavailable"): never means no successful sync has EVER happened; failed
// means a sync DID succeed at some point but the most recent attempt errored.
const (
	healthNever  = "never"
	healthFailed = "failed"
	healthStale  = "stale"
	healthFresh  = "fresh"
)

// Freshness thresholds by connector type — TIGHTER than digestStaleHours (48h,
// digest.go), which stays as-is for the human digest heading. This is the
// product-invalid alarm threshold (HEALTH-03): the network connectors are
// polled hourly and a 24h gap already means multiple missed cycles, while
// imessage/filesystem are local, slower-moving stores where 48h is still
// honest. Keep both constants; do not silently fork one from the other.
const (
	sourceHealthGoogleThreshold = 24 * time.Hour // gmail, calendar, applecalendar, github
	sourceHealthLocalThreshold  = 48 * time.Hour // imessage, filesystem (and any unknown type)
)

// sourceHealth is one enabled connector instance's freshness snapshot — the
// typed payload doctor's `sources` JSON array, the digest's SourceHealth, and
// the meeting brief's SourceHealth all share.
type sourceHealth struct {
	Key           string `json:"key"`
	State         string `json:"state"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	AgeHours      int    `json:"age_hours"`
	LastError     string `json:"last_error,omitempty"`
	// ErrorCode is the typed companion to LastError (CON-07), carried beside the
	// prose rather than replacing it. Empty on a fresh source. See
	// internal/mora/eval/error-code-registry.json for the published values and
	// docs/architecture/08-cli-and-ux.md for the error_class -> State mapping.
	ErrorCode string `json:"error_code,omitempty"`
}

// syncErrorCodeForState resolves the published error code one persisted sync
// record carries, given the state it classified to. A typed code always wins.
// A failure with no typed code reads as connector.unclassified — the backfill
// rule for records written before this taxonomy shipped. A stale record with no
// recorded failure carries connector.stale, because staleness IS the CON-07
// discrimination that state expresses. A fresh source carries nothing.
func syncErrorCodeForState(state, code, lastError string) string {
	if backfilled := syncErrorCodeOrUnclassified(code, lastError); backfilled != "" {
		return backfilled
	}
	// No typed code and no recorded prose: the state is the only signal left.
	switch state {
	case healthFailed:
		return errCodeConnectorUnclassified
	case healthStale:
		return errCodeConnectorStale
	}
	return ""
}

// connectorErrorCodeFor reports the published code a connector failure already
// carries. An error nothing typed reads as connector.unclassified, which is the
// same value a pre-taxonomy record backfills to — "we failed and nothing named
// the cause" has exactly one representation, never two.
func connectorErrorCodeFor(err error) string {
	var typed moraError
	if errors.As(err, &typed) && classForErrorCode(typed.Code) == errClassConnector {
		return typed.Code
	}
	return errCodeConnectorUnclassified
}

// sourceHealthThreshold dispatches the freshness threshold on Source.Type —
// NEVER on the provider/status-filename alias ("applecal" is only the on-disk
// alias; the catalog type is "applecalendar").
func sourceHealthThreshold(sourceType string) time.Duration {
	switch sourceType {
	case "gmail", "calendar", "applecalendar", "github":
		return sourceHealthGoogleThreshold
	default: // imessage, filesystem, and any future/unknown type
		return sourceHealthLocalThreshold
	}
}

// sourceHealthUnreadableKey is the sentinel entry sourceHealthAll reports when
// sources.json itself cannot be read: a corrupt/unreadable config must NOT
// silently collapse to "no sources enabled" (which would read as healthy —
// the exact silent-failure shape this gate exists to close). It fails CLOSED:
// one synthetic "failed" entry, so doctor/--strict/--pulse all alarm on it.
const sourceHealthUnreadableKey = "sources_config"

// healthInstanceKeyForSource is the health-only identity of one configured
// source row. Most connectors use the digest/watermark instance key verbatim.
// Filesystem memories intentionally have no Provider and are excluded from
// digest watermarks, but each configured folder owns a distinct SyncStatus
// file. Include its source name here so a healthy "docs" folder can never
// dedupe away a failed "notes" folder. Keep this distinction local to health:
// changing instanceKeyForSource would also change persisted brief snapshot
// keys and would require a briefHashSchemaVersion migration.
func healthInstanceKeyForSource(s Source) string {
	if s.Type == "filesystem" && s.Account == "" && s.Name != "" {
		return s.Type + ":" + s.Name
	}
	return instanceKeyForSource(s)
}

// sourceHealthAll walks every ENABLED source (mirroring loadConnectorSyncStatus's
// walk, digest.go:1055-1065) and classifies each instance's freshness against
// its injected `now` — never time.Now(), so doctor/banner checks and their
// tests agree on the same clock (D-03 determinism invariant). Always returns a
// non-nil, deterministically Key-sorted slice — including empty — so a JSON
// caller gets `[]`, never `null`.
func sourceHealthAll(cfg Config, now time.Time) []sourceHealth {
	out := []sourceHealth{}
	sources, err := loadSources(cfg)
	if err != nil {
		// Fail closed (not open): a corrupt sources.json must alarm, not vanish.
		// This one is NOT a connector failure — the config Mora reads to find its
		// connectors is the thing that could not be decoded, so it carries the
		// data class rather than a connector.* code.
		return []sourceHealth{{
			Key:       sourceHealthUnreadableKey,
			State:     healthFailed,
			LastError: "sources.json: " + err.Error(),
			ErrorCode: errCodeDataCorrupt,
		}}
	}
	seen := map[string]bool{}
	for _, s := range sources {
		if !s.IsEnabled() {
			continue
		}
		key := healthInstanceKeyForSource(s)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, sourceHealthFor(cfg, s, key, now))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// sourceHealthFor classifies one source instance. Order (first match wins,
// mirrors classifyState's precedence but keeps never/failed distinct):
//  1. never  — no LastSuccessAt ever recorded.
//  2. failed — the LAST attempt recorded an error (LastError/ErrorCount), even
//     if an OLDER success exists; a failed attempt must never read as merely
//     "stale" (HEALTH-04: the by-construction success-only watermark still
//     ages honestly, but the alarm must name the failure, not just the age).
//  3. stale  — the last success is older than this type's threshold.
//  4. fresh  — otherwise.
func sourceHealthFor(cfg Config, s Source, key string, now time.Time) sourceHealth {
	h := sourceHealth{Key: key}
	path := syncStatusPathFor(cfg, s)
	var st *memory.SyncStatus
	if path != "" {
		st, _ = memory.LoadStatus(path)
	}
	if st == nil || st.LastSuccessAt == "" {
		h.State = healthNever
		if st != nil {
			h.LastError = st.LastError
			h.ErrorCode = syncErrorCodeForState(h.State, st.ErrorCode, st.LastError)
		}
		return h
	}
	h.LastSuccessAt = st.LastSuccessAt
	h.LastError = st.LastError
	t, perr := time.Parse(time.RFC3339, st.LastSuccessAt)
	if perr != nil {
		// An unparseable stamp is as good as no stamp — fail closed to never
		// rather than silently reading as fresh (age 0).
		h.State = healthNever
		h.ErrorCode = syncErrorCodeForState(h.State, st.ErrorCode, st.LastError)
		return h
	}
	age := now.Sub(t)
	if age < 0 {
		age = 0
	}
	h.AgeHours = int(age / time.Hour)
	switch {
	case st.LastError != "" || st.ErrorCount > 0:
		h.State = healthFailed
	case age > sourceHealthThreshold(s.Type):
		h.State = healthStale
	default:
		h.State = healthFresh
	}
	h.ErrorCode = syncErrorCodeForState(h.State, st.ErrorCode, st.LastError)
	return h
}

// healthStateRank orders unhealthy states worst-first for the banner: failed
// (an active error) outranks never (no data point at all) outranks stale (data
// exists, just aging). fresh never reaches this — callers filter it out first.
func healthStateRank(state string) int {
	switch state {
	case healthFailed:
		return 0
	case healthNever:
		return 1
	case healthStale:
		return 2
	default:
		return 3
	}
}

// healthBannerFromSources is the pure render-time half of the banner: given an
// already-computed []sourceHealth (carried on Digest/MeetingBrief from build
// time), pick the single worst source and format ONE line, or "" when every
// source is fresh. Kept separate from healthBanner so render paths never need
// cfg/now (D-03: no time.Now() in a render path) — the struct already pins the
// data as of when it was built.
func healthBannerFromSources(sources []sourceHealth) string {
	worst := worstSource(sources)
	if worst == nil {
		return ""
	}
	return healthBannerLine(*worst)
}

// worstSource picks the single worst non-fresh source (failed > never > stale, ties
// by age desc), or nil when every source is fresh. Extracted so the aggregate
// banner (healthBannerFrom) can rank the source arm against the index/producer arms.
func worstSource(sources []sourceHealth) *sourceHealth {
	var worst *sourceHealth
	worstRank := 0
	for i := range sources {
		h := &sources[i]
		if h.State == healthFresh {
			continue
		}
		rank := healthStateRank(h.State)
		if worst == nil || rank < worstRank || (rank == worstRank && h.AgeHours > worst.AgeHours) {
			worst = h
			worstRank = rank
		}
	}
	return worst
}

// healthBannerErrorCap bounds how much of a raw connector error rides into the
// ONE-line banner (Markdown budget frame, the MCP always-included frame, and
// the AppleScript toast all assume a single bounded line — a raw SQLite/Google
// API error can be a multi-line blob, and an unbounded string in any of those
// three would violate the "one line"/budget contract this whole packet exists
// to protect). The JSON `sources`/`source_health` payload keeps the RAW,
// uncapped LastError for programmatic consumers; only the rendered banner caps it.
const healthBannerErrorCap = 200

// sanitizeHealthError collapses a raw error string to one bounded line: control
// characters (embedded newlines/tabs a multi-line driver error can carry)
// become spaces, then the result is capped to healthBannerErrorCap runes.
func sanitizeHealthError(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > healthBannerErrorCap {
		return strings.TrimSpace(string(r[:healthBannerErrorCap])) + "…"
	}
	return s
}

// healthBannerLine renders one source's alarm line, e.g.:
//
//	🔴 MORA HEALTH: gmail — no successful sync for 52h (database or disk is full (13)). Run: mora doctor
func healthBannerLine(h sourceHealth) string {
	var detail string
	if h.State == healthNever {
		detail = h.Key + " — never synced"
	} else {
		detail = fmt.Sprintf("%s — no successful sync for %dh", h.Key, h.AgeHours)
	}
	if h.LastError != "" {
		detail += fmt.Sprintf(" (%s)", sanitizeHealthError(h.LastError))
	}
	return fmt.Sprintf("🔴 MORA HEALTH: %s. Run: mora doctor", detail)
}

// healthBanner computes the current banner directly from cfg/now — the
// convenience entry point for callers that don't already have a built
// []sourceHealth (doctor's --pulse path, and the tests that pin this
// contract). Build-time callers (buildDigest, buildMeetingBriefFromEvent)
// should call sourceHealthAll once and store it on the struct instead of
// calling this twice.
func healthBanner(cfg Config, now time.Time) string {
	return healthBannerFrom(healthOf(cfg, now))
}

// stampSyncAttemptFailure closes the pre-Ingest stamping gap (▸CX): OAuth
// config, token load, fetcher construction, and DB-open failures in
// ingestGoogle/ingestIMessage/ingestAppleCal all return BEFORE memory.Ingest
// ever loads or saves the per-source SyncStatus, leaving LastAttemptAt/
// LastError untouched — doctor could see a source was OLD but never WHY.
//
// Called from the single ingestSource dispatch chokepoint on any returned
// error, with attemptStart captured BEFORE dispatch ran. It first checks
// whether the inner path (persistSyncStatus, after memory.Ingest ran) already
// stamped THIS attempt — i.e. the on-disk LastAttemptAt is already at or after
// attemptStart — and if so is a deliberate no-op: re-loading+re-saving would
// risk clobbering a checkpoint/counter update the inner path just persisted,
// for no benefit. Identity is attempt TIMING, never error TEXT: the six-day
// incident was the SAME error ("database or disk is full (13)") recurring
// every hour, and a text-equality check cannot distinguish "the inner path
// stamped this during the CURRENT attempt" from "the PREVIOUS attempt failed
// with the same string" — the latter must still advance LastAttemptAt. Only a
// genuinely untouched/earlier attempt (the pre-Ingest gap, or a save that
// failed deeper in the stack) gets (re)stamped, and ErrorCount is bumped so
// `mora sync status` never reads "0 errors" beside a LastError. Best-effort: a
// save failure here is warned, never returned — it must not mask the real
// ingest error the caller is already propagating.
//
// Precision note: SyncStatus.LastAttemptAt round-trips through RFC3339, which
// drops fractional seconds — but attemptStart is a raw time.Now() capture and
// carries nanoseconds. Comparing the (second-truncated) persisted stamp
// against a nanosecond attemptStart would misclassify an inner-path stamp
// that lands LATER in the SAME wall-clock second as "before attemptStart",
// causing a spurious double-stamp. attemptStart is truncated to second
// precision up front so the comparison matches the precision it was actually
// persisted at.
func stampSyncAttemptFailure(cfg Config, s Source, ingestErr error, attemptStart time.Time, out io.Writer) {
	attemptStart = attemptStart.Truncate(time.Second)
	path := syncStatusPathFor(cfg, s)
	if path == "" {
		return
	}
	st, err := memory.LoadStatus(path)
	if err != nil {
		return
	}
	if lastAttempt, perr := time.Parse(time.RFC3339, st.LastAttemptAt); perr == nil && !lastAttempt.Before(attemptStart) {
		return // the inner path already stamped this attempt.
	}
	st.Source = s.Name
	st.LastAttemptAt = attemptStart.UTC().Format(time.RFC3339)
	st.LastError = ingestErr.Error()
	// The typed companion lands in the SAME stamp as the prose (CON-07). This is
	// the one place every connector failure is persisted, so classifying here is
	// what makes `sync status --json` and doctor's `sources` array carry a code
	// rather than an empty slot.
	st.ErrorCode = connectorErrorCodeFor(ingestErr)
	st.ErrorCount++
	if serr := saveSyncStatusFn(path, st); serr != nil && out != nil {
		warnf(out, "could not stamp sync failure (%s): %v", path, serr)
	}
}

// saveSyncStatusFn is the injectable seam over memory.SaveStatus. Production
// always uses the real save; tests override it to deterministically simulate a
// write failure (an unwritable status directory, a full disk) WITHOUT relying
// on real filesystem permission enforcement — needed on Windows, where chmod
// semantics don't reliably deny a directory write the way POSIX does (the
// alarm-still-fires property is pinned via real chmod on POSIX and via this
// seam everywhere, including Windows CI).
var saveSyncStatusFn = memory.SaveStatus
