package imessage

import (
	"database/sql"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"testing"
)

func TestNewLiveFetcher(t *testing.T) {
	t.Run("invalid path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "nonexistent.db")
		f, err := NewLiveFetcher(path, DenyList{})
		if err == nil {
			if f != nil {
				f.Close()
			}
			t.Fatal("expected error for nonexistent path, got nil")
		}
	})

	t.Run("not a database file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "invalid.db")
		if err := os.WriteFile(path, []byte("not a database file data"), 0644); err != nil {
			t.Fatal(err)
		}
		f, err := NewLiveFetcher(path, DenyList{})
		if err == nil {
			if f != nil {
				f.Close()
			}
			t.Fatal("expected error for invalid db format, got nil")
		}
	})

	t.Run("valid db but missing message table", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "valid_no_msg.db")
		db, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec("CREATE TABLE not_message (id INTEGER);")
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
		f, err := NewLiveFetcher(path, DenyList{})
		if err == nil {
			t.Fatalf("expected error for missing message table, got nil")
		}
		if f != nil {
			f.Close()
		}
	})

	t.Run("valid db but missing columns", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "valid.db")
		db, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec("CREATE TABLE message (id INTEGER);")
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
		f, err := NewLiveFetcher(path, DenyList{})
		if err == nil {
			t.Fatalf("expected error for missing columns, got nil")
		}
		if f != nil {
			f.Close()
		}
	})

	t.Run("valid db with all required columns", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "valid_req.db")
		db, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`CREATE TABLE message (
            date INTEGER, text TEXT, attributedBody BLOB,
            is_from_me INTEGER, handle_id INTEGER,
            associated_message_type INTEGER
        );`)
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
		f, err := NewLiveFetcher(path, DenyList{})
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		defer f.Close()
		if f.hasItemType || f.hasRetracted {
			t.Fatalf("expected false for optional columns, got hasItemType=%v, hasRetracted=%v", f.hasItemType, f.hasRetracted)
		}
	})

	t.Run("valid db with optional columns", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "valid_opt.db")
		db, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`CREATE TABLE message (
            date INTEGER, text TEXT, attributedBody BLOB,
            is_from_me INTEGER, handle_id INTEGER,
            associated_message_type INTEGER,
            item_type INTEGER, date_retracted INTEGER
        );`)
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
		f, err := NewLiveFetcher(path, DenyList{})
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		defer f.Close()
		if !f.hasItemType || !f.hasRetracted {
			t.Fatalf("expected true, got hasItemType=%v, hasRetracted=%v", f.hasItemType, f.hasRetracted)
		}
	})

	t.Run("denylist initialization", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "valid_deny.db")
		db, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`CREATE TABLE message (
            date INTEGER, text TEXT, attributedBody BLOB,
            is_from_me INTEGER, handle_id INTEGER,
            associated_message_type INTEGER
        );`)
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
		f, err := NewLiveFetcher(path, DenyList{
			Contacts:      []string{"+1 (555) 123-4567", "UPPER@EXAMPLE.COM"},
			Conversations: []string{"MIXED Case Group"},
		})
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		defer f.Close()

		if !f.denyContacts["15551234567"] {
			t.Errorf("expected normalized phone contact to be in deny set")
		}
		if !f.denyContacts["upper@example.com"] {
			t.Errorf("expected normalized email contact to be in deny set")
		}
		if !f.denyConvos["mixed case group"] {
			t.Errorf("expected lowercased convo to be in deny set")
		}
	})
}
