// Package governance owns the durable cross-device governance decision ledger.
package governance

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/commitment"
	"github.com/pyranthus-hq/mora/internal/leasefile"
	"io/fs"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	SchemaVersion  = 1
	FileName       = ".mora-governance.json"
	LeaseTTL       = 30 * time.Second
	AcquireTimeout = 2 * time.Second
)

type Atom struct {
	Provider string `json:"provider,omitempty"`
	Kind     string `json:"kind"`
	Value    string `json:"value"`
}
type Entry struct {
	ID                 string               `json:"id"`
	Kind               string               `json:"kind"`
	Atom               Atom                 `json:"atom"`
	Action             string               `json:"action"`
	Reason             string               `json:"reason,omitempty"`
	Atom2              *Atom                `json:"atom2,omitempty"`
	Decision           string               `json:"decision,omitempty"`
	TargetID           string               `json:"target_id,omitempty"`
	CommitmentID       string               `json:"commitment_id,omitempty"`
	ReplacementID      string               `json:"replacement_id,omitempty"`
	CorrectedAtom      *Atom                `json:"corrected_atom,omitempty"`
	CorrectedDirection commitment.Direction `json:"corrected_direction,omitempty"`
	DuplicateOf        string               `json:"duplicate_of,omitempty"`
	CreatedAt          string               `json:"created_at"`
	CreatedBy          string               `json:"created_by"`
	RevokedAt          string               `json:"revoked_at,omitempty"`
}
type Ledger struct {
	Schema  int     `json:"schema"`
	Entries []Entry `json:"entries"`
}
type Store struct {
	VaultDir  string
	NewID     func() string
	Now       func() time.Time
	CreatedBy func() string
	PostLoad  func()
	WallNow   func() time.Time
	Sleep     func(time.Duration)
	Backoff   func(int) time.Duration
	PID       func() int
}

func (s Store) Path() string     { return filepath.Join(s.VaultDir, FileName) }
func (s Store) LockPath() string { return s.Path() + ".lock" }
func (s Store) Load() (Ledger, error) {
	b, err := os.ReadFile(s.Path())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Ledger{Schema: SchemaVersion}, nil
		}
		return Ledger{}, err
	}
	var g Ledger
	if err := json.Unmarshal(b, &g); err != nil {
		return Ledger{}, fmt.Errorf("governance ledger %s is unreadable (corrupt JSON) — restore it from your vault backup (e.g. `mora sync git`) rather than deleting it: %w", s.Path(), err)
	}
	return g, nil
}
func (s Store) Save(g Ledger) error {
	if g.Schema == 0 {
		g.Schema = SchemaVersion
	}
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	return atomicio.Write(s.Path(), append(b, '\n'), 0600)
}
func (s Store) wallNow() time.Time {
	if s.WallNow != nil {
		return s.WallNow()
	}
	return time.Now()
}
func (s Store) sleep(d time.Duration) {
	if s.Sleep != nil {
		s.Sleep(d)
		return
	}
	time.Sleep(d)
}
func (s Store) backoff(attempt int) time.Duration {
	if s.Backoff != nil {
		return s.Backoff(attempt)
	}
	capMs := 1 << min(attempt, 5)
	return time.Duration(1+mrand.IntN(capMs)) * time.Millisecond
}
func (s Store) pid() int {
	if s.PID != nil {
		return s.PID()
	}
	return os.Getpid()
}
func (s Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
func (s Store) Acquire(now time.Time) (func(), error) {
	if err := os.MkdirAll(s.VaultDir, 0700); err != nil {
		return nil, err
	}
	body, _ := json.Marshal(struct {
		PID        int    `json:"pid"`
		AcquiredAt string `json:"acquired_at"`
	}{s.pid(), now.UTC().Format(time.RFC3339)})
	deadline := s.wallNow().Add(AcquireTimeout)
	path := s.LockPath()
	for attempt := 0; ; attempt++ {
		published, err := leasefile.Publish(path, body)
		wait := s.backoff(attempt)
		switch {
		case err == nil && published:
			return leasefile.Releaser(path, body, leasefile.DefaultRemovalOptions()), nil
		case err != nil && !atomicio.SharingViolationRetryable(err):
			return nil, err
		case err == nil:
			reaped, rerr := leasefile.Reap(path, now, LeaseTTL, leasefile.DefaultRemovalOptions())
			if rerr != nil && !atomicio.SharingViolationRetryable(rerr) {
				return nil, rerr
			}
			if rerr == nil && reaped {
				wait = 0
			}
		}
		if s.wallNow().Add(wait).After(deadline) {
			break
		}
		s.sleep(wait)
	}
	return nil, fmt.Errorf("governance ledger is locked by another mora process (%s); retry in a moment", path)
}
func (s Store) mint(e Entry) Entry {
	if e.ID == "" {
		id := ""
		if s.NewID != nil {
			id = s.NewID()
		}
		e.ID = "gov_" + strings.TrimPrefix(id, "mem_")
	}
	if e.CreatedAt == "" {
		e.CreatedAt = s.now().Format(time.RFC3339)
	}
	if e.CreatedBy == "" && s.CreatedBy != nil {
		e.CreatedBy = s.CreatedBy()
	}
	return e
}
func (s Store) Append(e Entry) (Entry, error) {
	release, err := s.Acquire(s.now())
	if err != nil {
		return Entry{}, err
	}
	defer release()
	g, err := s.Load()
	if err != nil {
		return Entry{}, err
	}
	if s.PostLoad != nil {
		s.PostLoad()
	}
	return s.AppendLocked(g, e)
}
func (s Store) AppendLocked(g Ledger, e Entry) (Entry, error) {
	e = s.mint(e)
	g.Entries = append(g.Entries, e)
	if err := s.Save(g); err != nil {
		return Entry{}, err
	}
	return e, nil
}
func (s Store) Revoke(id string) (bool, error) {
	release, err := s.Acquire(s.now())
	if err != nil {
		return false, err
	}
	defer release()
	g, err := s.Load()
	if err != nil {
		return false, err
	}
	found := false
	for i := range g.Entries {
		if g.Entries[i].ID == id && g.Entries[i].RevokedAt == "" {
			g.Entries[i].RevokedAt = s.now().Format(time.RFC3339)
			found = true
		}
	}
	if !found {
		return false, nil
	}
	return true, s.Save(g)
}
func ItemAtom(provider, stableID string) Atom {
	return Atom{Provider: provider, Kind: "stable_id", Value: stableID}
}
func NormalizeIdentity(kind, raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	if kind == "address" || strings.Contains(v, "@") {
		return strings.ToLower(v)
	}
	return v
}
func ProviderMatches(entryProvider, memoryProvider string) bool {
	return entryProvider == "" || entryProvider == memoryProvider
}
