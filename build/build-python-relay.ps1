$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$source = Join-Path $root 'relay/python'
$dashboardAssets = @(
    (Join-Path $root 'internal/relaydashboard/dashboard.html'),
    (Join-Path $root 'internal/relaydashboard/icon.svg')
)
$requirements = Join-Path $source 'requirements.txt'
$publish = Join-Path $root 'dist/python-relay/publish'
$zip = Join-Path $root 'dist/python-relay/deskferry-python-relay.zip'
$vendor = Join-Path $root 'dist/python-relay/vendor-linux-cp39'
$vendoredPublish = Join-Path $root 'dist/python-relay/publish-linux-cp39-vendored'
$vendoredZip = Join-Path $root 'dist/python-relay/deskferry-python-relay-linux-cp39-vendored.zip'

Add-Type -AssemblyName System.IO.Compression
Add-Type -AssemblyName System.IO.Compression.FileSystem

function New-PortableZip {
    param(
        [string] $SourceDirectory,
        [string] $DestinationPath
    )

    if (Test-Path -LiteralPath $DestinationPath) {
        Remove-Item -LiteralPath $DestinationPath -Force
    }
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $DestinationPath) | Out-Null

    $stream = [System.IO.File]::Open($DestinationPath, [System.IO.FileMode]::Create)
    $archive = New-Object System.IO.Compression.ZipArchive -ArgumentList @($stream, [System.IO.Compression.ZipArchiveMode]::Create)
    try {
        $sourceRoot = (Resolve-Path -LiteralPath $SourceDirectory).ProviderPath.TrimEnd('\', '/')
        Get-ChildItem -LiteralPath $SourceDirectory -Recurse -File | ForEach-Object {
            $relative = $_.FullName.Substring($sourceRoot.Length).TrimStart('\', '/').Replace('\', '/')
            [System.IO.Compression.ZipFileExtensions]::CreateEntryFromFile($archive, $_.FullName, $relative, [System.IO.Compression.CompressionLevel]::Optimal) | Out-Null
        }
    }
    finally {
        $archive.Dispose()
        $stream.Dispose()
    }
}

Push-Location $root
try {
    python -m pytest relay/python/tests
    if ($LASTEXITCODE -ne 0) { throw 'python relay tests failed' }

    if (Test-Path -LiteralPath $publish) {
        Remove-Item -LiteralPath $publish -Recurse -Force
    }
    New-Item -ItemType Directory -Force -Path $publish | Out-Null
    Copy-Item -LiteralPath (Join-Path $source 'app.py') -Destination $publish
    Copy-Item -LiteralPath (Join-Path $source 'requirements.txt') -Destination $publish
    Copy-Item -LiteralPath (Join-Path $source 'startup.sh') -Destination $publish
    # The dashboard page and icon are shared with the Go and .NET relays; app.py
    # loads them from its own directory when published.
    Copy-Item -LiteralPath $dashboardAssets -Destination $publish

    Get-ChildItem -LiteralPath (Split-Path -Parent $zip) -Filter 'tunneldesktop-python-relay*.zip' -File -ErrorAction SilentlyContinue | ForEach-Object {
        Remove-Item -LiteralPath $_.FullName -Force
    }
    New-PortableZip -SourceDirectory $publish -DestinationPath $zip
    Write-Host "built $zip"

    if (Test-Path -LiteralPath $vendor) {
        Remove-Item -LiteralPath $vendor -Recurse -Force
    }
    New-Item -ItemType Directory -Force -Path $vendor | Out-Null
    python -m pip install --target $vendor --platform manylinux2014_x86_64 --implementation cp --python-version 3.9 --abi cp39 --only-binary=:all: -r $requirements
    if ($LASTEXITCODE -ne 0) { throw 'python relay Linux dependency vendoring failed' }
    python -m pip install --target $vendor --platform manylinux2014_x86_64 --implementation cp --python-version 3.9 --abi cp39 --only-binary=:all: 'exceptiongroup>=1.0' 'eval-type-backport>=0.2'
    if ($LASTEXITCODE -ne 0) { throw 'python relay Linux compatibility dependency vendoring failed' }

    if (Test-Path -LiteralPath $vendoredPublish) {
        Remove-Item -LiteralPath $vendoredPublish -Recurse -Force
    }
    New-Item -ItemType Directory -Force -Path $vendoredPublish | Out-Null
    Copy-Item -LiteralPath (Join-Path $source 'app.py') -Destination $vendoredPublish
    Copy-Item -LiteralPath (Join-Path $source 'requirements.txt') -Destination $vendoredPublish
    Copy-Item -LiteralPath (Join-Path $source 'startup.sh') -Destination $vendoredPublish
    # The dashboard page and icon are shared with the Go and .NET relays; app.py
    # loads them from its own directory when published.
    Copy-Item -LiteralPath $dashboardAssets -Destination $vendoredPublish
    Copy-Item -LiteralPath $vendor -Destination (Join-Path $vendoredPublish 'vendor') -Recurse

    New-PortableZip -SourceDirectory $vendoredPublish -DestinationPath $vendoredZip
    Write-Host "built $vendoredZip"
}
finally {
    Pop-Location
}
