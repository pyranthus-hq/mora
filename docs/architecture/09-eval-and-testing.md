# Evaluation & Testing

Two purpose-built test harnesses that measure what the rest of the codebase cannot self-assert: **retrieval recall quality** (the T2 eval, which attributes every miss to a fixable cause) and **MCP output-size discipline** (the T0 gate, which pins per-tool token blowups so they can't silently regress).

## Files

| File | Lines | Responsibility |
|---|---|---|
| `internal/mora/eval_test.go` | 717 | T2 retrieval-recall eval: golden-set TSV loaders, the `classifyBucket` §6 attribution switch, the deterministic synthetic gate (`TestEvalSynthetic`), the read-only live diagnosis (`TestEvalLive`), the static-vs-Ollama A/B (`TestEvalAB`), the per-surface histogram printer (`reportEval`), the `kFTS`/`kHybrid`/`tracePoolDepth` constants |
| `internal/mora/eval_metrics.go` | 120 | Pure-Go IR metrics ported from `trec_eval`: `recallAtK`, `hitAtK`, `reciprocalRank`, `rankOf`, `meanBy`, and the `existsInMemoriesTable` COVERAGE probe. stdlib only, no CGO/network/Python |
| `internal/mora/mora_mcp_budget_test.go` | 324 | T0 MCP token-budget regression gate: `budgetCase` table, `wantRED` quarantine semantics, per-tool ceilings anchored to 20000, `seedBudgetFixture`, `TestMCPBudgetCeilings`, `TestMCPSearchDefaultLimitIsEight` |

These are the only files this doc owns. The production code they couple to (`hybrid.go`, `mora.go`, `embed.go`, `digest.go`) is documented in [retrieval](./02-retrieval-search.md), [the MCP server](./06-mcp-server.md), and [synthesis](./07-synthesis-think-digest.md).

---

## T2: retrieval-recall attribution

The hard question for a retrieval system is not "what's my recall" — it's "**when I miss a relevant doc, whose fault is it and what fixes it**." A miss can mean: the doc was never ingested (connector bug), the embedder couldn't surface it on meaning (embedder bug), or an arm found it but fusion buried it past the cutoff (RRF/pool/rank bug). Each routes work to a different subsystem. Conflating them is the named #1 risk in the design doc. The T2 eval's load-bearing deliverable is therefore **not a recall number** — it's a histogram that mechanically attributes every gold-doc miss to exactly one of those causes, with zero metric math (`eval_test.go:18-20`).

### The §6 attribution buckets

`classifyBucket` (`eval_test.go:164-177`) is the entire diagnosis logic, isolated into a pure function so it can be exhaustively unit-tested without any embedder or DB in the loop:

```mermaid
flowchart TD
    A["gold doc for a query,<br/>scored on ONE surface"] --> B{"qrel row is<br/>doc_id=NONE?"}
    B -->|yes| NEG["NEGCONTROL<br/>(abstention; excluded from recall)"]
    B -->|no| C{"row exists in<br/>memories table?<br/>existsInMemoriesTable"}
    C -->|"no (not ingested)"| COV["COVERAGE<br/>fix = connector"]
    C -->|"yes"| D{"rank in THIS surface's<br/>ranked list &lt; k?<br/>(0 ≤ rank &lt; k)"}
    D -->|yes| HIT["HIT<br/>surfaced within cutoff"]
    D -->|"no (buried or absent)"| E{"did ANY arm<br/>feeding this surface<br/>find it at any depth?"}
    E -->|yes| FUS["FUSION<br/>arm found it,<br/>surface buried it past k<br/>fix = RRF / pool / rank"]
    E -->|no| RET["RETRIEVAL<br/>indexed but no arm surfaced it<br/>fix = embedder (paraphrase miss)"]
```

The branch **order is the invariant**: COVERAGE is checked *before* any retrieval state (`eval_test.go:162-169`). An un-ingested doc must never be blamed on the embedder, and an embedder miss must never be blamed on the connector — the COVERAGE↔RETRIEVAL misroute is exactly what sends work to the wrong fix. `TestClassifyBucket` (`eval_test.go:512-536`) drives all eight branches with pure inputs, including the `coverage-beats-rank` case (`!inIndex` dominates even when a rank is present) so this ordering can never silently flip in CI.

### Per-surface separation (the wrong-surface-confidence guard)

`classifyBucket` scores **one surface at a time**, and `reportEval` (`eval_test.go:198-263`) keeps two separate histograms: `search_memory` (FTS-only, `k=kFTS`) and `context_memory` (hybrid, `k=kHybrid`). This is the spec's primary guard against false confidence: an FTS miss that the vector arm recovers must **not** read as HIT on the FTS surface (`eval_test.go:150-153`). For the FTS surface, `foundByAnyArm` is `rFTS >= 0` only; for the hybrid surface it is `rFTS >= 0 || rVec >= 0 || rGraph >= 0` (`eval_test.go:237-238`). The per-arm ranks come from `retrievalTrace` (see below).

Recall and MRR are scored over each surface's **production-emitted** list — `search_memory`'s real top-`kFTS` via `mustSearchIDs` → `searchMemories`, and `context_memory`'s real top-`kHybrid` via `mustHybridTrace` → `hybridSearchTrace` (`eval_test.go:213-228`). A doc the surface never returns therefore can never earn recall or reciprocal rank; the eval is **surface-honest**, not measuring some idealized fused list the agent never sees.

### Where the per-arm ranks come from: `hybridSearchTrace`

`classifyBucket` needs to know the rank of a gold doc in *each individual arm* (FTS, Vec, Graph) and in the *full pre-limit fused ranking*. Production `hybridSearch` computes these and discards them. `retrievalTrace` (`hybrid.go:32-39`) exposes them, and `hybridSearchTrace` (`hybrid.go:88`) is the one code path both production and the eval share — `hybridSearch` is a thin wrapper passing `tracePool=0` (`hybrid.go:49-52`).

The subtle part is `tracePoolDepth = 200` (`eval_test.go:184`), which is deliberately **larger** than production's pool of `limit*5` (min 50, `hybrid.go:101-104`). The fused production ranking is always computed from arms queried at the *production* pool and fed whole to RRF, so it stays byte-identical to `hybridSearch` regardless of `tracePool` (`hybrid.go:77-87`). But the arm lists *recorded in the trace* are re-queried at the deeper `tracePool`. Without this, a gold doc that an arm found at rank #55 — beyond the production pool — would be invisible to the trace and misclassified as RETRIEVAL (falsely blaming the embedder) instead of FUSION (`hybrid.go:34-38`, `eval_test.go:174`). The deep trace is what separates "an arm found it but fusion buried it" from "no arm found it at all."

### The golden set

The committed synthetic golden set lives in `internal/mora/testdata/eval/`:

- `golden_queries.tsv` — `qid<TAB>query_text`, loaded by `loadQueries` (`eval_test.go:54-65`).
- `golden_qrels.tsv` — TREC-style qrels: `qid iter doc_id rel source archetype gen surface` (tab-separated), loaded by `loadQrels` (`eval_test.go:71-112`). `rel>0` rows form the relevant set; `doc_id=NONE` rows are kept as negative controls. The first-seen row per `qid` wins for the metadata tags (`eval_test.go:89-104`).

The committed set has exactly four queries (verified): `q1` exact-phrase (PKCE), `q2` person (Neil Patel, reachable only via the graph arm), `q3` paraphrase (cash-runway), and `q4` a `NONE` negative control. The synthetic fixture (`seedEvalFixture`, `eval_test.go:423-462`) writes these docs plus three lexically-disjoint decoys so recall isn't trivially 1.0. **All dates are fixed, no `time.Now`, no randomness** — so `buildGraph` and the static embedder produce byte-identical output every run.

The **live** golden set — `internal/mora/live_queries.tsv` and `live_qrels.tsv` — is **hand-labeled against the real vault and gitignored** (`.gitignore:20-21`). `TestEvalLive`/`TestEvalAB` skip if it's absent (`eval_test.go:616-618`, `640-642`). The design doc's recipe: 3 RED seeds (queries Codex's MCP calls actually failed) expanded to ~15 stratified queries.

### The ONE gated synthetic invariant

`TestEvalSynthetic` (`eval_test.go:566-604`) reports the full eval but **gates exactly one assertion**:

> `Recall@5[gen=seed, archetype=exact, surface=fts] == 1.0`

i.e. an exact phrase that exists verbatim in a body must be returned by the FTS surface. If that breaks, FTS itself is broken (`eval_test.go:588`). The test finds the gated query by its metadata tags and fails loudly with the ranked list if recall isn't 1.0 (`eval_test.go:590-600`); it also fails if the golden set is missing that tagged query at all (`eval_test.go:601-603`), and if the set lacks a `NONE` negative control to exercise the abstention/exclusion path (`eval_test.go:584-586`).

**Everything else is logged, not gated.** The comment states the reasoning directly: "you cannot freeze a recall floor blind, before the live numbers exist" (`eval_test.go:564-565`). Freezing a recall threshold against the synthetic fixture would bake in a meaningless number; the real recall verdict is read off the live histogram, by a human, not asserted in CI.

`kFTS` is **coupled to production** (`eval_test.go:181-185`) — `mcpSearchDefaultLimit` is `8` (`mora.go:2168`):

```go
kFTS           = mcpSearchDefaultLimit // search_memory default cutoff — coupled to production so they can't drift
kHybrid        = 10                    // context_memory hybridSearch cutoff
tracePoolDepth = 200                   // > production pool=limit*5; separates FUSION from RETRIEVAL
```

If someone bumps `mcpSearchDefaultLimit`, the eval's FTS cutoff moves with it automatically — the gate is measuring the real surface, never a stale constant.

### The four run modes

```mermaid
flowchart LR
    M["TestEvalMetrics<br/>pure metric math, no DB"] --> CI1["always runs (CI)"]
    S["TestEvalSynthetic<br/>deterministic fixture"] --> CI2["runs in CI;<br/>gates the 1 invariant,<br/>logs the rest"]
    L["TestEvalLive<br/>MORA_EVAL_LIVE=1"] --> SK1["read-only real vault;<br/>report-only; t.Skip in CI"]
    AB["TestEvalAB<br/>static-hash vs Ollama"] --> SK2["isolated COPY, re-indexed<br/>per embedder; never gates"]
```

- **`TestEvalMetrics`** (`eval_test.go:467-505`) locks the metric math with hand-checked fixtures, no DB. Always runs.
- **`TestEvalSynthetic`** (`eval_test.go:566`) — described above. Forces `MORA_EMBEDDER=""` so a dev with Ollama opted in still gets the CI floor under static-hash (`eval_test.go:567`).
- **`TestEvalLive`** (`eval_test.go:610-626`) scores the real vault read-only via `MORA_EVAL_LIVE=1` (or a path to a data dir) and prints the histogram. It **never rebuilds the live index** (`eval_test.go:613`) — it opens the DB `?mode=ro` (`eval_test.go:300-307`). This is where the embedder-vs-coverage verdict is read.
- **`TestEvalAB`** (`eval_test.go:635-717`) runs static-hash vs Ollama on an **isolated deep copy** of the live vault (`copyTree`, `eval_test.go:388-414`), re-indexing under each embedder. The headline is the bucket migration: **RETRIEVAL→HIT proves the gain is semantic** (`eval_test.go:715-716`). It has multiple correctness guards that make the A/B trustworthy, described next.

### Why the A/B can't lie to itself

`chooseEmbedder` (`embed.go`/`embed_ollama.go:92`) **silently degrades to static-hash** if the Ollama daemon is down — which would turn a static-vs-Ollama A/B into a static-vs-static comparison, the most dangerous possible bug because it would report "no improvement" and look correct. `TestEvalAB` defends against this at every step:

1. Re-index per embedder (`eval_test.go:679`, `689`) — vectors are keyed by `ModelID`, so an Ollama query against a static-keyed index would return an empty arm.
2. Probe Ollama before trusting it; if the embedder resolves to anything not prefixed `ollama:`, **skip** (never fatal) (`eval_test.go:644-648`).
3. After re-indexing under Ollama, count `mem_vectors WHERE model = ?` and `t.Fatal` if zero — "would compare static-vs-static" (`eval_test.go:693-701`).
4. Re-check the embedder *after scoring*, not just before, so a daemon that drops mid-test can't silently mix arms (`eval_test.go:703-707`).
5. `t.Fatal` if the Ollama vec arm returned **0 hits across all queries** despite stored vectors — the query embedder mismatched the index model (`eval_test.go:708-710`).

`bucketHistogram` (`eval_test.go:270-296`) returns the hybrid-surface histogram *plus* `vecHits` (total vec-arm hits across the set) precisely so guard #5 can prove the arm is live. The whole `static-hash-v1` model id (`embed.go:31`) is the named static floor; `embedderIsSemantic` (`hybrid.go:59`) is just `ModelID() != defaultEmbedder().ModelID()`.

> The CLAUDE.md "Retrieval & embeddings" verdict (hybrid beats FTS-only **only** under Ollama; static-hash hybrid *regresses* recall 0.591→0.394@5) is the output of this A/B against Adit's real golden set. The eval is the instrument that produced it.

---

## T0: MCP output-size budget gate

Neil's redline: **one tool result must not dominate the 20000-token context window** (`mora.go:2849`, `mora_mcp_budget_test.go:14-16`). The T0 gate (`mora_mcp_budget_test.go`) pins every MCP tool's *full `CallToolResult` envelope* to a fixed per-tool token ceiling. It measures the whole serialized result map — the text content block **plus** the `structuredContent` mirror — because that is what the agent pays for on the wire (`mora_mcp_budget_test.go:17-21`, `measureEnvelope` at `:68-76`).

### The envelope-doubling bug it's built around

`toCallToolResult` (`mora.go:2935-2957`) JSON-marshals an object-shaped return into a text block **and** attaches the same value as `structuredContent` (`mora.go:2953-2954`). So object-returning tools serialize their payload twice on the wire. The budget gate exists to keep that doubling — and three other structural blowups — visible and bounded.

Tokens are computed as `ceil(bytes / charsPerToken)` with `charsPerToken = 4` (`mora.go:2847`, `mora_mcp_budget_test.go:301`), matching the codebase's own budget unit so the ceilings mean the same thing the runtime budgeting means.

### The ceilings are policy lines, tiered by role

Ceilings (`budgetCase.ceil`, `mora_mcp_budget_test.go:218-269`) are **fixed token lines anchored to 20000**, tiered by tool role (`mora_mcp_budget_test.go:41-49`):

| Role | Tools | Ceiling | Rationale |
|---|---|---|---|
| Mutation / point-read | `write_memory` 1500, `delete_memory` 500, `read_memory` 4000 | tiny | a point op must not approach the window |
| Synthesis / briefing | `think` 6000, `digest` 10000, `context_memory` 12000 | ≤ half the window | a briefing that fills the window defeats its purpose |
| Raw enumeration | `search_memory` 8000, `list_memory` 10000, `list_entities` 8000 | row headroom + fixed cap | should force per-result snippeting + limits before higher limits unlock |

The ceilings are **never scaled with the `limit`/`max_tokens` argument** (`mora_mcp_budget_test.go:31-33`). A regression ceiling is a fixed line; scaling it with the input would hide the very regression it exists to catch. That's why `context_max` (`max_tokens=20000`) and `context_default` share the same 12000 ceiling (`mora_mcp_budget_test.go:247-255`) — a tool that can claim the whole window on one call is exactly the failure mode.

### The `wantRED` quarantine state machine

Several tools are **over budget today, by design** — the RED rows are the deliverable, pinning real bugs (`mora_mcp_budget_test.go:23-30`). Rather than `t.Skip` them (invisible; wouldn't notice a fix), they're quarantined via `wantRED:true` with a `redBaseline` magnitude. The gate stays green on the known baseline but flips RED on two events: a quarantined tool gets *fixed*, or one gets *meaningfully worse*.

```mermaid
stateDiagram-v2
    [*] --> Green: tool ≤ ceiling, wantRED:false
    [*] --> Quarantined: tool > ceiling, wantRED:true (+ redBaseline)

    Green --> FailRegression: tok > ceiling
    note right of FailRegression
      t.Fatal "REGRESSION"
      (mora_mcp_budget_test.go:284-285)
    end note

    Quarantined --> FailFixed: tok ≤ ceiling
    note right of FailFixed
      t.Fatal "FIXED — flip wantRED:false
      to lock the win as a green gate"
      (mora_mcp_budget_test.go:275-277)
    end note

    Quarantined --> FailWorsened: tok > 1.25× redBaseline
    note left of FailWorsened
      t.Fatal "WORSENED — re-baseline"
      (so 408KB→4MB can't stay green)
      (mora_mcp_budget_test.go:278-281)
    end note

    Quarantined --> Quarantined: ceiling < tok ≤ 1.25× baseline
    note right of Quarantined
      t.Logf "known-RED ... tracked"
      CI green-on-known-issue
      (mora_mcp_budget_test.go:282-283)
    end note

    FailFixed --> Green: dev flips wantRED:false
```

`assertBudget` (`mora_mcp_budget_test.go:272-289`) is the whole state machine. The `redBaseline*5/4` worsening guard (`:278`) is what stops a known-RED tool from quietly ballooning 10× while staying "green-on-known-issue" — the explicit example is `408KB→4MB must not stay green`.

### The still-RED tools (the pinned bugs)

From `budgetCases` (`mora_mcp_budget_test.go:218-269`), the rows with `wantRED:true` today:

- **`list_entities`** — ceiling 8000, baseline 11443. Unbounded entity count + full `MemoryIDs` per entity, no limit/pagination (`graph_read.go graphListEntities`, `mora_mcp_budget_test.go:240-241`).
- **`get_entity_found`** ("Neil Patel") — ceiling 12000, baseline **189602**. Dumps every evidence body in full, no snippet/limit, then doubled by `structuredContent` (`graph_read.go graphGetEntity`, `mora_mcp_budget_test.go:243-244`).
- **`digest_default`** and **`digest_max`** — ceiling 10000, baseline 17007 each. Renders a budget-clipped digest string but ships full `d.Sections` beside it, then doubles via the envelope (`mora.go` digest case, `mora_mcp_budget_test.go:257-260`). That the two are **identical size** proves `max_tokens` is a dead knob on the sidecar (`:260`).

`get_entity_notfound` is green at 12000 (the 404 path), proving the blowup is the evidence dump, not the lookup. `search_big`/`search_default_limit` are green with notes recording that the body-bloat bug was **fixed** — `snippetMemories` caps each row at `searchSnippetLen=240` (`mora.go:2172`, `:2180`, `mora_mcp_budget_test.go:225-230`).

### The fixture is deterministic by construction

`seedBudgetFixture` (`mora_mcp_budget_test.go:84-184`) reproduces each blowup structurally, not by accident:

- 200 emails sharing one sender ("Neil Patel") → one high-degree person that `get_entity` dumps in full; 200 distinct recipients → ~200 low-degree persons that bloat `list_entities` (`mora_mcp_budget_test.go:78-83`, `90-109`).
- For `digest`, 12 sources × 8 items = 96 in-window items > the `digestDefaultCap=8` per-source cap (`digest.go:16-18`), so the blowup is **count-driven across sections**, not body-size-driven — robust, not fragile (`mora_mcp_budget_test.go:110-122`). These are the **only** now-relative timestamps in the fixture (`recent := time.Now().Add(-12h)`, `:123`), pinned well inside the 24h window so the size stays stably over-ceiling.
- 9 "bulktext" rows (> the default limit 8) so a no-arg `search_memory` returns a full 8 snippet-capped rows (`mora_mcp_budget_test.go:170-178`).
- Everything else uses **fixed 2026-05 dates and no randomness** (`mora_mcp_budget_test.go:97`, `:82-83`), so `buildGraph` + the static embedder are byte-identical run to run.

A footgun baked into the fixture comment (`mora_mcp_budget_test.go:141-150`): the read/delete subjects use **slash-free ids** (`read-target`, not `gmail_thread/x`) because a connector-style id nests under `memoryPath` into a subdirectory whose file base `findMemory` can't resolve via the generic `writeMemory` path — it would silently measure a ~22-token error envelope. Production is fine (the real connector files via `writeMappedMemory`+`SafeFilename`); this only bites the test's generic write path.

### The coupled default-limit assertion

`TestMCPSearchDefaultLimitIsEight` (`mora_mcp_budget_test.go:189-211`) proves the no-arg `search_memory` default is `mcpSearchDefaultLimit` (bumped 5→8) **and** that every returned body is snippet-capped at `searchSnippetLen` runes (+ ellipsis) with `Truncated=true`. This is what holds the 8000-token line — snippeting, not the old limit of 5. `kFTS` in the T2 eval reads the same constant, so the two harnesses agree on the production cutoff by construction.

### `TestMCPBudgetLive`

`TestMCPBudgetLive` (`mora_mcp_budget_test.go:318-324`) measures the same contracts against a real vault via `MORA_BUDGET_LIVE=/path`, but is **double-skipped** today: it needs read-only config repointing that isn't wired yet. The documented fast path for live numbers is the stdio binary directly: `printf '<jsonrpc line>' | mora mcp serve | wc -c` (`mora_mcp_budget_test.go:311-317`).

---

## The cross-model TDD workflow

Per CLAUDE.md, Mora is built via cross-model TDD: **Codex CLI authors the RED tests, Claude implements GREEN, each task reviewed.** Both harnesses here are artifacts of that loop:

- The T0 gate's `wantRED` rows are RED tests for bugs that are *intentionally not yet fixed* — they encode the failing state and the fix-detection so CI carries the bug forward visibly instead of forgetting it. A Codex-authored RED that Claude later fixes trips the `FIXED` fatal, forcing the win to be locked in as a green gate (`mora_mcp_budget_test.go:275-277`).
- The T2 eval's single gated invariant is the minimal RED-able assertion (exact-phrase FTS must work); everything richer is logged for human reading because freezing it blind would be a false RED.

Get live numbers with `MORA_EMBEDDER=ollama MORA_EVAL_LIVE=1 go test ./internal/mora/ -run TestEvalAB` (needs hand-labeled live qrels + a running Ollama daemon).

---

## Invariants & gotchas

- **`classifyBucket` branch order is load-bearing.** COVERAGE (`!inIndex`) is checked before any rank/arm state (`eval_test.go:162-169`). WHY: an un-ingested doc must route to the connector and an embedder miss must route to the embedder; the COVERAGE↔RETRIEVAL misroute sends work to the wrong subsystem. `TestClassifyBucket`'s `coverage-beats-rank` case (`eval_test.go:524`) guards the ordering.

- **Per-surface, never cross-surface, attribution.** A surface's `foundByAnyArm` includes only the arms feeding *that* surface — FTS surface uses `rFTS>=0`; hybrid uses FTS∨Vec∨Graph (`eval_test.go:237-238`). WHY: an FTS miss recovered by the vector arm reading as HIT on the FTS surface is "wrong-surface false confidence," the spec's primary failure mode.

- **`existsInMemoriesTable` must never swallow a DB error into `false`.** It returns `(false, nil)` only for `sql.ErrNoRows`; any other error propagates (`eval_metrics.go:109-120`). WHY: a swallowed infra fault would misclassify *every* gold doc as COVERAGE and misroute the entire diagnosis to the connector. `TestExistsInMemoriesTable` (`eval_test.go:541-559`) pins both real outcomes.

- **`tracePoolDepth` (200) must exceed production pool (`limit*5`, min 50).** `eval_test.go:184`, `hybrid.go:101-104`. WHY: a gold doc an arm found beyond the production pool would otherwise be invisible to the trace and misread as RETRIEVAL (blaming the embedder) instead of FUSION. The fused production result is still computed at the production pool, so byte-identity with `hybridSearch` holds (`hybrid.go:77-87`).

- **`kFTS = mcpSearchDefaultLimit`, not a literal.** `eval_test.go:182`. WHY: so the eval's FTS cutoff can never drift from the real `search_memory` cutoff. Bumping the production limit moves the eval automatically.

- **`TestEvalSynthetic` gates exactly ONE invariant.** Recall@5[fts,exact,seed]==1.0 (`eval_test.go:588-600`). WHY: you cannot freeze a recall floor before live numbers exist; gating richer numbers against a synthetic fixture would bake in a meaningless threshold. Everything else is `t.Logf`.

- **Synthetic + budget fixtures must be byte-deterministic.** Fixed dates, no `time.Now`, no randomness (`eval_test.go:416-422`, `mora_mcp_budget_test.go:82-83`). WHY: `buildGraph` and the static embedder must produce identical output every run, or the gate flickers. The *only* exception is `seedBudgetFixture`'s digest items, which are now-relative-but-pinned inside the 24h window (`mora_mcp_budget_test.go:118-123`); they're count-driven so timestamps jitter the total by only a few bytes.

- **`TestEvalAB` must prove Ollama is actually live.** Re-index per embedder, count `mem_vectors`, re-check the model after scoring, require `vecHits>0` (`eval_test.go:679-710`). WHY: `chooseEmbedder` silently degrades to static-hash when the daemon is down; without these guards a static-vs-static comparison would masquerade as "no improvement" and look correct — the most dangerous bug.

- **Live golden set is gitignored; never rebuild the live index.** `live_*.tsv` (`.gitignore:20-21`); `TestEvalLive` opens `?mode=ro` (`eval_test.go:300-307`, `613`). WHY: gold labels are personal vault data, and the eval must measure the production index as-is, not a re-indexed copy.

- **Budget ceilings are fixed, never scaled with the input arg.** `mora_mcp_budget_test.go:31-33`. WHY: a regression ceiling that scaled with `limit`/`max_tokens` would hide the regression it exists to catch (e.g. `context_max` at 20000 shares the 12000 ceiling).

- **`wantRED` over `t.Skip` for known bugs.** `mora_mcp_budget_test.go:30`. WHY: a skip is invisible and won't notice when the bug is fixed; `wantRED` flips the gate RED on a fix (`FIXED` fatal) so the win is locked in, and on a >25%-worse regression (`WORSENED` fatal) so a quarantined tool can't balloon unnoticed.

- **The budget gate measures the FULL envelope, not just the text block.** `measureEnvelope` marshals the whole result map including the `structuredContent` mirror (`mora_mcp_budget_test.go:68-76`). WHY: the doubling bug lives in `toCallToolResult` (`mora.go:2953-2954`); measuring only the text block would hide half the cost the agent actually pays.

## Related

- [retrieval & search](./02-retrieval-search.md) — `hybridSearchTrace`, RRF, the arms (FTS/Vec/Graph), `defaultSearch`/`embedderIsSemantic` routing, and the static-hash vs Ollama embedder choice the T2 eval measures.
- [MCP server](./06-mcp-server.md) — `toCallToolResult` envelope, `callMCPTool`, `snippetMemories`, and the per-tool returns the T0 gate ceilings bound.
- [synthesis: think & digest](./07-synthesis-think-digest.md) — the `digest`/`think`/`context_memory` sidecar shapes the T0 gate pins as still-RED.
- [entity graph](./03-entity-graph.md) — `graphListEntities`/`graphGetEntity`, the source of the two largest T0 blowups.
- [overview](./00-overview.md)

## Open questions / unverified

- **`TestMCPBudgetLive` is dead code today.** It unconditionally `t.Skip`s pending read-only config repointing (`mora_mcp_budget_test.go:323`); the documented live path is the `mora mcp serve | wc -c` shell one-liner instead. The Go parity path is unverified at runtime.
- **Whether the still-RED tools have been fixed since the recorded baselines.** This doc reports the `wantRED` rows and `redBaseline` values as committed in `budgetCases()` (`mora_mcp_budget_test.go:240-260`); I did not run the gate to confirm the live token counts still match those baselines. If a fix landed, `assertBudget` would already be failing CI with the `FIXED` fatal — so green CI implies they're still RED, but I did not execute the test to verify.
