package mora

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// I5 freezes the contracts that exist only when the independently accepted
// retrieval nodes are combined. D and E each shipped a different physical v4
// index, E adds a segment retrieval arm to D's filtered paths, B classifies the
// resulting scores, and F measures E's bounded evidence reads. These tests must
// fail against the naive E+F union before any integration repair lands.

const retrievalDAGSchemaVersion = 5

func TestRetrievalDAGSchemaV5RebuildsEveryPredecessorShape(t *testing.T) {
	for _, shape := range []string{"v3", "d_v4", "e_v4", "partial_v5"} {
		t.Run(shape, func(t *testing.T) {
			cfg := seedGmailSegmentsSearchFixture(t)
			retrievalDAGMutateSchema(t, cfg, shape)

			m := Memory{
				ID:        "note/i5-schema-" + shape,
				Scope:     "global",
				Type:      "note",
				Source:    "filesystem",
				Title:     "I5 schema proof " + shape,
				CreatedAt: "2026-07-31T12:00:00Z",
				Text:      "schema migration proof",
			}
			if err := writeMemory(cfg, m); err != nil {
				t.Fatalf("write post-migration memory: %v", err)
			}
			if err := indexUpsert(context.Background(), cfg, m); err != nil {
				t.Fatalf("indexUpsert from %s: %v", shape, err)
			}
			retrievalDAGAssertCompleteV5(t, cfg, m.ID)
		})
	}
}

