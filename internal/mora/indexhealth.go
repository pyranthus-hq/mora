package mora

import (
	"database/sql"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"
)

// indexhealth.go — the typed health kernel (Gate 2, HEALTH-09/-10/-12). Gate 1
// proved a SOURCE can die silently and made it impossible; this proves the derived
// INDEX can die silently — and that a perfectly fresh source can never mask an
// index that is stale, degraded, half-built, or missing the last write. The gate is
// a health PREDICATE every surface consults (computed from state, carried as data),
// never a check inside the index-open path — half the product (the daily brief,
// list, read) never opens the index at all (Finding 1).

// The fail-closed rule, once: any state Mora cannot COMPUTE is unhealthy, never
// healthy. There is no "assume fine" branch anywhere in this kernel.

// Aggregate Health.State vocabulary.
const (
	healthHealthy   = "healthy"
	healthDegraded  = "degraded"
	healthUnhealthy = "unhealthy"
)

// indexHealth.State vocabulary.
const (
	idxFresh    = "fresh"
	idxDirty    = "dirty"
	idxFailed   = "failed"
	idxDegraded = "degraded"
	idxNever    = "never"
)

// producerHealth.State vocabulary (populated by Packet E / PR 4; defined here so
// the kernel type is complete and PR 4 only adds the logic).
const (
	prodFresh  = "fresh"
	prodStale  = "stale"
	prodFailed = "failed"
	prodNever  = "never"
)

// indexProjectionLagThreshold is the fifth freshness family (Landmine 13 — do not
// unify it with the four source/digest thresholds). It is a RELATION, not a
// wall-clock age: an idle vault has fts_indexed_at == graph_indexed_at and never
// reddens by aging; the alarm fires only when an authored write has advanced FTS
// past the graph and a rebuild is genuinely owed (B1 rule 6 / Landmine 14).
const indexProjectionLagThreshold = 6 * time.Hour

// Health is the typed kernel every surface consults — Gate 1's sourceHealth
// extended with the two states it never had (index, producers).
type Health struct {
	State     string           `json:"state"`     // healthy | degraded | unhealthy (worst-of; UNKNOWN => unhealthy)
	Sources   []sourceHealth   `json:"sources"`   // Gate 1, unchanged
	Index     indexHealth      `json:"index"`     // HEALTH-09/-10/-12
	Producers []producerHealth `json:"producers"` // HEALTH-11 (PR 4)
}

type indexHealth struct {
	State         string             `json:"state"` // fresh | dirty | failed | degraded | never
	IndexedAt     string             `json:"indexed_at,omitempty"`
	DirtySince    string             `json:"dirty_since,omitempty"`
	PendingOps    int                `json:"pending_ops"`
	LastAttemptAt string             `json:"last_attempt_at,omitempty"`
	LastError     string             `json:"last_error,omitempty"`
	SchemaVersion int                `json:"schema_version"`
	Blocked       bool               `json:"blocked"`
	Embedder      embedderProvenance `json:"embedder"`
	Projections   projectionHealth   `json:"projections"`
}

type projectionHealth struct { // Finding 2: three projections, three stamps
	FTSIndexedAt     string `json:"fts_indexed_at,omitempty"`     // upsert + rebuild
	GraphIndexedAt   string `json:"graph_indexed_at,omitempty"`   // rebuild only
	VectorsIndexedAt string `json:"vectors_indexed_at,omitempty"` // rebuild only
	GraphLagHours    int    `json:"graph_lag_hours"`
}

type embedderProvenance struct { // HEALTH-12 (mismatch arm — PR 1; semantic digest — PR 3)
	Model      string `json:"model"` // recorded at index-commit time, e.g. "static-hash-v1"
	Dim        int    `json:"dim"`
	Digest     string `json:"digest,omitempty"` // the ollama model digest (PR 3)
	Configured string `json:"configured"`       // what the config ASKS for, resolved WITHOUT probing
	Match      bool   `json:"match"`            // false => indexHealth.State = "degraded"
}

type producerHealth struct { // HEALTH-11 (PR 4)
	Name            string `json:"name"`
	State           string `json:"state"`
	LastSuccessAt   string `json:"last_success_at,omitempty"`
	LastAttemptAt   string `json:"last_attempt_at,omitempty"`
	LastError       string `json:"last_error,omitempty"`
	IntervalSeconds int    `json:"interval_seconds"`
	AgeHours        int    `json:"age_hours"`
	Source          string `json:"source"`
}

