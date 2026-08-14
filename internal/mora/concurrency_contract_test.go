package mora

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// This file is the P5 concurrency-contract stress test: an integration-style
// exercise of the FULL concurrent write/read/rebuild contract that P1–P4
// hardened, driving the REAL user-write code paths (cmdWrite via Run, and MCP
// write_memory via callMCPTool) from many goroutines at once.
//
// It is the end-to-end guard that the individual unit tests cannot be:
//   - createexclusive_test.go proves atomicCreate never clobbers on a single path.
//   - index_upsert_test.go proves indexUpsert reflects one memory.
//   - index_busy_test.go proves a reader waits out the writer lock (busy_timeout).
//   - mora_rebuild_atomic_test.go proves a rebuild lists the vault inside its tx.
//   - sources_lock_test.go proves the sources.json lease serializes RMW.
//
// Each pins one seam in isolation. This test runs them together under real
// contention and asserts the emergent whole-system properties that only appear
// when N writers, concurrent readers, and a full rebuild collide:
//
//   1. No lost writes:       every memory a writer reports success for is on disk
//      exactly once (the P4 create-exclusive guarantee — a colliding newID must
//      re-mint, never silently overwrite a rival's memory via atomicWrite's
//      last-rename-wins).
//   2. No torn files:        parseMemory succeeds on every vault file (atomicCreate
//      publishes fully-formed via os.Link; no reader ever sees half-written
//      frontmatter).
//   3. No surfaced lock:     not a single goroutine surfaces "database is locked"
//      (the 15s busy_timeout on both the writer and read-only DSNs lets contenders
//      wait out each other's sub-millisecond commit windows — P1/P2).
//   4. Eventual consistency: after the storm quiesces and one reconciling full
//      rebuild runs (the documented reconciliation point), every vault memory is
//      present in the index. The vault is the source of truth; the index is a
//      derived, eventually-consistent cache (invariant I1).
//
// Run under -race for the strongest signal. The heavy variant is skipped under
// -short (TestConcurrencyContractStress); a smaller always-on variant
// (TestConcurrencyContractSmoke) keeps concurrency coverage even under -short.

// concStressMarker is a token embedded in every stored body so concurrent search
// readers hit a growing result set (and so a human debugging a failure can grep
// the sandbox vault for the storm's memories).
const concStressMarker = "concstress"

// concParams sizes one run of the contract harness so the smoke and stress
// variants share the exact same code path at different scales.
type concParams struct {
	writers              int // concurrent writer goroutines
	writesPerWriter      int // distinct memories each writer creates
	readers              int // concurrent reader goroutines
	readerIters          int // search+read cycles per reader
	rebuilders           int // concurrent full-rebuild goroutines (>=1: the "concurrent full rebuildIndex")
	rebuildsPerRebuilder int // full rebuilds each rebuilder performs
	seeds                int // memories written before the storm (read-by-id targets + initial search hits)
}

// parseWriteResult normalizes the MCP write_memory result. On success the tool
// returns a Memory ({"id":...}); on a degraded index update it returns a map with
// a nested "memory" plus "index_stale":true and a "warning" string. Marshalling
// then re-decoding handles both without depending on the concrete Go type. It
// returns the minted id AND the degraded warning (empty on success) — CRUCIAL,
// because callMCPTool returns a nil error on degraded success, so a failed
// incremental index update (and a "database is locked" that a broken busy_timeout
// would bury inside that warning) is INVISIBLE unless we surface the warning as an
// error ourselves. This is the MCP counterpart of the CLI path, where a non-block
// index error is returned by cmdWrite and so is already caught.
func parseWriteResult(v any) (id string, warning string, err error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", "", err
	}
	var shape struct {
		ID     string `json:"id"`
		Memory struct {
			ID string `json:"id"`
		} `json:"memory"`
		IndexStale         bool   `json:"index_stale"`
		ProjectionsPending bool   `json:"projections_pending"`
		Warning            string `json:"warning"`
	}
	if err := json.Unmarshal(b, &shape); err != nil {
		return "", "", err
	}
	id = shape.ID
	if id == "" {
		id = shape.Memory.ID
	}
	// A successful FTS upsert deliberately leaves graph/vector/commitment work for
	// the scheduled or explicit full rebuild. That expected state is not an index
	// update failure, so the concurrency contract must keep looking for actual
	// failed-upsert warnings without rejecting every normal authored write.
	if (shape.IndexStale && !shape.ProjectionsPending) || shape.Warning != "" {
		warning = shape.Warning
		if warning == "" {
			warning = "index_stale"
		}
	}
	return id, warning, nil
}

