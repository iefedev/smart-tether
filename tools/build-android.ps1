# Builds the gomobile AAR and the APK. Run from anywhere.
#   build-android.ps1            debug APK
#   build-android.ps1 -Release   release APK; needs the release key (see android/app/build.gradle.kts)
# Tools are found as described in tools/toolchain.ps1 (JAVA_HOME, ANDROID_HOME, ANDROID_NDK_HOME, PATH).
param([switch]$Release)
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot\toolchain.ps1"
$root = Split-Path $PSScriptRoot -Parent

$env:ANDROID_HOME = Find-AndroidSdk
$env:ANDROID_NDK_HOME = Find-AndroidNdk $env:ANDROID_HOME
$env:JAVA_HOME = Find-JavaHome
$go = Find-Go
$gomobile = Find-Gomobile $go
# gomobile runs go and gobind itself, so both must be on PATH.
$gopathBin = Join-Path ((& $go env GOPATH).Split([IO.Path]::PathSeparator)[0]) 'bin'
$env:Path = "$(Split-Path $go -Parent);$gopathBin;$env:JAVA_HOME\bin;$env:Path"
"sdk: $env:ANDROID_HOME"
"ndk: $env:ANDROID_NDK_HOME"
"jdk: $env:JAVA_HOME"

Push-Location $root
try {
    New-Item -ItemType Directory -Force android\app\libs | Out-Null
    # Keep the build machine's paths (user name, GOROOT, NDK) out of the shipped library: -trimpath,
    # -s -w for the cgo glue's debug info, and a temporary drive letter because gomobile records
    # the module folder in the build info even with -trimpath.
    $drive = [char[]](90..68) | ForEach-Object { "${_}:" } | Where-Object { -not (Test-Path "$_\") } | Select-Object -First 1
    subst $drive $root
    Push-Location "$drive\"
    try {
        & $gomobile bind -trimpath -ldflags '-s -w' -target android/arm64 -androidapi 31 -javapkg dev.smarttether.core -o android\app\libs\core.aar ./mobile
        if ($LASTEXITCODE) { throw "gomobile bind failed" }
    } finally {
        Pop-Location
        subst $drive /d
    }
    $task = if ($Release) { 'assembleRelease' } else { 'assembleDebug' }
    # --no-daemon: a Gradle daemon would outlive this script and keep its console handles open.
    # Gradle's stderr is not redirected, so its warnings don't become PowerShell errors.
    & "$root\android\gradlew.bat" -p "$root\android" $task --no-daemon --console=plain -q
    if ($LASTEXITCODE) { throw "gradle failed" }
    if ($Release) { Get-Item android\app\build\outputs\apk\release\app-release.apk }
    else { Get-Item android\app\build\outputs\apk\debug\app-debug.apk }
} finally {
    Pop-Location
}
