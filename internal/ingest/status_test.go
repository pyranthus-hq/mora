package ingest

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestStatusPathsAndThresholds(t *testing.T) {
	cfg := config.Config{StateDir: "/state"}
	if got := StatusPath(cfg, "google", "gmail-work"); got != filepath.Join("/state", "sync", "google-gmail-work.json") {
		t.Fatalf("path=%q", got)
	}
	for _, name := range []string{"google-x.json", "applecal-x.json", "github-x.json"} {
		if got := StatusFileThreshold(name); got != 24*time.Hour {
			t.Errorf("%q=%v", name, got)
		}
	}
	for _, name := range []string{"imessage-x.json", "filesystem-x.json", "future-x.json"} {
		if got := StatusFileThreshold(name); got != 48*time.Hour {
			t.Errorf("%q=%v", name, got)
		}
	}
}
func TestStatusFileState(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		st        memory.SyncStatus
		threshold time.Duration
		want      string
	}{{"never", memory.SyncStatus{}, time.Hour, StateNever}, {"malformed", memory.SyncStatus{LastSuccessAt: "bad"}, time.Hour, StateNever}, {"error", memory.SyncStatus{LastSuccessAt: now.Format(time.RFC3339), LastError: "boom"}, time.Hour, StateFailed}, {"count", memory.SyncStatus{LastSuccessAt: now.Format(time.RFC3339), ErrorCount: 1}, time.Hour, StateFailed}, {"fresh", memory.SyncStatus{LastSuccessAt: now.Add(-time.Hour).Format(time.RFC3339)}, time.Hour, StateFresh}, {"stale", memory.SyncStatus{LastSuccessAt: now.Add(-time.Hour - time.Second).Format(time.RFC3339)}, time.Hour, StateStale}, {"future", memory.SyncStatus{LastSuccessAt: now.Add(time.Hour).Format(time.RFC3339)}, time.Minute, StateFresh}}
	for _, tc := range cases {
		if got := StatusFileState(&tc.st, tc.threshold, now); got != tc.want {
			t.Errorf("%s=%q", tc.name, got)
		}
	}
}
func TestPersistStatusErrorPrecedence(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "file")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(blocked, "status.json")
	ingErr := errors.New("ingest")
	saveErr, got := PersistStatus(path, &memory.SyncStatus{}, ingErr)
	if saveErr == nil || !errors.Is(got, ingErr) {
		t.Fatalf("both=(%v,%v)", saveErr, got)
	}
	saveErr, got = PersistStatus(path, &memory.SyncStatus{}, nil)
	if saveErr == nil || got == nil || got.Error()[:23] != "persisting sync status:" {
		t.Fatalf("save-only=(%v,%v)", saveErr, got)
	}
	okpath := filepath.Join(root, "sync", "ok.json")
	saveErr, got = PersistStatus(okpath, &memory.SyncStatus{Source: "gmail"}, ingErr)
	if saveErr != nil || !errors.Is(got, ingErr) {
		t.Fatalf("success=(%v,%v)", saveErr, got)
	}
}
func TestSourceFreshnessUsesSourceAndLegacyFilename(t *testing.T) {
	cfg := config.Config{StateDir: t.TempDir()}
	dir := filepath.Join(cfg.StateDir, "sync")
	if err := memory.SaveStatus(filepath.Join(dir, "google-gmail.json"), &memory.SyncStatus{Source: "mail-work", LastSynced: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := memory.SaveStatus(filepath.Join(dir, "imessage-local.json"), &memory.SyncStatus{LastSynced: "two"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := SourceFreshness(cfg)
	want := map[string]string{"mail-work": "one", "local": "two"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got=%v", got)
	}
}

func TestSourceFreshnessIgnoresConnectSidecars(t *testing.T) {
	for _, name := range []string{"imessage", "work"} {
		t.Run(name, func(t *testing.T) {
			cfg := config.Config{StateDir: t.TempDir()}
			dir := filepath.Join(cfg.StateDir, "sync")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			const synced = "2026-09-11T12:00:00Z"
			if err := memory.SaveStatus(StatusPath(cfg, "imessage", name), &memory.SyncStatus{Source: name, LastSynced: synced}); err != nil {
				t.Fatal(err)
			}
			for _, suffix := range []string{".progress.json", ".receipt.json", ".chats.json"} {
				// Real detached sidecars name the connector, including for named sources.
				if err := os.WriteFile(filepath.Join(dir, "imessage-"+name+suffix), []byte(`{"source":"imessage"}`), 0600); err != nil {
					t.Fatal(err)
				}
				// Empty-source sidecars must not create legacy-filename keys either.
				if err := os.WriteFile(filepath.Join(dir, "imessage-empty"+suffix), []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			want := map[string]string{name: synced}
			if got := SourceFreshness(cfg); !reflect.DeepEqual(got, want) {
				t.Fatalf("freshness=%v, want %v", got, want)
			}
		})
	}
}
