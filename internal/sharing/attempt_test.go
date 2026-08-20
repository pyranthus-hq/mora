package sharing

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
)

func attemptPath(s AttemptStore, name string) string {
	return filepath.Join(SubscriptionRoot(s.DataDir, name), "attempt.json")
}
func TestAttemptStoreStartLoadExactRecordAndDurabilityOrder(t *testing.T) {
	base := time.Date(2026, 2, 3, 4, 5, 6, 987, time.FixedZone("x", 3600))
	var trace []string
	s := AttemptStore{DataDir: t.TempDir(), FileSync: func(f *os.File) error { trace = append(trace, "file"); return f.Sync() }, StartDirSync: func(dir string) error { trace = append(trace, "dir"); return atomicio.SyncDir(dir) }}
	if err := s.Start("team", "run-1", base); err != nil {
		t.Fatal(err)
	}
	if strings.Join(trace, ",") != "file,dir" {
		t.Fatal(trace)
	}
	want := "{\n  \"run_id\": \"run-1\",\n  \"state\": \"active\",\n  \"started_at\": \"2026-02-03T03:05:06Z\"\n}\n"
	body, err := os.ReadFile(attemptPath(s, "team"))
	if err != nil || string(body) != want {
		t.Fatalf("body=%q err=%v", body, err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(attemptPath(s, "team"))
		if info.Mode().Perm() != 0600 {
			t.Fatalf("mode=%o", info.Mode().Perm())
		}
	}
	got, ok, err := s.Load("team")
	if err != nil || !ok || got.RunID != "run-1" || got.State != "active" {
		t.Fatalf("got=%+v ok=%v err=%v", got, ok, err)
	}
}
func TestAttemptStoreStartFailuresAreSurfaced(t *testing.T) {
	boom := errors.New("boom")
	s := AttemptStore{DataDir: t.TempDir(), FileSync: func(*os.File) error { return boom }}
	if err := s.Start("x", "r", time.Now()); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, err := os.Stat(attemptPath(s, "x")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published after fsync error: %v", err)
	}
	s = AttemptStore{DataDir: t.TempDir(), StartDirSync: func(string) error { return boom }}
	if err := s.Start("x", "r", time.Now()); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, err := os.Stat(attemptPath(s, "x")); err != nil {
		t.Fatalf("rename should precede dir sync error: %v", err)
	}
	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := (AttemptStore{DataDir: parent}).Start("x", "r", time.Now()); err == nil {
		t.Fatal("mkdir error swallowed")
	}
}
func TestAttemptStoreLoadFailsClosed(t *testing.T) {
	s := AttemptStore{DataDir: t.TempDir()}
	if got, ok, err := s.Load("none"); err != nil || ok || got.RunID != "" {
		t.Fatalf("%+v %v %v", got, ok, err)
	}
	path := attemptPath(s, "x")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, body, want string }{{"json", "{", "unreadable"}, {"incomplete", "{}", "incomplete"}, {"state", "{\"run_id\":\"r\",\"started_at\":\"x\",\"state\":\"wat\"}", "invalid state"}, {"seq", "{\"run_id\":\"r\",\"started_at\":\"x\",\"state\":\"succeeded\"}", "no committed sequence"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.Load("x"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatal(err)
			}
		})
	}
	if err := os.WriteFile(path, []byte(`{"run_id":"r","started_at":"x","state":"failed"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.Load("x"); err != nil || !ok || got.State != "failed" {
		t.Fatalf("%+v %v %v", got, ok, err)
	}
	claim := path + ".claim-r"
	if err := os.WriteFile(claim, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Load("x"); err == nil || !strings.Contains(err.Error(), "transition is incomplete") {
		t.Fatal(err)
	}
}
func TestAttemptStoreFinishSuccessAndOwnershipFencing(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	s := AttemptStore{DataDir: t.TempDir(), Now: func() time.Time { return now }}
	if err := s.Start("x", "r", now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish("x", "r", Attempt{State: "succeeded", Seq: 9}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Load("x")
	if err != nil || !ok || got.State != "succeeded" || got.Seq != 9 || got.CompletedAt != "2026-03-04T05:06:07Z" || got.StartedAt != "2026-03-04T04:06:07Z" {
		t.Fatalf("%+v %v %v", got, ok, err)
	}
	if err := s.Finish("x", "r", Attempt{State: "failed"}); !errors.Is(err, ErrAttemptOwnershipLost) {
		t.Fatal(err)
	}
	if err := s.Finish("x", "r", Attempt{State: "active"}); err == nil || !strings.Contains(err.Error(), "invalid terminal") {
		t.Fatal(err)
	}
	s2 := AttemptStore{DataDir: t.TempDir()}
	if err := s2.Finish("none", "r", Attempt{State: "failed"}); !errors.Is(err, ErrAttemptOwnershipLost) {
		t.Fatal(err)
	}
	path := attemptPath(s2, "bad")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s2.Finish("bad", "r", Attempt{State: "failed"}); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatal(err)
	}
}
func TestAttemptStoreRecoverClaims(t *testing.T) {
	t.Run("lone", func(t *testing.T) {
		s := AttemptStore{DataDir: t.TempDir()}
		path := attemptPath(s, "x")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		claim := path + ".claim-r"
		if err := os.WriteFile(claim, []byte(`{"run_id":"r","state":"active","started_at":"x"}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.RecoverClaims("x"); err != nil {
			t.Fatal(err)
		}
		if got, ok, err := s.Load("x"); err != nil || !ok || got.RunID != "r" {
			t.Fatalf("%+v %v %v", got, ok, err)
		}
	})
	t.Run("authoritative_record_discards_claim", func(t *testing.T) {
		s := AttemptStore{DataDir: t.TempDir()}
		if err := s.Start("x", "new", time.Now()); err != nil {
			t.Fatal(err)
		}
		claim := attemptPath(s, "x") + ".claim-old"
		if err := os.WriteFile(claim, []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.RecoverClaims("x"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(claim); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	})
	t.Run("ambiguous", func(t *testing.T) {
		s := AttemptStore{DataDir: t.TempDir()}
		path := attemptPath(s, "x")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{"a", "b"} {
			if err := os.WriteFile(path+".claim-"+n, []byte(n), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.RecoverClaims("x"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatal(err)
		}
	})
}
func TestAttemptStoreStaleCompleterCannotReplaceSuccessor(t *testing.T) {
	s := AttemptStore{DataDir: t.TempDir()}
	if err := s.Start("x", "old", time.Now()); err != nil {
		t.Fatal(err)
	}
	path := attemptPath(s, "x")
	successor := []byte(`{"run_id":"new","state":"active","started_at":"x"}`)
	s.ClaimExclusive = func(temp, dest string) error {
		if dest == path {
			if err := os.WriteFile(dest, successor, 0600); err != nil {
				return err
			}
			return os.ErrExist
		}
		return atomicio.ClaimExclusiveDurable(temp, dest)
	}
	if err := s.Finish("x", "old", Attempt{State: "failed"}); !errors.Is(err, ErrAttemptOwnershipLost) {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != string(successor) {
		t.Fatalf("successor=%q err=%v", body, err)
	}
	claims, err := s.ClaimPaths("x")
	if err != nil || len(claims) != 0 {
		t.Fatalf("claims=%v err=%v", claims, err)
	}
}

func TestAttemptStoreFilesystemAndRecoveryErrors(t *testing.T) {
	readErr := errors.New("readdir")
	bad := AttemptStore{DataDir: t.TempDir(), ReadDir: func(string) ([]os.DirEntry, error) { return nil, readErr }}
	if _, err := bad.ClaimPaths("x"); !errors.Is(err, readErr) {
		t.Fatal(err)
	}
	if _, _, err := bad.Load("x"); !errors.Is(err, readErr) || !strings.Contains(err.Error(), "checking attempt transition debris") {
		t.Fatal(err)
	}
	if err := bad.RecoverClaims("x"); !errors.Is(err, readErr) {
		t.Fatal(err)
	}
	s := AttemptStore{DataDir: t.TempDir()}
	path := attemptPath(s, "x")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Load("x"); err == nil || !strings.Contains(err.Error(), "reading attempt record") {
		t.Fatal(err)
	}
	t.Run("claim", func(t *testing.T) {
		boom := errors.New("claim")
		s := AttemptStore{DataDir: t.TempDir(), ClaimExclusive: func(string, string) error { return boom }}
		path := attemptPath(s, "x")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".claim-r", []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.RecoverClaims("x"); !errors.Is(err, boom) {
			t.Fatal(err)
		}
	})
	t.Run("sync", func(t *testing.T) {
		boom := errors.New("sync")
		calls := 0
		s := AttemptStore{DataDir: t.TempDir(), StartDirSync: atomicio.SyncDir, DirSync: func(string) error { calls++; return boom }}
		path := attemptPath(s, "x")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".claim-r", []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.RecoverClaims("x"); !errors.Is(err, boom) || calls != 1 {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	})
	t.Run("cleanup", func(t *testing.T) {
		boom := errors.New("sync")
		s := AttemptStore{DataDir: t.TempDir(), StartDirSync: atomicio.SyncDir, DirSync: func(string) error { return boom }}
		if err := s.Start("x", "r", time.Now()); err != nil {
			t.Fatal(err)
		}
		claim := attemptPath(s, "x") + ".claim-dir"
		if err := os.MkdirAll(filepath.Join(claim, "child"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := s.RecoverClaims("x"); err == nil || !errors.Is(err, boom) {
			t.Fatal(err)
		}
	})
}
func TestAttemptStoreFinishFailureBranches(t *testing.T) {
	boom := errors.New("boom")
	t.Run("claim_rename_not_exist", func(t *testing.T) {
		s := AttemptStore{DataDir: t.TempDir(), RenameClaim: func(string, string) error { return os.ErrNotExist }}
		if err := s.Start("x", "r", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := s.Finish("x", "r", Attempt{State: "failed"}); !errors.Is(err, ErrAttemptOwnershipLost) {
			t.Fatal(err)
		}
	})
	t.Run("claim_rename_error", func(t *testing.T) {
		s := AttemptStore{DataDir: t.TempDir(), RenameClaim: func(string, string) error { return boom }}
		if err := s.Start("x", "r", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := s.Finish("x", "r", Attempt{State: "failed"}); !errors.Is(err, boom) {
			t.Fatal(err)
		}
	})
	t.Run("publish_error_leaves_claim", func(t *testing.T) {
		s := AttemptStore{DataDir: t.TempDir(), ClaimExclusive: func(string, string) error { return boom }}
		if err := s.Start("x", "r", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := s.Finish("x", "r", Attempt{State: "failed"}); !errors.Is(err, boom) {
			t.Fatal(err)
		}
		claims, _ := s.ClaimPaths("x")
		if len(claims) != 1 {
			t.Fatal(claims)
		}
	})
	t.Run("terminal_sync_error", func(t *testing.T) {
		calls := 0
		s := AttemptStore{DataDir: t.TempDir(), StartDirSync: atomicio.SyncDir, DirSync: func(string) error { calls++; return boom }}
		if err := s.Start("x", "r", time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := s.Finish("x", "r", Attempt{State: "failed"}); !errors.Is(err, boom) || calls != 1 {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	})
	t.Run("mismatch_restore_and_sync", func(t *testing.T) {
		dir := t.TempDir()
		s := AttemptStore{DataDir: dir}
		if err := s.Start("x", "old", time.Now()); err != nil {
			t.Fatal(err)
		}
		path := attemptPath(s, "x")
		successor := []byte(`{"run_id":"new","state":"active","started_at":"x"}`)
		s.RenameClaim = func(old, claim string) error {
			if err := atomicio.RenameReplaceWithRetry(old, claim); err != nil {
				return err
			}
			if err := os.WriteFile(claim, []byte("changed"), 0600); err != nil {
				return err
			}
			return os.WriteFile(path, successor, 0600)
		}
		if err := s.Finish("x", "old", Attempt{State: "failed"}); !errors.Is(err, ErrAttemptOwnershipLost) {
			t.Fatal(err)
		}
		body, _ := os.ReadFile(path)
		if string(body) != string(successor) {
			t.Fatalf("body=%q", body)
		}
	})
	t.Run("mismatch_restore_error", func(t *testing.T) {
		s := AttemptStore{DataDir: t.TempDir()}
		if err := s.Start("x", "old", time.Now()); err != nil {
			t.Fatal(err)
		}
		s.RenameClaim = func(old, claim string) error {
			if err := atomicio.RenameReplaceWithRetry(old, claim); err != nil {
				return err
			}
			return os.WriteFile(claim, []byte("changed"), 0600)
		}
		s.ClaimExclusive = func(string, string) error { return boom }
		if err := s.Finish("x", "old", Attempt{State: "failed"}); !errors.Is(err, ErrAttemptOwnershipLost) || !errors.Is(err, boom) {
			t.Fatal(err)
		}
	})
	t.Run("mismatch_sync_error", func(t *testing.T) {
		s := AttemptStore{DataDir: t.TempDir()}
		if err := s.Start("x", "old", time.Now()); err != nil {
			t.Fatal(err)
		}
		s.RenameClaim = func(old, claim string) error {
			if err := atomicio.RenameReplaceWithRetry(old, claim); err != nil {
				return err
			}
			return os.WriteFile(claim, []byte("changed"), 0600)
		}
		s.DirSync = func(string) error { return boom }
		if err := s.Finish("x", "old", Attempt{State: "failed"}); !errors.Is(err, ErrAttemptOwnershipLost) || !errors.Is(err, boom) {
			t.Fatal(err)
		}
	})
}
