#!/usr/bin/env python3
"""Opt-in counts-only smoke. The candidate sees an isolated copy, never the live vault."""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--allow-vault-read", action="store_true")
    parser.add_argument("--binary", type=Path, required=True)
    parser.add_argument("--vault", type=Path, required=True)
    parser.add_argument("--hours", type=int, default=168)
    parser.add_argument("--query", default="meeting")
    args = parser.parse_args()
    if not args.allow_vault_read:
        parser.error("explicit --allow-vault-read is required")
    if not args.binary.is_absolute() or not args.binary.is_file():
        parser.error("--binary must name an absolute candidate executable")
    if not args.vault.is_absolute() or not args.vault.is_dir():
        parser.error("--vault must name an absolute directory")
    if not 1 <= args.hours <= 8784:
        parser.error("--hours must be 1-8784")
    try:
        # Refuse links rather than risk the copied vault pointing back to live data.
        if args.vault.is_symlink() or any(p.is_symlink() for p in args.vault.rglob("*")):
            raise ValueError("symlink")
        with tempfile.TemporaryDirectory(prefix="mora-smoke-") as temporary:
            root = Path(temporary)
            vault = root / "snapshot"
            shutil.copytree(args.vault, vault, ignore=shutil.ignore_patterns(".git"))
            env = {key: value for key, value in os.environ.items() if not key.startswith("MORA_")}
            env.update(MORA_CONFIG_DIR=str(root / "config"), MORA_VAULT=str(vault),
                       DO_NOT_TRACK="1", HOME=str(root / "home"))
            (root / "home").mkdir()
            commands = []
            for source in ("gmail", "imessage", "whatsapp", "calendar", "applecalendar"):
                commands.append(("list", source, ["list", "--source", source,
                    "--event-since-hours", str(args.hours), "--limit", "50", "--json"]))
            commands.append(("search", "gmail", ["search", args.query, "--source", "gmail", "--json"]))
            commands.append(("search-event", "gmail", ["search", args.query, "--source", "gmail",
                "--event-since-hours", str(args.hours), "--json"]))
            commands.append(("search-exclude", "gmail", ["search", args.query, "--source", "gmail",
                "--event-since-hours", str(args.hours), "--dispositions", "exclude:not-context", "--json"]))
            receipts = []
            for kind, source, command in commands:
                result = subprocess.run([str(args.binary), *command], env=env,
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=120, check=False)
                if result.returncode != 0:
                    raise ValueError("candidate command failed")
                payload = json.loads(result.stdout)
                payload = payload.get("data", payload)
                rows = payload.get("memories")
                if not isinstance(rows, list) or not all(isinstance(row, dict) for row in rows):
                    raise ValueError("invalid receipt")
                receipts.append({"command": kind, "source": source, "rows": len(rows),
                    "with_projection": sum(bool((row.get("evidence") or {}).get("readable")) for row in rows),
                    "with_participation": sum(isinstance(row.get("participation"), dict) for row in rows),
                    "no_basis": sum(row.get("automated") is None for row in rows)})
            for receipt in receipts:
                print(json.dumps(receipt, sort_keys=True))
    except (OSError, ValueError, subprocess.TimeoutExpired, TypeError, AttributeError):
        # Never echo raw stdout, stderr, subjects, paths, queries, or message text.
        print("Smoke failed; no personal output was emitted. Check candidate/configuration privately.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