// cliWrite drives the REAL `mora write` code path (Run -> cmdWrite -> createMemory
// -> atomicCreate -> indexUpsert) and returns the minted id. It is goroutine-safe:
// it owns its output buffer and never touches *testing.T.
func cliWrite(ctx context.Context, scope, title, text string) (string, error) {
	var out bytes.Buffer
	args := []string{"write", "--json", "--scope", scope, "--type", "insight", "--title", title, "--text", text}
	if err := Run(ctx, args, &out, &out, strings.NewReader("")); err != nil {
		return "", err
	}
	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		return "", fmt.Errorf("decode write --json: %w (out=%q)", err, out.String())
	}
	if m.ID == "" {
		return "", fmt.Errorf("write --json emitted no id: %q", out.String())
	}
	return m.ID, nil
}

// mcpWrite drives the REAL MCP write_memory code path (callMCPTool ->
// createMemory -> indexUpsert), the server's most concurrent write surface. A
// degraded (index_stale) result is turned into an error so it is visible to both
// the zero-writer-errors assertion and the "database is locked" scan — the vault
// id is still returned (the memory IS saved) so presence checks still cover it.
func mcpWrite(ctx context.Context, scope, title, text string) (string, error) {
	res, err := callMCPTool(ctx, "write_memory", map[string]any{
		"scope": scope, "type": "insight", "title": title, "text": text,
	})
	if err != nil {
		return "", err
	}
	id, warning, perr := parseWriteResult(res)
	if perr != nil {
		return "", perr
	}
	if warning != "" {
		return id, fmt.Errorf("mcp write_memory degraded (index not updated): %s", warning)
	}
	return id, nil
}

