package health

import (
	"time"

	"github.com/pyranthus-hq/mora/internal/operation"
)

const (
	Healthy        = "healthy"
	Degraded       = "degraded"
	Unhealthy      = "unhealthy"
	IndexFresh     = "fresh"
	IndexDirty     = "dirty"
	IndexFailed    = "failed"
	IndexDegraded  = "degraded"
	IndexNever     = "never"
	ProducerFresh  = "fresh"
	ProducerStale  = "stale"
	ProducerFailed = "failed"
	ProducerNever  = "never"
)

// Embedder records committed and configured vector provenance.
type Embedder struct {
	Model      string `json:"model"`
	Dim        int    `json:"dim"`
	Digest     string `json:"digest,omitempty"`
	Configured string `json:"configured"`
	Match      bool   `json:"match"`
}

// Projection records independent FTS, graph, and vector commit stamps.
type Projection struct {
	FTSIndexedAt     string `json:"fts_indexed_at,omitempty"`
	GraphIndexedAt   string `json:"graph_indexed_at,omitempty"`
	VectorsIndexedAt string `json:"vectors_indexed_at,omitempty"`
	GraphLagHours    int    `json:"graph_lag_hours"`
}

// Index is the personal or subscription index health arm.
type Index struct {
	State         string     `json:"state"`
	IndexedAt     string     `json:"indexed_at,omitempty"`
	DirtySince    string     `json:"dirty_since,omitempty"`
	PendingOps    int        `json:"pending_ops"`
	LastAttemptAt string     `json:"last_attempt_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	SchemaVersion int        `json:"schema_version"`
	Blocked       bool       `json:"blocked"`
	Embedder      Embedder   `json:"embedder"`
	Projections   Projection `json:"projections"`
	Shares        []Index    `json:"shares,omitempty"`
}

// ProducerSubject distinguishes a producer record from a ledger-integrity record.
type ProducerSubject string

const (
	ProducerSubjectProducer ProducerSubject = "producer"
	ProducerSubjectLedger   ProducerSubject = "ledger"
)

// Producer is one expected background artifact producer health record.
type Producer struct {
	Name            string          `json:"name"`
	State           string          `json:"state"`
	LastSuccessAt   string          `json:"last_success_at,omitempty"`
	LastAttemptAt   string          `json:"last_attempt_at,omitempty"`
	LastError       string          `json:"last_error,omitempty"`
	IntervalSeconds int             `json:"interval_seconds"`
	AgeHours        int             `json:"age_hours"`
	Source          string          `json:"source"`
	Subject         ProducerSubject `json:"subject"`
}

// Health is the canonical snapshot carried by all user-facing surfaces.
type Health struct {
	State      string               `json:"state"`
	Sources    []Source             `json:"sources"`
	Index      Index                `json:"index"`
	Producers  []Producer           `json:"producers"`
	Activities []operation.Activity `json:"activities"`
}

// AggregateState returns the fail-closed worst state across every health arm.
func AggregateState(h Health) string {
	unhealthy, degraded := false, false
	for _, s := range h.Sources {
		switch s.State {
		case Failed, Never:
			unhealthy = true
		case Stale:
			degraded = true
		}
	}
	switch h.Index.State {
	case IndexFailed, IndexNever, IndexDirty:
		unhealthy = true
	case IndexDegraded:
		degraded = true
	}
	for _, s := range h.Index.Shares {
		switch s.State {
		case IndexFailed, IndexNever, IndexDirty:
			unhealthy = true
		case IndexDegraded:
			degraded = true
		}
	}
	for _, p := range h.Producers {
		if p.Subject == ProducerSubjectLedger {
			unhealthy = true
			continue
		}
		switch p.State {
		case ProducerFailed, ProducerNever, ProducerStale:
			degraded = true
		}
	}
	for _, a := range h.Activities {
		if a.State == operation.Stalled || a.State == operation.Failed {
			unhealthy = true
		}
	}
	if unhealthy {
		return Unhealthy
	}
	if degraded {
		return Degraded
	}
	return Healthy
}

// ProjectionLagHours computes non-negative FTS-to-graph lag from stored clocks.
func ProjectionLagHours(p Projection) int {
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
