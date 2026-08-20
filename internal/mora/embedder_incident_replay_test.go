package mora

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// embedder_incident_replay_test.go — Packet D (HEALTH-12): the silent-embedder-swap
// incident, replayed through the REAL rebuild chokepoint. The bug that shipped:
// `embedderForPref` substituted the static embedder when the Ollama daemon was down,
// and `ollamaEmbedder.Embed` fabricated a zero vector on any transport failure, so a
// rebuild re-embedded ~3,400 memories with the static fallback (or committed zero
// vectors stamped with the real Ollama model id) and exited 0. These tests pin the
// fail-closed contract at the one chokepoint every rebuild trigger funnels through.

// embVectors dumps mem_vectors as memory_id -> raw vec bytes, for byte-for-byte
// "the previous vectors are preserved" assertions.
func embVectors(t *testing.T, cfg Config) map[string][]byte {
	t.Helper()
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT memory_id, vec FROM mem_vectors ORDER BY memory_id`)
	if err != nil {
		t.Fatalf("select mem_vectors: %v", err)
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var id string
		var vec []byte
		if err := rows.Scan(&id, &vec); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cp := make([]byte, len(vec))
		copy(cp, vec)
		out[id] = cp
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// vectorRowCount returns the number of mem_vectors rows, or 0 when the index or the
// table does not exist (a rolled-back / never-committed rebuild).
func vectorRowCount(t *testing.T, cfg Config) int {
	t.Helper()
	if _, err := os.Stat(dbPath(cfg)); err != nil {
		return 0
	}
	db, err := sql.Open("sqlite", roIndexDSN(cfg))
	if err != nil {
		return 0
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM mem_vectors`).Scan(&n); err != nil {
		return 0 // table absent ⇒ nothing committed
	}
	return n
}

func embVectorsEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || !bytes.Equal(va, vb) {
			return false
		}
	}
	return true
}

// deadLoopbackURL is a loopback address with nothing listening — an unreachable
// daemon that still passes the zero-egress loopback guard (so the failure is
// "daemon down", not "non-loopback refused").
const deadLoopbackURL = "http://127.0.0.1:1"

// TestOllamaDownRebuildFailsClosed — matrix row 17. Config opts into ollama; the
// daemon is down when a rebuild runs through the REAL CLI chokepoint
// (`mora index rebuild`). The rebuild must (1) exit nonzero, (2) leave the previous
// vectors byte-for-byte intact (no static re-embed), (3) read `dirty` (the rebuild's
// own op survived, A4), (4) redden the daily brief banner, (5) fail `doctor --pulse`
// with the typed exit code 2.
func TestOllamaDownRebuildFailsClosed(t *testing.T) {
	// Pre-incident: a healthy semantic index built while the daemon was UP.
	srv := fakeOllama(t, []float64{0.6, 0.8})
	t.Setenv("MORA_EMBEDDER", "ollama")
	t.Setenv("MORA_OLLAMA_URL", srv.URL)
	t.Setenv("MORA_OLLAMA_MODEL", "nomic-embed-text")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	run(t, "write", "--scope", "global", "--title", "Alpha", "--text", "alpha body one")
	run(t, "write", "--scope", "global", "--title", "Beta", "--text", "beta body two")
	run(t, "index", "rebuild")
	before := embVectors(t, cfg)
	if len(before) != 2 {
		t.Fatalf("pre-incident index should hold 2 ollama vectors, got %d", len(before))
	}

	// The daemon dies. A rebuild through the real CLI chokepoint must fail closed.
	srv.Close()
	t.Setenv("MORA_OLLAMA_URL", deadLoopbackURL)

	out, err := runErr(t, "index", "rebuild")
	if err == nil {
		t.Fatalf("index rebuild must fail closed with the daemon down; output:\n%s", out)
	}
	if !errors.Is(err, errEmbedderUnavailable) {
		t.Fatalf("rebuild error must wrap errEmbedderUnavailable, got %v", err)
	}

	// (2) the previous vectors are preserved byte-for-byte — NOTHING was re-embedded
	// with the static fallback.
	after := embVectors(t, cfg)
	if !embVectorsEqual(before, after) {
		t.Fatalf("mem_vectors changed across a failed rebuild — a static re-embed leaked in\nbefore=%v\nafter=%v", before, after)
	}

	// (3) the index reads dirty: the rebuild marked its own op (A4) and never cleared
	// it, so a fresh indexed_at can never mask the failed rebuild.
	if st := indexHealthOf(cfg, time.Unix(1_700_000_000, 0)).State; st != idxDirty {
		t.Fatalf("index health after a failed rebuild = %q, want %q", st, idxDirty)
	}

	// (4) the daily brief carries the red banner.
	briefOut := run(t, "brief")
	if !strings.Contains(briefOut, "🔴 MORA HEALTH:") {
		t.Fatalf("daily brief must carry the health banner after a failed rebuild:\n%s", briefOut)
	}

	// (5) doctor --pulse exits with the typed code 2.
	var pulseOut bytes.Buffer
	pulseErr := Run(context.Background(), []string{"doctor", "--pulse"}, &pulseOut, &pulseOut, strings.NewReader(""))
	if pulseErr == nil {
		t.Fatalf("doctor --pulse must fail on a dirty index; output:\n%s", pulseOut.String())
	}
	if code, ok := ExitCodeFor(pulseErr); !ok || code != 2 {
		t.Fatalf("doctor --pulse error = %v, want a typed exit code 2", pulseErr)
	}
}

