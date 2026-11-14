// Package resource stores immutable resource evidence. It deliberately has no
// authority, network, loader, or client integration.
package resource

import (
	"errors"
	"time"

	"github.com/pyranthus-hq/mora/internal/managedtree"
)

var (
	ErrInvalidCandidate      = errors.New("invalid resource candidate")
	ErrPriorStableIDRequired = errors.New("changed resource requires prior stable ID")
	ErrUnsafeSource          = errors.New("unsafe resource source")
	ErrNotFound              = errors.New("resource revision not found")
)

type Kind string

const (
	KindSkill        Kind = "skill"
	KindBestPractice Kind = "best_practice"
	KindRepository   Kind = "repository"
	KindTool         Kind = "tool"
)

type SourceMode string

const (
	SourcePasted    SourceMode = "pasted"
	SourceFolder    SourceMode = "folder"
	SourceReference SourceMode = "reference"
)

type Availability string

const (
	AvailabilityAvailable  Availability = "available"
	AvailabilityUnfetched  Availability = "unfetched"
	AvailabilityIncomplete Availability = "incomplete"
)

// Reference is declarative provenance only. The leaf never contacts it or
// treats a local checkout's declared origin as verified.
type Reference struct {
	URL          string `json:"url,omitempty"`
	Subdirectory string `json:"subdirectory,omitempty"`
	Version      string `json:"version,omitempty"`
}

// Candidate is untrusted intake. EffectiveScope is captured provenance, not
// authority; callers must not use it to activate or materialize a resource.
type Candidate struct {
	StableID                 string
	Kind                     Kind
	CanonicalSource          string
	Attribution              string
	Purpose                  string
	Applicability            []string
	KnownLimits              []string
	EffectiveScope           string
	UntrustedScopeSuggestion string
	Source                   SourceMode
	Pasted                   []byte
	Folder                   string
	Reference                Reference
}

type Record struct {
	Schema          int       `json:"schema"`
	StableID        string    `json:"stable_id"`
	Kind            Kind      `json:"kind"`
	CanonicalSource string    `json:"canonical_source"`
	Attribution     string    `json:"attribution,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
}

// Revision is immutable intrinsic evidence. It intentionally contains neither
// approval nor an active pointer.
type Revision struct {
	Schema                   int                `json:"schema"`
	ResourceID               string             `json:"resource_id"`
	Kind                     Kind               `json:"kind"`
	Purpose                  string             `json:"purpose"`
	Applicability            []string           `json:"applicability,omitempty"`
	KnownLimits              []string           `json:"known_limits,omitempty"`
	EffectiveScope           string             `json:"effective_scope,omitempty"`
	UntrustedScopeSuggestion string             `json:"untrusted_scope_suggestion,omitempty"`
	Source                   SourceMode         `json:"source"`
	Reference                Reference          `json:"reference,omitempty"`
	SourceVerified           bool               `json:"source_verified"`
	Availability             Availability       `json:"availability"`
	Complete                 bool               `json:"complete"`
	Files                    []managedtree.File `json:"files,omitempty"`
	TreeDigest               string             `json:"tree_digest,omitempty"`
	RevisionDigest           string             `json:"revision_digest"`
}

type ImportOutcome struct {
	CreatedRecord   bool
	CreatedRevision bool
	Duplicate       bool
}

// Binding is supplied by the trusted integration layer. The resource package
// records it only in memory and cannot mint it.
type Binding struct {
	ResourceID     string
	RevisionDigest string
	Scope          string // exact project:<id> or global
	Eligible       bool
	Active         bool
	Retired        bool
}

type Query struct {
	ProjectScope string
	Task         string
	Bindings     []Binding
}

// Result is metadata only: callers receive neither files nor instructions.
type Result struct {
	ResourceID     string
	Kind           Kind
	Purpose        string
	MatchReason    string
	Scope          string
	Source         SourceMode
	RevisionDigest string
	Availability   Availability
	KnownLimits    []string
	ReviewState    string
}

type EventType string

const (
	EventRecommended EventType = "recommended"
	EventLoaded      EventType = "loaded"
	EventOutcome     EventType = "outcome"
)

// Event contains only bounded receipt metadata. Query/body payloads are not
// represented so they cannot be logged accidentally.
type Event struct {
	Type             EventType `json:"type"`
	ResourceID       string    `json:"resource_id"`
	RevisionDigest   string    `json:"revision_digest"`
	ProjectScope     string    `json:"project_scope,omitempty"`
	Evidence         string    `json:"evidence,omitempty"`
	OccurredAt       time.Time `json:"occurred_at"`
	TrackingDisabled bool      `json:"-"`
}
