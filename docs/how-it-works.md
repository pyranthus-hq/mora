# How the intelligence works

> The narrative version. For the subsystem-level spec with diagrams and `file:line`
> citations, see [`docs/architecture/`](architecture/00-overview.md).

Mora's retrieval is three deterministic layers that compile from your own data at ingest time — no NER, no cloud, no API key, no model download. Everything below runs in the single static Go binary against a local SQLite index. The only optional network socket is a *loopback-only* Ollama upgrade (covered below), and it refuses any non-loopback URL.

## 1. The entity graph — derived from your real mail and messages, not inferred

An **entity** is a thing your vault refers to repeatedly: a **person**, a **scope** (project/namespace), a **tag**, a `[[wikilink]]`, or a `- [Category]` line. Mora materializes these — and the edges between them — into `entities` / `edges` tables **in the same transaction as the index rebuild** (`buildGraph` in `internal/mora/graph.go`), so the graph is always atomically consistent with the search index and byte-identical across rebuilds.

People are the interesting part, and they come straight from connector identity capture — no name-entity-recognition model is involved. When Gmail, Calendar, and iMessage ingest, they already carry structured identity in each memory's metadata: `from` / `to` / `cc` for mail, `organizer` / `attendees` for calendar, and iMessage handle↔name `participants` pairs. `personRefs` resolves those into canonical `person:<lowercased-identity>` nodes (so `neil@x.com` referenced in 40 threads collapses to one node), and emits edges with bi-temporal stamps and provenance back to the source memory:

- **`PARTICIPATED_IN`** — a person was on a thread or chat
- **`ATTENDED`** — a person was on a calendar event
- **`EMAILED`** — sender → each recipient, mail only
- **`MENTIONS`** — a person *known from metadata* who also appears by name in another message's body, matched by a gazetteer built **from the graph's own person aliases** (`gazetteer.go`) — word-boundary, multi-token names, stoplisted, deterministic tie-break. Still no model.

A blast email with 500 recipients won't explode the graph: person fan-out is capped (`maxParticipantFanout = 64`, and it *warns* rather than silently dropping — the repo's honesty rule), and co-occurrence ("who else was on Sam's threads") is a **query-time self-join**, never materialized, so an N-person thread costs O(N) edge rows, not O(N²).

```bash
mora entities                  # grouped: People / Scopes / Links / Categories / Tags, with counts
mora entities "Sam Rivera"     # the memories that reference one entity
mora entities "neil@x.com"     # same person — exact alias/email/handle lookup
mora entities --json
```

**Concrete example.** You and Sam traded 40 emails and a few iMessages. `mora entities` shows `Sam Rivera  43` under **People**. `mora entities "Sam Rivera"` lists those 43 memories; via MCP, `get_entity` additionally returns his aliases (every address/handle/name variant seen), his `degree`, the incoming edges with their `evidence_id`, and his 1-hop `neighbors` — the people he shares threads or events with. No cloud service can build this, because no cloud service can see your `chat.db`. That's the moat.

The person graph is also cleaned so it reflects *people*, not raw addresses:

- **Automated senders are demoted.** no-reply / receipts / notifications / "LinkedIn Job Alerts"
  bots are classified `service` and kept out of the People view (still searchable via `get_entity`).
- **Aliases are trusted by provenance.** A name only becomes a match key if its owner *presented it
  themselves* (as an email sender, or an iMessage contact) — so spam mail-merge labels and other
  people's mislabels never pollute who you are.
- **The same human across addresses collapses into one person.** Gmail dot/`+tag` variants and
  cross-domain matches (with a full-name anchor) merge, so `get_entity` returns a *complete* picture
  no matter which address you ask by. Conservative on purpose: it never fuses two different people on
  a weak signal.

## 2. Hybrid retrieval — BM25 + static embeddings + graph expansion, fused by RRF

Keyword search misses paraphrase ("launch" vs "shipping"); pure vector search drifts off exact terms and proper nouns. `hybridSearch` (`internal/mora/hybrid.go`) runs three retrievers and fuses them:

