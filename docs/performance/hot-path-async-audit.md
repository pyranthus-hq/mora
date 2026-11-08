# Hot-path I/O audit (2026-11-02)

Scope: source inspection of the Go CLI/MCP runtime on main `74e97e7`, concentrating on repeated external/local I/O and independent operations on latency-sensitive paths. This is not a production profile of a user's vault; improvements below are validated by request counts, overlap barriers, ordering and failure tests rather than claimed end-user speedup percentages.

## Changes

- **Gmail snapshot and history thread hydration:** fetch up to four independent threads concurrently. A fetcher-wide pacing gate remains separate from the worker bound, so network latency can overlap without multiplying the two-starts/second allowance. Retries also pass through the gate. Keep page ordering, snapshot 404 omission, history 404 tombstones, cancellation/join and fail-closed checkpoint behavior.
- **Semantic retrieval:** pass the already-resolved embedder from routing into hybrid retrieval. This removes a second serial `/api/tags` request and keeps routing and vector lookup on one resolved model digest. Reuse one query embedding for production and deeper diagnostic vector pools; deeper attribution never enters production fusion.
- **Shared-corpus search:** run independent verification/index reads in a bounded four-worker pool, then collect in subscription order. Repair generation publication remains serial, using the existing leases and write policy. Preserve healthy-corpus results and read-only repair refusal behavior.

## Inspected and deliberately unchanged

| Surface | Observation / reason |
| --- | --- |
| Source orchestration (`source_run.go`, ingest callers) | Already uses a bounded pool (default four) with ordered outcomes and joined cancellation. Another parallel layer across sources is unnecessary. |
| Calendar/GitHub pagination (`calendar.go`, `github.go`, shared ingest) | Next cursor and checkpoint depend on the preceding page. Do not speculate pages or advance cursors before successful processing. Calendar page items require no extra per-event HTTP read. |
| Local iMessage/WhatsApp/Apple Calendar connectors | Local database snapshot/iteration work is not a chain of independent network calls; no arbitrary goroutine-per-row conversion. |
| Index rebuild (`index.go`) | Schema, row/FTS/segment writes, graph, vectors and commit form an atomic SQLite transaction. Keep write ordering and fail-closed rollback. Parallel file parse or embeddings need separate memory/CPU profiling and an explicit concurrent/cancellable embedder contract. |
| Authored-memory updates | Small upsert transactions and background reconciliation already avoid per-write full rebuilds. Do not parallelize marker retirement or durable write steps. |
| CLI entry/config (`cmd/mora/main.go`, `mora.go`, `config.go`) | Configuration and policy resolution precede dispatch by dependency; no independent network startup fan-out found in this path. |
| MCP framing (`internal/mcp/protocol.go`) | Stdio dispatch is sequential. Parallelizing whole tool calls would change mutation/read ordering, output synchronization and shutdown behavior, so it is not a mechanical async fix. A read-only request scheduler would require an explicit protocol design and separate tests. |
| HTTP/MCP invocation (`serve_http.go`, `usage_mcp.go`) | HTTP serves concurrent requests already; usage state is request-local. Keep policy checks before handler execution and usage accounting after envelope construction. |
| Recall hooks (`hook.go`) | Recall is bounded by its existing timeout; no speculative background work is introduced. Semantic search changes above do not alter the hook's keyword score contract. |
| Think/context/digest assembly (`think.go`, `search.go`, `digest.go`) | Some disk/SQL reads could overlap, but repair-capable helpers and deterministic assembly introduce dependencies. No unmeasured goroutine fan-out over small pure helpers. Shared-corpus improvement applies to think as well. |

## Limits

No live Gmail mailbox, personal vault, production Ollama daemon, macOS source databases or deployed client was used. Provider quota behavior is checked with an injected wait seam and local HTTP fixtures, not load-tested against Google. Shared-corpus digest/commit-file reads also retain their existing non-context-aware cancellation behavior; the bounded workers are joined rather than detached. The general Embedder API still lacks context-aware cancellation; this audit does not conceal that limitation by launching embeddings in detached goroutines. Platform-specific CI remains necessary in addition to local Linux tests.
