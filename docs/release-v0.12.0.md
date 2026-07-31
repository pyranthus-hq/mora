<!-- mora-v0.12.0-app-migration -->
## macOS: migrate once to the signed Mora.app

v0.12.0 introduces the branded, Developer ID-signed, notarized, and stapled
`Mora.app`. The app is Mora's stable Full Disk Access target. Install the whole
bundle; do not replace only its inner binary, clear quarantine, or re-sign it.

```bash
(
  set -e
  mora_installer="$(mktemp -t mora-install)"
  trap '/bin/rm -f "$mora_installer"' EXIT
  curl -fsSLo "$mora_installer" https://raw.githubusercontent.com/pyranthus-hq/mora/v0.12.0/install-app.sh
  sh "$mora_installer"
)
```

The installer verifies `checksums-app.txt`, the exact Apple identity, both code
signatures, the stapled ticket, Gatekeeper acceptance, architecture, and
version. It installs `~/Applications/Mora.app`, changes the active `mora`
command to point inside that app, and preserves the prior standalone binary as
`mora.standalone-backup` next to the command.

Then add `~/Applications/Mora.app` in **System Settings > Privacy & Security >
Full Disk Access**. Keep the old Mora entry until both checks pass:

```bash
mora doctor
mora sync imessage
```

If either check fails, keep the old FDA entry and remove only the verified app:

```bash
(
  set -e
  mora_uninstaller="$(mktemp -t mora-uninstall)"
  trap '/bin/rm -f "$mora_uninstaller"' EXIT
  curl -fsSLo "$mora_uninstaller" https://raw.githubusercontent.com/pyranthus-hq/mora/v0.12.0/uninstall-app.sh
  sh "$mora_uninstaller"
)
```

Then move the exact `.standalone-backup` path printed by the installer back to
its original `mora` pathname. The uninstaller preserves the vault, config,
state, and backup.

This is the planned app migration grant. It is not yet proven to be the last.
We will call FDA continuity proven only after a real signed v0.12.0 to v0.12.1
whole-bundle upgrade passes the protected-data checks without a re-grant.
