#!/usr/bin/env python3
"""FREE pre-spend gate for the MMR on/off agent A/B.

MMR only changes an agent's end-task answer if it changes what the agent RETRIEVES.
This measures, for $0, whether MMR raises distinct-gold-evidence recall@k on the
large vault under Ollama. If it does not, the billed agent A/B (mora-mcp vs
mora-mcp-mmr) cannot plausibly show an MMR win — so DO NOT fund it.

Usage:
  mmr_recall_gate.py <MORA_CONFIG_DIR> [world.json]
Prereqs:
  - the vault is built + indexed under Ollama (build_vault.py + `mora config embedder
    ollama` + `mora index rebuild`);
  - `mora` on PATH supports `config mmr` (a from-source build; an old install does NOT).

Gate: PASS only if MMR-on raises distinct-gold recall@8 by >=1 evidence doc on >=3
questions (and does not net-regress in aggregate). Today's stock corpus FAILS this:
its distractors DILUTE (push gold down by volume) rather than near-DUPLICATE a needed
fact, so MMR has nothing to demote that was crowding out a needed gold doc.
"""
import json, sqlite3, subprocess, os, glob, statistics, sys
from collections import defaultdict

cfg = sys.argv[1] if len(sys.argv) > 1 else sys.exit("usage: mmr_recall_gate.py <MORA_CONFIG_DIR> [world.json]")
here = os.path.dirname(os.path.abspath(__file__))
world_path = sys.argv[2] if len(sys.argv) > 2 else os.path.join(here, "world.json")
env = dict(os.environ, MORA_CONFIG_DIR=cfg)

# Hard-fail if the binary lacks the setter (else we'd silently compare MMR-off to MMR-off).
if subprocess.run(["mora", "config", "mmr", "off"], env=env, capture_output=True).returncode != 0:
    sys.exit("FATAL: 'mora config mmr' unsupported by the mora on PATH — build from source "
             "(go build -o /tmp/mora-src ./cmd/mora && export PATH=/tmp/mora-src:$PATH).")

dbs = [d for d in glob.glob(cfg + "/**/*.db", recursive=True) if "index" in d.lower()] \
      or glob.glob(cfg + "/**/*.db", recursive=True)
if not dbs:
    sys.exit("no index db under " + cfg + " — build + index the vault first")
t2ids = defaultdict(list)
for i, t in sqlite3.connect(dbs[0]).execute("SELECT id, title FROM memories"):
    t2ids[t].append(i)
qs = json.load(open(world_path))["questions"]

def setmmr(v): subprocess.run(["mora", "config", "mmr", v], env=env, capture_output=True)
def search(q, k):
    r = subprocess.run(["mora", "search", q, "--limit", str(k), "--json"], env=env, capture_output=True, text=True)
    try: d = json.loads(r.stdout)
    except Exception: return []
    return [m["id"] for m in (d["results"] if isinstance(d, dict) else d)][:k]

K = 8  # the agent's operating window (search_memory default limit)
wins = 0
deltas = []
print(f"db={dbs[0]}  k={K}")
for q in qs:
    gold = {sorted(t2ids[t])[0] for t in q["gold_evidence_titles"] if t in t2ids}  # earliest id = gold
    setmmr("off"); off = len(set(search(q["question"], K)) & gold)
    setmmr("on");  on  = len(set(search(q["question"], K)) & gold)
    d = on - off
    deltas.append(d / max(1, len(gold)))
    if d >= 1: wins += 1
    print(f"  {q['id']}: distinct-gold@{K}  off={off}/{len(gold)}  on={on}/{len(gold)}  delta={d:+d}")
setmmr("off")

net = statistics.mean(deltas)
print(f"\nquestions improved by >=1 gold doc: {wins}/{len(qs)}   mean recall delta: {net:+.3f}")
ok = wins >= 3 and net >= 0
print("GATE:", "PASS — MMR changes retrieval; the paid A/B can plausibly show an effect."
      if ok else "FAIL — MMR does not improve gold retrieval; a billed A/B would measure noise. DO NOT fund.")
sys.exit(0 if ok else 2)
