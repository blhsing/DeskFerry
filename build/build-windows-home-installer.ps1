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
$dependencyDir = Join-Path $root 'dist/deps/home-installer'
$extractDir = Join-Path $root 'dist/tmp/home-installer-dependencies'
$outputDir = Join-Path $root 'dist/windows-home-installer'
$payloadDir = Join-Path $root 'dist/tmp/home-installer-payload'
$payloadZip = Join-Path $outputDir 'DeskFerryHomePayload.zip'
$setupOutputName = if ($DebugArtifactSuffix) { 'DeskFerryHomeSetup-debug.exe' } else { 'DeskFerryHomeSetup.exe' }
$setupOutput = Join-Path $outputDir $setupOutputName

$wintunVersion = '0.14.1'
$wintunURL = "https://www.wintun.net/builds/wintun-$wintunVersion.zip"
$wintunSHA256 = '07C256185D6EE3652E09FA55C0B673E2624B565E02C4B9091C79CA7D2F24EF51'
$tun2socksVersion = '2.6.0'
$tun2socksURL = "https://github.com/xjasonlyu/tun2socks/releases/download/v$tun2socksVersion/tun2socks-windows-amd64.zip"
$tun2socksSHA256 = '1429E2E3B1EA09052DA2C65E5005538B5730D63DA37E304F4AD6FD2698A66695'
$tun2socksLicenseURL = "https://raw.githubusercontent.com/xjasonlyu/tun2socks/v$tun2socksVersion/LICENSE"
$tun2socksLicenseSHA256 = '796ABBF6D0258A01321F88F72C1D712417ACDD75342510F0796F7C567B6F05E5'

function Get-VerifiedDownload {
    param(
        [string] $URL,
        [string] $Destination,
        [string] $SHA256
    )
    if (-not (Test-Path -LiteralPath $Destination)) {
        Invoke-WebRequest -UseBasicParsing -Uri $URL -OutFile $Destination
    }
    $actual = (Get-FileHash -LiteralPath $Destination -Algorithm SHA256).Hash
    if ($actual -ne $SHA256) {
        throw "SHA256 mismatch for $Destination. Expected $SHA256, got $actual."
    }
}

function Assert-BuildFile {
    param([string] $Path)
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        throw "Required build file is missing: $Path"
    }
}

New-Item -ItemType Directory -Force -Path $dependencyDir, $extractDir, $outputDir, $payloadDir | Out-Null

if (-not $SkipGoBuild) {
    $buildArgs = @()
    if ($DebugWindows) {
        $buildArgs += '-DebugWindows'
    }
    if ($DebugArtifactSuffix) {
        $buildArgs += '-DebugArtifactSuffix'
    }
    & (Join-Path $PSScriptRoot 'build-go.ps1') @buildArgs
    if ($LASTEXITCODE -ne 0) { throw 'Go artifact build failed' }
}

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

$payloadNames = @(
    'DeskFerryHome.exe',
    'DeskFerryHomeNetwork.exe',
    'tun2socks.exe',
    'wintun.dll',
    'LICENSE-Wintun.txt',
    'LICENSE-tun2socks.txt'
)
foreach ($name in $payloadNames) {
    $existing = Join-Path $payloadDir $name
    if (Test-Path -LiteralPath $existing) {
        Remove-Item -LiteralPath $existing -Force
    }
}

$debugSuffix = if ($DebugArtifactSuffix) { '-debug' } else { '' }
$homeBinary = Join-Path $binDir "deskferry-home-windows-amd64$debugSuffix.exe"
$networkBinary = Join-Path $binDir "deskferry-home-network-windows-amd64$debugSuffix.exe"
$setupBase = Join-Path $binDir "deskferry-home-setup-windows-amd64$debugSuffix.exe"
$wintunDLL = Join-Path $wintunExtract 'wintun/bin/amd64/wintun.dll'
$wintunLicense = Join-Path $wintunExtract 'wintun/LICENSE.txt'
$tun2socksBinary = Join-Path $tun2socksExtract 'tun2socks-windows-amd64.exe'
foreach ($path in @($homeBinary, $networkBinary, $setupBase, $wintunDLL, $wintunLicense, $tun2socksBinary, $tun2socksLicense)) {
    Assert-BuildFile -Path $path
}

Copy-Item -LiteralPath $homeBinary -Destination (Join-Path $payloadDir 'DeskFerryHome.exe') -Force
Copy-Item -LiteralPath $networkBinary -Destination (Join-Path $payloadDir 'DeskFerryHomeNetwork.exe') -Force
Copy-Item -LiteralPath $tun2socksBinary -Destination (Join-Path $payloadDir 'tun2socks.exe') -Force
Copy-Item -LiteralPath $wintunDLL -Destination (Join-Path $payloadDir 'wintun.dll') -Force
Copy-Item -LiteralPath $wintunLicense -Destination (Join-Path $payloadDir 'LICENSE-Wintun.txt') -Force
Copy-Item -LiteralPath $tun2socksLicense -Destination (Join-Path $payloadDir 'LICENSE-tun2socks.txt') -Force

if (Test-Path -LiteralPath $payloadZip) {
    Remove-Item -LiteralPath $payloadZip -Force
}
Compress-Archive -Path (Join-Path $payloadDir '*') -DestinationPath $payloadZip -CompressionLevel Optimal

if (Test-Path -LiteralPath $setupOutput) {
    Remove-Item -LiteralPath $setupOutput -Force
}
Push-Location $root
try {
    go run ./build/tools/sfxzip -stub $setupBase -zip $payloadZip -output $setupOutput
    if ($LASTEXITCODE -ne 0) { throw 'Self-extracting setup assembly failed' }
}
finally {
    Pop-Location
}

Assert-BuildFile -Path $setupOutput
$setupHash = (Get-FileHash -LiteralPath $setupOutput -Algorithm SHA256).Hash
Write-Host "built $setupOutput"
Write-Host "SHA256 $setupHash"
