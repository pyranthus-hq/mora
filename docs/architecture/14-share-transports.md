# 14 — Share transports

`mora share` (see [13-sharing.md](13-sharing.md)) originally moved memories over
exactly one transport: a private git remote. This document describes the seam
that lets a share travel over a **user-owned S3/R2 bucket** as well, and how the
same confidentiality/authenticity/freshness guarantees are provided on a backend
that — unlike a private git repo — has no write-ACL and whose locator is a
read-all bearer token.

## The one idea

A share is `scope + recipients + a transport locator`. The neutral core —
collect a scope, diff, age-encrypt, decrypt, validate, index, fuse — never names
a backend. Every transport obeys one rule:

> **Write content-addressed ciphertext blobs, then flip ONE signed manifest that
> names the current set. A reader resolves the set via the manifest, verifies it
> before touching a byte, and hash-checks every blob.**

This is a strict generalization of what git already did (`commit` + `share.json`).
Once it holds, the publish and pull loops are identical across backends.

## The two axes

v1 entangled two independent concerns; they are now split.

| Axis | When | git | bucket |
|---|---|---|---|
| **Coordination / bootstrap** | once | clone / grant repo read | paste locator + confirm fingerprint out of band |
| **Transport** | recurring | commit + push / pull `--ff-only` | put/get objects + flip manifest |

All *trust* lives in coordination (TOFU-pinning the publisher key); transport just
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

1. Encrypt each memory in scope; the blob's storage key is `sha256(ciphertext)`
   (`blobKey`). v1 re-encrypts the full set each push (authored notes are small);
   dedup is a later optimization.
2. Build `shareManifestV2` (id → blob → size), version = *remote version + 1* so a
   lost local state cannot regress subscribers.
3. `sealManifest`: age-encrypt the manifest to the recipients, then ed25519-sign
   `locator ‖ version ‖ payload`.
4. **Commit in manifest-pointer order:** put all blobs → egress-audit the prefix →
   put the manifest (the single linearization point) → delete now-orphaned blobs.

A reader mid-publish sees either the old manifest (its blobs still present) or the
new one (its blobs uploaded before the flip); orphan deletion only removes blobs
no manifest references.

## Fetch (bucket)

`bucketFetch` verifies in order — **authenticity → TOFU pin → freshness**
(`verifyEnvelope`) — *before* any blob is fetched or the payload decrypted, then
downloads and `sha256`-checks each blob and **materializes a v1-layout directory**
(`memories/<id>.md.age` + a schema-1 `share.json`) into a throwaway dir. The
existing `shareImport` validates and indexes that dir unchanged — so id-spoof,
scope-mismatch, size, and case-fold checks are shared across every backend, not
reimplemented. A mid-fetch failure discards the throwaway dir; the real corpus is
never partially written.

## Ledger

`shares.json` grew a backward-compatible discriminator: `transportRef{kind, bucket}`
on each publish/subscription. **Absent ⇒ git**, so v1 rows parse and behave
unchanged. Subscriptions also carry `pinned_pubkey` (TOFU) and `last_version`
(anti-rollback). Credentials are never stored — `bucketConfig.secret_ref` names an
env-var prefix (`MORA_SHARE_*`, falling back to `AWS_*`).

## CLI

git stays the default; `--via r2|s3|bucket` opts into a bucket. `push`/`pull`/`list`
are identical regardless of transport; only bootstrap differs:

```
mora share init  acme --scope project:acme --recipient age1… --via r2 --bucket b --endpoint <url> --prefix shares/acme
mora share push  acme                                  # previews the full set, then publishes
mora share subscribe neil --via r2 --bucket b --endpoint <url> --prefix shares/neil --confirm-pin <fingerprint>
mora share pull  [neil | --all]
```

`--remote`/`--github` still select git. Because a pasted bucket URL is a MITM-able
first-contact channel, a first bucket `subscribe` **requires** `--confirm-pin` to
match the publisher's fingerprint (printed by the publisher); TOFU alone would pin
whatever key first contact served.

## Invariants (implementation seam)

- **Pure Go / CGO=0.** The S3 adapter uses `aws-sdk-go-v2` (pure Go); CI's
  `CGO_ENABLED=0 go build ./...` gate keeps the static-binary thesis honest.
- **git path unchanged.** `bucketOf(nil) == nil`, so every git verb runs its
  original code; the existing share suite is the equivalence oracle.
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
