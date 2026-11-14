param([Parameter(Mandatory = $true)][string]$Source)
$ErrorActionPreference = 'Stop'
$required = @('MoraReview.exe', 'mora.exe')
foreach ($name in $required) { if (!(Test-Path -LiteralPath (Join-Path $Source $name))) { throw "Missing $name in $Source." } }
$target = Join-Path $env:LOCALAPPDATA 'Mora Review'
New-Item -ItemType Directory -Force -Path $target | Out-Null
foreach ($name in $required) { Copy-Item -LiteralPath (Join-Path $Source $name) -Destination (Join-Path $target $name) -Force }
$start = Join-Path $env:APPDATA 'Microsoft\Windows\Start Menu\Programs\Mora Review.lnk'
$shell = New-Object -ComObject WScript.Shell
$shortcut = $shell.CreateShortcut($start)
$shortcut.TargetPath = Join-Path $target 'mora.exe'
$shortcut.Arguments = 'review desktop --stdio'
$shortcut.WorkingDirectory = $target
$shortcut.Save()
Write-Output "Installed Mora Review at $target. Developer builds are read-only; release builds must pin the final helper SHA-256 before packaging."
