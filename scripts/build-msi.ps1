param(
    [string]$Version = "0.1.0",
    [string]$SourceExe = "",
    [string]$OutputDir = ""
)

$ErrorActionPreference = "Stop"

# $IsWindows only exists in PowerShell 6+, $env:OS works everywhere.
if ($env:OS -ne "Windows_NT") {
    throw "MSI generation must run on Windows because WiX only supports Windows builds."
}

# MSI versions are numeric (major.minor.patch); drop any pre-release suffix.
if ($Version -notmatch '^v?(\d+)\.(\d+)\.(\d+)') {
    throw "Version '$Version' must look like 1.2.3"
}
$msiVersion = "$($Matches[1]).$($Matches[2]).$($Matches[3])"

$repoRoot = Split-Path -Parent $PSScriptRoot

if ([string]::IsNullOrWhiteSpace($SourceExe)) {
    # Layout produced by GoReleaser.
    $SourceExe = Join-Path $repoRoot "dist/k8scli_windows_amd64_v1/k8scli.exe"
}

if ([string]::IsNullOrWhiteSpace($OutputDir)) {
    $OutputDir = Join-Path $repoRoot "dist"
}

$SourceExe = (Resolve-Path $SourceExe).Path
New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null
$OutputDir = (Resolve-Path $OutputDir).Path

$wix = Get-Command wix -ErrorAction SilentlyContinue
if (-not $wix) {
    throw "WiX not found in PATH. Install it first with: dotnet tool install --global wix --version 4.0.5"
}

# WiX 7+ asks for the OSMF EULA to be accepted; older versions have no such command.
$wixMajor = [int](((& $wix.Source --version) -split '\.')[0])
if ($wixMajor -ge 7) {
    & $wix.Source eula accept wix7 | Out-Null
}

$wxsPath = Join-Path $repoRoot "windows/msi/k8scli.wxs"
$outputPath = Join-Path $OutputDir "k8scli_${Version}_windows_amd64.msi"

& $wix.Source build $wxsPath `
    -arch x64 `
    -d Version=$msiVersion `
    -d SourceExe=$SourceExe `
    -o $outputPath

if ($LASTEXITCODE -ne 0) {
    throw "wix build failed with exit code $LASTEXITCODE"
}

Write-Host "MSI created at: $outputPath"
