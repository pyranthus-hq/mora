package briefstate

import (
	"bytes"
	"github.com/pyranthus-hq/mora/internal/config"
	"github.com/pyranthus-hq/mora/internal/memory"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// brief_test.go drives the Phase-12 delta core (Plan 03): the watermark store
// (load/save/lock) and the PURE Classify() hash-diff. Written RED-first.
//
// The load-bearing invariant under test: the delta is the CONTENT-HASH set, not
// timestamps. writeMappedMemory preserves created_at on a content change, so a
// grown conversation / edited thread keeps its original created_at and only its
// content_hash moves — a created_at delta would provably miss the exact case the
// phase exists to catch (TestClassifyTimestampIndependentUpdated pins this).

// fixedNow is a deterministic instant used everywhere a now is injected.
var fixedNow = time.Date(2026, 6, 8, 15, 4, 5, 0, time.UTC)

func testCfg(t *testing.T) config.Config {
	t.Helper()
	return config.Config{StateDir: t.TempDir()}
}

// ---------------------------------------------------------------------------
// DELTA CLASSIFICATION — pure Classify(snapshot, memories) -> {new, updated}
// ---------------------------------------------------------------------------

// TestClassifySteadyState asserts the three hash-diff buckets: not-in-snapshot
// => new, in-snapshot-but-hash-differs => updated, in-snapshot-and-hash-equal =>
// skipped (not surfaced).
func TestClassifySteadyState(t *testing.T) {
	snap := Snapshot{
		Key:               "gmail",
		HashSchemaVersion: HashSchemaVersion,
		Items: map[string]string{
			"gmail_thread/aaa": "h_aaa",
			"gmail_thread/bbb": "h_bbb_old",
			"gmail_thread/ccc": "h_ccc",
		},
	}
	mems := []memory.Memory{
		{ID: "gmail_thread/aaa", Provider: "gmail", ContentHash: "h_aaa"},     // unchanged
		{ID: "gmail_thread/bbb", Provider: "gmail", ContentHash: "h_bbb_new"}, // updated
		{ID: "gmail_thread/ccc", Provider: "gmail", ContentHash: "h_ccc"},     // unchanged
		{ID: "gmail_thread/ddd", Provider: "gmail", ContentHash: "h_ddd"},     // new
	}

	d := Classify(snap, mems, fixedNow)

	if d.ColdStart {
		t.Fatalf("ColdStart=true, want false (snapshot present, matching schema)")
	}
	if d.SchemaReset {
		t.Fatalf("SchemaReset=true, want false")
	}

	changes := map[string]string{}
	for _, it := range d.Items {
		changes[it.ID] = it.Change
	}
	if got := changes["gmail_thread/ddd"]; got != "new" {
		t.Fatalf("ddd Change=%q, want new", got)
	}
	if got := changes["gmail_thread/bbb"]; got != "updated" {
		t.Fatalf("bbb Change=%q, want updated", got)
	}
	if _, surfaced := changes["gmail_thread/aaa"]; surfaced {
		t.Fatalf("aaa (unchanged) was surfaced, want skipped")
	}
	if _, surfaced := changes["gmail_thread/ccc"]; surfaced {
		t.Fatalf("ccc (unchanged) was surfaced, want skipped")
	}
	if len(d.Items) != 2 {
		t.Fatalf("len(d.Items)=%d, want 2 (new ddd + updated bbb)", len(d.Items))
	}

	// Baseline must be ALL current present hashes (caller persists this on commit),
	// NOT just the surfaced items — so unchanged ids stay in the watermark.
	wantBaseline := map[string]string{
		"gmail_thread/aaa": "h_aaa",
		"gmail_thread/bbb": "h_bbb_new",
		"gmail_thread/ccc": "h_ccc",
		"gmail_thread/ddd": "h_ddd",
	}
	if !mapsEqual(d.Baseline, wantBaseline) {
		t.Fatalf("Baseline=%v, want %v (all current hashes)", d.Baseline, wantBaseline)
	}
}

// TestClassifyTimestampIndependentUpdated is the load-bearing test: a memory
// whose created_at is OLD (unchanged) but whose content_hash moved must classify
// as "updated". This proves the delta is hash-driven, never created_at-driven.
func TestClassifyTimestampIndependentUpdated(t *testing.T) {
	oldTS := "2020-01-01T00:00:00Z"
	snap := Snapshot{
		Key:               "imessage",
		HashSchemaVersion: HashSchemaVersion,
		Items:             map[string]string{"imessage_chat/x": "old_hash"},
	}
	// Same created_at as the prior brief — only the hash grew (grown conversation).
	mems := []memory.Memory{{ID: "imessage_chat/x", Provider: "imessage", CreatedAt: oldTS, ContentHash: "grown_hash"}}

	d := Classify(snap, mems, fixedNow)

	if len(d.Items) != 1 {
		t.Fatalf("len(d.Items)=%d, want 1", len(d.Items))
	}
	if d.Items[0].Change != "updated" {
		t.Fatalf("Change=%q, want updated (hash changed despite identical OLD created_at)", d.Items[0].Change)
	}
}

// TestClassifyEmptyKeySkipped asserts a memory whose sourceInstanceKey returns
// ok=false (empty Provider — the filesystem connector) is never bucketed (M-1).
func TestClassifyEmptyKeySkipped(t *testing.T) {
	snap := Snapshot{Key: "gmail", HashSchemaVersion: HashSchemaVersion, Items: map[string]string{}}
	mems := []memory.Memory{
		{ID: "fs/note", Provider: "", ContentHash: "h_fs"},           // empty key => skip entirely
		{ID: "gmail_thread/a", Provider: "gmail", ContentHash: "ha"}, // real => new
	}

	d := Classify(snap, mems, fixedNow)

	for _, it := range d.Items {
		if it.ID == "fs/note" {
			t.Fatalf("empty-key memory fs/note was bucketed as %q, want skipped", it.Change)
		}
	}
	if _, ok := d.Baseline["fs/note"]; ok {
		t.Fatalf("empty-key memory fs/note leaked into Baseline, want skipped")
	}
	if d.Baseline["gmail_thread/a"] != "ha" {
		t.Fatalf("real memory missing from Baseline: %v", d.Baseline)
	}
}

// TestClassifyColdStart asserts D-04 semantics: no snapshot => ColdStart=true, and
// the baseline is ALL current hashes (archived backfill becomes the starting
// line, not a flood). Display-window selection is the caller's concern (Plan 04).
func TestClassifyColdStart(t *testing.T) {
	snap := Snapshot{} // zero value => no snapshot for this instance
	mems := []memory.Memory{
		{ID: "gmail_thread/a", Provider: "gmail", ContentHash: "ha"},
		{ID: "gmail_thread/b", Provider: "gmail", ContentHash: "hb"},
	}

	d := Classify(snap, mems, fixedNow)

	if !d.ColdStart {
		t.Fatalf("ColdStart=false, want true (no snapshot)")
	}
	if d.SchemaReset {
		t.Fatalf("SchemaReset=true on a plain cold start, want false")
	}
	wantBaseline := map[string]string{"gmail_thread/a": "ha", "gmail_thread/b": "hb"}
	if !mapsEqual(d.Baseline, wantBaseline) {
		t.Fatalf("cold-start Baseline=%v, want ALL current hashes %v", d.Baseline, wantBaseline)
	}
}

// TestClassifySchemaResetRebaselines asserts a snapshot whose hash_schema_version
// differs from current is cold-start-equivalent: re-baseline to all current
// hashes AND set SchemaReset so an empty post-upgrade brief isn't misread.
func TestClassifySchemaResetRebaselines(t *testing.T) {
	snap := Snapshot{
		Key:               "gmail",
		HashSchemaVersion: HashSchemaVersion + 1, // future/old schema mismatch
		Items:             map[string]string{"gmail_thread/a": "stale_under_old_scheme"},
	}
	mems := []memory.Memory{{ID: "gmail_thread/a", Provider: "gmail", ContentHash: "ha"}}

	d := Classify(snap, mems, fixedNow)

	if !d.SchemaReset {
		t.Fatalf("SchemaReset=false on a hash_schema_version mismatch, want true")
	}
	if !d.ColdStart {
		t.Fatalf("ColdStart=false on schema reset, want cold-start-equivalent true")
	}
	if len(d.Items) != 0 {
		t.Fatalf("schema reset surfaced %d items, want 0 (suppress the flood)", len(d.Items))
	}
	if d.Baseline["gmail_thread/a"] != "ha" {
		t.Fatalf("schema reset Baseline did not re-baseline to current hash: %v", d.Baseline)
	}
}

// ---------------------------------------------------------------------------
// WATERMARK STORE — load / save / corruption-recovery / byte-stability
// ---------------------------------------------------------------------------

// TestLoadBriefSnapshotMissing mirrors LoadStatus's os.ErrNotExist convention:
// a missing snapshot loads as a zero value, not an error.
func TestLoadBriefSnapshotMissing(t *testing.T) {
	cfg := testCfg(t)
	snap := Load(cfg, "gmail")
	if len(snap.Items) != 0 {
		t.Fatalf("missing snapshot had %d items, want zero value", len(snap.Items))
	}
	if snap.LastBriefAt != "" {
		t.Fatalf("missing snapshot LastBriefAt=%q, want empty", snap.LastBriefAt)
	}
}

// TestBriefPathOutsideSync asserts the watermark lives under brief/, never sync/,
// so sourceFreshness never reads it.
func TestBriefPathOutsideSync(t *testing.T) {
	cfg := testCfg(t)
	p := Path(cfg, "gmail")
	if filepath.Dir(p) != filepath.Join(cfg.StateDir, "brief") {
		t.Fatalf("briefPath dir = %q, want <StateDir>/brief", filepath.Dir(p))
	}
	if filepath.Base(p) != "gmail.json" {
		t.Fatalf("briefPath base = %q, want gmail.json", filepath.Base(p))
	}
	if bytes.Contains([]byte(p), []byte(filepath.Join("sync", ""))) {
		t.Fatalf("briefPath %q lives under sync/, must stay out so sourceFreshness never reads it", p)
	}
}

// TestSaveBriefSnapshotRoundTrip asserts a save then load preserves the logical
// snapshot.
func TestSaveBriefSnapshotRoundTrip(t *testing.T) {
	cfg := testCfg(t)
	snap := Snapshot{
		Key:               "gmail",
		HashSchemaVersion: HashSchemaVersion,
		Items:             map[string]string{"gmail_thread/a": "ha", "gmail_thread/b": "hb"},
	}
	if err := Save(cfg, snap, fixedNow); err != nil {
		t.Fatalf("saveBriefSnapshot: %v", err)
	}
	got := Load(cfg, "gmail")
	if got.Key != "gmail" || got.HashSchemaVersion != HashSchemaVersion {
		t.Fatalf("round-trip header mismatch: %+v", got)
	}
	if !mapsEqual(got.Items, snap.Items) {
		t.Fatalf("round-trip items mismatch: got %v want %v", got.Items, snap.Items)
	}
	if got.LastBriefAt != fixedNow.UTC().Format(time.RFC3339) {
		t.Fatalf("LastBriefAt=%q, want UTC RFC3339 of injected now %q", got.LastBriefAt, fixedNow.UTC().Format(time.RFC3339))
	}
}

// TestSaveBriefSnapshotPermissions asserts the file is 0600 (stores sensitive
// stableIDs — gmail_thread/<id>, imessage_chat/<guid> — at rest, T-12-06).
func TestSaveBriefSnapshotPermissions(t *testing.T) {
	cfg := testCfg(t)
	snap := Snapshot{Key: "gmail", HashSchemaVersion: HashSchemaVersion, Items: map[string]string{"gmail_thread/a": "ha"}}
	if err := Save(cfg, snap, fixedNow); err != nil {
		t.Fatalf("saveBriefSnapshot: %v", err)
	}
	fi, err := os.Stat(Path(cfg, "gmail"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	assertPermUnix(t, fi.Mode(), 0o600)
}

// TestSaveBriefSnapshotByteStable is the determinism invariant (T-12-08): two
// saves of the same logical snapshot — built with differently-ordered maps and
// re-marshaled — produce byte-identical files (sorted-key marshal, UTC string).
func TestSaveBriefSnapshotByteStable(t *testing.T) {
	cfg := testCfg(t)
	now := fixedNow

	snap1 := Snapshot{
		Key:               "gmail",
		HashSchemaVersion: HashSchemaVersion,
		Items:             map[string]string{"z": "1", "a": "2", "m": "3", "b": "4"},
	}
	if err := Save(cfg, snap1, now); err != nil {
		t.Fatalf("save 1: %v", err)
	}
	first, err := os.ReadFile(Path(cfg, "gmail"))
	if err != nil {
		t.Fatalf("read 1: %v", err)
	}

	// Reload, re-save with the SAME logical content and a different now-UTC source
	// instant that canonicalizes to the same RFC3339 string.
	snap2 := Load(cfg, "gmail")
	// Inject in a deliberately different insertion order to defeat any accidental
	// map-iteration-order dependence.
	snap2.Items = map[string]string{"b": "4", "m": "3", "a": "2", "z": "1"}
	if err := Save(cfg, snap2, now.Local()); err != nil {
		t.Fatalf("save 2: %v", err)
	}
	second, err := os.ReadFile(Path(cfg, "gmail"))
	if err != nil {
		t.Fatalf("read 2: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Fatalf("snapshot not byte-stable across saves:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
}

// TestLoadBriefSnapshotCorruptRecovers asserts T-12-05: a garbage/truncated file
// recovers as a zero/cold-start snapshot for that instance — never a fatal blank
// of the whole brief.
func TestLoadBriefSnapshotCorruptRecovers(t *testing.T) {
	cfg := testCfg(t)
	p := Path(cfg, "gmail")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte("{ this is not valid json \x00\x00 truncated"), 0o600); err != nil {
		t.Fatalf("write garbage: %v", err)
	}

	snap := Load(cfg, "gmail")
	if len(snap.Items) != 0 {
		t.Fatalf("corrupt snapshot recovered with %d items, want zero/cold-start", len(snap.Items))
	}
	if snap.LastBriefAt != "" {
		t.Fatalf("corrupt snapshot LastBriefAt=%q, want empty (cold-start-equivalent)", snap.LastBriefAt)
	}

	// And it must be cold-start-equivalent through the classifier, not steady-state.
	mems := []memory.Memory{{ID: "gmail_thread/a", Provider: "gmail", ContentHash: "ha"}}
	if d := Classify(snap, mems, fixedNow); !d.ColdStart {
		t.Fatalf("Classify(corrupt-recovered snapshot) ColdStart=false, want true (cold-start-equivalent)")
	}
}

// TestLoadBriefSnapshotSchemaMismatchRecovers asserts a persisted snapshot whose
// hash_schema_version differs from current loads as cold-start-equivalent: its
// stale items are dropped so classify re-baselines.
func TestLoadBriefSnapshotSchemaMismatchRecovers(t *testing.T) {
	cfg := testCfg(t)
	p := Path(cfg, "gmail")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Seed a well-formed snapshot whose hash_schema_version is NOT the current
	// one (saveBriefSnapshot would stamp the current version, so write raw JSON).
	staleJSON := []byte(`{"key":"gmail","last_brief_at":"2026-06-01T00:00:00Z","hash_schema_version":999,"items":{"gmail_thread/a":"old_scheme_hash"}}` + "\n")
	if err := os.WriteFile(p, staleJSON, 0o600); err != nil {
		t.Fatalf("seed stale snapshot: %v", err)
	}

	snap := Load(cfg, "gmail")
	if len(snap.Items) != 0 {
		t.Fatalf("schema-mismatch load kept %d items, want re-baseline (0)", len(snap.Items))
	}
	mems := []memory.Memory{{ID: "gmail_thread/a", Provider: "gmail", ContentHash: "ha"}}
	d := Classify(snap, mems, fixedNow)
	if !d.ColdStart {
		t.Fatalf("schema-mismatch recovered snapshot is not cold-start-equivalent")
	}
	// End-to-end: the load path preserves the post-upgrade signal so the brief can
	// say "baseline reset after upgrade" rather than be misread as broken.
	if !d.SchemaReset {
		t.Fatalf("schema-mismatch end-to-end SchemaReset=false, want true (baseline reset after upgrade)")
	}
}

// ---------------------------------------------------------------------------
// COMMIT LOCK — fail-fast OS-guard contention
// ---------------------------------------------------------------------------

// TestAcquireBriefLockExclusion asserts that while one holder owns the lock, a
// second acquire fails (does not interleave). Release lets it succeed again.
func TestAcquireBriefLockExclusion(t *testing.T) {
	cfg := testCfg(t)

	release1, err := AcquireLock(cfg)
	if err != nil {
		t.Fatalf("first acquireBriefLock: %v", err)
	}

	if _, err := AcquireLock(cfg); err == nil {
		t.Fatalf("second acquireBriefLock succeeded while first held, want failure (no interleave)")
	}

	release1()

	release2, err := AcquireLock(cfg)
	if err != nil {
		t.Fatalf("acquireBriefLock after release failed: %v", err)
	}
	release2()
}

// TestAcquireBriefLockSerializesWriters asserts the lock actually serializes two
// concurrent load->classify->write cycles: with the lock held around the whole
// cycle, no two cycles interleave (exactly one wins each contention; the loser
// errors out rather than corrupting state). Run under -race.
func TestAcquireBriefLockSerializesWriters(t *testing.T) {
	cfg := testCfg(t)

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		acquired  int
		contended int
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := AcquireLock(cfg)
			if err != nil {
				mu.Lock()
				contended++
				mu.Unlock()
				return
			}
			// Hold briefly to maximize contention overlap.
			time.Sleep(time.Millisecond)
			mu.Lock()
			acquired++
			mu.Unlock()
			release()
		}()
	}
	wg.Wait()

	if acquired+contended != 8 {
		t.Fatalf("acquired(%d)+contended(%d) != 8", acquired, contended)
	}
	if acquired == 0 {
		t.Fatalf("no goroutine ever acquired the lock")
	}
	// At least one must have been blocked out, proving exclusion held.
	if contended == 0 {
		t.Fatalf("no contention observed across 8 racing acquires; lock may not be exclusive")
	}
}

// TestBriefLockCrashHelper is a subprocess-only holder used by
// TestAcquireBriefLockReleasedByProcessDeath. A persistent guard file remains,
// but killing this process must release the kernel lock on its open handle.
func TestBriefLockCrashHelper(t *testing.T) {
	if os.Getenv("MORA_TEST_BRIEF_LOCK_HELPER") != "1" {
		return
	}
	cfg := config.Config{StateDir: os.Getenv("MORA_TEST_BRIEF_LOCK_STATE")}
	release, err := AcquireLock(cfg)
	if err != nil {
		t.Fatalf("helper acquire brief lock: %v", err)
	}
	defer release()
	if err := os.WriteFile(os.Getenv("MORA_TEST_BRIEF_LOCK_READY"), []byte("ready\n"), 0o600); err != nil {
		t.Fatalf("helper ready marker: %v", err)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestAcquireBriefLockReleasedByProcessDeath(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "state")
	ready := filepath.Join(root, "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestBriefLockCrashHelper$")
	cmd.Env = append(os.Environ(),
		"MORA_TEST_BRIEF_LOCK_HELPER=1",
		"MORA_TEST_BRIEF_LOCK_STATE="+stateDir,
		"MORA_TEST_BRIEF_LOCK_READY="+ready,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start brief-lock helper: %v", err)
	}
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			killed = true
			t.Fatal("brief-lock helper did not acquire in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := AcquireLock(config.Config{StateDir: stateDir}); err == nil {
		t.Fatal("parent acquired brief lock while helper was alive")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill brief-lock helper: %v", err)
	}
	_ = cmd.Wait()
	killed = true

	release, err := AcquireLock(config.Config{StateDir: stateDir})
	if err != nil {
		t.Fatalf("brief lock remained stranded after process death: %v", err)
	}
	release()
}

// mapsEqual is a tiny test helper for map[string]string equality.
func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestBrokenKeyingEraSnapshotResetsNotFloods pins the v1→v2 schema bump that
// accompanies the applecal instance-keying fix. During the broken-keying era,
// scheduled --advance runs committed a STAMPED-EMPTY snapshot under the
// enumerated key "applecalendar" (its memories were grouped under "applecal"
// and never reconciled). After the keying fix the whole backlog reconciles
// under this key; against a stamped-empty v1 snapshot the committed-empty-is-
// steady-state rule would surface the ENTIRE backlog as [new] deltas across
// consecutive briefs (with a notification each) — the exact D-04 flood the
// watermark exists to suppress. The schema bump makes the broken-era snapshot
// read as SchemaReset: one clean re-baseline, nothing surfaced.
func TestBrokenKeyingEraSnapshotResetsNotFloods(t *testing.T) {
	brokenEra := Snapshot{
		Key:               "applecalendar",
		LastBriefAt:       "2026-06-01T00:00:00Z",
		HashSchemaVersion: 1, // the version every broken-era install has on disk
		Items:             map[string]string{},
	}
	mems := []memory.Memory{
		{ID: "e1", Provider: "applecal", ContentHash: "h1"},
		{ID: "e2", Provider: "applecal", ContentHash: "h2"},
	}
	d := Classify(brokenEra, mems, time.Now())
	if len(d.Items) != 0 {
		t.Fatalf("broken-era snapshot flooded %d backlog item(s) as deltas; want a clean re-baseline", len(d.Items))
	}
	if !d.ColdStart || !d.SchemaReset {
		t.Fatalf("ColdStart=%v SchemaReset=%v; want true/true (cold-start-equivalent reset)", d.ColdStart, d.SchemaReset)
	}
	if len(d.Baseline) != 2 {
		t.Fatalf("baseline must still record all current hashes, got %d", len(d.Baseline))
	}
}

func assertPermUnix(t *testing.T, got, want os.FileMode) {
	t.Helper()
	if runtime.GOOS != "windows" && got.Perm() != want.Perm() {
		t.Fatalf("mode = %v, want %v", got.Perm(), want.Perm())
	}
}

func TestMt_LoadBriefSnapshotNilItemsNormalized(t *testing.T) {
	cfg := testCfg(t)
	p := Path(cfg, "gmail")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	// hash_schema_version matches current; "items" is absent → unmarshal leaves it nil.
	raw := `{"key":"gmail","last_brief_at":"2026-06-08T00:00:00Z","hash_schema_version":` +
		strconv.Itoa(HashSchemaVersion) + `}` + "\n"
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	snap := Load(cfg, "gmail")
	if snap.Items == nil {
		t.Fatal("loadBriefSnapshot left Items nil, want a normalized empty map")
	}
	if len(snap.Items) != 0 {
		t.Fatalf("normalized Items should be empty, got %v", snap.Items)
	}
}

func TestMt_SaveBriefSnapshotNilItems(t *testing.T) {
	cfg := testCfg(t)
	if err := Save(cfg, Snapshot{Key: "gmail", Items: nil}, fixedNow); err != nil {
		t.Fatalf("saveBriefSnapshot: %v", err)
	}
	body, err := os.ReadFile(Path(cfg, "gmail"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"items": {}`) {
		t.Fatalf("nil Items should serialize to an empty object, got:\n%s", body)
	}
	got := Load(cfg, "gmail")
	if got.Items == nil || len(got.Items) != 0 {
		t.Fatalf("round-tripped Items = %v, want a non-nil empty map", got.Items)
	}
}

func TestMt_AcquireBriefLockMkdirError(t *testing.T) {
	root := t.TempDir()
	stateFile := filepath.Join(root, "state-is-a-file")
	if err := os.WriteFile(stateFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{StateDir: stateFile} // StateDir is a file → MkdirAll(<file>/brief) fails
	if _, err := AcquireLock(cfg); err == nil {
		t.Fatal("acquireBriefLock should fail when StateDir/brief cannot be created")
	}
}

func TestMt_AcquireBriefLockDoubleRelease(t *testing.T) {
	cfg := testCfg(t)
	release, err := AcquireLock(cfg)
	if err != nil {
		t.Fatalf("acquireBriefLock: %v", err)
	}
	release()
	release() // idempotent — must not panic or error

	again, err := AcquireLock(cfg)
	if err != nil {
		t.Fatalf("re-acquire after double-release failed: %v", err)
	}
	again()
}
