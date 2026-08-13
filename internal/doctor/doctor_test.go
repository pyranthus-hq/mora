package doctor

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDoctorFlagsTokenDirInsideVault(t *testing.T) {
	if PathsDisjoint("/a/vault", "/a/vault/tokens") {
		t.Fatal("token dir inside vault must NOT be disjoint")
	}
	if !PathsDisjoint("/a/vault", "/b/config/tokens") {
		t.Fatal("separate dirs must be disjoint")
	}
}
func TestDoctorFlagsSymlinkedTokenDirInsideVault(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privileges vary on Windows")
	}
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	tokens := filepath.Join(vault, "tokens")
	link := filepath.Join(root, "token-link")

	if err := os.MkdirAll(tokens, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(tokens, link); err != nil {
		t.Fatal(err)
	}

	if PathsDisjoint(vault, link) {
		t.Fatal("token dir symlink resolving inside vault must NOT be disjoint")
	}
}
func TestDoctorWarnsSyncedRoot(t *testing.T) {
	if !LooksSynced("/Users/x/Library/Mobile Documents/com~apple~CloudDocs/mora/tokens") {
		t.Fatal("iCloud path should be flagged as synced")
	}
	if LooksSynced("/Users/x/.config/mora/tokens") {
		t.Fatal("plain config path should not be flagged")
	}
}
func TestCoreA_HumanizeAgoAndPlural(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{-5 * time.Second, "just now"},
		{30 * time.Second, "just now"},
		{time.Minute, "1 minute ago"},
		{5 * time.Minute, "5 minutes ago"},
		{time.Hour, "1 hour ago"},
		{3 * time.Hour, "3 hours ago"},
		{24 * time.Hour, "1 day ago"},
		{72 * time.Hour, "3 days ago"},
	}
	for _, tc := range cases {
		if got := HumanizeAgo(tc.d); got != tc.want {
			t.Errorf("HumanizeAgo(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
	if got := plural(1, "row"); got != "row" {
		t.Errorf("plural(1) = %q, want row", got)
	}
	if got := plural(0, "row"); got != "rows" {
		t.Errorf("plural(0) = %q, want rows", got)
	}
}

func TestFailSummary(t *testing.T) {
	checks := []Check{{Name: "ok", OK: true, Critical: true}, {Name: "warn", OK: false}, {Name: "bad", OK: false, Critical: true}}
	if got := FailSummary(checks); got != "1 critical check(s) failed: bad" {
		t.Fatalf("FailSummary=%q", got)
	}
}
func TestPrintIMessageReadiness(t *testing.T) {
	cases := []struct {
		name, goos                      string
		exists, readable, setup, wantOK bool
		want                            string
	}{
		{"other_os", "linux", false, false, false, false, "skipping chat.db checks on linux"},
		{"missing", "darwin", false, false, false, false, "No Messages database found"},
		{"denied", "darwin", true, false, true, false, "then `mora sync imessage`"},
		{"ready", "darwin", true, true, false, true, "iMessage is ready to sync"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			stat := func(string) (os.FileInfo, error) {
				if !tc.exists {
					return nil, errors.New("missing")
				}
				return fakeInfo{}, nil
			}
			probe := func(string) (bool, error) { return tc.readable, nil }
			got := PrintIMessageReadiness(&out, tc.setup, IMessageSeams{GOOS: func() string { return tc.goos }, ChatDBPath: func() string { return "/chat.db" }, Stat: stat, ProbeReadable: probe})
			if got != tc.wantOK || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("got=(%v,%q), want ok=%v substring=%q", got, out.String(), tc.wantOK, tc.want)
			}
		})
	}
}

type fakeInfo struct{}

func (fakeInfo) Name() string       { return "chat.db" }
func (fakeInfo) Size() int64        { return 0 }
func (fakeInfo) Mode() os.FileMode  { return 0 }
func (fakeInfo) ModTime() time.Time { return time.Time{} }
func (fakeInfo) IsDir() bool        { return false }
func (fakeInfo) Sys() any           { return nil }
