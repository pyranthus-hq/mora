$ErrorActionPreference = 'Stop'
$compiler = Join-Path $env:WINDIR 'Microsoft.NET\Framework64\v4.0.30319\csc.exe'
if (!(Test-Path -LiteralPath $compiler)) { throw 'The inbox x64 .NET Framework C# compiler is required.' }
Push-Location $PSScriptRoot
try {
    # Build first, optionally Authenticode-sign the resulting PE, then run
    # pin-helper.ps1 and build mora.exe. Hashing before signing is invalid.
    & $compiler /nologo /warnaserror+ /platform:x64 /target:winexe /out:MoraReview.exe /reference:System.Windows.Forms.dll /reference:System.Drawing.dll /reference:System.Web.Extensions.dll MoraReview.cs DesktopChannel.cs WindowsHello.cs
    if ($LASTEXITCODE -ne 0) { throw "Compile failed with exit code $LASTEXITCODE." }
    $stdout = Join-Path $PSScriptRoot 'MoraReview-self-test.stdout.log'
    $stderr = Join-Path $PSScriptRoot 'MoraReview-self-test.stderr.log'
    $test = Start-Process -FilePath (Join-Path $PSScriptRoot 'MoraReview.exe') -ArgumentList '--self-test' -Wait -PassThru -NoNewWindow -RedirectStandardOutput $stdout -RedirectStandardError $stderr
    if ($test.ExitCode -ne 0) { throw "Self-test failed with exit code $($test.ExitCode). See $stdout and $stderr." }
    if (!(Select-String -LiteralPath $stdout -SimpleMatch 'PASS:' -Quiet)) { throw "Self-test emitted no pass receipt. See $stdout." }
} finally { Pop-Location }