func retrievalDAGMutateSchema(t *testing.T, cfg Config, shape string) {
	t.Helper()
	db, err := sql.Open("sqlite", rwIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	exec := func(stmt string) {
		t.Helper()
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	switch shape {
	case "v3":
		exec(`PRAGMA user_version = 3`)
	case "d_v4":
		// D's v4 has the filter columns but none of E's derived tables.
		exec(`DROP TABLE gmail_segments_fts`)
		exec(`DROP TABLE gmail_segments`)
		exec(`DROP TABLE gmail_segment_diagnostics`)
		exec(`PRAGMA user_version = 4`)
	case "e_v4":
		// E's v4 has the segment projection but the pre-D nine-column memory
		// table. Rebuild must add D's columns before the 12-value upsert.
		exec(`ALTER TABLE memories RENAME TO memories_i5_new`)
		exec(`CREATE TABLE memories (id TEXT PRIMARY KEY, scope TEXT, type TEXT, title TEXT, tags TEXT, source TEXT, created_at TEXT, path TEXT, text TEXT)`)
		exec(`INSERT INTO memories SELECT id, scope, type, title, tags, source, created_at, path, text FROM memories_i5_new`)
		exec(`DROP TABLE memories_i5_new`)
		exec(`PRAGMA user_version = 4`)
	case "partial_v5":
		// A matching stamp is insufficient: the physical schema must also be
		// complete or the incremental path would accept a broken cache.
		exec(`DROP TABLE gmail_segment_diagnostics`)
		exec(`PRAGMA user_version = 5`)
	default:
		t.Fatalf("unknown predecessor shape %q", shape)
	}
}

func retrievalDAGAssertCompleteV5(t *testing.T, cfg Config, wantID string) {
	t.Helper()
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != retrievalDAGSchemaVersion || indexSchemaVersion != retrievalDAGSchemaVersion {
		t.Fatalf("schema version disk/binary = %d/%d, want v%d", version, indexSchemaVersion, retrievalDAGSchemaVersion)
	}
	for _, table := range []string{"memories", "memories_fts", "index_meta", "gmail_segments", "gmail_segments_fts", "gmail_segment_diagnostics"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("required v5 table %s count=%d err=%v", table, n, err)
		}
	}
	columns := map[string]bool{}
	rows, err := db.Query(`PRAGMA table_info(memories)`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		columns[name] = true
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"provider", "account", "created_at_unix"} {
		if !columns[column] {
			t.Fatalf("v5 memories table missing %s; columns=%v", column, columns)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories WHERE id=?`, wantID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("post-migration memory %s count=%d err=%v", wantID, n, err)
	}
	ready, _, err := indexReadyForUpsert(context.Background(), cfg)
	if err != nil || !ready {
		t.Fatalf("complete v5 not ready for incremental upsert: ready=%v err=%v", ready, err)
	}
}

func TestRetrievalDAGGmailSegmentFiltersApplyBeforeRanking(t *testing.T) {
	for _, semantic := range []bool{false, true} {
		name := "static"
		if semantic {
			name = "semantic"
		}
		t.Run(name, func(t *testing.T) {
			if semantic {
				srv := fakeOllama(t, []float64{1, 0, 0, 0})
				defer srv.Close()
				t.Setenv("MORA_EMBEDDER", "ollama")
				t.Setenv("MORA_OLLAMA_URL", srv.URL)
				t.Setenv("MORA_OLLAMA_MODEL", "nomic-embed-text")
			} else {
				t.Setenv("MORA_EMBEDDER", "static")
			}
			seedGmailSegmentsSearchFixture(t)
			oldClock := briefClock
			briefClock = func() time.Time { return time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC) }
			t.Cleanup(func() { briefClock = oldClock })

			for _, args := range []string{
				`{"query":"` + gsSearchAlpha + `","source":"imessage","limit":5}`,
				`{"query":"` + gsSearchAlpha + `","since_hours":1,"limit":5}`,
			} {
				res := mcpResult(t, budgetCall("search_memory", args))
				var rows []map[string]any
				if structuredPayload(t, res)["results"] != nil {
					rows = resultRows(t, res)
				}
				for _, row := range rows {
					if rowID(t, row) == gsSearchWellFormedID {
						t.Fatalf("excluded Gmail segment survived pre-rank filter %s: %#v", args, rows)
					}
				}
			}
		})
	}
}

func TestRetrievalDAGSegmentTraceIsDirectEvidenceAndEvalArm(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "static")
	cfg := seedGmailSegmentsSearchFixture(t)
	mems, tr, err := hybridSearchTrace(context.Background(), cfg, gsSearchAlpha, "", 5, 0)
	if err != nil {
		t.Fatal(err)
	}
	segmentField := reflect.ValueOf(&tr).Elem().FieldByName("Segment")
	if !segmentField.IsValid() {
		t.Fatal("retrievalTrace has no Segment arm")
	}
	segmentIDs, ok := segmentField.Interface().([]string)
	if !ok || rankOf(gsSearchWellFormedID, segmentIDs) < 0 {
		t.Fatalf("segment trace = %v, want %s", segmentField.Interface(), gsSearchWellFormedID)
	}

	// Eval must treat a segment-only hit as arm-found (FUSION if the surface
	// later buries it), never as a RETRIEVAL miss.
	foundByAnyArm := rankOf(gsSearchWellFormedID, tr.FTS) >= 0 ||
		rankOf(gsSearchWellFormedID, tr.Vec) >= 0 ||
		rankOf(gsSearchWellFormedID, tr.Graph) >= 0 ||
		rankOf(gsSearchWellFormedID, segmentIDs) >= 0
	if got := classifyBucket(false, true, -1, 10, foundByAnyArm); got != bFUSION {
		t.Fatalf("segment-only attribution = %s, want %s", got, bFUSION)
	}

	var target Memory
	for _, m := range mems {
		if m.ID == gsSearchWellFormedID {
			target = m
			break
		}
	}
	if target.ID == "" {
		t.Fatalf("hybrid results missing %s: %v", gsSearchWellFormedID, idList(mems))
	}
	// Force the caveat decision to distinguish graph association from the
	// segment arm. Segment is direct textual evidence just like FTS/vector.
	tr.FTS, tr.Vec, tr.Graph = nil, nil, []string{target.ID}
	segmentField = reflect.ValueOf(&tr).Elem().FieldByName("Segment")
	segmentField.Set(reflect.ValueOf([]string{target.ID}))
	gaps, err := computeGaps(context.Background(), cfg, "status update", []Memory{target}, tr, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps.RetrievalCaveats) != 0 {
		t.Fatalf("segment-direct result mislabeled graph-only: %v", gaps.RetrievalCaveats)
	}
}

func TestRetrievalDAGStaticSegmentConfidenceUsesRRFFusedScale(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "static")
	seedGmailSegmentsSearchFixture(t)
	res := mcpResult(t, budgetCall("search_memory", `{"query":"`+gsSearchAlpha+`","confidence":true}`))
	rows := resultRows(t, res)
	var score float64
	found := false
	for _, row := range rows {
		if rowID(t, row) == gsSearchWellFormedID {
			score, found = row["score"].(float64)
			break
		}
	}
	if !found || score <= 0 {
		t.Fatalf("fixture did not exercise positive segment RRF score: found=%v score=%v rows=%v", found, score, rows)
	}
	conf := mustConfidence(t, structuredPayload(t, res))
	if conf["scale"] != confidenceScaleRRFFused {
		t.Fatalf("confidence scale=%v for positive segment-RRF score, want %q", conf["scale"], confidenceScaleRRFFused)
	}
}

func TestRetrievalDAGStaticSharedFusionConfidenceUsesRRFFusedScale(t *testing.T) {
	t.Setenv("MORA_EMBEDDER", "static")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)

	const query = "sharerankfusionprobe"
	local := Memory{
		ID: "i5-local-share-score", Scope: "project:acme", Type: "insight",
		Title: "Local share score", Source: "notes", CreatedAt: "2026-07-31T12:00:00Z",
		Text: query + " local evidence",
	}
	if err := writeMemory(cfg, local); err != nil {
		t.Fatal(err)
	}
	if _, err := rebuildIndex(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}

	id := writeTestIdentity(t, cfg)
	registerSub(t, cfg, "i5-score-domain")
	publishGen(t, cfg, "i5-score-domain", id, []Memory{{
		ID: "i5-shared-score", Scope: "project:acme", Type: "insight",
		Title: "Shared score", Source: "notes", CreatedAt: "2026-07-31T12:00:00Z",
		Text: query + " shared evidence",
	}})

	res := mcpResult(t, budgetCall("search_memory", `{"query":"`+query+`","confidence":true}`))
	rows := resultRows(t, res)
	if len(rows) < 2 {
		t.Fatalf("fixture did not exercise local+shared fusion: rows=%v", rows)
	}
	for _, row := range rows {
		score, ok := row["score"].(float64)
		if !ok || score <= 0 {
			t.Fatalf("fused row has non-RRF score: row=%v", row)
		}
	}
	conf := mustConfidence(t, structuredPayload(t, res))
	if conf["scale"] != confidenceScaleRRFFused {
		t.Fatalf("confidence scale=%v for shared-corpus RRF scores, want %q", conf["scale"], confidenceScaleRRFFused)
	}
}

func TestRetrievalDAGEvidenceRefUsageMeasuresFinalEnvelopeWithoutContent(t *testing.T) {
	t.Setenv("DO_NOT_TRACK", "")
	t.Setenv("MORA_LOG_QUERIES", "1")
	cfg := seedGmailSegmentsSearchFixture(t)
	result := usageContractCall(t, "read_memory", map[string]any{
		"id": gsSearchWellFormedID, "evidence_ref": gsSearchMsg1Ref, "max_tokens": float64(12),
	})
	events, raw := usageContractEvents(t, cfg)
	if len(events) != 1 {
		t.Fatalf("evidence_ref usage events=%d, want 1; log=%s", len(events), raw)
	}
	usageContractAssertReadShape(t, events[0])
	if events[0]["mode"] != "evidence_ref" {
		t.Fatalf("evidence_ref usage mode=%v", events[0]["mode"])
	}
	b, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if got := usageContractInt(t, events[0], "output_bytes"); got != len(b) {
		t.Fatalf("evidence_ref output_bytes=%d, want final envelope size %d", got, len(b))
	}
	for _, secret := range []string{gsSearchWellFormedID, gsSearchMsg1Ref, "alice@example.com", gsSearchAlpha, gsSearchBeta} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("content-free evidence_ref event leaked %q: %s", secret, raw)
		}
	}
	if _, err := os.Stat(dbPath(cfg)); err != nil {
		t.Fatalf("fixture index missing: %v", err)
	}
}

func TestRetrievalDAGEvidenceRefLookupCountsAsRetrieval(t *testing.T) {
	t.Setenv("DO_NOT_TRACK", "")
	cfg := seedGmailSegmentsSearchFixture(t)

	// Make only the gmail_segments SELECT deliberately expensive. findMemory
	// reads the ordinary memories table before this lookup, while response
	// shaping happens after it. The recursive view therefore gives the usage
	// event an empirical, deterministic witness for the phase boundary without
	// sleeping or adding a production-only test hook.
	db, err := sql.Open("sqlite", rwIndexDSN(cfg))
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`ALTER TABLE gmail_segments RENAME TO gmail_segments_timing_source`,
		`CREATE VIEW gmail_segments AS
			WITH RECURSIVE spin(n) AS (
				VALUES(0) UNION ALL SELECT n + 1 FROM spin WHERE n < 500000
			), delay AS (SELECT sum(n) AS total FROM spin)
			SELECT s.evidence_ref, s.memory_id, s.sender, s.recipients, s.at, s.block_refs, s.text
			FROM gmail_segments_timing_source AS s CROSS JOIN delay
			WHERE delay.total >= 0`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatalf("install delayed segment view: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	usageContractCall(t, "read_memory", map[string]any{
		"id": gsSearchWellFormedID, "evidence_ref": gsSearchMsg1Ref,
	})
	events, _ := usageContractEvents(t, cfg)
	if len(events) != 1 {
		t.Fatalf("evidence_ref usage events=%d, want 1", len(events))
	}
	phases, ok := events[0]["phases"].(map[string]any)
	if !ok {
		t.Fatalf("usage phases=%T, want object: %v", events[0]["phases"], events[0])
	}
	retrievalMillis := usageContractInt(t, phases, "retrieval_ms")
	assemblyMillis := usageContractInt(t, phases, "assembly_ms")
	if retrievalMillis < 20 || retrievalMillis <= assemblyMillis {
		t.Fatalf("delayed evidence lookup timed outside retrieval: retrieval_ms=%d assembly_ms=%d", retrievalMillis, assemblyMillis)
	}
}

func TestRetrievalDAGInstructionsExposeFiltersAndEvidenceRefs(t *testing.T) {
	for _, phrase := range []string{"applied BEFORE ranking", "evidence_ref", "read ONLY that message"} {
		if !strings.Contains(mcpInstructions, phrase) {
			t.Fatalf("merged MCP instructions missing %q", phrase)
		}
	}
}
