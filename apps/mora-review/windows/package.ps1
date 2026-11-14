[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Source,
    [Parameter(Mandatory = $true)][string]$OutputDirectory,
    [string]$ScratchDirectory = ""
)

# Candidate-only packaging: source stays unpinned; this script pins only a private
# archive snapshot after the final helper bytes have been produced.
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Resolve-CanonicalPath([string]$Path, [string]$Name) {
    if ([string]::IsNullOrWhiteSpace($Path)) { throw "$Name is required." }
    $resolved = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Path)
    $resolved = [System.IO.Path]::GetFullPath($resolved)
    # Refuse aliases rather than guessing whether a source-contained path is safe.
    if ($resolved -notmatch '^[A-Za-z]:\\' -or $resolved.Contains('~')) { throw "$Name requires an ordinary local Windows path." }
    $ancestor = $resolved
    while (![string]::IsNullOrWhiteSpace($ancestor)) {
        if (Test-Path -LiteralPath $ancestor) {
            $item = Get-Item -LiteralPath $ancestor -Force
            if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) { throw "$Name cannot contain a reparse point." }
        }
        $ancestor = Split-Path -Parent $ancestor
    }
    return $resolved
}

function Resolve-Directory([string]$Path, [string]$Name) {
    $resolved = Resolve-CanonicalPath $Path $Name
    if (!(Test-Path -LiteralPath $resolved -PathType Container)) { throw "$Name is not a directory: $resolved" }
    return $resolved
}

