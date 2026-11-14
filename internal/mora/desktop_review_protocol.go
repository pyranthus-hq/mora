package mora

import (
	"bytes"
	"encoding/binary"
	"errors"
)

const desktopReviewProtocolVersion = 1

type desktopReviewAction string

const (
	desktopReviewKeep     desktopReviewAction = "keep"
	desktopReviewDiscard  desktopReviewAction = "discard"
	desktopReviewActivate desktopReviewAction = "activate"
	desktopReviewRetire   desktopReviewAction = "retire"
)

const desktopReviewProjectRule = "project_rule"

type desktopReviewChallenge struct {
	Schema                string              `json:"schema"`
	SchemaVersion         int                 `json:"schema_version"`
	Nonce                 string              `json:"nonce"`
	ExpiresAt             int64               `json:"expires_at_unix"`
	InstallationID        string              `json:"installation_id"`
	KeyID                 string              `json:"key_id"`
	Action                desktopReviewAction `json:"action"`
	Kind                  string              `json:"kind"`
	ID                    string              `json:"id"`
	Scope                 string              `json:"scope"`
	CanonicalRevision     string              `json:"canonical_revision"`
	DisplayDigest         string              `json:"display_digest"`
	Client                string              `json:"client,omitempty"`
	ProjectRoot           string              `json:"project_root,omitempty"`
	LoaderRelPath         string              `json:"loader_rel_path,omitempty"`
	SourceMemoryID        string              `json:"source_memory_id,omitempty"`
	MaterializationDigest string              `json:"materialization_digest,omitempty"`
	IssuedDigest          string              `json:"issued_digest"`
	State                 string              `json:"state"`
	Consumed              bool                `json:"consumed"`
	Applied               bool                `json:"applied"`
}

func (c desktopReviewChallenge) bytes() ([]byte, error) {
	if c.Schema != "mora.review.desktop" || c.SchemaVersion != desktopReviewProtocolVersion || c.Nonce == "" || c.Kind == "" || c.ID == "" || c.Scope == "" ||
		c.InstallationID == "" || c.KeyID == "" || c.CanonicalRevision == "" || c.DisplayDigest == "" || c.ExpiresAt <= 0 {
		return nil, errors.New("invalid desktop review challenge")
	}
	projectRule := c.Kind == desktopReviewProjectRule
	if projectRule {
		if (c.Action != desktopReviewActivate && c.Action != desktopReviewRetire) || c.Client != "codex" || c.ProjectRoot == "" || c.LoaderRelPath == "" || c.SourceMemoryID == "" {
			return nil, errors.New("invalid project rule desktop review challenge")
		}
	} else if c.Action != desktopReviewKeep && c.Action != desktopReviewDiscard {
		return nil, errors.New("invalid desktop review action")
	}
	var out bytes.Buffer
	domain := "mora-review-decision-v1\x00"
	values := []string{c.Nonce, c.InstallationID, c.KeyID, string(c.Action), c.Kind, c.ID, c.Scope, c.CanonicalRevision, c.DisplayDigest}
	if projectRule {
		domain = "mora-project-rule-decision-v1\x00"
		values = append(values, c.Client, c.ProjectRoot, c.LoaderRelPath, c.SourceMemoryID, c.MaterializationDigest)
	}
	out.WriteString(domain)
	for _, value := range values {
		if len(value) > 1<<20 {
			return nil, errors.New("desktop review challenge field is too large")
		}
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(value)))
		out.Write(n[:])
		out.WriteString(value)
	}
	var expires [8]byte
	binary.BigEndian.PutUint64(expires[:], uint64(c.ExpiresAt))
	out.Write(expires[:])
	return out.Bytes(), nil
}

type desktopReviewOutcome struct {
	Status          string `json:"status"`
	Applied         *bool  `json:"applied,omitempty"`
	IndexVisibility string `json:"index_visibility"`
}
