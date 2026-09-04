param(
    [switch] $DebugWindows,
    [switch] $DebugArtifactSuffix,
    [switch] $SkipGoBuild
)

$ErrorActionPreference = 'Stop'

if ($DebugArtifactSuffix -and -not $DebugWindows) {
    throw '-DebugArtifactSuffix requires -DebugWindows'
}

$root = Split-Path -Parent $PSScriptRoot
$binDir = Join-Path $root 'dist/bin'
$dependencyDir = Join-Path $root 'dist/deps/windows'
$extractDir = Join-Path $root 'dist/tmp/windows-dependencies'
$payloadDir = Join-Path $root 'dist/tmp/windows-payload'
$payloadZip = Join-Path $root 'dist/tmp/DeskFerryPayload.zip'

$wintunVersion = '0.14.1'
$wintunURL = "https://www.wintun.net/builds/wintun-$wintunVersion.zip"
$wintunSHA256 = '07C256185D6EE3652E09FA55C0B673E2624B565E02C4B9091C79CA7D2F24EF51'
$tun2socksVersion = '2.6.0'
$tun2socksURL = "https://github.com/xjasonlyu/tun2socks/releases/download/v$tun2socksVersion/tun2socks-windows-amd64.zip"
$tun2socksSHA256 = '1429E2E3B1EA09052DA2C65E5005538B5730D63DA37E304F4AD6FD2698A66695'
$tun2socksLicenseURL = "https://raw.githubusercontent.com/xjasonlyu/tun2socks/v$tun2socksVersion/LICENSE"
$tun2socksLicenseSHA256 = '796ABBF6D0258A01321F88F72C1D712417ACDD75342510F0796F7C567B6F05E5'

function Get-VerifiedDownload {
    param([string] $URL, [string] $Destination, [string] $SHA256)
    if (-not (Test-Path -LiteralPath $Destination)) {
        Invoke-WebRequest -UseBasicParsing -Uri $URL -OutFile $Destination
    }
    $actual = (Get-FileHash -LiteralPath $Destination -Algorithm SHA256).Hash
    if ($actual -ne $SHA256) {
        throw "SHA256 mismatch for $Destination. Expected $SHA256, got $actual."
    }
}

if (-not $SkipGoBuild) {
    $goBuildParams = @{}
    if ($DebugWindows) { $goBuildParams.DebugWindows = $true }
    if ($DebugArtifactSuffix) { $goBuildParams.DebugArtifactSuffix = $true }
    & (Join-Path $PSScriptRoot 'build-go.ps1') @goBuildParams
    if ($LASTEXITCODE -ne 0) { throw 'Go artifact build failed' }
}

foreach ($path in @($extractDir, $payloadDir)) {
    if (Test-Path -LiteralPath $path) {
        Remove-Item -LiteralPath $path -Recurse -Force
    }
}
New-Item -ItemType Directory -Force -Path $dependencyDir, $extractDir, $payloadDir | Out-Null
$wintunArchive = Join-Path $dependencyDir "wintun-$wintunVersion.zip"
$tun2socksArchive = Join-Path $dependencyDir "tun2socks-windows-amd64-v$tun2socksVersion.zip"
$tun2socksLicense = Join-Path $dependencyDir 'LICENSE-tun2socks.txt'
Get-VerifiedDownload -URL $wintunURL -Destination $wintunArchive -SHA256 $wintunSHA256
Get-VerifiedDownload -URL $tun2socksURL -Destination $tun2socksArchive -SHA256 $tun2socksSHA256
Get-VerifiedDownload -URL $tun2socksLicenseURL -Destination $tun2socksLicense -SHA256 $tun2socksLicenseSHA256

$wintunExtract = Join-Path $extractDir "wintun-$wintunVersion"
$tun2socksExtract = Join-Path $extractDir "tun2socks-$tun2socksVersion"
New-Item -ItemType Directory -Force -Path $wintunExtract, $tun2socksExtract | Out-Null
Expand-Archive -LiteralPath $wintunArchive -DestinationPath $wintunExtract -Force
Expand-Archive -LiteralPath $tun2socksArchive -DestinationPath $tun2socksExtract -Force

$payloadFiles = @{
    'wintun.dll' = Join-Path $wintunExtract 'wintun/bin/amd64/wintun.dll'
    'LICENSE-Wintun.txt' = Join-Path $wintunExtract 'wintun/LICENSE.txt'
    'tun2socks.exe' = Join-Path $tun2socksExtract 'tun2socks-windows-amd64.exe'
    'LICENSE-tun2socks.txt' = $tun2socksLicense
}
foreach ($entry in $payloadFiles.GetEnumerator()) {
    if (-not (Test-Path -LiteralPath $entry.Value -PathType Leaf)) {
        throw "Required Windows component is missing: $($entry.Value)"
    }
    Copy-Item -LiteralPath $entry.Value -Destination (Join-Path $payloadDir $entry.Key) -Force
}

if (Test-Path -LiteralPath $payloadZip) { Remove-Item -LiteralPath $payloadZip -Force }
Compress-Archive -Path (Join-Path $payloadDir '*') -DestinationPath $payloadZip -CompressionLevel Optimal

$suffix = if ($DebugArtifactSuffix) { '-debug' } else { '' }
$output = Join-Path $binDir "deskferry-windows-amd64$suffix.exe"
$stub = "$output.stub"
if (-not (Test-Path -LiteralPath $output -PathType Leaf)) { throw "Missing Windows executable: $output" }
Copy-Item -LiteralPath $output -Destination $stub -Force
try {
    Push-Location $root
    try {
        go run ./build/tools/sfxzip -stub $stub -zip $payloadZip -output $output
        if ($LASTEXITCODE -ne 0) { throw 'Self-contained Windows executable assembly failed' }
    }
    finally { Pop-Location }
}
finally {
    Remove-Item -LiteralPath $stub -Force -ErrorAction SilentlyContinue
}

if (-not (Test-Path -LiteralPath $output -PathType Leaf)) { throw "Build did not produce $output" }
$hash = (Get-FileHash -LiteralPath $output -Algorithm SHA256).Hash
Write-Host "built $output"
Write-Host "SHA256 $hash"
