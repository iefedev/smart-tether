# Finds the build tools; dot-sourced by the build scripts. Nothing here is tied to one machine:
# each tool comes from the usual environment variable, then PATH, then the tool's default
# install location.
#   JAVA_HOME                              JDK 21 (or java/keytool on PATH)
#   ANDROID_HOME / ANDROID_SDK_ROOT        Android SDK (default %LOCALAPPDATA%\Android\Sdk)
#   ANDROID_NDK_HOME / ANDROID_NDK_ROOT /  Android NDK (default: the newest under <sdk>\ndk)
#   ANDROID_NDK_LATEST_HOME
#   go, gomobile on PATH                   (gomobile also from $(go env GOPATH)\bin)

function Find-AndroidSdk {
    foreach ($p in $env:ANDROID_HOME, $env:ANDROID_SDK_ROOT, (Join-Path $env:LOCALAPPDATA 'Android\Sdk')) {
        if ($p -and (Test-Path (Join-Path $p 'platform-tools'))) { return (Resolve-Path $p).Path }
    }
    throw 'Android SDK not found: set ANDROID_HOME (or ANDROID_SDK_ROOT).'
}

function Find-AndroidNdk([string]$Sdk) {
    foreach ($p in $env:ANDROID_NDK_HOME, $env:ANDROID_NDK_ROOT, $env:ANDROID_NDK_LATEST_HOME) {
        if ($p -and (Test-Path (Join-Path $p 'source.properties'))) { return (Resolve-Path $p).Path }
    }
    $ndkDir = Join-Path $Sdk 'ndk'
    if (Test-Path $ndkDir) {
        $newest = Get-ChildItem $ndkDir -Directory |
            Where-Object { Test-Path (Join-Path $_.FullName 'source.properties') } |
            Sort-Object { try { [version]$_.Name } catch { [version]'0.0' } } -Descending |
            Select-Object -First 1
        if ($newest) { return $newest.FullName }
    }
    throw 'Android NDK not found: install it with the SDK manager or set ANDROID_NDK_HOME.'
}

function Find-JavaHome {
    if ($env:JAVA_HOME -and (Test-Path (Join-Path $env:JAVA_HOME 'bin\java.exe'))) { return $env:JAVA_HOME }
    $java = Get-Command java.exe -ErrorAction SilentlyContinue
    if ($java) { return (Split-Path (Split-Path $java.Source -Parent) -Parent) }
    throw 'JDK not found: set JAVA_HOME to a JDK 21.'
}

function Find-Tool([string]$Name, [string[]]$Extra = @()) {
    $cmd = Get-Command "$Name.exe" -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    foreach ($p in $Extra) { if ($p -and (Test-Path $p)) { return $p } }
    throw "$Name not found on PATH."
}

function Find-Go { Find-Tool 'go' @((Join-Path $env:ProgramFiles 'Go\bin\go.exe'), (Join-Path $env:LOCALAPPDATA 'Programs\go\bin\go.exe')) }

function Find-Gomobile([string]$Go) {
    $gopath = (& $Go env GOPATH).Split([IO.Path]::PathSeparator)[0]
    Find-Tool 'gomobile' @((Join-Path $gopath 'bin\gomobile.exe'))
}