1. **FTS5 / BM25** — the exact-match correctness anchor. Proper nouns, IDs, and literal phrases always rank.
2. **Static-embedding cosine** — recall for paraphrase, over per-memory vectors in `mem_vectors`. Zero-similarity rows are dropped so the vector arm never drags in unrelated memories.
3. **1-hop graph expansion** — if the query *names a person* (gazetteer + exact alias match), pull that person's evidence memories into the candidate pool. GraphRAG-lite, no LLM.

The three ranked lists are merged with **Reciprocal Rank Fusion**, `score = Σ 1/(k + rank)`. RRF is *rank*-based, so it fuses BM25's unbounded scores with cosine's `[0,1]` without any normalization, and dampens the head so no single arm dominates.

**Be honest about the embedder.** By default it is a pure-Go, deterministic **feature-hashing static embedder** (`staticEmbedder` in `embed.go`): it hashes word tokens and character trigrams into a fixed 256-dim space (signed hashing trick, TF-weighted, L2-normalized). Cosine then tracks shared lexical + subword features — so "launching" and "launch" share signal. This is *not* a vendored transformer (no model2vec/potion weights baked in); it's the deterministic, $0, single-binary floor. It sits behind an `Embedder` interface, so a real potion checkpoint or an **Ollama** model (`nomic-embed-text`) drops in unchanged. Ollama is strictly opt-in (`mora config embedder ollama`, or `MORA_EMBEDDER=ollama`), and `chooseEmbedder` **refuses any non-loopback `MORA_OLLAMA_URL`** — memory text never leaves the machine, and an unreachable daemon degrades to the static embedder with a warning, never an error.

**Graceful degradation.** On an index with no `mem_vectors` table, `hybridSearch` is simply FTS-only — search still works. As soon as vectors exist, the cosine and graph arms light up automatically. The model id is stored per vector, so changing embedders triggers a clean re-embed rather than silently mixing incompatible vectors.

```bash
mora search "the pricing thing Sam mentioned" --json   # hybrid when a semantic embedder is active
```

## 3. `mora think` — a synthesis envelope your own agent fills in

`mora think` (`think.go`) does **not** contain an LLM and holds no API key. It returns a *synthesis envelope* — everything an agent needs to write a cited answer — and rents the actual prose generation from the agent that called it (via MCP). The envelope has three parts:

- **Cited evidence** — the top hybrid-retrieval hits, each with `stable_id`, scope, timestamp, fused score, and a snippet, so every downstream claim is attributable.
- **Gap analysis — "what the vault does NOT know"** — computed deterministically *before any model runs*, the trust feature that guards against confidently-wrong RAG. Three honest signals: **stale** (freshest matching memory older than 30 days), **thin coverage** (a distinctively-named person in the query has fewer than 2 memories), and **coverage holes** (a real-name-shaped phrase in the query that resolves to no entity at all). The gap logic reuses the gazetteer's eligibility guards, so a title-cased question phrase like "What Did" is never mistaken for a missing person.
- **A synthesis prompt** — a ready-to-run instruction: *answer using only this evidence, cite every claim with its `[stable_id]`, and surface the known gaps in a "What the vault does not know" section.*

**Concrete example.** `mora think "what did we decide with Sam about pricing?"` retrieves the relevant threads as cited evidence, and if the freshest is two months old it adds a `stale` gap; if Sam has only one memory it flags thin coverage. Your agent reads the envelope and composes the answer — grounded, cited, and honest about its blind spots. Mora did the retrieval and the gap accounting for $0; the agent paid for the sentence-writing.

```bash
mora think "what did we decide with Sam about pricing?"
mora think "..." --json        # full envelope: evidence + gaps + synthesis_prompt
```

**Over MCP** (`mora mcp serve`), the same three layers are exposed as tools any MCP-capable agent (Claude Code, Codex) can call: **`think`**, **`list_entities`**, and **`get_entity`** — alongside search, digest, and the session-start `brief`.

Relevant source: `internal/mora/entities.go`, `graph.go`, `graph_read.go`, `hybrid.go`, `embed.go`, `embed_ollama.go`, `gazetteer.go`, `think.go`. Verified: the full suite runs green under `-race`, `go vet` + `golangci-lint` clean, `CGO_ENABLED=0` builds.
