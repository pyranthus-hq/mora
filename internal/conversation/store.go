// Package conversation preserves local agent conversations independently of
// authored memories. Summaries are navigation aids, never evidence or authority.
package conversation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/leasefile"
)

type Store struct{ Dir string }
type Session struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Scope    string `json:"scope"`
	Source   string `json:"source"`
	CWD      string `json:"cwd,omitempty"`
	Messages int    `json:"messages"`
	Updated  string `json:"updated,omitempty"`
}
type Message struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
	Date string `json:"date,omitempty"`
	// Raw preserves the accepted source item; hidden reasoning is excluded by
	// provider readers before reaching the archive.
	Raw json.RawMessage `json:"raw,omitempty"`
}
type archive struct {
	Session  Session   `json:"session"`
	Messages []Message `json:"messages"`
}

func (s Store) path(id string) string {
	h := sha256.Sum256([]byte(id))
	return filepath.Join(s.Dir, "archive", hex.EncodeToString(h[:])+".json")
}
func (s Store) load(id string) (archive, error) {
	var a archive
	b, err := os.ReadFile(s.path(id))
	if err != nil {
		return a, err
	}
	if err = json.Unmarshal(b, &a); err != nil {
		return a, fmt.Errorf("corrupt conversation %s: %w", id, err)
	}
	if a.Session.ID != id || a.Session.Messages != len(a.Messages) {
		return a, errors.New("conversation identity/count mismatch")
	}
	return a, nil
}

// Raw JSON is a value: encoding/json compacts whitespace and escapes HTML
// during archival. Compare decoded values with lossless numbers so formatting
// differences never masquerade as a history rewrite.
func sameMessage(a, b Message) bool {
	if a.Kind != b.Kind || a.Text != b.Text || a.Date != b.Date {
		return false
	}
	if bytes.Equal(a.Raw, b.Raw) {
		return true
	}
	decode := func(raw json.RawMessage) (any, error) {
		var v any
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		err := d.Decode(&v)
		return v, err
	}
	av, ae := decode(a.Raw)
	bv, be := decode(b.Raw)
	return ae == nil && be == nil && reflect.DeepEqual(av, bv)
}

// Import atomically publishes a logically append-only archive. Replacement
// writes retain the entire prior prefix, with a durability barrier and an OS
// lease spanning archive and index publication. A crash cannot publish half a
// message. The separate FTS projection is rebuildable via Reindex.
func (s Store) Import(meta Session, messages []Message) error {
	if meta.ID == "" || meta.Scope == "" || (meta.Provider != "codex" && meta.Provider != "claude") {
		return errors.New("conversation id, scope and supported provider required")
	}
	return leasefile.WithGuard(filepath.Join(s.Dir, "capture.lock"), func() error {
		old, err := s.load(meta.ID)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			if old.Session.Scope != meta.Scope || old.Session.Provider != meta.Provider || old.Session.Source != meta.Source || old.Session.CWD != meta.CWD {
				return errors.New("conversation metadata changed; refusing to rewrite history")
			}
			if len(messages) < len(old.Messages) {
				return errors.New("source transcript shrank; original archive retained")
			}
			for i, m := range old.Messages {
				if !sameMessage(m, messages[i]) {
					return fmt.Errorf("source transcript changed at message %d; original archive retained", i)
				}
			}
		}
		meta.Messages = len(messages)
		if len(messages) > 0 {
			meta.Updated = messages[len(messages)-1].Date
		}
		if len(messages) != len(old.Messages) || err != nil {
			b, e := json.Marshal(archive{meta, messages})
			if e != nil {
				return e
			}
			if e = atomicio.WriteDurable(s.path(meta.ID), append(b, '\n'), 0600); e != nil {
				return e
			}
			nodes := buildNodes(messages)
			b, e = json.Marshal(nodes)
			if e != nil {
				return e
			}
			// Tree nodes are immutable deterministic previews; this file is only cache.
			if e = atomicio.Write(s.path(meta.ID)+".tree", append(b, '\n'), 0600); e != nil {
				return e
			}
		}
		return s.indexSession(meta, messages, false)
	})
}

func (s Store) List(scope string) ([]Session, error) {
	files, err := filepath.Glob(filepath.Join(s.Dir, "archive", "*.json"))
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(files))
	for _, p := range files {
		b, e := os.ReadFile(p)
		if e != nil {
			return nil, e
		}
		var a archive
		if e = json.Unmarshal(b, &a); e != nil {
			return nil, fmt.Errorf("corrupt archive %s: %w", p, e)
		}
		if a.Session.Messages != len(a.Messages) || s.path(a.Session.ID) != p {
			return nil, errors.New("conversation archive identity/count mismatch")
		}
		if scope == "" || a.Session.Scope == scope {
			out = append(out, a.Session)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Updated == out[j].Updated {
			return out[i].ID < out[j].ID
		}
		return out[i].Updated > out[j].Updated
	})
	return out, nil
}

// Reindex repairs only the disposable search projection, never originals.
func (s Store) Reindex() error {
	return leasefile.WithGuard(filepath.Join(s.Dir, "capture.lock"), func() error {
		sessions, err := s.List("")
		if err != nil {
			return err
		}
		// One transaction replaces the entire projection, removing deleted
		// archives too. Readers see either complete generation, never a mix.
		db, err := s.db(true)
		if err != nil {
			return err
		}
		defer db.Close()
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.Exec("DELETE FROM messages; DELETE FROM generations"); err != nil {
			return err
		}
		for _, meta := range sessions {
			a, e := s.load(meta.ID)
			if e != nil {
				return e
			}
			if e = indexMessages(tx, meta, a.Messages); e != nil {
				return e
			}
		}
		return tx.Commit()
	})
}