// runConcurrencyContract runs one full storm and asserts the four contract
// properties. It is the shared body of the smoke and stress tests.
func runConcurrencyContract(t *testing.T, p concParams) {
	t.Helper()
	withTempHome(t)
	// Pin the static-hash embedder so defaultSearch stays FTS-only regardless of the
	// developer's environment (withTempHome does not clear MORA_EMBEDDER, and a set
	// MORA_EMBEDDER=ollama would flip search to the hybrid arm). "" resolves to the
	// static floor via chooseEmbedderFor — the same CI-determinism knob the eval uses
	// — which is what makes the missing-vector / FTS-only searchability assertions
	// below environment-independent.
	t.Setenv("MORA_EMBEDDER", "")
	// init scaffolds the vault AND builds an identity-bound index, so the very
	// first storm write takes the warm incremental-upsert fast path (P2), not the
	// cold-start full-rebuild herd.
	run(t, "init")
	cfg := mustConfig(t)
	ctx := context.Background()

	// Seed a handful of memories BEFORE the storm: they are the read-by-id targets
	// for the reader goroutines (findMemory needs an id that already exists) and
	// guarantee search has hits from the first iteration.
	seedIDs := make([]string, 0, p.seeds)
	for i := 0; i < p.seeds; i++ {
		id, err := cliWrite(ctx, "global",
			fmt.Sprintf("seed %d", i),
			fmt.Sprintf("%s seed body number %d", concStressMarker, i))
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
		seedIDs = append(seedIDs, id)
	}

	// --- the storm: writers, readers, and full rebuilds, all at once ---
	// All goroutines launch under a single WaitGroup so writers, readers, and
	// rebuilds genuinely overlap (each loops, so the windows interleave rather than
	// run in phases). Every goroutine records into its OWN preallocated slot, so
	// there is no shared-state race to muddy -race's signal about the code
	// under test.
	totalWrites := p.writers * p.writesPerWriter
	writeIDs := make([]string, totalWrites)
	writeErrs := make([]error, totalWrites)
	readerErrs := make([][]error, p.readers)
	rebuildErrs := make([][]error, p.rebuilders)

	var wg sync.WaitGroup
	// start is a launch barrier: every goroutine blocks on it until all are
	// spawned, so writers, readers, and rebuilds genuinely fire together and
	// contend, rather than the first-spawned finishing before the last is even
	// scheduled (which would make the overlap — and thus the contention this test
	// exists to exercise — merely probabilistic).
	start := make(chan struct{})

	// Writers: half drive the CLI path, half the MCP path. Distinct titles/bodies
	// per (writer, n) so every memory is unique — a lost write shows up as a
	// missing id, a clobber as two ids landing on one file.
	for w := 0; w < p.writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for n := 0; n < p.writesPerWriter; n++ {
				idx := w*p.writesPerWriter + n
				title := fmt.Sprintf("storm w%d n%d", w, n)
				text := fmt.Sprintf("%s unique-%d-%d body for writer %d write %d", concStressMarker, w, n, w, n)
				var (
					id  string
					err error
				)
				if w%2 == 0 {
					id, err = cliWrite(ctx, "global", title, text)
				} else {
					id, err = mcpWrite(ctx, "global", title, text)
				}
				writeIDs[idx] = id
				writeErrs[idx] = err
			}
		}(w)
	}

	// Readers: each cycle exercises all four read code paths under write/rebuild
	// contention — CLI search, MCP search_memory, CLI read, MCP read_memory — the
	// surfaces where a missing busy_timeout would surface a raw "database is
	// locked". A reader records EVERY error it hits (not just the first) so the
	// post-storm scan sees them all.
	for r := 0; r < p.readers; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			<-start
			var errs []error
			seedID := seedIDs[r%len(seedIDs)]
			for i := 0; i < p.readerIters; i++ {
				var sout bytes.Buffer
				if err := Run(ctx, []string{"search", concStressMarker, "--json"}, &sout, &sout, strings.NewReader("")); err != nil {
					errs = append(errs, fmt.Errorf("cli search: %w", err))
				}
				if _, err := callMCPTool(ctx, "search_memory", map[string]any{"query": concStressMarker}); err != nil {
					errs = append(errs, fmt.Errorf("mcp search_memory: %w", err))
				}
				// read of a known-present seed id (findMemory walks the vault, skips any
				// transient temp with a parse error, returns the match).
				var rout bytes.Buffer
				if err := Run(ctx, []string{"read", seedID}, &rout, &rout, strings.NewReader("")); err != nil {
					errs = append(errs, fmt.Errorf("cli read %s: %w", seedID, err))
				}
				if _, err := callMCPTool(ctx, "read_memory", map[string]any{"id": seedID}); err != nil {
					errs = append(errs, fmt.Errorf("mcp read_memory %s: %w", seedID, err))
				}
			}
			readerErrs[r] = errs
		}(r)
	}

	// Rebuilders: at least one goroutine performing full rebuildIndex runs
	// concurrently with the writes — the "concurrent full rebuildIndex" the
	// contract must survive. Two rebuilds serialize on the immediate write lock
	// (never both mutate at once); a rebuild's whole-vault DELETE-then-reinsert must
	// neither drop a committed on-disk memory nor surface a lock error.
	for rb := 0; rb < p.rebuilders; rb++ {
		wg.Add(1)
		go func(rb int) {
			defer wg.Done()
			<-start
			var errs []error
			for i := 0; i < p.rebuildsPerRebuilder; i++ {
				if _, err := rebuildIndex(ctx, cfg); err != nil {
					errs = append(errs, fmt.Errorf("rebuild %d/%d: %w", rb, i, err))
				}
			}
			rebuildErrs[rb] = errs
		}(rb)
	}

	close(start) // release the barrier: all goroutines contend from here
	wg.Wait()

	// ---- collect every error the storm produced, for the lock-surfacing scan ----
	var allErrs []error
	for _, e := range writeErrs {
		if e != nil {
			allErrs = append(allErrs, e)
		}
	}
	for _, es := range readerErrs {
		allErrs = append(allErrs, es...)
	}
	for _, es := range rebuildErrs {
		allErrs = append(allErrs, es...)
	}

	// PROPERTY 3 (headline): zero surfaced "database is locked". This is the whole
	// point of the 15s busy_timeout on the writer/RO DSNs — under contention a
	// caller must wait out the commit window, never surface a raw SQLITE_BUSY.
	assertNoDatabaseLocked(t, allErrs)

	// Stronger than (3) alone: in an identity-bound, single-vault storm no writer,
	// reader, or rebuild should return ANY error. (A blocked index update degrades
	// to success on the write paths — Run/callMCPTool return nil — so a non-nil
	// error here is a real fault.)
	for i, e := range writeErrs {
		if e != nil {
			t.Errorf("writer op %d failed: %v", i, e)
		}
	}
	for r, es := range readerErrs {
		for _, e := range es {
			t.Errorf("reader %d: %v", r, e)
		}
	}
	for rb, es := range rebuildErrs {
		for _, e := range es {
			t.Errorf("rebuilder %d: %v", rb, e)
		}
	}

	// PROPERTY 1 (no lost writes) + PROPERTY 2 (no torn files): every writer
	// reported a non-empty id, and the vault holds exactly those ids (plus the
	// seeds) as fully-parseable files — one file per id, none lost, none clobbered.
	wantIDs := make(map[string]bool, totalWrites+p.seeds)
	for i, id := range writeIDs {
		if id == "" {
			t.Errorf("writer op %d returned an empty id (lost write)", i)
			continue
		}
		if wantIDs[id] {
			t.Errorf("duplicate minted id %q across writers (create-exclusive should have re-minted)", id)
		}
		wantIDs[id] = true
	}
	for _, id := range seedIDs {
		wantIDs[id] = true
	}

	// Parse EVERY vault memory file: parseMemory must succeed on all of them
	// (property 2 — no torn/half-written frontmatter ever lands under a *.md name),
	// and the on-disk id set must equal the expected set exactly (property 1 — no
	// memory silently overwritten by a same-id race, no phantom extras).
	files, err := allMemoryFiles(cfg)
	if err != nil {
		t.Fatalf("list vault files: %v", err)
	}
	onDisk := make(map[string]bool, len(files))
	for _, path := range files {
		m, perr := parseMemory(path)
		if perr != nil {
			t.Errorf("torn/corrupt memory file %s: parseMemory: %v", path, perr)
			continue
		}
		if m.ID == "" {
			t.Errorf("memory file %s parsed with an empty id", path)
			continue
		}
		if onDisk[m.ID] {
			t.Errorf("id %q appears in more than one vault file", m.ID)
		}
		onDisk[m.ID] = true
	}
	if len(onDisk) != len(wantIDs) {
		t.Errorf("vault holds %d distinct memories, want %d (writes=%d + seeds=%d): lost or extra writes",
			len(onDisk), len(wantIDs), totalWrites, p.seeds)
	}
	for id := range wantIDs {
		if !onDisk[id] {
			t.Errorf("memory %q reported saved but missing from the vault (lost write)", id)
		}
	}

	// PROPERTY 4 (eventual consistency): the vault is the source of truth; the
	// index is a derived, eventually-consistent cache. Run one final reconciling
	// full rebuild after the storm has quiesced — the documented reconciliation
	// point (invariant I1) — then every vault memory must be present in the index.
	if _, err := rebuildIndex(ctx, cfg); err != nil {
		t.Fatalf("final reconciling rebuild: %v", err)
	}
	assertIndexHasAll(ctx, t, cfg, wantIDs)

	// End-to-end searchability: every memory body contains concStressMarker, so the
	// real read path (defaultSearch -> FTS) must surface all of them. This guards
	// against an "indexed but not findable" regression that a row-count check alone
	// would miss.
	hits, err := defaultSearch(ctx, cfg, concStressMarker, "", len(wantIDs)+16)
	if err != nil {
		t.Fatalf("final searchability probe: %v", err)
	}
	if len(hits) != len(wantIDs) {
		t.Errorf("search %q returned %d hits, want %d (every storm memory contains the marker)", concStressMarker, len(hits), len(wantIDs))
	}
}

