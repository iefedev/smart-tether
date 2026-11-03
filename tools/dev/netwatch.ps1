# Logs PC internet reachability every 2 s, so a failover can be read back afterwards:
# HTTPS by name, a raw TCP connect by IP, a DNS lookup through the tunnel, and the TUN adapter status.
param([int]$Minutes = 20, [string]$Out = "$PSScriptRoot\..\bin\netwatch.log")
New-Item -ItemType Directory -Force (Split-Path $Out) | Out-Null
$end = (Get-Date).AddMinutes($Minutes)
"start $(Get-Date -Format o)" | Set-Content $Out
while ((Get-Date) -lt $end) {
    $r = curl.exe -s -o NUL -m 3 -w "%{http_code} %{time_total} %{errormsg}" https://www.google.com/generate_204
    $tcp = 'ok'
    $c = New-Object Net.Sockets.TcpClient
    try {
        $t = $c.ConnectAsync('1.1.1.1', 443)
        if (-not $t.Wait(3000)) { $tcp = 'timeout' }
    } catch { $tcp = $_.Exception.InnerException.InnerException.Message }
    $c.Close()
    $dns = try { (Resolve-DnsName ("t{0}.example.com" -f (Get-Random)) -Server 198.18.0.2 -DnsOnly -QuickTimeout -ErrorAction Stop | Out-Null); 'ok' } catch { if ($_.Exception.NativeErrorCode -eq 9003) { 'nx' } else { 'fail' } } # 9003 = DNS_ERROR_RCODE_NAME_ERROR (NXDOMAIN)
    $tun = (Get-NetAdapter -Name SmartTether -ErrorAction SilentlyContinue).Status
    "{0} https=[{1}] tcp1111={2} dns={3} tun={4}" -f (Get-Date -Format HH:mm:ss), $r, $tcp, $dns, $tun | Add-Content $Out
    Start-Sleep -Seconds 2
}