// TestOllamaDiesMidRebuildFailsClosed — matrix row 18. The daemon answers probe()
// (so the embedder resolves) but then fails on the Nth /api/embeddings call: no zero
// vectors are committed, the tx rolls back, and the last good index is intact. The
// pre-D1 `Embed` fabricated a zero vector here and the rebuild committed it stamped
// with the real model id.
func TestOllamaDiesMidRebuildFailsClosed(t *testing.T) {
	// Last good index: built with the deterministic static embedder.
	t.Setenv("MORA_EMBEDDER", "")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	run(t, "write", "--scope", "global", "--title", "Alpha", "--text", "alpha body one")
	run(t, "write", "--scope", "global", "--title", "Beta", "--text", "beta body two")
	run(t, "write", "--scope", "global", "--title", "Gamma", "--text", "gamma body three")
	run(t, "index", "rebuild")
	before := embVectors(t, cfg)
	if len(before) != 3 {
		t.Fatalf("last-good static index should hold 3 vectors, got %d", len(before))
	}

	// A daemon that is reachable (answers /api/tags) but dies on the 2nd embedding —
	// mid-rebuild death, after at least one memory was embedded.
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"nomic-embed-text:latest","digest":"sha256:deadbeef"}]}`))
	})
	mux.HandleFunc("/api/embeddings", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) >= 2 {
			http.Error(w, "daemon crashed", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float64{0.6, 0.8}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("MORA_EMBEDDER", "ollama")
	t.Setenv("MORA_OLLAMA_URL", srv.URL)
	t.Setenv("MORA_OLLAMA_MODEL", "nomic-embed-text")

	out, err := runErr(t, "index", "rebuild")
	if err == nil {
		t.Fatalf("a daemon that dies mid-rebuild must fail the rebuild closed; output:\n%s", out)
	}

	// The last good index is intact, byte-for-byte: the rolled-back tx committed
	// NOTHING — not a partial re-embed, not a single zero vector.
	after := embVectors(t, cfg)
	if !embVectorsEqual(before, after) {
		t.Fatalf("mem_vectors changed after a rolled-back mid-rebuild failure")
	}
	// Belt-and-braces: assert no committed vector is the all-zero fabrication.
	for id, vec := range after {
		if isAllZeroBytes(vec) {
			t.Fatalf("memory %q holds a fabricated zero vector after the failed rebuild", id)
		}
	}
	// The index reads dirty (the rebuild's own op survived).
	if st := indexHealthOf(cfg, time.Unix(1_700_000_000, 0)).State; st != idxDirty {
		t.Fatalf("index health after a mid-rebuild failure = %q, want %q", st, idxDirty)
	}
}

func isAllZeroBytes(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return len(b) > 0
}

// TestMixedVectorProvenanceIsDegraded — matrix row 20's D3 arm (the base
// recorded≠configured arm is TestEmbedderMismatchIsDegraded, owned by PR 1). D3 adds
// a cheap corroboration over columns that already exist: more than one DISTINCT
// mem_vectors.model — a partially-completed re-embed the single embedder_model meta
// row still reads as a MATCH — is a mixed-provenance index ⇒ degraded. MUTATION: drop
// the mixedVectorProvenance arm of rule 5 ⇒ this index reads fresh ⇒ RED.
func TestMixedVectorProvenanceIsDegraded(t *testing.T) {
	// Build a clean static index, then inject a SECOND distinct model row into
	// mem_vectors — a partial re-embed. embedder_model still reads static-hash-v1
	// (a match), so only the DISTINCT-model corroboration (D3) can catch this.
	t.Setenv("MORA_EMBEDDER", "")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	run(t, "write", "--scope", "global", "--title", "Alpha", "--text", "alpha body")
	run(t, "index", "rebuild")
	if st := indexHealthOf(cfg, time.Unix(1_700_000_000, 0)).State; st != idxFresh {
		t.Fatalf("freshly built static index should be fresh, got %q", st)
	}

	db, err := sql.Open("sqlite", rwIndexDSN(cfg))
	if err != nil {
		t.Fatalf("open rw: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO mem_vectors(memory_id, dim, model, vec) VALUES ('ghost', 2, 'ollama:nomic-embed-text@sha256:deadbeef', ?)`, encodeVec([]float32{1, 0})); err != nil {
		db.Close()
		t.Fatalf("inject mixed-provenance row: %v", err)
	}
	db.Close()

	if st := indexHealthOf(cfg, time.Unix(1_700_000_000, 0)).State; st != idxDegraded {
		t.Fatalf("a mixed-provenance mem_vectors (two distinct models) must be degraded, got %q", st)
	}
}