// assertNoDatabaseLocked fails if any collected error surfaced a raw SQLite busy
// condition — the exact failure the busy_timeout contract exists to prevent.
func assertNoDatabaseLocked(t *testing.T, errs []error) {
	t.Helper()
	for _, e := range errs {
		if e == nil {
			continue
		}
		msg := strings.ToLower(e.Error())
		if strings.Contains(msg, "database is locked") ||
			strings.Contains(msg, "database table is locked") ||
			strings.Contains(msg, "sqlite_busy") {
			t.Errorf("a caller surfaced a raw lock error (busy_timeout should have absorbed it): %v", e)
		}
	}
}

// assertIndexHasAll opens the index on the real read-only path and asserts every
// expected id is a row in `memories`, and that the row count matches exactly.
func assertIndexHasAll(ctx context.Context, t *testing.T, cfg Config, wantIDs map[string]bool) {
	t.Helper()
	db, err := openIndexRO(ctx, cfg)
	if err != nil {
		t.Fatalf("open index ro: %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories`).Scan(&count); err != nil {
		t.Fatalf("count indexed memories: %v", err)
	}
	if count != len(wantIDs) {
		t.Errorf("index has %d memories, want %d (index not reconciled with the vault)", count, len(wantIDs))
	}
	// The FTS row set — the chokepoint every search arm JOINs through — must match
	// the memories table, or a memory would be "indexed" yet unsearchable.
	var ftsCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories_fts`).Scan(&ftsCount); err != nil {
		t.Fatalf("count FTS rows: %v", err)
	}
	if ftsCount != len(wantIDs) {
		t.Errorf("index has %d FTS rows, want %d (searchable surface out of sync with memories)", ftsCount, len(wantIDs))
	}
	for id := range wantIDs {
		var present int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE id=?`, id).Scan(&present); err != nil {
			t.Fatalf("probe indexed id %q: %v", id, err)
		}
		if present != 1 {
			t.Errorf("memory %q present in the vault but not in the index after reconciliation (want 1 row, got %d)", id, present)
		}
	}
}

