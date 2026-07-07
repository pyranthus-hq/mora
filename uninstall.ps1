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

function Send-EnvironmentChanged {
    # A raw registry write does not notify running processes; broadcast
    # WM_SETTINGCHANGE('Environment') so Explorer/new shells refresh. Best-effort.
    try {
        if (-not ('Mora.NativeEnv' -as [type])) {
            Add-Type -Namespace Mora -Name NativeEnv -MemberDefinition @'
[System.Runtime.InteropServices.DllImport("user32.dll", SetLastError=true, CharSet=System.Runtime.InteropServices.CharSet.Auto)]
public static extern System.IntPtr SendMessageTimeout(System.IntPtr hWnd, uint Msg, System.IntPtr wParam, string lParam, uint fuFlags, uint uTimeout, out System.UIntPtr lpdwResult);
'@
        }
        $result = [System.UIntPtr]::Zero
        [void][Mora.NativeEnv]::SendMessageTimeout([System.IntPtr]0xffff, 0x1A, [System.IntPtr]::Zero, 'Environment', 0x2, 5000, [ref]$result)
    } catch { }
}

function Remove-UserPath {
    param([string]$PathToRemove)

    # Raw HKCU:\Environment read/write, preserving REG_EXPAND_SZ — see the matching
    # note in install.ps1's Add-UserPath. GetEnvironmentVariable/SetEnvironmentVariable
    # would flatten every %VAR% entry to REG_SZ on the way out.
    $key = 'HKCU:\Environment'
    $item = Get-Item -LiteralPath $key
    $current = $item.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    if (-not $current) {
        return $false
    }
    $kind = [Microsoft.Win32.RegistryValueKind]::ExpandString
    try { if ($null -ne $item.GetValue('Path')) { $kind = $item.GetValueKind('Path') } } catch { }

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
        Set-ItemProperty -LiteralPath $key -Name 'Path' -Value ($kept -join ";") -Type $kind
        Send-EnvironmentChanged
        $env:Path = (($env:Path -split ";") | Where-Object {
            $_ -and -not (Get-NormalizedPathEntry $_).Equals($want, [StringComparison]::OrdinalIgnoreCase)
        }) -join ";"
    }
    return $removed
}

function Remove-MoraScheduledTasks {
    # Prefer the ScheduledTasks module: Get-ScheduledTask returns typed objects
    # with stable TaskPath/TaskName, so it is locale-independent and StrictMode
    # safe. Parsing `schtasks /FO CSV` instead throws under Set-StrictMode when
    # the CSV headers are localized (non-English Windows), which would abort the
    # whole uninstall before the binary and PATH are removed.
    if (Get-Command Get-ScheduledTask -ErrorAction SilentlyContinue) {
        $tasks = @()
        try {
            $tasks = @(Get-ScheduledTask -TaskPath "\Mora\*" -ErrorAction SilentlyContinue)
        } catch {
            Write-Warning "could not query Task Scheduler: $($_.Exception.Message)"
            $tasks = @()
        }
        if ($tasks.Count -eq 0) {
            Write-Step "No Mora scheduled tasks found."
            return
        }
        foreach ($t in $tasks) {
            $full = ($t.TaskPath.TrimEnd("\") + "\" + $t.TaskName).TrimStart("\")
            try {
                Unregister-ScheduledTask -TaskName $t.TaskName -TaskPath $t.TaskPath -Confirm:$false -ErrorAction Stop
                Write-Step "Removed scheduled task: $full"
            } catch {
                Write-Warning "could not remove scheduled task: $full"
            }
        }
        return
    }

    # Fallback (older hosts without the cmdlet): parse header-free CSV so there is
    # no localized-header row and no property access under StrictMode.
    $names = @()
    try {
        $raw = & schtasks.exe /Query /FO CSV /NH 2>$null
        if ($LASTEXITCODE -eq 0 -and $raw) {
            foreach ($line in $raw) {
                if (-not $line) { continue }
                $name = (($line -split '","')[0]).Trim('"').Trim()
                if ($name -like "\Mora\*" -or $name -like "Mora\*") {
                    $names += $name
                }
            }
        }
    } catch {
        Write-Warning "could not query Task Scheduler: $($_.Exception.Message)"
        return
    }

    if ($names.Count -eq 0) {
        Write-Step "No Mora scheduled tasks found."
        return
    }

    foreach ($name in ($names | Sort-Object -Unique)) {
        $tn = $name.TrimStart("\")
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

    # Consume the machine-readable `mora config --json`, not the human output. The
    # old scrape (`-split "\s{2,}"`) truncated any path containing a double space —
    # so `-Purge` could Remove-Item the WRONG directory — and mojibake'd non-ASCII
    # paths under PowerShell 5.1's OEM-codepage native decoding, silently missing
    # the vault. Force UTF-8 decoding of the binary's stdout, then parse JSON.
    $prevEnc = [Console]::OutputEncoding
    try {
        [Console]::OutputEncoding = [System.Text.Encoding]::UTF8
        $raw = & $Exe config --json 2>$null
        if ($LASTEXITCODE -eq 0 -and $raw) {
            $j = ($raw -join "`n") | ConvertFrom-Json
            if ($j.vault_dir)  { $paths.vault  = $j.vault_dir }
            if ($j.data_dir)   { $paths.data   = $j.data_dir }
            if ($j.state_dir)  { $paths.state  = $j.state_dir }
            if ($j.config_dir) { $paths.config = $j.config_dir }
        }
    } catch {
        Write-Warning "could not read mora config paths before uninstall: $($_.Exception.Message)"
    } finally {
        [Console]::OutputEncoding = $prevEnc
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
