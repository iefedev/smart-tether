# Builds everything a new user needs into build\release:
#   SmartTether-Setup.exe   Windows installer (service, tray app, wintun, adb)
#   SmartTether.apk         phone app, signed with the release key (see android/app/build.gradle.kts)
#   build-release.ps1 [-Version 0.2.0] [-SkipAndroid]
# Tools are found as described in tools/toolchain.ps1; Inno Setup 6 from PATH (iscc) or its
# default install folders, or set ISCC to ISCC.exe.
param([string]$Version = '0.2.0', [switch]$SkipAndroid)
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot\toolchain.ps1"
$root = Split-Path $PSScriptRoot -Parent
$out = Join-Path $root 'build\release'
$stage = Join-Path $out 'stage'
$sdkTools = Join-Path (Find-AndroidSdk) 'platform-tools'
$go = Find-Go
$iscc = @($env:ISCC, (Get-Command ISCC.exe -ErrorAction SilentlyContinue).Source,
    "$env:LOCALAPPDATA\Programs\Inno Setup 6\ISCC.exe", "${env:ProgramFiles(x86)}\Inno Setup 6\ISCC.exe",
    "$env:ProgramFiles\Inno Setup 6\ISCC.exe") |
    Where-Object { $_ -and (Test-Path $_) } | Select-Object -First 1
if (-not $iscc) { throw 'Inno Setup 6 not found (winget install JRSoftware.InnoSetup, or set ISCC)' }

if (Test-Path $stage) { Remove-Item -Recurse -Force $stage }
New-Item -ItemType Directory -Force "$stage\bin", "$stage\tools", "$stage\licenses" | Out-Null

Push-Location $root
try {
    'service'
    $env:GOOS = 'windows'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
    & $go build -trimpath -ldflags "-s -w -X main.version=$Version" -o "$stage\bin\SmartTetherService.exe" ./cmd/smart-tether-service
    if ($LASTEXITCODE) { throw 'go build failed' }
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED

    'tray app'
    dotnet publish windows\SmartTether\SmartTether.csproj -c Release -o "$out\tray" -p:Version=$Version -nologo -v q
    if ($LASTEXITCODE) { throw 'dotnet publish failed' }
    Copy-Item "$out\tray\SmartTether.exe" $stage

    'third-party files'
    Copy-Item LICENSE "$stage\licenses\SmartTether-LICENSE.txt"
    Copy-Item third_party\wintun\wintun.dll "$stage\bin"
    Copy-Item third_party\wintun\LICENSE.txt "$stage\licenses\wintun-LICENSE.txt"
    if (-not (Test-Path "$sdkTools\adb.exe")) { throw "adb.exe not found in $sdkTools" }
    foreach ($f in 'adb.exe', 'AdbWinApi.dll', 'AdbWinUsbApi.dll', 'libwinpthread-1.dll') {
        if (Test-Path "$sdkTools\$f") { Copy-Item "$sdkTools\$f" "$stage\bin" }
    }
    Copy-Item "$sdkTools\NOTICE.txt" "$stage\licenses\adb-NOTICE.txt"
    Copy-Item tools\mobile-hotspot.ps1 "$stage\tools"

    'installer'
    & $iscc /Q "/DAppVersion=$Version" installer\smart-tether.iss
    if ($LASTEXITCODE) { throw 'ISCC failed' }

    if (-not $SkipAndroid) {
        'phone app'
        & "$PSScriptRoot\build-android.ps1" -Release | Out-Null
        Copy-Item android\app\build\outputs\apk\release\app-release.apk "$out\SmartTether.apk" -Force
    }
    Get-ChildItem $out -File | Select-Object Name, @{ n = 'MB'; e = { [math]::Round($_.Length / 1MB, 1) } }
} finally {
    Pop-Location
}
