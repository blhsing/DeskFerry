$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$project = Join-Path $root 'home-agent/android'
$dist = Join-Path $root 'dist/android'
$apk = Join-Path $project 'app/build/outputs/apk/debug/app-debug.apk'
$out = Join-Path $dist 'deskferry-home-android-debug.apk'

if ($env:DESKFERRY_ANDROID_EXPECT_SIGNED -eq '1') {
    foreach ($name in @('DESKFERRY_ANDROID_KEYSTORE', 'DESKFERRY_ANDROID_KEYSTORE_PASSWORD', 'DESKFERRY_ANDROID_KEY_ALIAS', 'DESKFERRY_ANDROID_KEY_PASSWORD')) {
        $value = [Environment]::GetEnvironmentVariable($name)
        if ([string]::IsNullOrWhiteSpace($value)) {
            throw "required Android signing variable is missing: $name"
        }
    }
    if (-not (Test-Path -LiteralPath $env:DESKFERRY_ANDROID_KEYSTORE -PathType Leaf)) {
        throw "Android signing keystore is missing: $env:DESKFERRY_ANDROID_KEYSTORE"
    }
}

if (-not $env:ANDROID_HOME) {
    if (Test-Path -LiteralPath 'D:\Android\Sdk') {
        $env:ANDROID_HOME = 'D:\Android\Sdk'
    } elseif ($env:ANDROID_SDK_ROOT) {
        $env:ANDROID_HOME = $env:ANDROID_SDK_ROOT
    }
}
if (-not $env:ANDROID_HOME -or -not (Test-Path -LiteralPath $env:ANDROID_HOME)) {
    throw 'ANDROID_HOME is not set to a readable Android SDK.'
}

if (-not $env:JAVA_HOME -or -not (Test-Path -LiteralPath $env:JAVA_HOME)) {
    $jdk = @(
        'C:\Program Files\Java\jdk-25',
        'C:\Program Files\Eclipse Adoptium\jdk-21',
        'C:\Program Files\Java\jdk-21'
    ) | Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
    if ($jdk) {
        $env:JAVA_HOME = $jdk
    }
}
if (-not $env:JAVA_HOME -or -not (Test-Path -LiteralPath $env:JAVA_HOME)) {
    throw 'JAVA_HOME is not set to a readable JDK.'
}
$env:Path = (Join-Path $env:JAVA_HOME 'bin') + ';' + $env:Path

$gradlew = Join-Path $project 'gradlew.bat'
if (Test-Path -LiteralPath $gradlew) {
    $gradleCommand = $gradlew
    $gradleArgs = @('--no-daemon', ':app:assembleDebug')
} else {
    $gradleCommand = 'gradle'
    $gradleArgs = @('--no-daemon', ':app:assembleDebug')
}

Push-Location $project
try {
    & $gradleCommand @gradleArgs
    if ($LASTEXITCODE -ne 0) {
        throw 'Android home APK build failed'
    }
}
finally {
    Pop-Location
}

if (-not (Test-Path -LiteralPath $apk)) {
    throw "missing Android APK: $apk"
}
New-Item -ItemType Directory -Force -Path $dist | Out-Null
Get-ChildItem -LiteralPath $dist -Filter 'tunneldesktop-home-android*.apk' -File -ErrorAction SilentlyContinue | ForEach-Object {
    Remove-Item -LiteralPath $_.FullName -Force
}
Copy-Item -LiteralPath $apk -Destination $out -Force
Write-Host "built $out"
