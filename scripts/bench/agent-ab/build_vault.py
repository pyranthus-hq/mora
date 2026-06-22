#!/usr/bin/env python3
"""Materialize a synthetic Mora vault from the authoring workflow's JSON output.

Usage: build_vault.py <world.json> <MORA_CONFIG_DIR>
  world.json: { memories: [{source,from,participants,date,mailbox,scope,title,body}], questions: [...] }

Everything is written into the isolated MORA_CONFIG_DIR sandbox (vault, index,
data, state all re-rooted there) — it never touches the user's real ~/vault/mora
or ~/.config/mora. Re-runnable: wipes and rebuilds the sandbox each time.
"""
import json, os, shutil, subprocess, sys

def main():
    world_path, cfg = sys.argv[1], sys.argv[2]
    world = json.load(open(world_path))
    mems = world.get("memories", [])
    env = {**os.environ, "MORA_CONFIG_DIR": cfg}

    shutil.rmtree(cfg, ignore_errors=True)
    os.makedirs(cfg, exist_ok=True)
    subprocess.run(["mora", "init", "--vault", os.path.join(cfg, "vault")],
                   env=env, check=True, capture_output=True)

    ok = 0
    for m in mems:
        meta = f"From: {m.get('from','')} | Participants: {', '.join(m.get('participants',[]))} | Date: {m.get('date','')} | Source: {m.get('source','')}"
        if m.get("mailbox"):
            meta += f" | Mailbox: {m['mailbox']}"
        text = meta + "\n\n" + (m.get("body","") or "")
        r = subprocess.run(
            ["mora", "write",
             "--scope", m.get("scope", "personal"),
             "--type", m.get("source", "note"),
             "--title", m.get("title", "(untitled)"),
             "--text", text],
            env=env, capture_output=True, text=True)
        if r.returncode == 0:
            ok += 1
        else:
            print(f"  WARN write failed: {m.get('title','?')[:50]} :: {r.stderr.strip()[:120]}", file=sys.stderr)

    subprocess.run(["mora", "index", "rebuild"], env=env, check=True, capture_output=True)
    # smoke test: the vault must be queryable
    probe = subprocess.run(["mora", "search", "Northwind pilot", "--json"],
                           env=env, capture_output=True, text=True)
    print(f"built {ok}/{len(mems)} memories into {cfg}/vault ; index rebuilt ; "
          f"smoke search rc={probe.returncode}")
    # stash the gold questions next to the vault for the judge
    json.dump(world.get("questions", []),
              open(os.path.join(cfg, "questions.json"), "w"), indent=1)
    print(f"wrote {len(world.get('questions', []))} gold questions to {cfg}/questions.json")

if __name__ == "__main__":
    main()
