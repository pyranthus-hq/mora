# Mac approval spike

The native AppKit alert shows one of two fixed synthetic notes in full,
one saved and one held. Cancel is the default. **Keep with macOS approval** requests a signature
from a Secure Enclave P-256 key protected by a user-presence policy. The test
key is ephemeral and its device-wrapped representation stays in memory.
The runner uses a fresh LocalAuthentication context for signing with Touch ID
reuse set to zero. The native prompt may offer device credentials, depending
on the system.

## Build and verify without a prompt

Run from the repository root on an Apple silicon Mac with the Swift toolchain.

```sh
build_dir="$(mktemp -d /private/tmp/mora-review-mac.XXXXXX)"
TMPDIR="$build_dir" swiftc -parse-as-library -warnings-as-errors \
  -target arm64-apple-macos14.0 \
  experiments/review-approval/mac/ApprovalSpike.swift \
  -o "$build_dir/mora-review-mac-spike"
"$build_dir/mora-review-mac-spike" --self-test
"$build_dir/mora-review-mac-spike" --preflight
codesign --verify "$build_dir/mora-review-mac-spike"
```

The 24 assertions use software keys. The checks cover a successful decision
for each queue type, missing and malformed proofs, wrong signer, changed
text, scope and action, expiry, cancellation, replay, and reuse across a new
verifier session. Preflight queries OS capability and prints `decisions=0`.
It creates no key and requests no authentication. The compiler supplies the
usual local ad hoc signature. No production signing is needed for the build.

## Run with the owner

In an unlocked Mac desktop session, run:

```sh
"$build_dir/mora-review-mac-spike" --saved
```

1. Inspect the complete preview, revision and Keep action. Select **Cancel**.
   Expect `CANCELLED decisions=0` and no authentication prompt.
2. Run the command again, choose **Keep with macOS approval**, then cancel
   or decline the macOS authentication. Expect `READ_ONLY … decisions=0`.
3. Run it again, inspect the preview, choose **Keep with macOS approval**,
   and complete the macOS prompt yourself. Confirm a fresh native prompt
   appeared before recording `VERIFIED_SYNTHETIC_DECISION … decisions=1`.
4. Repeat the successful path with `--held`. If macOS cannot use the key,
   or signing succeeds without a fresh prompt, leave native approval open.

When platform verification is unavailable, the preview remains visible and
offers only **Close**. The runner prints `READ_ONLY … decisions=0`.

## What this tests

The preview digest covers item ID, full title and body, project scope, type,
source, provenance, queue kind and review state. The verifier expects a
90-second, single-use challenge bound to the process session, a random
nonce, the revision, scope and Keep. It checks the signature against the
ephemeral key registered in that process and rechecks the current preview.
Each run accepts at most one synthetic decision. There is no vault access,
IPC service, exported key, or persistent approval. macOS controls the
authentication prompt's layout and text. The signature receipt verifies
the challenge and signer. Confirm the fresh prompt by observing it during
each native test.

The spike can demonstrate hardware-backed user-presence signing in the
local runner. A production integration still needs a trusted app identity
and owner-controlled persistent key enrollment, an authenticated path to the
Go verifier, atomic receipt consumption and vault mutation, and a live
test that proves the person sees what the verifier commits. Before
approving arbitrary note text, use structured, unambiguous fields with
scrolling within a bounded preview area. Verify equality between the full
rendered preview and the revision bound to the challenge. Leave approval
read-only if the preview is incomplete. The canonical encoding shared by
Swift and Go remains an integration decision. The spike's sorted Swift JSON
only serves the in-process verifier.

[Apple Secure Enclave key guidance](https://developer.apple.com/documentation/security/protecting-keys-with-the-secure-enclave)

[Apple CryptoKit authentication context](https://developer.apple.com/documentation/cryptokit/secureenclave/p256/signing/privatekey/init(datarepresentation:authenticationcontext:))
