package sharing

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestValidation(t *testing.T) {
	for _, s := range []string{"alice", "team.one", "a-1", "a_b"} {
		if !ValidName(s) {
			t.Errorf("name %q rejected", s)
		}
	}
	for _, s := range []string{"", "Alice", "../x", "a/b", strings.Repeat("a", 65)} {
		if ValidName(s) {
			t.Errorf("name %q accepted", s)
		}
	}
	for _, s := range []string{"personal", "global", "project:acme", "project:A.B-1"} {
		if !ValidScope(s) {
			t.Errorf("scope %q rejected", s)
		}
	}
	for _, s := range []string{"", "project:", "team:x", "project:../x"} {
		if ValidScope(s) {
			t.Errorf("scope %q accepted", s)
		}
	}
}
func TestLedgerRoundTripAndPaths(t *testing.T) {
	configDir := t.TempDir()
	dataDir := t.TempDir()
	empty, err := Load(configDir)
	if err != nil || empty.Schema != LedgerSchema {
		t.Fatalf("empty=%+v err=%v", empty, err)
	}
	want := Ledger{Publishes: []Publish{{Name: "out", Scope: "project:x", Transport: &TransportRef{Kind: "bucket", Bucket: &BucketConfig{Bucket: "b", Prefix: "/p/"}}}}, Subscriptions: []Subscription{{Name: "in", Remote: "r", PinnedPubkey: []byte{1}, LastVersion: 2}}}
	if err := Save(configDir, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(configDir)
	if err != nil {
		t.Fatal(err)
	}
	want.Schema = LedgerSchema
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%+v want=%+v", got, want)
	}
	bucket := got.Publishes[0].Transport.Bucket
	if bucket.ObjectPrefix() != "p/" || bucket.Locator() != "bucket\x00\x00b\x00p" || bucket.Display() != "b/p" {
		t.Fatal("bucket canonicalization changed")
	}
	endpoint := BucketConfig{Endpoint: "https://r2.example/", Bucket: "bucket", Prefix: "/"}
	if endpoint.ObjectPrefix() != "" || endpoint.Display() != "https://r2.example/bucket" {
		t.Fatal("endpoint display or empty prefix changed")
	}
	if StagingDir(dataDir, "x") != filepath.Join(dataDir, "share", "publish", "x") || RepoDir(dataDir, "x") != filepath.Join(dataDir, "share", "subs", "x", "repo") || CorpusDir(dataDir, "x") != filepath.Join(dataDir, "share", "subs", "x", "corpus") || IndexPath(dataDir, "x") != filepath.Join(dataDir, "share", "subs", "x", "index.db") {
		t.Fatal("paths changed")
	}
	info, err := os.Stat(LedgerPath(configDir))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger mode=%v err=%v", info.Mode().Perm(), err)
	}
}
func TestLedgerRefusals(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(LedgerPath(dir), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "is corrupt") {
		t.Fatalf("corrupt error=%v", err)
	}
	ledger := Ledger{Subscriptions: []Subscription{{Name: "in"}}, Publishes: []Publish{{Name: "out"}}}
	if err := ValidateSubscriptionNameAvailable(ledger, "in"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("subscription error=%v", err)
	}
	if err := ValidateSubscriptionNameAvailable(ledger, "out"); err == nil || !strings.Contains(err.Error(), "one namespace") {
		t.Fatalf("publish error=%v", err)
	}
	if err := ValidateSubscriptionNameAvailable(ledger, "new"); err != nil {
		t.Fatal(err)
	}
}
