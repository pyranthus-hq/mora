# Agent A/B/C benchmark — Mora vs brute-force file search

A codegraph-style, **with-vs-without** benchmark: hold the agent (Claude Code),
the model, and the question fixed, and vary **only the retrieval tool**. It
measures what an agent actually spends — cost, tokens, turns, tool calls — and
how accurate its final answer is, when backed by Mora versus plain file search.

Three arms:

| arm | how the agent retrieves |
| --- | --- |
| `baseline` | `Read`/`Grep`/`Glob` over the vault's markdown notes (brute-force RAG) |
| `mora-mcp` | Mora memory tools via MCP (`search_memory`, `think`, `graph`) — no filesystem |
| `mora-cli` | the `mora` CLI via Bash (`mora think`, `mora search`) — no filesystem |

Everything runs against a **hermetic synthetic vault**: `MORA_CONFIG_DIR`
re-roots Mora's vault, index, data, and state into a throwaway sandbox, so the
benchmark never touches a real `~/vault/mora` or `~/.config/mora`. The baseline
arm is physically read-only (`--disallowedTools Bash Edit Write …`) and every
arm runs in a clean temp CWD.

Cost/tokens come from Claude Code's own `result` event (authoritative). Accuracy
is graded by an **independent** LLM judge (`haiku`) against a gold reference,
plus `foundKey` = did the answer surface the single critical fact.

## Files

- `world.json` — the synthetic world: 104 gold memories (emails, iMessage,
  calendar, notes for a fictional founder, "Sam Rivera") + 6 gold questions with
  reference answers (`q1`–`q6`).
- `gen_distractors.py` — generates N plausible **distractor** memories that reuse
  the same cast/projects/date-range with deliberate keyword collisions, but leak
  no gold fact. Pure templates, deterministic, zero API cost.
- `build_vault.py` — materializes a `world.json` into an isolated Mora vault.
- `bench.sh` — the 3-arm harness (knobs below).
- `judge.mjs` — serial LLM judge; `judge-par.mjs` — parallel drop-in (`CONC`).
- `metrics.mjs` — aggregates transcripts into per-question + median tables.
- `*-mcp.json` — MCP configs (Mora server / empty baseline).

## Run it

```bash
# 1. small vault (104 memories)
python3 build_vault.py world.json /tmp/bench-mora
# recommended: use the production embedder (default is a weaker static hash)
MORA_CONFIG_DIR=/tmp/bench-mora mora config embedder ollama
MORA_CONFIG_DIR=/tmp/bench-mora mora index rebuild

# 2. large vault (104 gold + 1900 distractors = scaling test)
python3 gen_distractors.py world.json world-large.json 1900
python3 build_vault.py world-large.json /tmp/bench-mora-large
MORA_CONFIG_DIR=/tmp/bench-mora-large mora config embedder ollama
MORA_CONFIG_DIR=/tmp/bench-mora-large mora index rebuild

# 3. run + judge + report
SYNTH=/tmp/bench-mora-large OUT=$PWD/out-large REPS=2 MODEL=sonnet JUDGE=0 ./bench.sh
node judge-par.mjs out-large /tmp/bench-mora-large/questions.json
node metrics.mjs out-large /tmp/bench-mora-large/questions.json
```

**Env knobs:** `SYNTH` (sandbox `MORA_CONFIG_DIR`), `MODEL` (default `sonnet`),
`REPS`, `QFILTER` (id/substring), `ARMS` (restrict, e.g. `"mora-mcp mora-cli"`),
`OUT` (output dir), `KEEP_OUT=1` (don't wipe `OUT`), `JUDGE=1` (serial judge at
end), `CONC` (parallel-judge concurrency).

> Each cell and each judge call is a real billed `claude -p`. A full
> 6-question × 3-arm × 3-rep run is on the order of $20–30.

## What it found (2026-06)

Synthetic vault, `sonnet`, independent `haiku` judge. Three conditions:

| condition | baseline cost | mora-mcp | mora-cli | accuracy (base / mcp / cli) |
| --- | --- | --- | --- | --- |
| 104 mem, static-hash embedder | $0.197 | $0.150 (0.76×) | $0.145 (0.74×) | 75 / 68 / 68 |
| 104 mem, **Ollama** embedder | $0.197 | $0.150 (0.76×) | $0.158 (0.80×) | 75 / 68 / **75** |
| **2004 mem**, Ollama (19× scale) | **$0.242** (+23%) | $0.152 (**0.63×**) | $0.160 (**0.66×**) | 75 / 71 / 74 |

Honest read:

- **Mora's relative cost advantage widens as the vault grows.** Scaling the
  corpus 19× left Mora's cost ~flat (+1%) while brute-force search rose +23%;
  on the retrieval-hard identity question, baseline file-reads went 22 → 64
  while Mora stayed at 2.
- **Accuracy is rough parity**, not superiority — Mora *matches* brute-force at
  ~⅓ the tool calls and ~35% less cost at scale, and wins the cross-source
  identity question (two email aliases = one person).
- **Use the iterative MCP path for hard multi-hop questions**; the one-shot
  `mora think` (CLI) is cheapest but its critical-fact recall gets brittle at
  scale.
- The default `static-hash` embedder materially underperforms `Ollama`
  (`nomic-embed-text`) on multi-hop retrieval — run the production embedder.

Caveats: small n (reps), a single LLM judge, a synthetic vault, and distractors
that are dilution rather than organic mess (a real vault would likely stress
brute-force search *more*, not less). The baseline is a capable control, not a
strawman.
