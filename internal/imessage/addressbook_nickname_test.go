package imessage

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func ns(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// abRecord is one seeded AddressBook contact for seedAddressBook.
type abRecord struct {
	first, last, org, nick string
	phone, email           string
}

// seedAddressBook writes a temp AddressBook source DB (Sources/<uuid>/AddressBook-v22.abcddb)
// with the ZABCD* schema INCLUDING ZNICKNAME, and returns the Sources root for NewResolver.
func seedAddressBook(t *testing.T, records ...abRecord) string {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "ABCSOURCE")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(src, addressBookFilename)
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open ab db: %v", err)
	}
	defer db.Close()
	for _, s := range []string{
		`CREATE TABLE ZABCDRECORD (Z_PK INTEGER PRIMARY KEY, ZFIRSTNAME TEXT, ZLASTNAME TEXT, ZORGANIZATION TEXT, ZNICKNAME TEXT)`,
		`CREATE TABLE ZABCDPHONENUMBER (ZOWNER INTEGER, ZFULLNUMBER TEXT)`,
		`CREATE TABLE ZABCDEMAILADDRESS (ZOWNER INTEGER, ZADDRESS TEXT)`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create ab schema: %v", err)
		}
	}
	for i, rec := range records {
		pk := int64(i + 1)
		if _, err := db.Exec(`INSERT INTO ZABCDRECORD (Z_PK, ZFIRSTNAME, ZLASTNAME, ZORGANIZATION, ZNICKNAME) VALUES (?,?,?,?,?)`,
			pk, nullable(rec.first), nullable(rec.last), nullable(rec.org), nullable(rec.nick)); err != nil {
			t.Fatalf("insert record: %v", err)
		}
		if rec.phone != "" {
			if _, err := db.Exec(`INSERT INTO ZABCDPHONENUMBER (ZOWNER, ZFULLNUMBER) VALUES (?,?)`, pk, rec.phone); err != nil {
				t.Fatalf("insert phone: %v", err)
			}
		}
		if rec.email != "" {
			if _, err := db.Exec(`INSERT INTO ZABCDEMAILADDRESS (ZOWNER, ZADDRESS) VALUES (?,?)`, pk, rec.email); err != nil {
				t.Fatalf("insert email: %v", err)
			}
		}
	}
	return root
}

// TestComposeNameFallsBackToNickname locks the fallback order: first+last wins,
// then organization, then the nickname. A contact saved with ONLY a nickname must
// resolve to that nickname (not the empty string that left it as a raw handle).
func TestComposeNameFallsBackToNickname(t *testing.T) {
	cases := []struct{ name, first, last, org, nick, want string }{
		{"first+last wins over nickname", "Robert", "Smith", "", "Bug", "Robert Smith"},
		{"org before nickname", "", "", "Acme Inc", "Bug", "Acme Inc"},
		{"nickname-only fallback", "", "", "", "Bug", "Bug"},
		{"all empty stays empty", "", "", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := composeName(ns(c.first), ns(c.last), ns(c.org), ns(c.nick)); got != c.want {
				t.Fatalf("composeName(%q,%q,%q,%q) = %q, want %q", c.first, c.last, c.org, c.nick, got, c.want)
			}
		})
	}
}

// TestAddressBookResolvesNicknameOnly is Neil's exact bug: a friend saved in
// Contacts under ONLY a nickname showed as a raw phone number in `mora graph`.
// The nickname must now resolve; normal contacts must not regress.
func TestAddressBookResolvesNicknameOnly(t *testing.T) {
	root := seedAddressBook(t,
		abRecord{nick: "Bug", phone: "+14155551234"},                 // nickname-only (the bug)
		abRecord{first: "Real", last: "Name", phone: "+19998887777"}, // normal contact
	)
	r, err := NewResolver(root)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if got := r.Resolve("+14155551234"); got != "Bug" {
		t.Fatalf("nickname-only contact should resolve to %q, got %q (raw phone = Neil's bug)", "Bug", got)
	}
	if got := r.Resolve("+19998887777"); got != "Real Name" {
		t.Fatalf("normal contact regressed: got %q, want %q", got, "Real Name")
	}
}

// TestAddressBookWithoutNicknameColumn guards schema-defensiveness: an older
// AddressBook whose ZABCDRECORD lacks ZNICKNAME must still resolve normal contacts
// (the optional nickname query degrades to SELECT NULL, never breaks the source).
func TestAddressBookWithoutNicknameColumn(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "OLDSOURCE")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(src, addressBookFilename))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE ZABCDRECORD (Z_PK INTEGER PRIMARY KEY, ZFIRSTNAME TEXT, ZLASTNAME TEXT, ZORGANIZATION TEXT)`, // no ZNICKNAME
		`CREATE TABLE ZABCDPHONENUMBER (ZOWNER INTEGER, ZFULLNUMBER TEXT)`,
		`CREATE TABLE ZABCDEMAILADDRESS (ZOWNER INTEGER, ZADDRESS TEXT)`,
		`INSERT INTO ZABCDRECORD (Z_PK, ZFIRSTNAME, ZLASTNAME) VALUES (1, 'Real', 'Name')`,
		`INSERT INTO ZABCDPHONENUMBER (ZOWNER, ZFULLNUMBER) VALUES (1, '+14155551234')`,
	} {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed old schema: %v", err)
		}
	}
	db.Close()

	r, err := NewResolver(root)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	if got := r.Resolve("+14155551234"); got != "Real Name" {
		t.Fatalf("schema without ZNICKNAME must still resolve normal contacts; got %q", got)
	}
}
