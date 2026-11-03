# Creates the APK release key once, outside the repo: ~/.smart-tether/release.jks plus
# keystore.properties (read by android/app/build.gradle.kts). Back both up: an update to the
# installed app must be signed with the same key. Prints no secrets.
# keytool comes from JAVA_HOME or PATH (see tools/toolchain.ps1).
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot\toolchain.ps1"
$dir = Join-Path $env:USERPROFILE '.smart-tether'
$jks = Join-Path $dir 'release.jks'
$props = Join-Path $dir 'keystore.properties'
if (Test-Path $jks) { "keystore already exists: $jks"; return }
New-Item -ItemType Directory -Force $dir | Out-Null
$bytes = New-Object byte[] 24
[System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
$pass = [Convert]::ToBase64String($bytes) -replace '[+/=]', 'x'
$keytool = Join-Path (Find-JavaHome) 'bin\keytool.exe'
# keytool reports progress on stderr, which -ErrorAction Stop would turn into a failure.
$ErrorActionPreference = 'Continue'
& $keytool -genkeypair -keystore $jks -storetype PKCS12 -alias smarttether -keyalg RSA -keysize 4096 `
    -validity 10000 -storepass $pass -keypass $pass -dname 'CN=Smart Tether' 2>$null | Out-Null
if ($LASTEXITCODE) { throw 'keytool failed' }
$ErrorActionPreference = 'Stop'
@"
storeFile=$($jks -replace '\\', '/')
storePassword=$pass
keyAlias=smarttether
keyPassword=$pass
"@ | Set-Content -Encoding ascii $props
"created $jks and $props"
