# 14 — Share transports

`mora share` first moved memories through one transport: a private git remote.
See [13-sharing.md](13-sharing.md). A transport seam now also supports a
**user-owned S3/R2 bucket**. This document explains how both paths give the same
privacy, identity, and age guarantees. Unlike a private git repo, this backend
has no write ACL. Its locator is a read-all bearer token.

## The one idea

A share is `scope + recipients + a transport locator`. The shared core does not
name a backend. It collects a scope, finds changes, encrypts with age, decrypts,
checks, indexes, and joins results. Each transport follows one rule:

> **Write content-addressed ciphertext blobs, then flip ONE signed manifest that
> names the current set. A reader resolves the set via the manifest, verifies it
> before touching a byte, and hash-checks every blob.**

This rule extends what git already did with `commit` + `share.json`. With this
rule, all backends use the same publish and pull loops.

## The two axes

v1 entangled two independent concerns. They are now split.

| Axis | When | git | bucket |
|---|---|---|---|
| **Coordination / bootstrap** | once | clone / grant repo read | paste locator + confirm fingerprint out of band |
| **Transport** | recurring | commit + push / pull `--ff-only` | put/get objects + flip manifest |

All *trust* lives in coordination (TOFU-pinning the publisher key). Transport just
moves ciphertext and knows nothing about who is trusted.

## Where each guarantee comes from

| Guarantee | git | bucket |
|---|---|---|
| Authenticity | remote write-ACL | ed25519-signed manifest (`sealManifest` / `verifyEnvelope`) |
| Identity | the repo you cloned | TOFU pin of the signing key, confirmed via `--confirm-pin` fingerprint |
| Freshness (no rollback) | `git pull --ff-only` | monotonic manifest **version** (persisted `LastVersion`) |
| Confidentiality | private repo | blobs **and** manifest age-encrypted to recipients |
| Ciphertext-only egress | `git ls-files` allowlist | `bucketEgressAudit` over the object prefix |

A signature proves *who* wrote a manifest, not that it is *current* — so the
version check, not the signature, is what replaces `--ff-only`. The signature also
binds the **locator** (`manifestSigningMessage`), so a manifest signed for one
bucket/prefix cannot be replayed at another even though one machine signs all its
shares with the same key.

## Publish (bucket)

`bucketPublish` (`internal/mora/share_bucket.go`):

1. Encrypt each memory in scope. The blob's storage key is `sha256(ciphertext)`
   (`blobKey`). v1 re-encrypts the full set each push (authored notes are small);
   dedup is a later optimization.
2. Build `shareManifestV2` (id → blob → size), version = *remote version + 1* so a
   lost local state cannot regress subscribers.
3. `sealManifest`: age-encrypt the manifest to the recipients, then ed25519-sign
   `locator ‖ version ‖ payload`.
4. **Commit in manifest-pointer order:** put all blobs → egress-audit the prefix →
   put the manifest (the single linearization point) → delete now-orphaned blobs.

A reader mid-publish sees either the old manifest (its blobs still present) or the
new one (its blobs uploaded before the flip). Orphan deletion only removes blobs
no manifest references.

## Fetch (bucket)

`bucketFetch` verifies in order — **authenticity → TOFU pin → freshness**
(`verifyEnvelope`) — *before* any blob is fetched or the payload decrypted, then
downloads and `sha256`-checks each blob and **materializes a v1-layout directory**
(`memories/<id>.md.age` + a schema-1 `share.json`) into a throwaway dir. The
existing `shareImport` validates and indexes that dir unchanged — so id-spoof,
scope-mismatch, size, and case-fold checks run for **every** backend via that
shared backstop. (`bucketFetch` additionally pre-checks the signed manifest's
entries before download, as defense in depth on a distinct artifact.) A mid-fetch
failure discards the throwaway dir. The real corpus is never partially written.

## Ledger

`shares.json` grew a backward-compatible discriminator: `transportRef{kind, bucket}`
on each publish/subscription. **Absent ⇒ git**, so v1 rows parse and behave
unchanged. Subscriptions also carry `pinned_pubkey` (TOFU) and `last_version`
(anti-rollback). Credentials are never stored — `bucketConfig.secret_ref` names an
env-var prefix (`MORA_SHARE_*`, falling back to `AWS_*`).

## CLI

git stays the default; `--via r2|s3|b2|bucket` opts into a bucket. `push`/`pull`/`list`
are identical regardless of transport. Only bootstrap differs:

```
mora share init  acme --scope project:acme --recipient age1… --via r2 --bucket b --endpoint <url> --prefix shares/acme
mora share push  acme                                  # previews the full set, then publishes
mora share subscribe neil --via r2 --bucket b --endpoint <url> --prefix shares/neil --confirm-pin <fingerprint>
mora share pull  [neil]                                # no name → pulls every subscription
```

`--remote`/`--github` still select git. Because a pasted bucket URL is a MITM-able
first-contact channel, a first bucket `subscribe` **requires** `--confirm-pin` to
match the publisher's fingerprint (surfaced by the subscriber's first `subscribe` attempt, then confirmed out of band with the publisher); TOFU alone would pin
whatever key first contact served.

## Invariants (implementation seam)

- **Pure Go / CGO=0.** The S3 adapter uses `aws-sdk-go-v2` (pure Go); CI's
  `CGO_ENABLED=0 go build ./...` gate keeps the static-binary thesis honest.
- **git path unchanged.** `bucketOf(nil) == nil`, so every git verb runs its
  original code. The existing share suite is the equivalence oracle.
- **No live-vault fusion.** All bucket state lives under `<DataDir>/share/subs/<name>/`,
  exactly as the git clone did.

## Deferred (designed for, not built)

- **PAKE onboarding** (Option A "Codebook") — a spoken invite code that removes the
  paste-a-URL + confirm-fingerprint step. It is a synchronous both-online handshake
  (the recipient's read key must flow back to the publisher), a genuinely new UX vs
  v1's async model.
- **Nostr transport** — gated on two unsolved problems: an O(N) manifest exceeds the
  ~40 KiB event cap, and chunking breaks the one-blob-one-memory import invariant.
- **Dedup / partial re-upload**, **live transport migration**, and **dynamic
  group membership** (key-epoch re-encryption) are all future work.

Whether to build any of the above is gated on instrumenting v1 share usage first —
the strategic view is that a bucket path is the cheapest real friction win, and the
rest should follow evidence of demand.
