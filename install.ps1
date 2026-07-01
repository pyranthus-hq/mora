<#
.SYNOPSIS
Install Mora on Windows.

.DESCRIPTION
Downloads a Mora Windows release zip from GitHub, verifies it against the
release checksums.txt, installs mora.exe to %LOCALAPPDATA%\Mora\bin, and adds
that directory to the current user's PATH. The v1 Windows binary is unsigned,
so Windows SmartScreen may show "Windows protected your PC" on first run.

.PARAMETER Version
Release version to install, such as 0.9.1 or v0.9.1. Defaults to latest.

.PARAMETER Repo
GitHub repository in owner/name form. Defaults to pyranthus-hq/mora.

.PARAMETER Vault
Vault path passed to mora init. Defaults to %USERPROFILE%\vault\mora.

.EXAMPLE
powershell -ExecutionPolicy Bypass -File .\install.ps1

.EXAMPLE
powershell -ExecutionPolicy Bypass -File .\install.ps1 -Version 0.9.1
#>
[CmdletBinding()]
param(
    [string]$Version = $(if ($env:VERSION) { $env:VERSION } else { "latest" }),
    [string]$Repo = $(if ($env:REPO) { $env:REPO } else { "pyranthus-hq/mora" }),
    [string]$Vault = $(if ($env:MORA_VAULT) { $env:MORA_VAULT } else { Join-Path $HOME "vault\mora" })
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
Set-StrictMode -Version 2.0

try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
} catch {
    # PowerShell Core on newer .NETs manages TLS policy itself.
}

function Write-Step {
    param([string]$Message)
    Write-Host $Message
}

function Fail {
    param([string]$Message)
    throw "error: $Message"
}

