package conversation

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

type Hit struct {
	Session    string `json:"session"`
	Index      int    `json:"index"`
	Scope      string `json:"scope"`
	Kind       string `json:"kind"`
	Date       string `json:"date,omitempty"`
	Excerpt    string `json:"excerpt"`
	Provenance string `json:"provenance"`
}

func (s Store) db(write bool) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(s.Dir, "index.db"))}
	if !write {
		u.RawQuery = "mode=ro"
	}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, err
	}
	if write {
		if _, err = db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS messages USING fts5(session UNINDEXED, idx UNINDEXED, scope UNINDEXED, kind UNINDEXED, date UNINDEXED, text); CREATE TABLE IF NOT EXISTS generations(session TEXT PRIMARY KEY, messages INTEGER NOT NULL)`); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}
func (s Store) indexSession(meta Session, messages []Message, force bool) error {
	db, err := s.db(true)
	if err != nil {
		return err
	}
	defer db.Close()
	var indexed int
	if e := db.QueryRow("SELECT messages FROM generations WHERE session=?", meta.ID).Scan(&indexed); !force && e == nil && indexed == len(messages) {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.Exec("DELETE FROM messages WHERE session=?", meta.ID); err != nil {
		return err
	}
	if err = indexMessages(tx, meta, messages); err != nil {
		return err
	}
	return tx.Commit()
}
func indexMessages(tx *sql.Tx, meta Session, messages []Message) error {
	st, err := tx.Prepare("INSERT INTO messages(session,idx,scope,kind,date,text) VALUES(?,?,?,?,?,?)")
	if err != nil {
		return err
	}
	defer st.Close()
	for i, m := range messages {
		if _, err = st.Exec(meta.ID, i, meta.Scope, m.Kind, m.Date, m.Text); err != nil {
			return err
		}
	}
	_, err = tx.Exec("INSERT OR REPLACE INTO generations VALUES(?,?)", meta.ID, len(messages))
	return err
}

func (s Store) Search(query, scope string, limit int) ([]Hit, error) {
	hits := []Hit{}
	if limit < 1 || limit > 100 {
		return nil, errors.New("conversation search limit must be 1..100")
	}
	terms := strings.Fields(query)
	if len(terms) == 0 {
		return hits, nil
	}
	if len(terms) > 32 {
		terms = terms[:32]
	}
	for i, t := range terms {
		terms[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
	}
	sessions, err := s.List("")
	if err != nil {
		return nil, err
	}
	if len(sessions) == 0 {
		return hits, nil
	}
	if _, err = os.Stat(filepath.Join(s.Dir, "index.db")); err != nil {
		return nil, fmt.Errorf("conversation search index unavailable; run mora conversation reindex: %w", err)
	}
	db, err := s.db(false)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var generations int
	if err = db.QueryRow("SELECT COUNT(*) FROM generations").Scan(&generations); err != nil || generations != len(sessions) {
		return nil, errors.New("conversation search index stale; run mora conversation reindex")
	}
	for _, meta := range sessions {
		var count int
		if err = db.QueryRow("SELECT messages FROM generations WHERE session=?", meta.ID).Scan(&count); err != nil || count != meta.Messages {
			return nil, errors.New("conversation search index stale; run mora conversation reindex")
		}
	}
	rows, err := db.Query(`SELECT session,idx,scope,kind,date,snippet(messages,5,'','',' [...] ',48) FROM messages WHERE messages MATCH ? AND (?='' OR scope=?) ORDER BY bm25(messages),session,CAST(idx AS INTEGER) LIMIT ?`, strings.Join(terms, " OR "), scope, scope, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h Hit
		if err = rows.Scan(&h.Session, &h.Index, &h.Scope, &h.Kind, &h.Date, &h.Excerpt); err != nil {
			return nil, err
		}
		valid := false
		for _, meta := range sessions {
			if meta.ID == h.Session && meta.Scope == h.Scope && h.Index >= 0 && h.Index < meta.Messages {
				valid = true
				break
			}
		}
		if !valid {
			return nil, errors.New("conversation search index contains an orphan; run mora conversation reindex")
		}
		h.Excerpt = preview(h.Excerpt, 1600)
		h.Provenance = "original-conversation-message:" + h.Kind
		hits = append(hits, h)
	}
	return hits, rows.Err()
}
