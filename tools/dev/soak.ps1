# Repeated downloads through the tunnel; one line per attempt. Used to watch link stability.
#   soak.ps1 -Url <a large test file, e.g. a speed-test server's 100 MB file> [-Minutes 10] [-EverySeconds 20]
param([int]$Minutes = 10, [int]$EverySeconds = 20, [Parameter(Mandatory)][string]$Url,
      [string]$Out = "$PSScriptRoot\..\bin\soak.log")
New-Item -ItemType Directory -Force (Split-Path $Out) | Out-Null
$end = (Get-Date).AddMinutes($Minutes)
"start $(Get-Date -Format o) url=$Url" | Set-Content $Out
while ((Get-Date) -lt $end) {
    $r = curl.exe -s -o NUL -m 30 -w "%{http_code} %{size_download} %{speed_download}" $Url
    $p = $r -split ' '
    "{0} http={1} MB={2:N1} Mbit/s={3:N1}" -f (Get-Date -Format HH:mm:ss), $p[0], ([double]$p[1] / 1MB), ([double]$p[2] * 8 / 1e6) | Add-Content $Out
    Start-Sleep -Seconds $EverySeconds
}
"end $(Get-Date -Format o)" | Add-Content $Out
