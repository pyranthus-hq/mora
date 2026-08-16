package segments

import (
	"context"
	"database/sql"
	"github.com/pyranthus-hq/mora/internal/memory"
	searchpkg "github.com/pyranthus-hq/mora/internal/search"
	"reflect"
	"testing"
	"time"
)

func seedQueryDB(t *testing.T) *sql.DB {
	t.Helper()
	db := openDB(t)
	ctx := context.Background()
	stmts := append([]string{}, SchemaStatements...)
	stmts = append(stmts, `CREATE TABLE memories (id TEXT PRIMARY KEY,scope TEXT,type TEXT,title TEXT,tags TEXT,source TEXT,created_at TEXT,path TEXT,text TEXT,provider TEXT,account TEXT,created_at_unix INTEGER)`)
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		id, scope, provider, path string
		at                        int64
	}{{"m1", "work", "gmail", "p1", 100}, {"m2", "work", "gmail", "p2", 200}, {"m3", "home", "imessage", "p3", 300}} {
		if _, err := db.Exec(`INSERT INTO memories VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, row.id, row.scope, "email", row.id, "a,b", "src", "2026-01-01", row.path, "parent", row.provider, "", row.at); err != nil {
			t.Fatal(err)
		}
	}
	segments := []struct{ ref, id, text, refs string }{{"m1#b", "m1", "needle weaker", "[]"}, {"m1#a", "m1", "needle winner", "[\"from_me:true\"]"}, {"m2#a", "m2", "needle second", "[]"}, {"m3#a", "m3", "needle chat", "[\"from_me:false\"]"}}
	for _, s := range segments {
		if _, err := db.Exec(`INSERT INTO gmail_segments VALUES (?,?,?,?,?,?,?)`, s.ref, s.id, "sender", "[]", "now", s.refs, s.text); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO gmail_segments_fts VALUES (?,?)`, s.ref, s.text); err != nil {
			t.Fatal(err)
		}
	}
	return db
}
func TestSegmentWinnerQueryDistinctParentsAndFilters(t *testing.T) {
	db := seedQueryDB(t)
	ctx := context.Background()
	ids, ev, err := Query(ctx, db, "needle", "work", 2, searchpkg.Filter{}, 80)
	if err != nil || !reflect.DeepEqual(ids, []string{"m1", "m2"}) || ev["m1"].EvidenceRef != "m1#a" || ev["m1"].Direction != "outgoing" {
		t.Fatalf("ids=%v ev=%+v err=%v", ids, ev, err)
	}
	f := searchpkg.Filter{SourceFamily: "gmail", SinceHours: 1, Now: time.Unix(4000, 0)}
	ids, _, err = Query(ctx, db, "needle", "work", 5, f, 80)
	if err != nil || len(ids) != 0 {
		t.Fatalf("filtered ids=%v err=%v", ids, err)
	}
	ids, ev, err = WinnerQuery(ctx, db, "needle", "", 3, []string{"m3"}, searchpkg.Filter{}, 80)
	if err != nil || !reflect.DeepEqual(ids, []string{"m3"}) || ev["m3"].Direction != "incoming" {
		t.Fatalf("ids=%v ev=%v err=%v", ids, ev, err)
	}
	for _, pool := range []int{0, -1} {
		ids, ev, err = Query(ctx, db, "needle", "", pool, searchpkg.Filter{}, 80)
		if ids != nil || ev != nil || err != nil {
			t.Fatal("nonpositive pool")
		}
	}
	ids, ev, err = Query(ctx, db, "!!!", "", 2, searchpkg.Filter{}, 80)
	if ids != nil || ev != nil || err != nil {
		t.Fatal("empty match")
	}
}
func TestCompleteEvidenceChunksAndBestEffortError(t *testing.T) {
	db := seedQueryDB(t)
	ctx := context.Background()
	rows := []memory.Memory{{ID: "m1", Provider: "gmail"}, {ID: "m2", ProviderID: "gmail/work"}, {ID: "m3", Provider: "imessage"}, {ID: "other", Provider: "calendar"}, {ID: "m1", Provider: "gmail"}}
	ev := CompleteEvidence(ctx, db, "needle", "", rows, map[string]memory.GmailSegmentEvidence{"m1": {EvidenceRef: "kept"}}, searchpkg.Filter{}, 80)
	if len(ev) != 3 || ev["m1"].EvidenceRef != "kept" || ev["m2"].EvidenceRef == "" || ev["m3"].EvidenceRef == "" {
		t.Fatalf("ev=%+v", ev)
	}
	if got := CompleteEvidence(ctx, db, "needle", "", nil, ev, searchpkg.Filter{}, 80); len(got) != 3 {
		t.Fatal("empty")
	}
	db.Close()
	got := CompleteEvidence(ctx, db, "needle", "", []memory.Memory{{ID: "m2", Provider: "gmail"}}, nil, searchpkg.Filter{}, 80)
	if len(got) != 0 {
		t.Fatalf("error should preserve evidence: %v", got)
	}
}
func TestAdmitFuseAndAttachCandidates(t *testing.T) {
	db := seedQueryDB(t)
	ctx := context.Background()
	base := []memory.Memory{{ID: "m1"}}
	out, err := AdmitCandidates(ctx, db, base, []string{"m1", "m2", "missing"}, 3, func(path string) (memory.Memory, error) {
		if path == "p2" {
			return memory.Memory{ID: "m2", Title: "hydrated"}, nil
		}
		return memory.Memory{}, sql.ErrNoRows
	})
	if err != nil || len(out) != 2 || out[1].Title != "hydrated" || !reflect.DeepEqual(out[1].Tags, []string(nil)) {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	same, err := AdmitCandidates(ctx, db, base, nil, 3, nil)
	if err != nil || len(same) != 1 {
		t.Fatal("no ids")
	}
	fused := FuseCandidates(out, []string{"m1"}, []string{"m2", "m1"})
	if len(fused) != 2 || fused[0].ID != "m1" || fused[0].Score <= fused[1].Score {
		t.Fatalf("fused=%+v", fused)
	}
	if FuseCandidates(nil, nil, nil) != nil {
		t.Fatal("nil")
	}
	ev := map[string]memory.GmailSegmentEvidence{"m2": {EvidenceRef: "m2#a"}}
	AttachEvidence(fused, ev)
	found := false
	for _, m := range fused {
		if m.ID == "m2" {
			found = m.Evidence != nil && m.Evidence.EvidenceRef == "m2#a"
		}
	}
	if !found {
		t.Fatalf("fused=%+v", fused)
	}
	AttachEvidence(fused, nil)
}
