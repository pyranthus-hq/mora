package mora

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/imessage"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func TestConnectPublicErrorsRedactPrivateCauses(t *testing.T) {
	private := "iMessage;-;+15551234567 /Users/private/vault/state.json"
	_, msg := ErrorDetails(errors.New(private))
	if strings.Contains(msg, "15551234567") || strings.Contains(msg, "/Users/") {
		t.Fatal(msg)
	}
	for _, mode := range []string{"open", "fetch", "manifest"} {
		t.Run(mode, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			defer stubIMessageReadiness(t, true)()
			orig := newIMessageFetcher
			defer func() { newIMessageFetcher = orig }()
			newIMessageFetcher = func(string, imessage.DenyList) (iMessageFetcher, error) {
				if mode == "open" {
					return nil, errors.New(private)
				}
				return connectTestFetcher{SyntheticFetcher: imessage.NewSyntheticFetcher(1, 1), fetch: func(context.Context, memory.ItemKind, memory.FetchWindow, string) (memory.Page, error) {
					return memory.Page{}, errors.New(private)
				}}, nil
			}
			cfg, err := loadConfigFor(testCtx(t))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "manifest" {
				if err := os.MkdirAll(imessageManifestPath(cfg, "imessage"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			stdout, _, err := runSplit(t, "connect", "imessage", "--json", "--progress")
			if err == nil {
				t.Fatal("expected failure")
			}
			receipt, readErr := os.ReadFile(strings.TrimSuffix(connectProgressPath(cfg, "imessage"), ".progress.json") + ".receipt.json")
			if readErr != nil {
				t.Fatal(readErr)
			}
			status, _ := os.ReadFile(imessageStatusPath(cfg, "imessage"))
			for _, output := range []string{stdout, string(receipt), string(status)} {
				for _, secret := range []string{"15551234567", "/Users/", cfg.StateDir} {
					if strings.Contains(output, secret) {
						t.Fatalf("public receipt leaked %q", secret)
					}
				}
			}
		})
	}
}

func TestSyncStatusIgnoresAllConnectSidecars(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.StateDir, "sync")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{".chats.json", ".progress.json", ".receipt.json"} {
		if err := os.WriteFile(filepath.Join(dir, "imessage-imessage"+suffix), []byte(`{"source":"phantom-sidecar"}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := memory.SaveStatus(filepath.Join(dir, "imessage-real.json"), &memory.SyncStatus{Source: "real-source"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"sync", "status"}, {"sync", "status", "--json"}} {
		out := run(t, args...)
		if strings.Contains(out, "phantom-sidecar") || !strings.Contains(out, "real-source") {
			t.Fatalf("bad status: %s", out)
		}
	}
}

func TestLivePIDProgressExpiresWithoutHeartbeat(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	p := connectProgressFile{Schema: schemaConnectProgress, SchemaVersion: 1, Source: "imessage", PID: os.Getpid(), StartedAt: now.Add(-2 * connectProgressMaxAge).Format(time.RFC3339Nano), UpdatedAt: now.Add(-connectProgressMaxAge - time.Second).Format(time.RFC3339Nano)}
	if !staleConnectProgress(p, now) {
		t.Fatal("reused live PID retained stale heartbeat")
	}
	path := connectProgressPath(cfg, "imessage")
	if err := writeConnectFile(path, p); err != nil {
		t.Fatal(err)
	}
	if err := cleanupConnectProgress(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale reservation remains: %v", err)
	}
	p.UpdatedAt = now.Format(time.RFC3339Nano)
	if staleConnectProgress(p, now) {
		t.Fatal("fresh heartbeat expired solely because start is old")
	}
}

// This boundary fixture exercises durable manifest publication and reload, while
// the seeded connector test exercises the actual SQLite assembly failure.
type manifestPartialFetcher struct {
	*imessage.SyntheticFetcher
	manifest    *imessage.Manifest
	full        bool
	fail        bool
	beforeFetch func()
}

func (f *manifestPartialFetcher) SetFetchOptions(o imessage.FetchOptions) {
	f.manifest = o.Manifest
	f.full = o.Full
	if f.manifest.Chats == nil {
		f.manifest.Chats = map[string]imessage.ChatMark{}
	}
}
func (f *manifestPartialFetcher) FetchPageContext(context.Context, memory.ItemKind, memory.FetchWindow, string) (memory.Page, error) {
	if f.beforeFetch != nil {
		f.beforeFetch()
	}
	p := memory.Page{}
	for i, id := range []string{"synthetic-chat-1", "synthetic-chat-2"} {
		if _, done := f.manifest.Chats[id]; done && !f.full {
			continue
		}
		if i == 1 && f.fail {
			p.Failed++
			continue
		}
		page, err := imessage.NewSyntheticFetcher(2, 1).FetchPage(imessage.KindIMessageChat, memory.FetchWindow{}, []string{"", "1"}[i])
		if err != nil {
			return memory.Page{}, err
		}
		it := page.Items[0]
		p.Items = append(p.Items, it)
		f.manifest.Chats[id] = imessage.ChatMark{MaxDate: 1, MinDate: 1}
	}
	return p, nil
}
func TestPartialIMessageManifestPersistsAndRetries(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	defer stubIMessageReadiness(t, true)()
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	orig := newIMessageFetcher
	defer func() { newIMessageFetcher = orig }()
	fail := true
	newIMessageFetcher = func(string, imessage.DenyList) (iMessageFetcher, error) {
		return &manifestPartialFetcher{SyntheticFetcher: imessage.NewSyntheticFetcher(0, 0), fail: fail}, nil
	}
	source := Source{Name: "imessage", Type: "imessage", Scope: "personal"}
	res, err := ingestIMessageDetailed(context.Background(), cfg, source, nil)
	if err == nil || res.Materialized != 1 || res.Failed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	man, err := imessage.LoadManifest(imessageManifestPath(cfg, "imessage"))
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Chats) != 1 || man.Chats["synthetic-chat-1"].Hash == "" {
		t.Fatalf("partial manifest: %+v", man)
	}
	st, err := memory.LoadStatus(imessageStatusPath(cfg, "imessage"))
	if err != nil {
		t.Fatal(err)
	}
	if st.LastSuccessAt != "" || st.ErrorCount != 1 {
		t.Fatalf("partial freshness: %+v", st)
	}
	fail = false
	res, err = ingestIMessageDetailed(context.Background(), cfg, source, nil)
	if err != nil || res.Examined != 1 || res.Materialized != 1 {
		t.Fatalf("retry: %+v %v", res, err)
	}
	man, err = imessage.LoadManifest(imessageManifestPath(cfg, "imessage"))
	if err != nil || len(man.Chats) != 2 {
		t.Fatalf("retry manifest: %+v %v", man, err)
	}
}
func TestIMessageManifestSaveFailureDoesNotClaimFreshness(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	defer stubIMessageReadiness(t, true)()
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	orig := newIMessageFetcher
	defer func() { newIMessageFetcher = orig }()
	newIMessageFetcher = func(string, imessage.DenyList) (iMessageFetcher, error) {
		return &manifestPartialFetcher{SyntheticFetcher: imessage.NewSyntheticFetcher(0, 0), beforeFetch: func() {
			if err := os.MkdirAll(imessageManifestPath(cfg, "imessage"), 0700); err != nil {
				t.Fatal(err)
			}
		}}, nil
	}
	_, err = ingestIMessageDetailed(context.Background(), cfg, Source{Name: "imessage", Type: "imessage", Scope: "personal"}, nil)
	if err == nil {
		t.Fatal("manifest publication unexpectedly succeeded")
	}
	st, err := memory.LoadStatus(imessageStatusPath(cfg, "imessage"))
	if err != nil {
		t.Fatal(err)
	}
	if st.LastSuccessAt != "" || st.LastSynced != "" || st.ErrorCount == 0 || st.LastError == "" {
		t.Fatalf("failed publication claimed freshness: %+v", st)
	}
}

func TestFullOrWidenedPartialManifestInvalidatesOldFailedMarks(t *testing.T) {
	for _, mode := range []string{"widen", "full"} {
		t.Run(mode, func(t *testing.T) {
			withTempHome(t)
			run(t, "init")
			defer stubIMessageReadiness(t, true)()
			cfg, err := loadConfigFor(testCtx(t))
			if err != nil {
				t.Fatal(err)
			}
			orig := newIMessageFetcher
			defer func() { newIMessageFetcher = orig }()
			fail := false
			newIMessageFetcher = func(string, imessage.DenyList) (iMessageFetcher, error) {
				return &manifestPartialFetcher{SyntheticFetcher: imessage.NewSyntheticFetcher(0, 0), fail: fail}, nil
			}
			source := Source{Name: "imessage", Type: "imessage", Scope: "personal", SinceDays: 30}
			if _, err := ingestIMessageDetailed(context.Background(), cfg, source, nil); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if mode == "widen" {
				source.SinceDays = 60
			} else {
				ctx = context.WithValue(ctx, imessageFullKey{}, true)
			}
			fail = true
			res, err := ingestIMessageDetailed(ctx, cfg, source, nil)
			if err == nil || res.Failed != 1 {
				t.Fatalf("partial: %+v %v", res, err)
			}
			man, err := imessage.LoadManifest(imessageManifestPath(cfg, "imessage"))
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := man.Chats["synthetic-chat-2"]; ok {
				t.Fatal("failed full render retained old mark")
			}
			fail = false
			res, err = ingestIMessageDetailed(context.Background(), cfg, source, nil)
			if err != nil || res.Examined != 1 {
				t.Fatalf("retry skipped failed chat: %+v %v", res, err)
			}
		})
	}
}

func TestIMessageCheckpointRedactsFailedWriteAndRetries(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	defer stubIMessageReadiness(t, true)()
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	orig := newIMessageFetcher
	defer func() { newIMessageFetcher = orig }()
	checked := false
	// A file at the source directory forces a real filesystem write error with a private path.
	sourceDir := filepath.Join(cfg.VaultDir, "sources", "imessage")
	if err := os.MkdirAll(filepath.Dir(sourceDir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourceDir, []byte("block writes"), 0600); err != nil {
		t.Fatal(err)
	}
	newIMessageFetcher = func(string, imessage.DenyList) (iMessageFetcher, error) {
		return connectTestFetcher{SyntheticFetcher: imessage.NewSyntheticFetcher(0, 0), fetch: func(_ context.Context, _ memory.ItemKind, _ memory.FetchWindow, cursor string) (memory.Page, error) {
			if cursor == "" {
				return memory.Page{Items: []memory.Item{imessage.SyntheticConversationItem(1)}, NextCursor: "later"}, nil
			}
			st, err := memory.LoadStatus(imessageStatusPath(cfg, "imessage"))
			if err != nil {
				t.Fatal(err)
			}
			if st.LastError == "" || strings.Contains(st.LastError, cfg.VaultDir) || st.Checkpoint != "" {
				t.Fatalf("unsafe checkpoint: %+v", st)
			}
			checked = true
			return memory.Page{}, errors.New("later page failed")
		}}, nil
	}
	res, err := ingestIMessageDetailed(context.Background(), cfg, Source{Name: "imessage", Type: "imessage", Scope: "personal"}, nil)
	if err == nil || !checked || res.Failed != 1 {
		t.Fatalf("res=%+v err=%v checked=%v", res, err, checked)
	}
	st, _ := memory.LoadStatus(imessageStatusPath(cfg, "imessage"))
	if st.Checkpoint != "" {
		t.Fatal("retry skips failed write")
	}
}

func TestFullReadWriteFailureInvalidatesOldMarks(t *testing.T) {
	withTempHome(t)
	run(t, "init")
	defer stubIMessageReadiness(t, true)()
	cfg, err := loadConfigFor(testCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	orig := newIMessageFetcher
	defer func() { newIMessageFetcher = orig }()
	newIMessageFetcher = func(string, imessage.DenyList) (iMessageFetcher, error) {
		return &manifestPartialFetcher{SyntheticFetcher: imessage.NewSyntheticFetcher(0, 0)}, nil
	}
	source := Source{Name: "imessage", Type: "imessage", Scope: "personal"}
	if _, err := ingestIMessageDetailed(context.Background(), cfg, source, nil); err != nil {
		t.Fatal(err)
	}
	sourceDir := filepath.Join(cfg.VaultDir, "sources", "imessage")
	backup := sourceDir + "-test-backup"
	if err := os.Rename(sourceDir, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourceDir, []byte("block writes"), 0600); err != nil {
		t.Fatal(err)
	}
	res, err := ingestIMessageDetailed(context.WithValue(context.Background(), imessageFullKey{}, true), cfg, source, nil)
	if err == nil || res.Failed != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	man, err := imessage.LoadManifest(imessageManifestPath(cfg, "imessage"))
	if err != nil || len(man.Chats) != 0 {
		t.Fatalf("old marks survived failed writes: %+v %v", man, err)
	}
	if err := os.Remove(sourceDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, sourceDir); err != nil {
		t.Fatal(err)
	}
	res, err = ingestIMessageDetailed(context.Background(), cfg, source, nil)
	if err != nil || res.Examined != 2 || res.Unchanged != 2 {
		t.Fatalf("retry=%+v %v", res, err)
	}
}