// TestSearchCannotTriggerDegradedReEmbed — matrix rows 19a + 19b. The auto-heal
// amplifier, pinned: a read path may rebuild ONLY when the configured embedder
// resolves. With Ollama configured-but-down, neither read-path rebuild door may
// re-embed the vault with the static fallback and exit 0 — each refuses instead.
func TestSearchCannotTriggerDegradedReEmbed(t *testing.T) {
	t.Run("rebuild_on_missing", func(t *testing.T) {
		// The unconditional rebuild-on-missing door (search.go): a search against a
		// MISSING index with the daemon down must refuse, never rebuild with static.
		// Set up the vault under the static floor (so init/write never touch ollama),
		// then remove any index and switch to a dead ollama before the search.
		t.Setenv("MORA_EMBEDDER", "")
		withTempHome(t)
		run(t, "init")
		cfg := mustConfig(t)
		run(t, "write", "--scope", "global", "--title", "Alpha", "--text", "alpha body one")
		if err := os.RemoveAll(dbPath(cfg)); err != nil {
			t.Fatalf("remove index.db: %v", err)
		}
		t.Setenv("MORA_EMBEDDER", "ollama")
		t.Setenv("MORA_OLLAMA_URL", deadLoopbackURL)
		if _, err := os.Stat(dbPath(cfg)); err == nil {
			t.Fatalf("precondition: index.db must not exist yet")
		}

		out, err := runErr(t, "search", "alpha")
		if err == nil {
			t.Fatalf("search on a missing index with the daemon down must refuse, not exit 0; output:\n%s", out)
		}
		// No committed index carrying re-embedded vectors — the refused rebuild rolled
		// back, so mem_vectors is either absent (table never created) or empty. A
		// static re-embed would have committed rows here.
		if n := vectorRowCount(t, cfg); n != 0 {
			t.Fatalf("a refused read must not commit a static re-embed, found %d vectors", n)
		}
	})

	t.Run("schema_stale_autoheal", func(t *testing.T) {
		// The schema-stale auto-heal door (openIndexRO/indexAutoHeal): build a good
		// static index, corrupt its schema version, then read with the daemon down.
		// indexAutoHeal must refuse (embedder unresolvable) rather than auto-heal via
		// a silently-degraded static re-embed. The stale index is left untouched.
		t.Setenv("MORA_EMBEDDER", "")
		withTempHome(t)
		run(t, "init")
		cfg := mustConfig(t)
		run(t, "write", "--scope", "global", "--title", "Alpha", "--text", "alpha body one")
		run(t, "index", "rebuild")
		before := embVectors(t, cfg)

		// Make the index schema-stale so the auto-heal door is the one under test.
		staleDB, err := sql.Open("sqlite", rwIndexDSN(cfg))
		if err != nil {
			t.Fatalf("open rw: %v", err)
		}
		if _, err := staleDB.Exec(`PRAGMA user_version = 99`); err != nil {
			staleDB.Close()
			t.Fatalf("set stale user_version: %v", err)
		}
		staleDB.Close()

		// Daemon down + ollama configured.
		t.Setenv("MORA_EMBEDDER", "ollama")
		t.Setenv("MORA_OLLAMA_URL", deadLoopbackURL)

		out, err := runErr(t, "search", "alpha")
		if err == nil {
			t.Fatalf("a read against a schema-stale index with the daemon down must refuse, not auto-heal; output:\n%s", out)
		}
		// The stale index was NOT re-embedded: vectors byte-identical, schema still stale.
		after := embVectors(t, cfg)
		if !embVectorsEqual(before, after) {
			t.Fatalf("a refused auto-heal must leave the stale index's vectors untouched")
		}
		verDB, err := sql.Open("sqlite", roIndexDSN(cfg))
		if err != nil {
			t.Fatalf("open ro: %v", err)
		}
		var ver int
		_ = verDB.QueryRow(`PRAGMA user_version`).Scan(&ver)
		verDB.Close()
		if ver != 99 {
			t.Fatalf("the stale index must be untouched (user_version still 99), got %d", ver)
		}
	})
}

