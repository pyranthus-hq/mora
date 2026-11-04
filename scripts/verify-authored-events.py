#!/usr/bin/env python3
"""Compare baseline/candidate event CLI behavior using only a temporary synthetic vault.
Usage: python3 scripts/verify-authored-events.py BASELINE CANDIDATE SCRATCH_DIRECTORY
"""
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

baseline, candidate, scratch = sys.argv[1:]
root = Path(tempfile.mkdtemp(prefix="authored-events-", dir=scratch))
env = dict(os.environ, MORA_CONFIG_DIR=str(root / "config"), MORA_VAULT="", DO_NOT_TRACK="1")


def run(binary, *args):
    return subprocess.check_output([binary, *args], env=env, stdin=subprocess.DEVNULL)


def rows(raw):
    return json.loads(raw).get("memories") or []


def instant(value):
    return datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))


window = ("list", "--event-since-hours", "24", "--limit", "200", "--json")
run(baseline, "init")
run(baseline, "write", "--title", "Synthetic authored probe", "--text", "Synthetic note", "--json")
assert len(rows(run(baseline, "list", "--limit", "5", "--json"))) == 1
before = run(baseline, *window)
assert len(rows(before)) == 0
assert before == run(candidate, *window)
on = rows(run(candidate, *window, "--include-authored-writes"))
assert len(on) == 1 and on[0]["event_source"] == "authored_write"
assert instant(on[0]["event_at"]) == instant(on[0]["created_at"])
print("PASS: baseline gap=0; opt-in=1 authored_write; default bytes identical")

# Add a synthetic connector record so the byte comparison is nonempty too.
now = datetime.datetime.now(datetime.timezone.utc)
at = (now - datetime.timedelta(hours=1)).isoformat()
connector = root / "config/vault/sources/calendar/calendar_event_probe.md"
connector.parent.mkdir(parents=True, exist_ok=True)
connector.write_text('---\nid: "calendar_event/probe"\nscope: "global"\ntype: "event"\n'
                     'title: "Synthetic calendar probe"\nsource: "calendar"\nprovider: "calendar"\n'
                     'created_at: "' + at + '"\nmeta: {"occurred_at": "' + at + '"}\n---\nSynthetic event\n')
before = run(baseline, *window)
assert len(rows(before)) == 1
assert before == run(candidate, *window)
assert before == run(candidate, *window, "--include-authored-writes=false")
print("PASS: nonempty connector receipt byte-identical; sha256=" + hashlib.sha256(before).hexdigest())
limited = rows(run(candidate, "list", "--event-since-hours", "24", "--include-authored-writes", "--limit", "1", "--json"))
assert len(limited) == 1 and limited[0]["event_source"] == "authored_write"

# Exercise the real filesystem writer, keeping source content unchanged.
source = root / "source"
source.mkdir()
source_file = source / "note.md"
source_file.write_text("Synthetic unchanged agent note\n")
run(candidate, "connect", "filesystem", str(source), "--name", "codex-memory")
mirror = next((root / "config/vault/sources/filesystem/codex-memory").glob("*.md"))
old = (now - datetime.timedelta(hours=48)).isoformat()
mirror.write_text(re.sub(r'^created_at:.*$', 'created_at: "' + old + '"', mirror.read_text(), flags=re.MULTILINE))
assert len(rows(run(candidate, *window, "--include-authored-writes"))) == 2
before_mirror = mirror.read_text()
run(candidate, "sync", "filesystem")
assert mirror.read_text() == before_mirror  # unchanged manifest skips re-materialization
os.utime(str(source_file), (now.timestamp() + 60, now.timestamp() + 60))
run(candidate, "sync", "filesystem")
after = rows(run(candidate, *window, "--include-authored-writes"))
assert len(after) == 3
mirrors = [r for r in after if r["id"].startswith("src_")]
assert len(mirrors) == 1 and mirrors[0]["event_source"] == "authored_write"
assert instant(mirrors[0]["created_at"]) > instant(old)
assert mirrors[0]["text"].strip() == source_file.read_text().strip()
assert len(rows(run(candidate, *window))) == 1
print("PASS: unchanged manifest preserves age; mtime-only resync re-dates authored mirror into window; default remains connector-only")

native = [r for r in after if r["id"].startswith("mem_")][0]
run(candidate, "index", "rebuild", "--force")
after_rebuild = rows(run(candidate, *window, "--include-authored-writes"))
assert next(r for r in after_rebuild if r["id"] == native["id"])["created_at"] == native["created_at"]
print("PASS: index rebuild preserves native authored creation time")
print("Synthetic evidence directory: " + str(root))
