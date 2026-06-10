# iMessage decoder fixtures

This directory holds **real, consented `attributedBody` blob fixtures** used to
calibrate and regression-test the typedstream decoder (`typedstream.go`, IMSG-02).

## What goes here

- At least **one modern-era `attributedBody` blob** (macOS 15.6.1 Sequoia / macOS 26
  Tahoe) whose message body lives ONLY in `attributedBody` (the `text` column is
  NULL) — the exact case text-only ingest would render empty.
- Optionally one older-era blob to cover the preamble variance noted in RESEARCH
  Assumption A2.

Store each as a raw byte file: `fixtures/<short-name>.bin` (the raw `attributedBody`
BLOB bytes, no base64). Keep them small and personally consented — these are real
private message bytes, so use innocuous test messages you sent yourself.

## How the dev extracts a real blob (FDA-granted terminal)

The agent that authored this connector did NOT have Full Disk Access, so the
synthetic-blob unit tests (default CI) carry the decoder's correctness gates and the
real-fixture assertions are gated behind the `livedb` build tag. To populate a real
fixture, run in a terminal that HAS Full Disk Access (System Settings → Privacy &
Security → Full Disk Access → enable your terminal):

```sh
# 1. Find a recent message whose text is NULL but attributedBody is present:
sqlite3 "file:$HOME/Library/Messages/chat.db?mode=ro" \
  "SELECT ROWID FROM message
   WHERE text IS NULL AND attributedBody IS NOT NULL
   ORDER BY date DESC LIMIT 5;"

# 2. Dump that message's attributedBody BLOB to a raw .bin fixture (replace ROWID):
sqlite3 "file:$HOME/Library/Messages/chat.db?mode=ro" \
  "SELECT writefile('internal/imessage/fixtures/modern.bin', attributedBody)
   FROM message WHERE ROWID = <ROWID>;"
```

Then run the real-fixture decoder gate (manual, FDA terminal):

```sh
go test ./internal/imessage/ -run TestDecodeAttributedBody -tags=livedb
```

The synthetic-blob assertions in `typedstream_test.go` run in default CI and cover
the load-bearing edge cases (the 0x81 length-prefix advance-by-3 fix, emoji/CJK
UTF-8 safety, nil, and truncated/malformed-no-panic) without any real fixture.

## Never commit non-consented message bytes

Only commit blobs from test messages you sent yourself and are comfortable making
public in the repo. When in doubt, leave the real fixture out and rely on the
synthetic gates + the manual `livedb` run.
