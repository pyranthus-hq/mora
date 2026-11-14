[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$Source,
    [Parameter(Mandatory = $true)][string]$OutputDirectory
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Assert-True([bool]$Condition, [string]$Message) {
    if (!$Condition) { throw $Message }
}

function Assert-Clean([string]$Repository) {
    $status = @(& git -C $Repository status --porcelain)
    if ($LASTEXITCODE -ne 0 -or $status.Count -ne 0) { throw 'Source checkout changed during candidate packaging.' }
}

function Save-Environment([string[]]$Names) {
    $saved = @{}
    foreach ($name in $Names) {
        if (Test-Path -LiteralPath ("Env:" + $name)) {
            $saved[$name] = @{ Exists = $true; Value = (Get-Item -LiteralPath ("Env:" + $name)).Value }
        } else {
            $saved[$name] = @{ Exists = $false; Value = $null }
        }
    }
    return $saved
}

function Restore-Environment($Saved) {
    foreach ($name in $Saved.Keys) {
        if ($Saved[$name].Exists) {
            Set-Item -LiteralPath ("Env:" + $name) -Value $Saved[$name].Value
        } else {
            Remove-Item -LiteralPath ("Env:" + $name) -ErrorAction SilentlyContinue
        }
    }
}

function Assert-Environment($Expected) {
    foreach ($name in $Expected.Keys) {
        $exists = Test-Path -LiteralPath ("Env:" + $name)
        Assert-True ($exists -eq $Expected[$name].Exists) "Environment presence changed for $name"
        if ($exists) { Assert-True ((Get-Item -LiteralPath ("Env:" + $name)).Value -eq $Expected[$name].Value) "Environment value changed for $name" }
    }
}

function Assert-Amd64PE([string]$Path) {
    $bytes = [System.IO.File]::ReadAllBytes($Path)
    Assert-True ($bytes.Length -ge 64 -and $bytes[0] -eq 0x4d -and $bytes[1] -eq 0x5a) "$Path is not a PE file"
    $offset = [System.BitConverter]::ToInt32($bytes, 0x3c)
    Assert-True ($offset -ge 0 -and ($offset + 6) -le $bytes.Length) "$Path has an invalid PE header"
    Assert-True ($bytes[$offset] -eq 0x50 -and $bytes[$offset+1] -eq 0x45 -and $bytes[$offset+2] -eq 0 -and $bytes[$offset+3] -eq 0) "$Path has no PE signature"
    Assert-True ([System.BitConverter]::ToUInt16($bytes, $offset + 4) -eq 0x8664) "$Path is not AMD64"
}

$sourceRoot = (Resolve-Path -LiteralPath $Source).Path
$outputParent = Split-Path -Parent $OutputDirectory
Assert-True (Test-Path -LiteralPath $outputParent -PathType Container) "Output parent does not exist: $outputParent"
Assert-True (!(Test-Path -LiteralPath $OutputDirectory)) "OutputDirectory must not exist for this fixture: $OutputDirectory"
Assert-Clean $sourceRoot
$developerPin = Get-Content -LiteralPath (Join-Path $sourceRoot 'internal\mora\native_owner_helper_windows_generated.go') -Raw
Assert-True ($developerPin -match 'nativeOwnerWindowsHelperSHA256 = ""') 'Developer source must retain an empty helper pin.'

$hostile = Save-Environment @('CGO_ENABLED', 'GOOS', 'GOARCH', 'GOFLAGS', 'GOWORK')
try {
    $env:CGO_ENABLED = '1'
    $env:GOOS = 'linux'
    $env:GOARCH = '386'
    $env:GOFLAGS = '-not-a-real-go-flag'
    $env:GOWORK = Join-Path $outputParent 'nonexistent-hostile.go.work'
    $expected = Save-Environment @('CGO_ENABLED', 'GOOS', 'GOARCH', 'GOFLAGS', 'GOWORK')
    Push-Location $outputParent
    try {
        & (Join-Path $sourceRoot 'apps\mora-review\windows\package.ps1') -Source $sourceRoot -OutputDirectory (Split-Path -Leaf $OutputDirectory) -ScratchDirectory '.'
    } finally { Pop-Location }
    if ($LASTEXITCODE -ne 0) { throw "package.ps1 failed with exit code $LASTEXITCODE." }
    Assert-Environment $expected
} finally {
    Restore-Environment $hostile
}
$archive = @(Get-ChildItem -LiteralPath $OutputDirectory -File -Filter 'MoraReview-*-windows-amd64.zip')
Assert-True ($archive.Count -eq 1) 'Candidate output must contain exactly one archive.'
$extracted = Join-Path $OutputDirectory 'extracted proof'
Expand-Archive -LiteralPath $archive[0].FullName -DestinationPath $extracted
foreach ($name in @('MoraReview.exe', 'mora.exe', 'install.ps1', 'manifest.json')) {
    Assert-True (Test-Path -LiteralPath (Join-Path $extracted $name) -PathType Leaf) "Archive is missing $name"
}
$manifest = Get-Content -LiteralPath (Join-Path $extracted 'manifest.json') -Raw | ConvertFrom-Json
Assert-True ($manifest.schema_version -eq 1 -and @($manifest.artifacts).Count -eq 2) 'Archive manifest is not bounded to the executable pair.'
foreach ($artifact in @($manifest.artifacts)) {
    $actual = (Get-FileHash -LiteralPath (Join-Path $extracted $artifact.name) -Algorithm SHA256).Hash.ToLowerInvariant()
    Assert-True ($actual -eq $artifact.sha256) "Archive hash mismatch for $($artifact.name)"
}
Assert-Amd64PE (Join-Path $extracted 'MoraReview.exe')
Assert-Amd64PE (Join-Path $extracted 'mora.exe')
Assert-Clean $sourceRoot

$foreign = Join-Path $outputParent 'foreign candidate output'
New-Item -ItemType Directory -Path $foreign | Out-Null
$sentinel = Join-Path $foreign 'keep.txt'
[System.IO.File]::WriteAllText($sentinel, 'preserve me', [System.Text.UTF8Encoding]::new($false))
$rejected = $false
try {
    & (Join-Path $sourceRoot 'apps\mora-review\windows\package.ps1') -Source $sourceRoot -OutputDirectory $foreign
    if ($LASTEXITCODE -ne 0) { $rejected = $true }
} catch {
    $rejected = $true
}
Assert-True $rejected 'package.ps1 accepted a foreign nonempty output directory.'
Assert-True ((Get-Content -LiteralPath $sentinel -Raw) -eq 'preserve me') 'package.ps1 changed foreign output.'

$insideOutput = Join-Path $sourceRoot 'dist\candidate package'
$scratchOutput = Join-Path $outputParent 'scratch candidate output'
Assert-True (!(Test-Path -LiteralPath $insideOutput)) 'Inside-source output already exists; fixture requires an unused path.'
Assert-True (!(Test-Path -LiteralPath $scratchOutput)) 'Scratch rejection output already exists; fixture requires an unused path.'
foreach ($attempt in @(
    @{ Output = $insideOutput; Scratch = $outputParent },
    @{ Output = $scratchOutput; Scratch = $sourceRoot }
)) {
    $rejected = $false
    try {
        & (Join-Path $sourceRoot 'apps\mora-review\windows\package.ps1') -Source $sourceRoot -OutputDirectory $attempt.Output -ScratchDirectory $attempt.Scratch
        if ($LASTEXITCODE -ne 0) { $rejected = $true }
    } catch {
        Assert-True ($_.Exception.Message -like '*must be outside the source checkout*') "Wrong source rejection: $($_.Exception.Message)"
        $rejected = $true
    }
    Assert-True $rejected 'package.ps1 accepted an output or scratch path inside source.'
    Assert-True (!(Test-Path -LiteralPath $attempt.Output)) 'Rejected source path created an output directory.'
}
Assert-True (@(Get-ChildItem -LiteralPath $sourceRoot -Directory -Force -Recurse -Filter 'mora-review-package-*').Count -eq 0) 'Rejected source path left a private package work directory.'
# Exercise failure after private snapshot work starts, without a production test hook.
$failureOutput = Join-Path $outputParent 'injected failure output'
$failureScratch = Join-Path $outputParent 'injected failure scratch'
New-Item -ItemType Directory -Path $failureScratch | Out-Null
$gitExeForPackageFixture = (Get-Command git -CommandType Application | Select-Object -First 1).Source
function git {
    if ($args -contains 'archive') { throw 'injected package archive failure' }
    & $gitExeForPackageFixture @args
}
try {
    $message = ''
    try {
        & (Join-Path $sourceRoot 'apps\mora-review\windows\package.ps1') -Source $sourceRoot -OutputDirectory $failureOutput -ScratchDirectory $failureScratch
    } catch { $message = $_.Exception.Message }
    Assert-True ($message -like '*injected package archive failure*') "Original failure was lost: $message"
    Assert-True (@(Get-ChildItem -LiteralPath $failureScratch -Force).Count -eq 0) 'Failed package left private work.'
    Assert-True (@(Get-ChildItem -LiteralPath $failureOutput -Force).Count -eq 0) 'Failed package published an archive.'
} finally { Remove-Item Function:git }

$junction = Join-Path $outputParent 'source alias'
New-Item -ItemType Junction -Path $junction -Target $sourceRoot | Out-Null
try {
    $message = ''
    try {
        & (Join-Path $sourceRoot 'apps\mora-review\windows\package.ps1') -Source $sourceRoot -OutputDirectory (Join-Path $junction 'dist\alias output')
    } catch { $message = $_.Exception.Message }
    Assert-True ($message -like '*cannot contain a reparse point*') "Source alias was not refused: $message"
    Assert-True (!(Test-Path -LiteralPath (Join-Path $sourceRoot 'dist\alias output'))) 'Source alias created output.'
} finally { [System.IO.Directory]::Delete($junction) }
Assert-Clean $sourceRoot
Write-Output 'PASS: Windows candidate package, pin, archive, hostile environment, and source-isolation fixture'
