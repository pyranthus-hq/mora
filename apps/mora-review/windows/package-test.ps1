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

$sourceRoot = (Resolve-Path -LiteralPath $Source).Path
$outputParent = Split-Path -Parent $OutputDirectory
Assert-True (Test-Path -LiteralPath $outputParent -PathType Container) "Output parent does not exist: $outputParent"
Assert-True (!(Test-Path -LiteralPath $OutputDirectory)) "OutputDirectory must not exist for this fixture: $OutputDirectory"
Assert-Clean $sourceRoot
$developerPin = Get-Content -LiteralPath (Join-Path $sourceRoot 'internal\mora\native_owner_helper_windows_generated.go') -Raw
Assert-True ($developerPin -match 'nativeOwnerWindowsHelperSHA256 = ""') 'Developer source must retain an empty helper pin.'

& (Join-Path $sourceRoot 'apps\mora-review\windows\package.ps1') -Source $sourceRoot -OutputDirectory $OutputDirectory
if ($LASTEXITCODE -ne 0) { throw "package.ps1 failed with exit code $LASTEXITCODE." }
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
Assert-Clean $sourceRoot
Write-Output 'PASS: Windows candidate package, pin, archive, and source-isolation fixture'
