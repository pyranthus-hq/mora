# Forgettability Reranker

`internal/mora/forgettability.go` is the Wave-1 Track C kernel for future
pre-meeting briefs. It is deliberately **query-time only**: it reads no files,
opens no database, performs no network calls, and writes nothing to the vault or
index. P15 is the wiring step that will replace `buildMeetingPrep`'s current
per-attendee recency sort with this global value-ranked pool.

## Scoring Contract

`rankForgettability` scores candidate evidence memories for resolved attendees
with:

```
Forget = clamp01(.55*Age + .30*Dormancy + .15*Rarity)
Value  = clamp01(Gates * Freshness * Relevance' * clamp01(Forget + .15*Commit))
```

The default priors are the canonical Track C values: `H=90`, `D=60`,
`rarityScale=40`, `relFloor=0.5`, `shadowStrength=0.6`,
`shadowHardGateOverlap=0.8`, `hapaxCap=0.5`, `perAttendeeCap=3`, and
`evidenceCap=24`. `value_micros = round(Value * 1e6)`, matching the integer
sort-key style used by salience.

Inputs that would otherwise be read from the graph or memory meta are explicit
parameters on `forgettabilityCandidate`: person `Kind`, `LastSeen`,
`message_count`, `occurred_at`, identity-known/corroborated state, bulk/self
flags, deletion state, and commitment state. That keeps the kernel pure and
lets the future wiring decide how to hydrate candidates.

## Gates

The hard gates are multiplicative and strict:

- person kind must be `person`
- attendee identity must be known; single-message candidates also require a
  corroborated identity link
- self-authored/self attendee candidates are excluded
- bulk-authored candidates are excluded
- deleted/tombstoned memories are excluded
- near-verbatim newer same-person restatements trip the freshness hard rail

Unknown attendees and weak single-mention identities are therefore gap material,
not ranked evidence. That is the precision-first guard: a wrong-person line is
more severe than a missed useful line.

## Supersession

The scorer honors supersession in the three tiers from the FMB spec:

- Intra-thread: `occurred_at` is the fact timestamp when present, so a revived
  thread has low age and does not rank as forgotten.
- Cross-thread: newer same-person memories with overlapping distinctive tokens
  dampen old items through `Freshness`; overlap at or above the hard threshold
  drops the older item.
- Presentation: this file does not render prose. The P15 wiring must render
  forgettability evidence as dated historical context, never as current truth.

## Selection Shape

The scorer sorts a global cross-attendee pool by `value_micros DESC`, then
`dated DESC`, then `StableID ASC`. `Selected` keeps positive-value items only,
bounded by the global evidence cap and the per-attendee cap. `All` retains
zero-value and gated results for tests and diagnostics.

The known v1 ceiling is pinned in `forgettability_test.go`: a fact buried inside
an otherwise live high-volume thread remains invisible because Mora stores that
thread as one memory with a fresh `occurred_at` and huge `message_count`.