// indexHealthOf computes the index arm. First match wins; `now` is injected (never
// time.Now()) so a check and its test agree on the same clock.
func indexHealthOf(cfg Config, now time.Time) indexHealth {
	h := indexHealth{State: idxFresh, SchemaVersion: indexSchemaVersion}

	// Rule 1: index.db absent -> never.
	if _, err := os.Stat(dbPath(cfg)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			h.State = idxNever
			return h
		}
		h.State = idxFailed
		h.LastError = err.Error()
		return h
	}
	// Rule 2: cannot open / schema mismatch / any query error -> failed.
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		h.State = idxFailed
		h.LastError = err.Error()
		return h
	}
	defer db.Close()
	if serr := checkIndexSchema(db); serr != nil {
		h.State = idxFailed
		h.LastError = serr.Error()
		return h
	}
	meta, merr := readIndexMeta(db)
	if merr != nil {
		h.State = idxFailed
		h.LastError = merr.Error()
		return h
	}
	// Rule 2b (fail-closed floor): a schema-valid index whose provenance rows have
	// been wiped is NOT computable and must never read fresh. Every committed rebuild
	// — legacy (main) and current — stamps vault_dir into index_meta and never deletes
	// it (only the vault_manifest_* keys are ever cleared, by indexUpsert). So an
	// absent vault_dir means index_meta was truncated by a crash mid-rebuild, a hand
	// `DELETE FROM index_meta`, or a restore of a torn db — an uncomputable state.
	// Fail closed to `failed` (mirrors rule 2's query-error handling); a genuine
	// legacy v2 index still carries vault_dir, so this never reddens an upgrade. The
	// absent-is-not-dirty tolerance is for the NEW keys only (embedder provenance /
	// content manifest), never for the binding rows that prove a rebuild committed.
	if meta["vault_dir"] == "" {
		h.State = idxFailed
		h.LastError = "index_meta missing committed provenance (vault_dir); index cannot be verified"
		return h
	}
	h.IndexedAt = meta["indexed_at"]
	h.LastAttemptAt = meta["index_last_attempt_at"]
	if h.LastError == "" {
		h.LastError = meta["index_last_error"]
	}
	h.Projections = projectionHealth{
		FTSIndexedAt:     meta["fts_indexed_at"],
		GraphIndexedAt:   meta["graph_indexed_at"],
		VectorsIndexedAt: meta["vectors_indexed_at"],
	}
	h.Embedder = embedderProvenanceOf(cfg, meta)

	// Rule 3: a rebuild block record makes the index failed (Blocked). It is already
	// written (vaultid.go); this is finally what makes it MEAN something.
	if _, present, _ := readBlockRecord(cfg); present {
		h.State = idxFailed
		h.Blocked = true
		return h
	}
	// Rule 4: any pending op OR any non-empty ingest journal -> dirty.
	ops, operr := listPendingOps(cfg)
	if operr != nil {
		h.State = idxFailed
		h.LastError = operr.Error()
		return h
	}
	journalDirty, journalPaths, journalOldest, jerr := ingestJournalStatus(cfg)
	if jerr != nil {
		h.State = idxFailed
		h.LastError = jerr.Error()
		return h
	}
	if len(ops) > 0 || journalDirty {
		h.State = idxDirty
		h.PendingOps = len(ops) + journalPaths
		h.DirtySince = oldestPendingStamp(ops, journalOldest)
		return h
	}
	// Rule 5: recorded embedder != configured embedder -> degraded (HEALTH-12).
	if !h.Embedder.Match {
		h.State = idxDegraded
		return h
	}
	// Rule 6: projection lag as a RELATION (fts_indexed_at − graph_indexed_at).
	lag := projectionLagHours(h.Projections)
	h.Projections.GraphLagHours = lag
	if time.Duration(lag)*time.Hour > indexProjectionLagThreshold {
		h.State = idxDirty
		return h
	}
	// Rule 7: fresh.
	h.State = idxFresh
	return h
}