function Assert-OutsideSource([string]$Path, [string]$Source, [string]$Name) {
    $root = $Source.TrimEnd([char[]]@('\'))
    $candidate = $Path.TrimEnd([char[]]@('\'))
    if ($candidate.Equals($root, [System.StringComparison]::OrdinalIgnoreCase) -or $candidate.StartsWith($root + '\', [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "$Name must be outside the source checkout: $Path"
    }
}

function Assert-SameVolume([string]$First, [string]$Second) {
    $firstRoot = [System.IO.Path]::GetPathRoot($First)
    $secondRoot = [System.IO.Path]::GetPathRoot($Second)
    if (!$firstRoot.Equals($secondRoot, [System.StringComparison]::OrdinalIgnoreCase)) { throw 'ScratchDirectory and OutputDirectory must be on the same volume for atomic publication.' }
}

function Invoke-Checked([string]$Command, [string[]]$Arguments) {
    & $Command @Arguments
    if ($LASTEXITCODE -ne 0) { throw "$Command failed with exit code $LASTEXITCODE." }
}

function Assert-CleanSource([string]$Repository) {
    $status = @(& git -C $Repository status --porcelain)
    if ($LASTEXITCODE -ne 0) { throw "Could not read Git status for $Repository." }
    if ($status.Count -ne 0) { throw "Source snapshot must be clean; refusing to package uncommitted files." }
}

function Prepare-Output([string]$Requested) {
    $parent = Split-Path -Parent $Requested
    if ([string]::IsNullOrWhiteSpace($parent)) { throw "OutputDirectory must have an existing parent: $Requested" }
    $parent = Resolve-Directory $parent 'OutputDirectory parent'
    $leaf = Split-Path -Leaf $Requested
    if ([string]::IsNullOrWhiteSpace($leaf)) { throw "OutputDirectory must name a directory: $Requested" }
    $output = Join-Path $parent $leaf
    if (Test-Path -LiteralPath $output) {
        if (!(Test-Path -LiteralPath $output -PathType Container)) { throw "OutputDirectory is not a directory: $output" }
        if (@(Get-ChildItem -LiteralPath $output -Force).Count -ne 0) { throw "Refusing nonempty output directory: $output" }
    } else {
        New-Item -ItemType Directory -Path $output | Out-Null
    }
    return (Resolve-Path -LiteralPath $output).Path
}

function Write-PackagePinTest([string]$Path) {
    $test = @'
//go:build windows

package mora

import (
	"bytes"
	"os"
	"testing"
)

func TestMoraPackagePinnedHelper(t *testing.T) {
	path := os.Getenv("MORA_PACKAGE_HELPER") // test-only path; production accepts no runtime pin override.
	if path == "" {
		t.Fatal("MORA_PACKAGE_HELPER is required by the isolated package test")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = nativeOwnerHelperDigestMatches(f, nativeOwnerWindowsHelperSHA256)
	_ = f.Close()
	if err != nil {
		t.Fatalf("original pinned helper rejected: %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	contents[0] ^= 1
	if err := nativeOwnerHelperDigestMatches(bytes.NewReader(contents), nativeOwnerWindowsHelperSHA256); err == nil {
		t.Fatal("altered helper unexpectedly validated")
	}
}
'@
    [System.IO.File]::WriteAllText($Path, $test, [System.Text.UTF8Encoding]::new($false))
}

function Assert-Package([string]$PackageDirectory, [string]$Revision) {
    $manifestPath = Join-Path $PackageDirectory 'manifest.json'
    $required = @('MoraReview.exe', 'mora.exe', 'install.ps1', 'manifest.json')
    foreach ($name in $required) {
        if (!(Test-Path -LiteralPath (Join-Path $PackageDirectory $name) -PathType Leaf)) { throw "Package is missing $name." }
    }
    $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
    if ($manifest.schema_version -ne 1 -or $manifest.source_revision -ne $Revision) { throw 'Package manifest has an unexpected schema or source revision.' }
    $artifacts = @($manifest.artifacts)
    if ($artifacts.Count -ne 2) { throw 'Package manifest must list exactly the adjacent executable pair.' }
    foreach ($artifact in $artifacts) {
        $path = Join-Path $PackageDirectory $artifact.name
        if ($artifact.name -notin @('MoraReview.exe', 'mora.exe') -or !(Test-Path -LiteralPath $path -PathType Leaf)) { throw 'Package manifest names an unexpected artifact.' }
        $actual = (Get-FileHash -LiteralPath $path -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($artifact.sha256 -notmatch '^[0-9a-f]{64}$' -or $actual -ne $artifact.sha256) { throw "Package hash mismatch for $($artifact.name)." }
    }
}

$sourceRoot = Resolve-Directory $Source 'Source'
if (!(Test-Path -LiteralPath (Join-Path $sourceRoot 'go.mod') -PathType Leaf)) { throw "Source is not a Mora checkout: $sourceRoot" }
Assert-CleanSource $sourceRoot
$revision = (& git -C $sourceRoot rev-parse --verify HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or $revision -notmatch '^[0-9a-f]{40}$') { throw "Could not resolve an exact source revision for $sourceRoot." }
$outputRequested = Resolve-CanonicalPath $OutputDirectory 'OutputDirectory'
if ([string]::IsNullOrWhiteSpace($ScratchDirectory)) { $ScratchDirectory = Split-Path -Parent $outputRequested }
$scratchRequested = Resolve-CanonicalPath $ScratchDirectory 'ScratchDirectory'
Assert-OutsideSource $outputRequested $sourceRoot 'OutputDirectory'
Assert-OutsideSource $scratchRequested $sourceRoot 'ScratchDirectory'
Assert-SameVolume $outputRequested $scratchRequested
$outputRoot = Prepare-Output $outputRequested
$scratchRoot = Resolve-Directory $scratchRequested 'ScratchDirectory'
$work = Join-Path $scratchRoot ("mora-review-package-" + [guid]::NewGuid().ToString('N'))
$stagedArchive = $null

try {
    New-Item -ItemType Directory -Path $work | Out-Null
    $sourceArchive = Join-Path $work 'source.zip'
    Invoke-Checked git @('-C', $sourceRoot, 'archive', '--format=zip', ("--output=" + $sourceArchive), $revision)
    $snapshot = Join-Path $work 'source'
    Expand-Archive -LiteralPath $sourceArchive -DestinationPath $snapshot
    $windows = Join-Path $snapshot 'apps\mora-review\windows'
    $helper = Join-Path $windows 'MoraReview.exe'
    $generatedGo = Join-Path $snapshot 'internal\mora\native_owner_helper_windows_generated.go'
    $package = Join-Path $work 'package'
    New-Item -ItemType Directory -Path $package | Out-Null

    Push-Location $windows
    try {
        & (Join-Path $windows 'build.ps1')
        if ($LASTEXITCODE -ne 0) { throw "build.ps1 failed with exit code $LASTEXITCODE." }
        # Future signing belongs here. Pinning must always follow the final PE bytes.
        & (Join-Path $windows 'pin-helper.ps1') -Helper $helper -GeneratedGo $generatedGo
        if ($LASTEXITCODE -ne 0) { throw "pin-helper.ps1 failed with exit code $LASTEXITCODE." }
    } finally {
        Pop-Location
    }

    $helperHash = (Get-FileHash -LiteralPath $helper -Algorithm SHA256).Hash.ToLowerInvariant()
    $generated = Get-Content -LiteralPath $generatedGo -Raw
    $pin = [regex]::Match($generated, 'nativeOwnerWindowsHelperSHA256 = "([0-9a-f]{64})"')
    if (!$pin.Success -or $pin.Groups[1].Value -ne $helperHash) { throw 'Pinned Go source does not match the final helper bytes.' }
    Write-PackagePinTest (Join-Path $snapshot 'internal\mora\package_pin_windows_test.go')

    $environment = @{}
    foreach ($name in @('CGO_ENABLED', 'GOOS', 'GOARCH', 'GOFLAGS', 'GOWORK', 'MORA_PACKAGE_HELPER')) {
        if (Test-Path -LiteralPath ("Env:" + $name)) {
            $environment[$name] = @{ Exists = $true; Value = (Get-Item -LiteralPath ("Env:" + $name)).Value }
        } else {
            $environment[$name] = @{ Exists = $false; Value = $null }
        }
    }
    try {
        $env:CGO_ENABLED = '0'
        $env:GOOS = 'windows'
        $env:GOARCH = 'amd64'
        Remove-Item -LiteralPath 'Env:GOFLAGS' -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath 'Env:GOWORK' -ErrorAction SilentlyContinue
        $env:MORA_PACKAGE_HELPER = $helper
        Push-Location $snapshot
        try {
            & go test -count=1 -run '^TestMoraPackagePinnedHelper$' ./internal/mora
            if ($LASTEXITCODE -ne 0) { throw "Pinned helper verification failed with exit code $LASTEXITCODE." }
            & go build -trimpath -o (Join-Path $package 'mora.exe') ./cmd/mora
            if ($LASTEXITCODE -ne 0) { throw "Windows mora.exe build failed with exit code $LASTEXITCODE." }
        } finally {
            Pop-Location
        }
    } finally {
        foreach ($name in $environment.Keys) {
            if ($environment[$name].Exists) {
                Set-Item -LiteralPath ("Env:" + $name) -Value $environment[$name].Value
            } else {
                Remove-Item -LiteralPath ("Env:" + $name) -ErrorAction SilentlyContinue
            }
        }
    }

    Copy-Item -LiteralPath $helper -Destination (Join-Path $package 'MoraReview.exe')
    Copy-Item -LiteralPath (Join-Path $windows 'install.ps1') -Destination (Join-Path $package 'install.ps1')
    $manifest = [ordered]@{
        schema_version = 1
        source_revision = $revision
        artifacts = @(
            [ordered]@{ name = 'MoraReview.exe'; sha256 = $helperHash },
            [ordered]@{ name = 'mora.exe'; sha256 = (Get-FileHash -LiteralPath (Join-Path $package 'mora.exe') -Algorithm SHA256).Hash.ToLowerInvariant() }
        )
    } | ConvertTo-Json -Depth 3
    [System.IO.File]::WriteAllText((Join-Path $package 'manifest.json'), $manifest + "`n", [System.Text.UTF8Encoding]::new($false))
    Assert-Package $package $revision

    $archive = Join-Path $work 'MoraReview-windows-amd64.zip'
    Push-Location $package
    try {
        Compress-Archive -Path @('MoraReview.exe', 'mora.exe', 'install.ps1', 'manifest.json') -DestinationPath $archive
    } finally {
        Pop-Location
    }
    $extracted = Join-Path $work 'extracted'
    Expand-Archive -LiteralPath $archive -DestinationPath $extracted
    Assert-Package $extracted $revision

    $published = Join-Path $outputRoot ("MoraReview-" + $revision.Substring(0, 12) + '-windows-amd64.zip')
    $stagedArchive = Join-Path $outputRoot (".candidate-" + [guid]::NewGuid().ToString('N') + ".zip")
    Move-Item -LiteralPath $archive -Destination $stagedArchive
    Remove-Item -LiteralPath $work -Recurse -Force
    Assert-CleanSource $sourceRoot
    Move-Item -LiteralPath $stagedArchive -Destination $published
    $stagedArchive = $null
    Write-Output "Created candidate package: $published"
} finally {
    if ($null -ne $stagedArchive -and (Test-Path -LiteralPath $stagedArchive)) {
        try { Remove-Item -LiteralPath $stagedArchive -Force } catch { Write-Warning "Could not remove staged candidate archive: $($_.Exception.Message)" }
    }
    $cleanupFailure = $null
    if (Test-Path -LiteralPath $work) {
        try {
            Remove-Item -LiteralPath $work -Recurse -Force
        } catch {
            $cleanupFailure = $_
        }
    }
    if ($null -ne $cleanupFailure) { Write-Warning "Could not remove private package work: $($cleanupFailure.Exception.Message)" }
}
