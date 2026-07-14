package exam

import (
	"encoding/json"
	"fmt"
	"os"
)

type Ledger struct {
	Version        int             `json:"version"`
	AsOf           string          `json:"as_of"`
	Self           Identity        `json:"self"`
	People         []Identity      `json:"people"`
	Artifacts      []Artifact      `json:"artifacts"`
	Commitments    []Commitment    `json:"commitments"`
	NonObligations []NonObligation `json:"non_obligations"`
}

type Identity struct {
	ID      string   `json:"id"`
	Display string   `json:"display"`
	Emails  []string `json:"emails"`
	Handles []string `json:"handles"`
	Service bool     `json:"service"`
}

type Artifact struct {
	ID           string    `json:"id"`
	MemoryID     string    `json:"memory_id"`
	Channel      string    `json:"channel"`
	Subject      string    `json:"subject"`
	OccurredAt   string    `json:"occurred_at"`
	Participants []string  `json:"participants,omitempty"`
	Messages     []Message `json:"messages"`
}

type Message struct {
	ID   string   `json:"id"`
	From string   `json:"from"`
	To   []string `json:"to"`
	Cc   []string `json:"cc"`
	At   string   `json:"at"`
	Body []Block  `json:"body"`
}

type Block struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

type Span struct {
	ArtifactID string `json:"artifact_id"`
	MessageID  string `json:"message_id,omitempty"`
	BlockID    string `json:"block_id,omitempty"`
	Quote      string `json:"quote"`
}

type Commitment struct {
	ID              string       `json:"id"`
	Owner           string       `json:"owner"`
	Counterparty    string       `json:"counterparty"`
	Direction       string       `json:"direction"`
	Summary         string       `json:"summary"`
	OpenedBy        Span         `json:"opened_by"`
	DueAt           string       `json:"due_at,omitempty"`
	DueKind         string       `json:"due_kind"`
	State           string       `json:"state"`
	Transitions     []Transition `json:"transitions"`
	DuplicateOf     string       `json:"duplicate_of,omitempty"`
	ExpectedIn      []string     `json:"expected_in"`
	RequiresMerge   string       `json:"requires_merge,omitempty"`
	ExpectedFailure string       `json:"expected_failure,omitempty"`
}

type Transition struct {
	To       string `json:"to"`
	At       string `json:"at"`
	Evidence Span   `json:"evidence"`
	Note     string `json:"note,omitempty"`
}

type NonObligation struct {
	ID    string `json:"id"`
	Span  Span   `json:"span"`
	Class string `json:"class"`
	Why   string `json:"why"`
}

func Load(path string) (Ledger, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Ledger{}, err
	}
	var l Ledger
	decErr := json.Unmarshal(b, &l)
	if decErr != nil {
		return Ledger{}, fmt.Errorf("decode ledger: %w", decErr)
	}
	return l, nil
}
