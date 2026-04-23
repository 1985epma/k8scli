param(
    [string]$Version = "0.1.0",
    [string]$SourceExe = "",
    [string]$OutputDir = ""
)

$ErrorActionPreference = "Stop"

if (-not $IsWindows) {
    throw "MSI generation must run on Windows because WiX only supports Windows builds."
}

$repoRoot = Split-Path -Parent $PSScriptRoot

if ([string]::IsNullOrWhiteSpace($SourceExe)) {
    $SourceExe = Join-Path $repoRoot "dist-release-$Version/k8scli_windows_amd64_v1/k8scli.exe"
}

if ([string]::IsNullOrWhiteSpace($OutputDir)) {
    $OutputDir = Join-Path $repoRoot "dist-release-$Version"
}

$SourceExe = (Resolve-Path $SourceExe).Path
New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null

$wix = Get-Command wix -ErrorAction SilentlyContinue
if (-not $wix) {
    throw "WiX not found in PATH. Install it first with: dotnet tool install --global wix"
}

& $wix.Source eula accept | Out-Null

$wxsPath = Join-Path $repoRoot "windows/msi/k8scli.wxs"
$outputPath = Join-Path $OutputDir "k8scli_$Version_windows_amd64.msi"

& $wix.Source build $wxsPath \
    -arch x64 \
    -d Version=$Version \
    -d SourceExe=$SourceExe \
    -o $outputPath

Write-Host "MSI created at: $outputPath"