function Get-NormalizedPathEntry {
    param([string]$Path)
    return $Path.Trim().TrimEnd("\")
}

function Add-UserPath {
    param([string]$PathToAdd)

    $current = [Environment]::GetEnvironmentVariable("Path", "User")
    $parts = @()
    if ($current) {
        $parts = $current -split ";" | Where-Object { $_ -and $_.Trim() }
    }

    $want = Get-NormalizedPathEntry $PathToAdd
    foreach ($part in $parts) {
        if ((Get-NormalizedPathEntry $part).Equals($want, [StringComparison]::OrdinalIgnoreCase)) {
            return $false
        }
    }

    $next = @($parts + $PathToAdd) -join ";"
    [Environment]::SetEnvironmentVariable("Path", $next, "User")
    if ($env:Path) {
        $env:Path = @($env:Path, $PathToAdd) -join ";"
    } else {
        $env:Path = $PathToAdd
    }
    return $true
}

function Get-ReleaseVersion {
    param(
        [string]$RepoName,
        [string]$RequestedVersion
    )

    if ($RequestedVersion -and $RequestedVersion -ne "latest") {
        return $RequestedVersion.TrimStart("v")
    }

    $uri = "https://api.github.com/repos/$RepoName/releases/latest"
    Write-Step "Resolving latest release from $uri ..."
    $headers = @{ "User-Agent" = "mora-install.ps1" }
    $release = Invoke-RestMethod -Uri $uri -Headers $headers
    if (-not $release.tag_name) {
        Fail "latest release response did not include tag_name"
    }
    return ([string]$release.tag_name).TrimStart("v")
}

function Download-File {
    param(
        [string]$Uri,
        [string]$OutFile
    )

    $headers = @{ "User-Agent" = "mora-install.ps1" }
    Invoke-WebRequest -Uri $Uri -OutFile $OutFile -Headers $headers
}

function Find-Checksum {
    param(
        [string]$ChecksumsPath,
        [string]$AssetName
    )

    foreach ($line in Get-Content -LiteralPath $ChecksumsPath) {
        $clean = $line.Trim()
        if (-not $clean) {
            continue
        }
        $parts = $clean -split "\s+"
        if ($parts.Count -lt 2) {
            continue
        }
        $name = $parts[1].TrimStart("*")
        if ($name -eq $AssetName) {
            return $parts[0].ToLowerInvariant()
        }
    }
    return $null
}

function Invoke-MoraNoInput {
    param(
        [string]$Exe,
        [string[]]$Arguments
    )

    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $Exe
    $psi.UseShellExecute = $false
    $psi.RedirectStandardInput = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true

    try {
        foreach ($arg in $Arguments) {
            $psi.ArgumentList.Add($arg)
        }
    } catch {
        $quoted = foreach ($arg in $Arguments) {
            if ($arg -match '[\s"]') {
                '"' + $arg.Replace('"', '\"') + '"'
            } else {
                $arg
            }
        }
        $psi.Arguments = ($quoted -join " ")
    }

    $p = New-Object System.Diagnostics.Process
    $p.StartInfo = $psi
    [void]$p.Start()
    $p.StandardInput.Close()
    $stdout = $p.StandardOutput.ReadToEnd()
    $stderr = $p.StandardError.ReadToEnd()
    $p.WaitForExit()

    return [pscustomobject]@{
        ExitCode = $p.ExitCode
        Stdout   = $stdout
        Stderr   = $stderr
    }
}

if ([Environment]::OSVersion.Platform -ne [PlatformID]::Win32NT) {
    Fail "install.ps1 is for Windows. Use install.sh on macOS/Linux."
}

$nativeArch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
switch ($nativeArch) {
    "AMD64" { $releaseArch = "amd64" }
    default { Fail "unsupported Windows architecture: $nativeArch. v1 ships windows/amd64 only." }
}

$resolvedVersion = Get-ReleaseVersion -RepoName $Repo -RequestedVersion $Version
$tag = "v$resolvedVersion"
$asset = "mora_${resolvedVersion}_windows_${releaseArch}.zip"
$baseUrl = "https://github.com/$Repo/releases/download/$tag"
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("mora-install-" + [Guid]::NewGuid().ToString("N"))
$installDir = Join-Path $env:LOCALAPPDATA "Mora\bin"
$destExe = Join-Path $installDir "mora.exe"

New-Item -ItemType Directory -Path $tmp | Out-Null

try {
    $zipPath = Join-Path $tmp $asset
    $checksumsPath = Join-Path $tmp "checksums.txt"

    Write-Step "Fetching $asset from $Repo@$tag ..."
    Download-File -Uri "$baseUrl/$asset" -OutFile $zipPath
    Download-File -Uri "$baseUrl/checksums.txt" -OutFile $checksumsPath

    $want = Find-Checksum -ChecksumsPath $checksumsPath -AssetName $asset
    if (-not $want) {
        Fail "checksums.txt has no entry for $asset. Refusing to install an unverifiable download."
    }

    $got = (Get-FileHash -Algorithm SHA256 -LiteralPath $zipPath).Hash.ToLowerInvariant()
    if ($got -ne $want) {
        Fail "CHECKSUM MISMATCH for $asset (expected $want, got $got). Refusing to install a tampered or corrupt download."
    }
    Write-Step "Verified $asset against release checksums.txt"

    $extractDir = Join-Path $tmp "extract"
    New-Item -ItemType Directory -Path $extractDir | Out-Null
    Expand-Archive -LiteralPath $zipPath -DestinationPath $extractDir -Force

    $moraExe = Get-ChildItem -LiteralPath $extractDir -Recurse -Filter "mora.exe" -File | Select-Object -First 1
    if (-not $moraExe) {
        Fail "extracted archive has no mora.exe"
    }

    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    Copy-Item -LiteralPath $moraExe.FullName -Destination $destExe -Force
    try {
        Unblock-File -LiteralPath $destExe -ErrorAction SilentlyContinue
    } catch {
        # Unblock-File is best-effort. The checksum verification above is the hard gate.
    }

    $pathAdded = Add-UserPath -PathToAdd $installDir

    $init = Invoke-MoraNoInput -Exe $destExe -Arguments @("init", "--vault", $Vault)
    if ($init.ExitCode -ne 0) {
        $msg = ($init.Stdout + $init.Stderr).Trim()
        if ($msg) {
            Write-Warning "mora init did not run: $msg"
        } else {
            Write-Warning "mora init did not run."
        }
    }

    $ver = (& $destExe version 2>$null | Select-Object -First 1)
    if (-not $ver) {
        $ver = "mora"
    }

    Write-Host ""
    Write-Host "Installed $ver"
    Write-Host "  binary: $destExe"
    Write-Host "  vault:  $Vault"
    if ($pathAdded) {
        Write-Host "  PATH:   added $installDir to the User PATH"
        Write-Host ""
        Write-Host "Open a new PowerShell window so 'mora' resolves on PATH."
    } else {
        Write-Host "  PATH:   $installDir is already on the User PATH"
    }

    Write-Host ""
    Write-Host "Windows note: Mora for Windows is currently unsigned. If SmartScreen shows"
    Write-Host "'Windows protected your PC', choose More info > Run anyway after verifying"
    Write-Host "the checksum above, or run:"
    Write-Host "  Unblock-File `"$destExe`""
    Write-Host ""
    Write-Host "Wire Mora into your agents once:"
    Write-Host "  claude mcp add mora -s user -- mora mcp serve"
    Write-Host "  codex  mcp add mora -- mora mcp serve"
    Write-Host ""
    Write-Host "Next steps:"
    Write-Host "  mora doctor"
    Write-Host "  mora connect google"
    Write-Host "  mora connect filesystem `$HOME\Documents"
} finally {
    Remove-Item -LiteralPath $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