// TestConcurrencyContractSmoke is the always-on, small-scale contract check — it
// runs even under -short so -race CI never loses concurrency signal. Sized to
// finish in well under a second.
func TestConcurrencyContractSmoke(t *testing.T) {
	runConcurrencyContract(t, concParams{
		writers:              4,
		writesPerWriter:      3,
		readers:              3,
		readerIters:          4,
		rebuilders:           1, // the required "at least one concurrent full rebuildIndex"
		rebuildsPerRebuilder: 2,
		seeds:                3,
	})
}

// TestConcurrencyContractStress is the heavy variant: many writers across both
// write paths, sustained concurrent reads, and repeated full rebuilds colliding
// throughout. Skipped under -short (the heavy variant the task guards); sized to
// stay comfortably under 30s even under -race.
func TestConcurrencyContractStress(t *testing.T) {
	if testing.Short() {
		t.Skip("heavy concurrency stress test; skipped under -short (see TestConcurrencyContractSmoke for the always-on variant)")
	}
	runConcurrencyContract(t, concParams{
		writers:              12,
		writesPerWriter:      8, // 96 concurrent writes across the CLI and MCP paths
		readers:              6,
		readerIters:          25,
		rebuilders:           3,
		rebuildsPerRebuilder: 4, // 12 full rebuilds interleaved with the writes
		seeds:                5,
	})
}
