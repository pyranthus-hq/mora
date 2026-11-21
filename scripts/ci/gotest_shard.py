#!/usr/bin/env python3
"""Run one shard of a Go package's tests on every core, and prove coverage.

`go test` runs the tests of ONE package serially in ONE process unless a test
calls t.Parallel(). internal/mora has ~2,300 tests and almost none can be
parallel (t.Setenv / t.Chdir forbid it), so a 4-core runner spent ~6 minutes
on one core. This script compiles the test binary once and runs it as N
worker PROCESSES, one per core, each on a disjoint slice of the top-level
tests. Process isolation keeps every test exactly
as hermetic as it is under `go test` (one process, serial), just not alone on
the machine.

Partitioning is deterministic so every CI shard computes the SAME full plan
and can prove it locally, with no aggregation job:

  * the test list comes from the compiled binary (`-test.list`), not a regex;
  * weights come from a COMMITTED timings file (so the plan is a function of
    the checked-out commit and every shard agrees on it), else 1s each; a
    test missing from the timings gets the median weight. Main's full runs
    record fresh per-test times (--record, uploaded as an artifact) and
    scripts/ci/refresh-test-timings.sh rewrites the committed file from them;
  * tests are dealt longest-first onto the lightest bin (LPT), ties broken
    by name.

Before running, the script asserts the plan assigns every listed test to
exactly one shard. After running, it asserts every test assigned to THIS
shard reported a terminal result (pass/fail/skip) — a worker that panics or
times out mid-slice cannot silently drop the rest of its slice. Then it
writes this shard's per-test durations for main to record.

Nothing is retried. A failing test fails the shard and its output is printed.

Usage (from the repo root):

  python3 scripts/ci/gotest_shard.py --pkg ./internal/mora --shard 0 --shards 4 \
      --build-flag=-race --seed scripts/ci/test-timings/linux-race.tsv \
      --record out.tsv
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import shutil
import statistics
import subprocess
import sys
import tempfile
import threading
import time
from pathlib import Path

DEFAULT_WEIGHT = 1.0


def log(msg: str) -> None:
    print(msg, flush=True)


def run(cmd: list[str], **kw) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, check=True, text=True, **kw)


def load_timings(paths: list[Path]) -> dict[str, float]:
    out: dict[str, float] = {}
    for p in paths:
        for line in p.read_text(encoding="utf-8").splitlines():
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            name, _, secs = line.partition("\t")
            try:
                out[name] = float(secs)
            except ValueError:
                raise SystemExit(f"bad timings line in {p}: {line!r}")
    return out


def lpt(names: list[str], weight: dict[str, float], bins: int) -> list[list[str]]:
    """Longest-processing-time-first onto the lightest bin. Deterministic."""
    order = sorted(names, key=lambda n: (-weight[n], n))
    loads = [0.0] * bins
    out: list[list[str]] = [[] for _ in range(bins)]
    for n in order:
        i = min(range(bins), key=lambda b: (loads[b], b))
        out[i].append(n)
        loads[i] += weight[n]
    return [sorted(b) for b in out]


class Worker(threading.Thread):
    def __init__(self, idx, binary, pkg_dir, names, timeout, extra, env):
        super().__init__(daemon=True)
        self.idx, self.binary, self.pkg_dir = idx, binary, pkg_dir
        self.names, self.timeout, self.extra, self.env = names, timeout, extra, env
        self.results: dict[str, str] = {}  # top-level test -> pass|fail|skip
        self.elapsed: dict[str, float] = {}
        self.output: dict[str, list[str]] = {}
        self.stray: list[str] = []  # output not attributed to a test
        self.rc = None
        self.wall = 0.0

    def run(self):
        pattern = "^(" + "|".join(self.names) + ")$"
        cmd = ["go", "tool", "test2json", "-t", str(self.binary),
               "-test.v=test2json", "-test.run", pattern,
               "-test.timeout", self.timeout, *self.extra]
        start = time.monotonic()
        proc = subprocess.Popen(cmd, cwd=self.pkg_dir, env=self.env, text=True,
                                encoding="utf-8", errors="replace",
                                stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
        for line in proc.stdout:
            try:
                ev = json.loads(line)
            except json.JSONDecodeError:
                self.stray.append(line)
                continue
            test = ev.get("Test")
            action = ev.get("Action")
            if not test:
                if action == "output":
                    self.stray.append(ev.get("Output", ""))
                continue
            top = test.split("/", 1)[0]
            if action == "output":
                self.output.setdefault(top, []).append(ev.get("Output", ""))
            elif action in ("pass", "fail", "skip") and "/" not in test:
                self.results[test] = action
                self.elapsed[test] = float(ev.get("Elapsed", 0.0))
        self.rc = proc.wait()
        self.wall = time.monotonic() - start


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--pkg", required=True, help="package path, e.g. ./internal/mora")
    ap.add_argument("--shard", type=int, required=True)
    ap.add_argument("--shards", type=int, required=True)
    ap.add_argument("--workers", type=int, default=0, help="0 = one per core")
    ap.add_argument("--build-flag", action="append", default=[],
                    help="flag for `go test -c`, e.g. --build-flag=-race")
    ap.add_argument("--test-flag", action="append", default=[],
                    help="extra flag for the test binary, e.g. --test-flag=-test.shuffle=on")
    ap.add_argument("--timeout", default="20m")
    ap.add_argument("--seed", action="append", default=[],
                    help="committed timings TSV; the first that exists is used")
    ap.add_argument("--record", help="write this shard's per-test durations here")
    ap.add_argument("--plan-only", action="store_true",
                    help="print and verify the plan, run nothing")
    a = ap.parse_args()
    if not 0 <= a.shard < a.shards:
        raise SystemExit(f"--shard {a.shard} out of range for --shards {a.shards}")

    workers = a.workers or os.cpu_count() or 1
    pkg_dir = Path(a.pkg).resolve()
    work = Path(tempfile.mkdtemp(prefix="gotest-shard-"))
    exe = ".exe" if os.name == "nt" else ""
    binary = work / f"pkg.test{exe}"

    t0 = time.monotonic()
    run(["go", "test", "-c", "-vet=off", *a.build_flag, "-o", str(binary), a.pkg])
    build_s = time.monotonic() - t0

    listed = run([str(binary), "-test.list", "."], cwd=pkg_dir,
                 capture_output=True).stdout.split()
    # `go test` runs Test*, Example* and Fuzz* seed corpora; never benchmarks.
    names = sorted(n for n in listed if not n.startswith("Benchmark"))
    if not names:
        raise SystemExit(f"no tests listed in {a.pkg}")

    source = "uniform"
    timings: dict[str, float] = {}
    seed = next((Path(p) for p in a.seed if Path(p).is_file()), None)
    if seed:
        timings, source = load_timings([seed]), str(seed)
    known = [timings[n] for n in names if n in timings]
    fallback = statistics.median(known) if known else DEFAULT_WEIGHT
    weight = {n: max(timings.get(n, fallback), 0.001) for n in names}
    unknown = sum(1 for n in names if n not in timings)

    plan = lpt(names, weight, a.shards)
    # Proof 1: the plan is a partition of the listed tests.
    flat = [n for s in plan for n in s]
    if sorted(flat) != names or len(set(flat)) != len(flat):
        raise SystemExit("::error::shard plan is not an exact partition of the listed tests")
    fp = hashlib.sha256("\n".join(f"{i}\t{n}" for i, s in enumerate(plan) for n in s)
                        .encode()).hexdigest()[:16]
    log(f"package {a.pkg}: {len(names)} tests, weights from {source} "
        f"({unknown} without a recorded time, weighted {fallback:.2f}s)")
    log(f"plan {fp}: " + ", ".join(
        f"shard {i}={len(s)} tests/{sum(weight[n] for n in s):.0f}s" for i, s in enumerate(plan)))
    mine = plan[a.shard]
    slices = [s for s in lpt(mine, weight, min(workers, len(mine))) if s]
    log(f"shard {a.shard}: {len(mine)} tests on {len(slices)} worker processes; "
        f"test binary built in {build_s:.1f}s")
    if a.plan_only:
        return 0

    # A coverage build: give each worker the GOCOVERDIR `go test -cover` would,
    # so re-exec'd child test binaries do not warn about it on stderr.
    cover = any(f.startswith(("-cover", "-coverpkg")) for f in a.build_flag)
    base_env = dict(os.environ)
    threads = []
    covdirs = []
    for i, s in enumerate(slices):
        # Same environment `go test` gives the binary. TMPDIR is deliberately
        # NOT moved: t.TempDir() names are already unique per process, and some
        # tests' payloads embed the temp path (MCP budget ceilings count bytes),
        # so a longer TMPDIR would change what they measure.
        env = dict(base_env)
        extra = list(a.test_flag)
        if cover:
            cov = work / f"cover{i}"
            cov.mkdir()
            covdirs.append(str(cov))
            env["GOCOVERDIR"] = str(cov)
            extra.append(f"-test.gocoverdir={cov}")
        threads.append(Worker(i, binary, pkg_dir, s, a.timeout, extra, env))
    t1 = time.monotonic()
    for w in threads:
        w.start()
    for w in threads:
        w.join()
    run_s = time.monotonic() - t1

    failed: list[str] = []
    missing: list[str] = []
    bad_exit: list[Worker] = []
    elapsed: dict[str, float] = {}
    for w in threads:
        elapsed.update(w.elapsed)
        for n in w.names:
            r = w.results.get(n)
            if r is None:
                missing.append(n)
            elif r == "fail":
                failed.append(n)
                log(f"\n--- FAIL: {n} (worker {w.idx})")
                sys.stdout.write("".join(w.output.get(n, [])))
        if w.rc != 0 and not any(w.results.get(n) == "fail" for n in w.names):
            bad_exit.append(w)
    for w in bad_exit:
        # Exit status without a failing test: panic, timeout, or a race report
        # after the last test. Show what the process said outside any test.
        log(f"\n::error::worker {w.idx} exited {w.rc} without a failing test; its output:")
        sys.stdout.write("".join(w.stray[-200:]))
        last = [n for n in w.names if n in w.output and n not in w.results]
        for n in last:
            log(f"\n--- INCOMPLETE: {n}")
            sys.stdout.write("".join(w.output[n][-200:]))

    for w in threads:
        log(f"worker {w.idx}: {len(w.names)} tests, exit {w.rc}, {w.wall:.1f}s")
    slow = sorted(elapsed.items(), key=lambda kv: -kv[1])[:5]
    log("slowest: " + ", ".join(f"{n} {s:.1f}s" for n, s in slow))
    log(f"shard {a.shard}: build {build_s:.1f}s, run {run_s:.1f}s, "
        f"{len(failed)} failed, {len(missing)} without a result")

    if covdirs:
        pct = subprocess.run(["go", "tool", "covdata", "percent", "-i=" + ",".join(covdirs)],
                             text=True, capture_output=True)
        log("coverage (this shard, merged across workers): "
            + (pct.stdout.strip() or pct.stderr.strip()))

    if a.record:
        Path(a.record).parent.mkdir(parents=True, exist_ok=True)
        with open(a.record, "w", encoding="utf-8") as f:
            for n in sorted(elapsed):
                f.write(f"{n}\t{elapsed[n]:.3f}\n")

    shutil.rmtree(work, ignore_errors=True)
    rc = 0
    if failed:
        log(f"::error::{len(failed)} test(s) failed: {' '.join(failed)}")
        rc = 1
    if missing:
        # Proof 2: every test assigned to this shard reported a result.
        log(f"::error::{len(missing)} assigned test(s) never reported a result: "
            f"{' '.join(missing[:50])}")
        rc = 1
    if bad_exit:
        rc = 1
    if rc == 0:
        log(f"ok  {a.pkg} shard {a.shard}/{a.shards}: {len(mine)} tests")
    return rc


if __name__ == "__main__":
    sys.exit(main())