// TestConfigReportsResolvedEmbedder — matrix row 21. `mora config` reports the
// RESOLVED embedder, not the raw cfg.Embedder that "lies most confidently": an
// opted-in ollama daemon that is down reads "UNREACHABLE — index built with
// static-hash-v1", never a bare "ollama".
func TestConfigReportsResolvedEmbedder(t *testing.T) {
	// Build a static index (embedder_model = static-hash-v1), then durably opt into
	// ollama in config.toml with the daemon down.
	t.Setenv("MORA_EMBEDDER", "")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	run(t, "write", "--scope", "global", "--title", "Alpha", "--text", "alpha body")
	run(t, "index", "rebuild")

	cfg.Embedder = "ollama"
	if err := writeConfig(cfg); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
	// The env MUST be unset so the config opt-in is what drives resolution; a dead
	// loopback URL keeps the probe hermetic (never a real local daemon).
	os.Unsetenv("MORA_EMBEDDER")
	t.Setenv("MORA_OLLAMA_URL", deadLoopbackURL)

	out := run(t, "config")
	if !strings.Contains(out, "UNREACHABLE") {
		t.Fatalf("mora config must disclose the unreachable ollama daemon, got:\n%s", out)
	}
	if !strings.Contains(out, "static-hash-v1") {
		t.Fatalf("mora config must report what the index was actually built with, got:\n%s", out)
	}
	// The load-bearing regression: it must NOT print a bare, confident "embedder = ollama".
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "embedder") && !strings.Contains(line, "UNREACHABLE") {
			t.Fatalf("mora config printed the raw cfg.Embedder verbatim (no resolution): %q", line)
		}
	}
}

// TestRebuildStampsEmbedderDigest — D3: the semantic embedder_digest is written into
// index_meta inside the committing rebuild tx when the resolved ollama daemon lists a
// model digest (static builds record no digest).
func TestRebuildStampsEmbedderDigest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"name":"nomic-embed-text:latest","digest":"sha256:deadbeef"}]}`))
	})
	mux.HandleFunc("/api/embeddings", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float64{0.6, 0.8}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	t.Setenv("MORA_EMBEDDER", "ollama")
	t.Setenv("MORA_OLLAMA_URL", srv.URL)
	t.Setenv("MORA_OLLAMA_MODEL", "nomic-embed-text")
	withTempHome(t)
	run(t, "init")
	cfg := mustConfig(t)
	run(t, "write", "--scope", "global", "--title", "Alpha", "--text", "alpha body")
	run(t, "index", "rebuild")

	meta := gate2ReadMeta(t, cfg)
	if got, want := meta["embedder_digest"], "sha256:deadbeef"; got != want {
		t.Fatalf("embedder_digest = %q, want %q", got, want)
	}
	if got, want := meta["embedder_model"], "ollama:nomic-embed-text@sha256:deadbeef"; got != want {
		t.Fatalf("embedder_model = %q, want %q", got, want)
	}
}

// TestNoFabricatedZeroVector is the CI grep guard (Packet D acceptance): no embedder
// path may fabricate a dim-length zero vector on failure. The recorded incident was
// exactly `make([]float32, e.dim)` returned from ollamaEmbedder.Embed when the daemon
// was unreachable. This runs on every OS in CI (unlike a shell `git grep`), so a
// reintroduced fabrication is caught on Windows too.
func TestNoFabricatedZeroVector(t *testing.T) {
	src, err := os.ReadFile("../embed/ollama.go")
	if err != nil {
		t.Fatalf("read internal/embed/ollama.go: %v", err)
	}
	// The dim-sized allocation is the fabrication signature: a REAL Ollama vector is
	// sized to the response length (len(out.Embedding)), never e.dim.
	if bytes.Contains(src, []byte("make([]float32, e.dim)")) {
		t.Fatal("embed_ollama.go fabricates a dim-length zero vector — a failed Embed must return an error, never a substitute vector (HEALTH-12)")
	}
}
