$ErrorActionPreference = 'Stop'
$compiler = Join-Path $env:WINDIR 'Microsoft.NET\Framework64\v4.0.30319\csc.exe'
if (!(Test-Path -LiteralPath $compiler)) { throw 'The inbox x64 .NET Framework C# compiler is required.' }
Push-Location $PSScriptRoot
try {
    & $compiler /nologo /warnaserror+ /platform:x64 /target:winexe /out:MoraReview.exe /reference:System.Windows.Forms.dll /reference:System.Drawing.dll /reference:System.Web.Extensions.dll MoraReview.cs
    if ($LASTEXITCODE -ne 0) { throw "Compile failed with exit code $LASTEXITCODE." }
} finally { Pop-Location }
