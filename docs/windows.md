# Mora on Windows

Mora for Windows is the same local-first, pure-Go binary. It stores memories on
your disk, uses read-only connector scopes, and does not run a cloud service.

## Install

Run this from PowerShell:

```powershell
iwr https://raw.githubusercontent.com/pyranthus-hq/mora/main/install.ps1 -OutFile $env:TEMP\install-mora.ps1; powershell -ExecutionPolicy Bypass -File $env:TEMP\install-mora.ps1
```

The installer:

- resolves the latest release, or uses `-Version 0.9.1` when pinned
- downloads `mora_<version>_windows_amd64.zip`
- downloads `checksums.txt`
- verifies the zip with `Get-FileHash -Algorithm SHA256`
- extracts `mora.exe` to `%LOCALAPPDATA%\Mora\bin\mora.exe`
- adds `%LOCALAPPDATA%\Mora\bin` to the User PATH
- runs `mora init` against `%USERPROFILE%\vault\mora`

Open a new PowerShell window after install so `mora` resolves on PATH.

## SmartScreen

The v1 Windows binary is unsigned. If Windows shows **Windows protected your
PC**, confirm that the installer printed a checksum success, then choose
**More info > Run anyway**.

You can also clear the downloaded-file marker explicitly:

```powershell
Unblock-File "$env:LOCALAPPDATA\Mora\bin\mora.exe"
```

This does not replace checksum verification. The installer refuses to extract
or run a release zip when `checksums.txt` does not match.

## Connectors

Supported on Windows:

- Gmail
- Google Calendar
- filesystem folders
- notes in indexed folders
- local Ollama embeddings

macOS-only:

- iMessage
- Apple Calendar
- Address Book lookup

The macOS-only connectors should print a clear macOS-only message on Windows,
exit non-zero, and leave the source disabled.

## Scheduling

Mora does not run a background daemon. `mora schedule` installs OS scheduler
entries for known jobs.

On Windows, `mora schedule install <job>` uses Task Scheduler through
`schtasks`. Task names use the `Mora\<job>` form:

```powershell
mora schedule install ingest-hourly
mora schedule install pulse-daily
mora schedule list
```

The scheduled job names are:

- `pulse-daily`
- `index-hourly`
- `backup-daily`
- `lint-weekly`
- `ingest-hourly`
- `git-daily`

`uninstall.ps1` removes all scheduled tasks under `\Mora\`.

## Uninstall

Run:

```powershell
iwr https://raw.githubusercontent.com/pyranthus-hq/mora/main/uninstall.ps1 -OutFile $env:TEMP\uninstall-mora.ps1; powershell -ExecutionPolicy Bypass -File $env:TEMP\uninstall-mora.ps1
```

The uninstaller removes `%LOCALAPPDATA%\Mora`, removes the User PATH entry, and
deletes Task Scheduler entries under `\Mora\`. It preserves your vault,
configuration, data, and state paths by default.

To also delete Mora data paths:

```powershell
powershell -ExecutionPolicy Bypass -File $env:TEMP\uninstall-mora.ps1 -Purge
```

Use `-Yes` with `-Purge` only when you want to skip the confirmation prompt.

## Deferred

The v1 Windows package is a zip plus PowerShell installer. MSI/MSIX packages,
winget and Scoop manifests, Authenticode signing, native Windows notifications,
and windows/arm64 release archives are deferred.
