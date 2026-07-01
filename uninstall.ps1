<#
.SYNOPSIS
Uninstall Mora from Windows.

.DESCRIPTION
Removes the Windows Mora binary install under %LOCALAPPDATA%\Mora, removes that
bin directory from the current user's PATH, and deletes scheduled tasks under
\Mora\. Your vault and configuration are preserved unless -Purge is passed.

.PARAMETER Purge
Also delete the vault plus Mora config, data, and state paths. This is your data
and local credentials and is preserved by default.

.PARAMETER Yes
Do not prompt before deleting the vault when -Purge is passed.

.PARAMETER Vault
Vault path to purge. Defaults to %USERPROFILE%\vault\mora or MORA_VAULT.

.EXAMPLE
powershell -ExecutionPolicy Bypass -File .\uninstall.ps1

.EXAMPLE
powershell -ExecutionPolicy Bypass -File .\uninstall.ps1 -Purge
#>
[CmdletBinding()]
param(
    [switch]$Purge,
    [switch]$Yes,
    [string]$Vault = $(if ($env:MORA_VAULT) { $env:MORA_VAULT } else { Join-Path $HOME "vault\mora" })
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version 2.0

function Write-Step {
    param([string]$Message)
    Write-Host $Message
}

function Get-NormalizedPathEntry {
    param([string]$Path)
    return $Path.Trim().TrimEnd("\")
}

function Remove-UserPath {
    param([string]$PathToRemove)

    $current = [Environment]::GetEnvironmentVariable("Path", "User")
    if (-not $current) {
        return $false
    }

    $want = Get-NormalizedPathEntry $PathToRemove
    $parts = $current -split ";" | Where-Object { $_ -and $_.Trim() }
    $kept = @()
    $removed = $false

    foreach ($part in $parts) {
        if ((Get-NormalizedPathEntry $part).Equals($want, [StringComparison]::OrdinalIgnoreCase)) {
            $removed = $true
            continue
        }
        $kept += $part
    }

    if ($removed) {
        [Environment]::SetEnvironmentVariable("Path", ($kept -join ";"), "User")
        $env:Path = (($env:Path -split ";") | Where-Object {
            $_ -and -not (Get-NormalizedPathEntry $_).Equals($want, [StringComparison]::OrdinalIgnoreCase)
        }) -join ";"
    }
    return $removed
}

function Remove-MoraScheduledTasks {
    $rows = @()
    try {
        $raw = & schtasks.exe /Query /FO CSV 2>$null
        if ($LASTEXITCODE -eq 0 -and $raw) {
            $rows = $raw | ConvertFrom-Csv
        }
    } catch {
        Write-Warning "could not query Task Scheduler: $($_.Exception.Message)"
        return
    }

    $tasks = @()
    foreach ($row in $rows) {
        if ($null -eq $row.TaskName) {
            continue
        }
        $name = [string]$row.TaskName
        if ($name -like "\Mora\*" -or $name -like "Mora\*") {
            $tasks += $name
        }
    }

    if ($tasks.Count -eq 0) {
        Write-Step "No Mora scheduled tasks found."
        return
    }

    foreach ($task in ($tasks | Sort-Object -Unique)) {
        $tn = $task.TrimStart("\")
        & schtasks.exe /Delete /TN $tn /F | Out-Null
        if ($LASTEXITCODE -eq 0) {
            Write-Step "Removed scheduled task: $tn"
        } else {
            Write-Warning "could not remove scheduled task: $tn"
        }
    }
}

function Get-MoraDataPaths {
    param(
        [string]$Exe,
        [string]$VaultFallback
    )

    $paths = [ordered]@{
        vault  = $VaultFallback
        config = $(if ($env:MORA_CONFIG_DIR) { $env:MORA_CONFIG_DIR } else { Join-Path $HOME ".config\mora" })
        data   = Join-Path $HOME ".local\share\mora"
        state  = Join-Path $HOME ".local\state\mora"
    }

    if (-not (Test-Path -LiteralPath $Exe)) {
        return $paths
    }

    try {
        $out = & $Exe config 2>$null
        if ($LASTEXITCODE -ne 0 -or -not $out) {
            return $paths
        }
        foreach ($line in $out) {
            if ($line -match "^(vault_dir|data_dir|state_dir|config)\s*=\s*(.+)$") {
                $key = switch ($Matches[1]) {
                    "vault_dir" { "vault" }
                    "data_dir" { "data" }
                    "state_dir" { "state" }
                    "config" { "config" }
                }
                $value = (($Matches[2] -split "\s{2,}")[0]).Trim()
                if ($value) {
                    $paths[$key] = $value
                }
            }
        }
    } catch {
        Write-Warning "could not read mora config paths before uninstall: $($_.Exception.Message)"
    }

    return $paths
}

if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
    throw "error: uninstall.ps1 is for Windows. Use uninstall.sh on macOS/Linux."
}

$moraRoot = Join-Path $env:LOCALAPPDATA "Mora"
$binDir = Join-Path $moraRoot "bin"
$exe = Join-Path $binDir "mora.exe"
$dataPaths = Get-MoraDataPaths -Exe $exe -VaultFallback $Vault

Remove-MoraScheduledTasks

if (Test-Path -LiteralPath $exe) {
    Remove-Item -LiteralPath $exe -Force
    Write-Step "Removed binary: $exe"
}

if (Test-Path -LiteralPath $moraRoot) {
    Remove-Item -LiteralPath $moraRoot -Recurse -Force
    Write-Step "Removed install directory: $moraRoot"
}

if (Remove-UserPath -PathToRemove $binDir) {
    Write-Step "Removed PATH entry: $binDir"
}

if ($Purge) {
    $existing = @()
    foreach ($entry in $dataPaths.GetEnumerator()) {
        if ($entry.Value -and (Test-Path -LiteralPath $entry.Value)) {
            $existing += [pscustomobject]@{ Name = $entry.Key; Path = $entry.Value }
        }
    }

    if ($existing.Count -gt 0) {
        $delete = $Yes
        if (-not $delete) {
            Write-Host "Purge will delete:"
            foreach ($entry in $existing) {
                Write-Host "  $($entry.Name): $($entry.Path)"
            }
            $answer = Read-Host "Delete these Mora data paths? This cannot be undone. [y/N]"
            $delete = $answer -match "^(y|yes)$"
        }
        if ($delete) {
            foreach ($entry in $existing) {
                Remove-Item -LiteralPath $entry.Path -Recurse -Force
                Write-Step "Deleted $($entry.Name): $($entry.Path)"
            }
        } else {
            Write-Step "Kept Mora data paths."
        }
    } else {
        Write-Step "No Mora data paths found to purge."
    }
} else {
    $vaultPath = $dataPaths["vault"]
    if (Test-Path -LiteralPath $vaultPath) {
        Write-Step "Kept your vault: $vaultPath  (re-run with -Purge to delete it)"
    } else {
        Write-Step "Vault not found at $vaultPath; nothing to preserve."
    }
    Write-Step "Config and state directories are preserved."
}

Write-Host ""
Write-Host "Mora uninstalled."