// readIndexMeta loads the whole index_meta key/value table into a map. A query
// error is surfaced (fail closed at the caller), never swallowed.
func readIndexMeta(db *sql.DB) (map[string]string, error) {
	rows, err := db.Query(`SELECT key, value FROM index_meta`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// embedderProvenanceOf compares what RAN (embedder_model, from the committed
// rebuild tx) against what the config now ASKS for — resolved WITHOUT a liveness
// probe, so doctor stays fast/offline. An index with no recorded provenance (a
// legacy index at the same schema) is treated as a MATCH: absent provenance must
// not redden every existing user's first doctor, mirroring the manifest's
// absent-is-not-dirty rule.
func embedderProvenanceOf(cfg Config, meta map[string]string) embedderProvenance {
	recorded := meta["embedder_model"]
	dim, _ := strconv.Atoi(meta["embedder_dim"])
	configured := configuredEmbedderModel(cfg)
	return embedderProvenance{
		Model:      recorded,
		Dim:        dim,
		Digest:     meta["embedder_digest"],
		Configured: configured,
		Match:      embedderModelsMatch(recorded, configured),
	}
}

// configuredEmbedderModel resolves the config's declared embedder to a coarse
// model identity WITHOUT calling chooseEmbedderFor (which probe()s Ollama for up to
// 2s). Mirrors chooseEmbedderFor's precedence (MORA_EMBEDDER, then cfg.Embedder).
func configuredEmbedderModel(cfg Config) string {
	pref, ok := os.LookupEnv("MORA_EMBEDDER")
	if !ok {
		pref = cfg.Embedder
	}
	if pref != "ollama" {
		return defaultEmbedder().ModelID() // "static-hash-v1"
	}
	model := os.Getenv("MORA_OLLAMA_MODEL")
	if model == "" {
		model = "nomic-embed-text"
	}
	return "ollama:" + model
}

// embedderModelsMatch reports whether the recorded model satisfies the configured
// identity. PR 1 is the MINIMAL (family) check — Packet D/PR 3 adds the semantic
// digest. An empty recorded value (legacy index) matches (absent-is-not-degraded).
func embedderModelsMatch(recorded, configured string) bool {
	if recorded == "" {
		return true
	}
	if strings.HasPrefix(configured, "ollama:") {
		// Match the family+model prefix so "ollama:nomic-embed-text" accepts a
		// recorded "ollama:nomic-embed-text@<digest>" (PR 3 tightens to the digest).
		return strings.HasPrefix(recorded, configured)
	}
	return recorded == configured
}

// projectionLagHours is the honest graph-lag signal: fts_indexed_at −
// graph_indexed_at, in whole hours (never negative). Both empty or unparseable => 0.
func projectionLagHours(p projectionHealth) int {
	fts, ferr := time.Parse(time.RFC3339, p.FTSIndexedAt)
	graph, gerr := time.Parse(time.RFC3339, p.GraphIndexedAt)
	if ferr != nil || gerr != nil {
		return 0
	}
	d := fts.Sub(graph)
	if d < 0 {
		return 0
	}
	return int(d / time.Hour)
}

// oldestPendingStamp returns the earliest marked_at across the pending ops and the
// oldest ingest-journal header, for indexHealth.DirtySince.
func oldestPendingStamp(ops []pendingOp, journalOldest string) string {
	oldest := journalOldest
	for _, op := range ops {
		if op.MarkedAt == "" {
			continue
		}
		if oldest == "" || op.MarkedAt < oldest {
			oldest = op.MarkedAt
		}
	}
	return oldest
}

// healthOf is the ONE public entry point — it computes each arm independently
// (rule 6 no longer reads producerHealth, so there is no cycle to order around) and
// collapses them worst-of. Nothing recomputes a sub-arm on its own.
func healthOf(cfg Config, now time.Time) Health {
	h := Health{
		Sources:   sourceHealthAll(cfg, now),
		Index:     indexHealthOf(cfg, now),
		Producers: []producerHealth{}, // PR 1: producer ledger not built (fail-open contract)
	}
	h.State = aggregateHealthState(h)
	return h
}

// aggregateHealthState is B1b's collapse, stated once so no surface invents its
// own. "Could not compute" is captured as failed by each arm, so it lands in
// unhealthy — never healthy.
func aggregateHealthState(h Health) string {
	unhealthy, degraded := false, false
	for _, s := range h.Sources {
		switch s.State {
		case healthFailed, healthNever:
			unhealthy = true
		case healthStale:
			degraded = true
		}
	}
	switch h.Index.State {
	case idxFailed, idxNever, idxDirty:
		unhealthy = true
	case idxDegraded:
		degraded = true
	}
	for _, p := range h.Producers {
		switch p.State {
		case prodFailed, prodNever:
			unhealthy = true
		case prodStale:
			degraded = true
		}
	}
	switch {
	case unhealthy:
		return healthUnhealthy
	case degraded:
		return healthDegraded
	default:
		return healthHealthy
	}
}
