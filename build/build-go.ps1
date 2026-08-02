$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$out = Join-Path $root 'dist/bin'
$tmp = Join-Path $root 'dist/tmp'
$goCache = Join-Path $root 'dist/gocache'
New-Item -ItemType Directory -Force -Path $out | Out-Null
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
New-Item -ItemType Directory -Force -Path $goCache | Out-Null

$savedEnv = @{
    GOTMPDIR = $env:GOTMPDIR
    GOCACHE = $env:GOCACHE
    TEMP = $env:TEMP
    TMP = $env:TMP
}
$env:GOTMPDIR = $tmp
$env:GOCACHE = $goCache
$env:TEMP = $tmp
$env:TMP = $tmp

function Restore-EnvValue {
    param(
        [string] $Name,
        [string] $Value
    )
    if ($null -eq $Value) {
        Remove-Item "Env:\$Name" -ErrorAction SilentlyContinue
    }
    else {
        Set-Item "Env:\$Name" $Value
    }
}

function Ensure-Rsrc {
    if (Get-Command rsrc -ErrorAction SilentlyContinue) {
        return
    }
    $goBin = 'D:\Go\bin'
    New-Item -ItemType Directory -Force -Path $goBin | Out-Null
    $oldGoBin = $env:GOBIN
    try {
        $env:GOBIN = $goBin
        go install github.com/akavel/rsrc@latest
        if ($LASTEXITCODE -ne 0) { throw 'go install rsrc failed' }
    }
    finally {
        if ($null -eq $oldGoBin) {
            Remove-Item Env:\GOBIN -ErrorAction SilentlyContinue
        }
        else {
            $env:GOBIN = $oldGoBin
        }
    }
    $env:PATH = "$goBin;$env:PATH"
}

function Build-WindowsResources {
    Ensure-Rsrc
    & (Join-Path $PSScriptRoot 'generate-client-icon.ps1')
    if (-not $?) { throw 'client icon generation failed' }
    $resources = @(
        @{ Manifest = 'work-agent/windows/configurator/app.manifest'; Output = 'work-agent/windows/configurator/rsrc_windows_amd64.syso'; Name = 'agent configurator' },
        @{ Manifest = 'home-agent/windows/app.manifest'; Output = 'home-agent/windows/rsrc_windows_amd64.syso'; Name = 'home app'; Icon = 'home-agent/windows/app.ico' }
    )
    foreach ($resource in $resources) {
        $manifest = Join-Path $root $resource.Manifest
        $output = Join-Path $root $resource.Output
        $rsrcArgs = @('-manifest', $manifest, '-arch', 'amd64', '-o', $output)
        if ($resource.ContainsKey('Icon')) {
            $rsrcArgs += @('-ico', (Join-Path $root $resource.Icon))
        }
        rsrc @rsrcArgs
        if ($LASTEXITCODE -ne 0) { throw "rsrc failed for $($resource.Name) manifest" }
    }
}

Push-Location $root
try {
    Build-WindowsResources

    go mod download
    if ($LASTEXITCODE -ne 0) { throw 'go mod download failed' }
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw 'go test failed' }

    $targets = @(
        @{ GOOS = 'windows'; GOARCH = 'amd64'; Name = 'deskferry-agent-windows-amd64.exe'; Package = './work-agent/windows/service' },
        # Keep symbols in Windows GUI artifacts. Stripped PE files have repeatedly
        # triggered endpoint-protection false positives on deployment hosts.
        @{ GOOS = 'windows'; GOARCH = 'amd64'; Name = 'deskferry-agent-configurator-windows-amd64.exe'; Package = './work-agent/windows/configurator'; Ldflags = '-H windowsgui' },
        @{ GOOS = 'windows'; GOARCH = 'amd64'; Name = 'deskferry-home-windows-amd64.exe'; Package = './home-agent/windows'; Ldflags = '-H windowsgui' },
        @{ GOOS = 'darwin'; GOARCH = 'arm64'; Name = 'deskferry-home-macos-arm64'; Package = './home-agent/macos' },
        @{ GOOS = 'darwin'; GOARCH = 'amd64'; Name = 'deskferry-home-macos-amd64'; Package = './home-agent/macos' },
        @{ GOOS = 'linux'; GOARCH = 'amd64'; Name = 'deskferry-relay-linux-amd64'; Package = './relay/go' }
    )

    $legacyArtifactPatterns = @(
        'agent-installer-windows-amd64.exe',
        'agent-windows-amd64.exe',
        'agent-configurator-windows-amd64.exe',
        'client-windows-amd64.exe',
        'relay-*'
    )
    foreach ($legacyArtifactPattern in $legacyArtifactPatterns) {
        Get-ChildItem -LiteralPath $out -Filter $legacyArtifactPattern -File -ErrorAction SilentlyContinue | ForEach-Object {
            Remove-Item -LiteralPath $_.FullName -Force
        }
    }

    foreach ($target in $targets) {
        $env:GOOS = $target.GOOS
        $env:GOARCH = $target.GOARCH
        $env:CGO_ENABLED = '0'
        $path = Join-Path $out $target.Name
        $ldflags = '-s -w'
        if ($target.ContainsKey('Ldflags')) {
            $ldflags = $target.Ldflags
        }
        go build -trimpath -ldflags $ldflags -o $path $target.Package
        if ($LASTEXITCODE -ne 0) { throw "go build failed for $($target.Name)" }
        if (-not (Test-Path -LiteralPath $path)) {
            throw "go build did not produce $path. Endpoint protection may have quarantined the generated executable."
        }
        Start-Sleep -Seconds 3
        if (-not (Test-Path -LiteralPath $path)) {
            throw "$path was removed after build. Endpoint protection may have quarantined the generated executable."
        }
        Write-Host "built $path"
    }
}
finally {
    Remove-Item Env:\GOOS -ErrorAction SilentlyContinue
    Remove-Item Env:\GOARCH -ErrorAction SilentlyContinue
    Remove-Item Env:\CGO_ENABLED -ErrorAction SilentlyContinue
    Restore-EnvValue -Name 'GOTMPDIR' -Value $savedEnv.GOTMPDIR
    Restore-EnvValue -Name 'GOCACHE' -Value $savedEnv.GOCACHE
    Restore-EnvValue -Name 'TEMP' -Value $savedEnv.TEMP
    Restore-EnvValue -Name 'TMP' -Value $savedEnv.TMP
    Pop-Location
}
