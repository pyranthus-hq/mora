package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pyranthus-hq/mora/internal/atomicio"
	"github.com/pyranthus-hq/mora/internal/memory"
)

func watchFixture(t *testing.T) Config {
	t.Helper()
	cfg := coreBIngestInitCfg(t)
	source := Source{Name: "imessage", Type: "imessage", Scope: "personal"}
	if err := saveSources(cfg, []Source{source}); err != nil {
		t.Fatal(err)
	}
	if err := memory.SaveStatus(imessageStatusPath(cfg, "imessage"), &memory.SyncStatus{LastSuccessAt: time.Now().UTC().Format(time.RFC3339), LastSynced: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	return cfg
}
func watchMemory(t *testing.T, cfg Config, id, title string, texts ...string) Memory {
	t.Helper()
	body := ""
	evidence := []map[string]any{}
	for i, text := range texts {
		start := len(body)
		body += "Alex: " + text
		evidence = append(evidence, map[string]any{"evidence_ref": id + fmt.Sprintf("#m%d", i), "at": time.Now().UTC().Add(time.Duration(i) * time.Second).Format(time.RFC3339), "sender": "Alex", "from_me": false, "block_start": start, "block_end": len(body)})
		body += "\n"
	}
	// Keep occurrence timestamps stable across repeated fixture writes.
	for i, e := range evidence {
		e["at"] = fmt.Sprintf("2026-11-13T12:%02d:00Z", i)
	}
	m := Memory{ID: id, Title: title, Scope: "personal", Type: "imessage", Provider: "imessage", ProviderID: strings.TrimPrefix(id, "imessage_chat/"), Source: strings.TrimPrefix(id, "imessage_chat/"), CreatedAt: time.Now().UTC().Format(time.RFC3339), Text: strings.TrimSuffix(body, "\n"), Meta: map[string]any{"message_evidence_schema": 1, "message_evidence": evidence, "message_count": len(texts)}, ContentHash: fmt.Sprintf("hash-%s-%v", id, texts)}
	b, err := renderMemory(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sourcesRoot(cfg), "imessage", memory.SafeFilename(id)+".md")
	if err := atomicio.Write(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return m
}
func watchRun(t *testing.T, args ...string) map[string]json.RawMessage {
	t.Helper()
	args = append(append([]string{"watch"}, args...), "--json")
	out, stderr, err := runSplit(t, args...)
	if err != nil {
		t.Fatalf("%v: %v (%s)", args, err, stderr)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}
func watchCheck(t *testing.T, args ...string) chatWatchReport {
	t.Helper()
	doc := watchRun(t, append([]string{"check"}, args...)...)
	b, _ := json.Marshal(doc)
	var r chatWatchReport
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}
func TestWatchLifecycle(t *testing.T) {
	cfg := watchFixture(t)
	a := "imessage_chat/a"
	b := "imessage_chat/b"
	watchMemory(t, cfg, a, "Same name", "old")
	watchMemory(t, cfg, b, "Same name", "other old")
	sourceBefore, _ := os.ReadFile(filepath.Join(cfg.ConfigDir, "sources.json"))
	chats := watchRun(t, "chats", "--query", "Same name")
	var listed []map[string]string
	if err := json.Unmarshal(chats["chats"], &listed); err != nil || len(listed) != 2 {
		t.Fatalf("chats: %s %v", chats["chats"], err)
	}
	watchRun(t, "add", a)
	watchRun(t, "add", b)
	watchRun(t, "list")
	if r := watchCheck(t, a); len(r.Watches) != 1 || len(r.Watches[0].Messages) != 0 {
		t.Fatalf("baseline: %+v", r)
	}
	watchMemory(t, cfg, a, "Renamed group", "old", "first new", "second new")
	watchMemory(t, cfg, b, "Same name", "other old", "other new")
	r := watchCheck(t, a, "--limit", "1")
	if len(r.Watches) != 1 || len(r.Watches[0].Messages) != 1 || r.Watches[0].More != 1 || r.Watches[0].Messages[0].EvidenceRef != a+"#m1" {
		t.Fatalf("preview: %+v", r)
	}
	watchRun(t, "add", a) // Re-adding must not reset pending messages.
	r = watchCheck(t, a, "--ack", "--limit", "1")
	if !r.AckRequested {
		t.Fatal("ack request missing")
	}
	r = watchCheck(t, a)
	if len(r.Watches[0].Messages) != 1 || r.Watches[0].Messages[0].EvidenceRef != a+"#m2" {
		t.Fatalf("ack consumed unseen tail: %+v", r)
	}
	if r := watchCheck(t, b); len(r.Watches[0].Messages) != 1 {
		t.Fatal("other watch consumed")
	}
	watchCheck(t, a, "--ack")
	watchMemory(t, cfg, a, "Renamed group", "edited old", "first new", "second new")
	r = watchCheck(t, a)
	if len(r.Watches[0].Messages) != 1 || r.Watches[0].Messages[0].Change != "updated" {
		t.Fatalf("edit: %+v", r)
	}
	watchRun(t, "remove", a)
	if _, err := findMemory(cfg, a); err != nil {
		t.Fatal("removing a watch removed imported data")
	}
	sourceAfter, _ := os.ReadFile(filepath.Join(cfg.ConfigDir, "sources.json"))
	if !bytes.Equal(sourceBefore, sourceAfter) {
		t.Fatal("watch changed import policy")
	}
}

type watchBrokenWriter struct{}

func (watchBrokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
func TestWatchFailuresPreserveState(t *testing.T) {
	cfg := watchFixture(t)
	id := "imessage_chat/a"
	watchMemory(t, cfg, id, "Alex", "old")
	watchRun(t, "add", id)
	watchMemory(t, cfg, id, "Alex", "old", "new")
	path, _ := chatWatchPath(cfg)
	before, _ := os.ReadFile(path)
	err := Run(testCtx(t), []string{"watch", "check", id, "--ack", "--json"}, watchBrokenWriter{}, &bytes.Buffer{}, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "broken pipe") {
		t.Fatalf("failed output = %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed output acknowledged messages")
	}
	if err := memory.SaveStatus(imessageStatusPath(cfg, "imessage"), &memory.SyncStatus{LastSuccessAt: time.Now().Add(-72 * time.Hour).Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	r := watchCheck(t, id)
	if r.Watches[0].State == healthFresh || r.Watches[0].Warning == "" || len(r.Watches[0].Messages) != 1 {
		t.Fatalf("stale result: %+v", r)
	}
	if _, _, err := runSplit(t, "watch", "check", id, "--ack"); err == nil {
		t.Fatal("stale acknowledgement accepted")
	}
	after, _ = os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("stale acknowledgement changed state")
	}
	if err := os.WriteFile(path, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runSplit(t, "watch", "list", "--json"); err == nil {
		t.Fatal("corrupt state hidden")
	}
}
func TestWatchUsageAndRefusals(t *testing.T) {
	cfg := watchFixture(t)
	for _, args := range [][]string{{}, {"bogus"}, {"add"}, {"list", "extra"}, {"check", "a", "b"}, {"check", "--limit", "0"}, {"check", "--limit", "101"}, {"list", "--ack"}, {"check", "unknown"}, {"add", "does-not-exist"}, {"chats", "--query", "x", "extra"}} {
		full := append([]string{"watch"}, args...)
		if _, _, err := runSplit(t, full...); err == nil {
			t.Fatalf("accepted %v", full)
		}
	}
	m := watchMemory(t, cfg, "imessage_chat/bad", "bad", "one")
	delete(m.Meta, "message_evidence")
	if _, err := chatWatchRows(m); err == nil {
		t.Fatal("missing evidence accepted")
	}
	m.DeletedAt = time.Now().Format(time.RFC3339)
	if _, err := chatWatchRows(m); err == nil {
		t.Fatal("deleted memory accepted")
	}
	watchRun(t, "remove", "imessage_chat/absent")
}
func TestWatchDigestIsScopedBudgetedAndIndependent(t *testing.T) {
	cfg := watchFixture(t)
	id := "imessage_chat/a"
	watchMemory(t, cfg, id, "Alex", "old")
	watchRun(t, "add", id)
	m := watchMemory(t, cfg, id, "Alex", "old", "new")
	path, _ := chatWatchPath(cfg)
	before, _ := os.ReadFile(path)
	d := Digest{Generated: time.Now().Format(time.RFC3339)}
	if err := appendChatWatchDigest(cfg, time.Now(), &d, map[string][]Memory{"imessage": {m}}, true); err != nil {
		t.Fatal(err)
	}
	if len(d.Sections) != 1 || len(d.Sections[0].Items) != 1 || d.Sections[0].Items[0].ID != id {
		t.Fatalf("watch summary: %+v", d)
	}
	_, survived := budgetDigestForMarkdown(d, 10000)
	if survived[id] {
		t.Fatal("watch summary acknowledged global brief item")
	}
	payload := digestMCPPayload(cfg, d, 10000)
	bs, _ := json.Marshal(payload)
	var got struct {
		Sections []DigestSection `json:"sections"`
	}
	if err := json.Unmarshal(bs, &got); err != nil || len(got.Sections) != 1 {
		t.Fatalf("MCP watch: %s %v", bs, err)
	}
	filtered := Digest{}
	if err := appendChatWatchDigest(cfg, time.Now(), &filtered, map[string][]Memory{}, false); err != nil {
		t.Fatal(err)
	}
	if len(filtered.Sections) != 0 {
		t.Fatal("watch bypassed digest filters")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("brief preview changed watch state")
	}
	watchCheck(t, id, "--ack")
	d = Digest{}
	if err := appendChatWatchDigest(cfg, time.Now(), &d, map[string][]Memory{"imessage": {m}}, true); err != nil {
		t.Fatal(err)
	}
	if len(d.Sections) != 0 {
		t.Fatal("acknowledged update still in brief")
	}
}
func TestWatchRetainedHistoryAndVaultIsolation(t *testing.T) {
	cfg := watchFixture(t)
	id := "imessage_chat/a"
	m := watchMemory(t, cfg, id, "Alex", "old", "recent")
	watchRun(t, "add", id)
	store, _, err := loadChatWatches(cfg)
	if err != nil {
		t.Fatal(err)
	}
	w := store.Watches[id]
	// Dropping old history and moving byte offsets must not report a deletion/edit.
	raw := m.Meta["message_evidence"].([]map[string]any)
	row := raw[1]
	m.Text = "Alex: recent"
	row["block_start"] = 0
	row["block_end"] = len(m.Text)
	m.Meta["message_evidence"] = []map[string]any{row}
	m.Truncated = true
	u := chatWatchChanges(w, m, sourceHealth{State: healthFresh}, 20)
	if len(u.Messages) != 0 || !u.CoverageLimited {
		t.Fatalf("retained history: %+v", u)
	}
	other := cfg
	other.VaultDir = t.TempDir()
	empty, _, err := loadChatWatches(other)
	if err != nil || len(empty.Watches) != 0 {
		t.Fatalf("vault isolation: %+v %v", empty, err)
	}
	loaded, _, err := loadChatWatches(cfg)
	if err != nil || !reflect.DeepEqual(store, loaded) {
		t.Fatal("watch did not survive reload")
	}
}

func TestWatchBriefIntegrationAndCache(t *testing.T) {
	cfg := watchFixture(t)
	id := "imessage_chat/a"
	watchMemory(t, cfg, id, "Alex", "old")
	watchRun(t, "add", id)
	watchMemory(t, cfg, id, "Alex", "old", "new")
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	steadyStateSnapshots(t, cfg, now, "imessage")
	globalBefore, err := os.ReadFile(briefPath(cfg, "imessage"))
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicio.Write(briefArtifactPath(cfg, now), []byte("# Cached brief\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, generated, err := resolveBrief(cfg, now, briefOpts{}); err != nil || !generated {
		t.Fatalf("active watch cache: generated=%v err=%v", generated, err)
	}
	d, err := buildDigest(cfg, now, briefOpts{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range d.Sections {
		if s.Source == "imessage:watched" {
			found = true
		}
	}
	if !found {
		t.Fatal("actual digest omitted watch")
	}
	scoped, err := buildDigest(cfg, now, briefOpts{scope: "project:unrelated"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range scoped.Sections {
		if s.Source == "imessage:watched" {
			t.Fatal("watch escaped project filter")
		}
	}
	watchCheck(t, id, "--ack")
	globalAfter, _ := os.ReadFile(briefPath(cfg, "imessage"))
	if !bytes.Equal(globalBefore, globalAfter) {
		t.Fatal("watch acknowledgement advanced daily brief")
	}
	watchRun(t, "remove", id)
	if err := atomicio.Write(briefArtifactPath(cfg, now), []byte("# Cached brief\n\n## Watched iMessage chats (1)\nold watch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, generated, err := resolveBrief(cfg, now, briefOpts{}); err != nil || !generated {
		t.Fatalf("removed watch cache: generated=%v err=%v", generated, err)
	}
}

func TestWatchConcurrentAdds(t *testing.T) {
	cfg := watchFixture(t)
	ids := []string{"imessage_chat/a", "imessage_chat/b"}
	for _, id := range ids {
		watchMemory(t, cfg, id, id, "baseline")
	}
	errs := make(chan error, 2)
	ctx := testCtx(t)
	for _, id := range ids {
		go func(id string) {
			errs <- Run(ctx, []string{"watch", "add", id, "--json"}, &bytes.Buffer{}, &bytes.Buffer{}, strings.NewReader(""))
		}(id)
	}
	for range ids {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	s, _, err := loadChatWatches(cfg)
	if err != nil || len(s.Watches) != 2 {
		t.Fatalf("concurrent adds: %+v %v", s, err)
	}
}

func TestWatchUnavailableDoesNotBreakBrief(t *testing.T) {
	cfg := watchFixture(t)
	id := "imessage_chat/a"
	m := watchMemory(t, cfg, id, "Alex", "old")
	watchRun(t, "add", id)
	path, _ := chatWatchPath(cfg)
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := Digest{Sections: []DigestSection{{Source: "gmail", Items: []DigestItem{{ID: "mail", Title: "Kept"}}}}}
	if err := appendChatWatchDigest(cfg, time.Now(), &d, map[string][]Memory{"imessage": {m}}, true); err != nil {
		t.Fatal(err)
	}
	if len(d.Sections) != 2 || d.Sections[0].State != watchStateUnavailable || d.Sections[1].Items[0].ID != "mail" {
		t.Fatalf("degraded brief: %+v", d)
	}
	filtered := Digest{}
	if err := appendChatWatchDigest(cfg, time.Now(), &filtered, nil, false); err != nil || len(filtered.Sections) != 0 {
		t.Fatalf("filtered state disclosure: %+v %v", filtered, err)
	}
}

func TestWatchAckPersistenceFailureAndIdentityFence(t *testing.T) {
	cfg := watchFixture(t)
	id := "imessage_chat/a"
	watchMemory(t, cfg, id, "Alex", "old")
	watchRun(t, "add", id)
	watchMemory(t, cfg, id, "Alex", "old", "new")
	path, _ := chatWatchPath(cfg)
	before, _ := os.ReadFile(path)
	original := atomicio.MarkerSyncFn
	atomicio.MarkerSyncFn = func(*os.File) error { return errors.New("simulated disk failure") }
	out, _, err := runSplit(t, "watch", "check", id, "--ack", "--json")
	atomicio.MarkerSyncFn = original
	if err == nil {
		t.Fatal("failed persistence succeeded")
	}
	var report chatWatchReport
	if e := json.Unmarshal([]byte(out), &report); e != nil || !report.AckRequested {
		t.Fatalf("ack intent: %+v %v", report, e)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("failed save changed watch")
	}
	marker, _, err := readVaultMarker(cfg)
	if err != nil {
		t.Fatal(err)
	}
	marker.VaultID = "different-vault"
	b, _ := json.Marshal(marker)
	if err := os.WriteFile(markerPath(cfg), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadChatWatches(cfg); err == nil {
		t.Fatal("changed vault identity reused watch")
	}
}
