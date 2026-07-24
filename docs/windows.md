# Mora on Windows

Mora for Windows uses the same local-first, pure-Go binary. It stores memories
on your disk and uses read-only connector scopes. It runs no cloud service.

## Install

Run this from PowerShell:

```powershell
iwr https://raw.githubusercontent.com/pyranthus-hq/mora/main/install.ps1 -OutFile $env:TEMP\install-mora.ps1; powershell -ExecutionPolicy Bypass -File $env:TEMP\install-mora.ps1
```

The installer does these steps:

- resolves the latest release, or uses `-Version 0.10.0` when pinned
- downloads `mora_<version>_windows_amd64.zip`
- downloads `checksums.txt`
- verifies the zip with `Get-FileHash -Algorithm SHA256`
- extracts `mora.exe` to `%LOCALAPPDATA%\Mora\bin\mora.exe`
- adds `%LOCALAPPDATA%\Mora\bin` to the User PATH
- runs `mora init` against `%USERPROFILE%\vault\mora`

After the install, open a new PowerShell window. PowerShell can then find
`mora` on PATH.

## SmartScreen

The v1 Windows binary has no signature. Windows can show **Windows protected
your PC**. First, make sure the installer printed a checksum success. Then
choose **More info > Run anyway**.

You can also clear the downloaded-file marker:

```powershell
Unblock-File "$env:LOCALAPPDATA\Mora\bin\mora.exe"
```

This does not replace the checksum check. The installer will not extract or run
a release zip when `checksums.txt` does not match.

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

On Windows, the macOS-only connectors print a clear macOS-only message. They
exit non-zero and leave the source off.

## Scheduling

Mora does not run a background daemon. `mora schedule` adds known jobs to the
OS scheduler.

On Windows, `mora schedule install <job>` calls Task Scheduler through
`schtasks`. Task names use the `Mora\<job>` form:

```powershell
mora schedule install ingest-hourly
mora schedule install pulse-daily
mora schedule list
```

The scheduled job names are:

- `pulse-daily`
- `doctor-pulse`
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

The uninstaller removes `%LOCALAPPDATA%\Mora` and the User PATH entry. It also
deletes Task Scheduler entries under `\Mora\`. By default, it keeps your vault,
config, data, and state paths.

To also delete Mora data paths:

```powershell
powershell -ExecutionPolicy Bypass -File $env:TEMP\uninstall-mora.ps1 -Purge
```

Use `-Yes` with `-Purge` only to skip the confirmation prompt.

## Deferred

The v1 Windows package is a zip file with a PowerShell installer. Later work
includes MSI/MSIX packages, winget and Scoop manifests, and Authenticode
signing. It also includes native Windows alerts and windows/arm64 release
archives.
