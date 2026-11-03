# Windows Mobile Hotspot as the phone link (a Wi-Fi Direct legacy AP is not used: some phones'
# Wi-Fi firmware is unstable with it). The phone app only uses this network for the
# local link (never for internet).
# The shared ("public") connection is our own TUN adapter when -AdapterId is given: sharing whatever
# Windows calls the internet profile made the hotspot restart (and drop the phone) every time that
# profile changed, e.g. when another tethering adapter vanished on cable unplug.
# -Action start|stop|status|keep. "keep" restarts the hotspot whenever it turns off (Windows turns it
# off when no client is connected for a while) until -ParentPid exits. While the service's full routes are
# on our TUN, "keep" also takes the PC's Wi-Fi off any network it joins: a single-radio adapter
# and a joined network pull the hotspot onto that network's channel (often 2.4 GHz).
param(
    [ValidateSet('start', 'stop', 'status', 'keep')][string]$Action = 'status',
    [int]$ParentPid = 0,
    [string]$AdapterId = "",
    [string]$Ssid = "SmartTether",
    [string]$Passphrase = "",
    [ValidateSet('Auto', 'TwoPointFourGigahertz', 'FiveGigahertz')][string]$Band = 'FiveGigahertz'
)
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Runtime.WindowsRuntime
$null = [Windows.Networking.Connectivity.NetworkInformation, Windows.Networking.Connectivity, ContentType = WindowsRuntime]
$null = [Windows.Networking.NetworkOperators.NetworkOperatorTetheringManager, Windows.Networking.NetworkOperators, ContentType = WindowsRuntime]

$asTask = ([System.WindowsRuntimeSystemExtensions].GetMethods() | Where-Object {
        $_.Name -eq 'AsTask' -and $_.GetParameters().Count -eq 1 -and $_.GetParameters()[0].ParameterType.Name -eq 'IAsyncOperation`1' })[0]
function Await($op, [Type]$t) {
    $task = $asTask.MakeGenericMethod($t).Invoke($null, @($op))
    $task.Wait() | Out-Null
    $task.Result
}
function AwaitAction($op) {
    $m = [System.WindowsRuntimeSystemExtensions].GetMethod('AsTask', [Type[]]@([Windows.Foundation.IAsyncAction]))
    $m.Invoke($null, @($op)).Wait() | Out-Null
}

function Get-Manager {
    $profile = $null
    if ($AdapterId) {
        $profile = [Windows.Networking.Connectivity.NetworkInformation]::GetConnectionProfiles() |
            Where-Object { $_.NetworkAdapter -and $_.NetworkAdapter.NetworkAdapterId.ToString() -eq $AdapterId } |
            Select-Object -First 1
    }
    if (-not $profile) { $profile = [Windows.Networking.Connectivity.NetworkInformation]::GetInternetConnectionProfile() }
    if (-not $profile) { throw "no connection profile to share" }
    [Windows.Networking.NetworkOperators.NetworkOperatorTetheringManager]::CreateFromConnectionProfile($profile)
}

# The SSID the PC's own Wi-Fi (station) is joined to, or "". On a single-radio adapter, while it is
# joined the hotspot runs on that network's channel whatever -Band says.
function Get-Station {
    foreach ($p in [Windows.Networking.Connectivity.NetworkInformation]::GetConnectionProfiles()) {
        if (-not $p.IsWlanConnectionProfile) { continue }
        $s = $p.WlanConnectionProfileDetails.GetConnectedSsid()
        if ($s) { return [pscustomobject]@{ Ssid = $s; AdapterId = $p.NetworkAdapter.NetworkAdapterId.ToString() } }
    }
}

function Get-AdapterByGuid($id) { Get-NetAdapter -IncludeHidden | Where-Object { $_.InterfaceGuid -eq "{$id}" } }

# The service (-routes full) has its routes on our TUN (they are withdrawn while the phone is gone, and then
# the Wi-Fi may carry a fallback network).
function Test-FullRoutes {
    if (-not $AdapterId) { return $false }
    $tun = Get-AdapterByGuid $AdapterId
    if (-not $tun) { return $false }
    [bool](Get-NetRoute -InterfaceIndex $tun.ifIndex -DestinationPrefix '0.0.0.0/1' -ErrorAction SilentlyContinue)
}

function Start-Hotspot($mgr) {
    $sta = Get-Station
    if ($sta) { "$(Get-Date -Format o) Wi-Fi is joined to '$($sta.Ssid)': the hotspot will run on its channel, not $Band" }
    $script:onStationChannel = [bool]$sta
    $cfg = $mgr.GetCurrentAccessPointConfiguration()
    $cfg.Ssid = $Ssid
    $cfg.Passphrase = $Passphrase
    try { $cfg.Band = [Windows.Networking.NetworkOperators.TetheringWiFiBand]::$Band } catch { "band not settable: $_" }
    AwaitAction ($mgr.ConfigureAccessPointAsync($cfg))
    $r = Await ($mgr.StartTetheringAsync()) ([Windows.Networking.NetworkOperators.NetworkOperatorTetheringOperationResult])
    "$(Get-Date -Format o) start: $($r.Status) $($r.AdditionalErrorMessage)"
}

if ($Action -eq 'keep') {
    $lastSta = ""
    # True while the hotspot runs on a channel the station picked; it is restarted once the station is gone.
    $script:onStationChannel = $false
    while (-not $ParentPid -or (Get-Process -Id $ParentPid -ErrorAction SilentlyContinue)) {
        try {
            $sta = Get-Station
            if ("$($sta.Ssid)" -ne $lastSta) { "$(Get-Date -Format o) Wi-Fi station: '$lastSta' -> '$($sta.Ssid)'"; $lastSta = "$($sta.Ssid)" }
            $mgr = Get-Manager
            $state = $mgr.TetheringOperationalState
            if ($sta -and (Test-FullRoutes)) {
                "$(Get-Date -Format o) full routes are up: disconnecting Wi-Fi from '$($sta.Ssid)' to keep the hotspot on $Band"
                $ifName = (Get-AdapterByGuid $sta.AdapterId).Name
                netsh wlan disconnect "interface=$ifName" | Out-String
                if ($state -ne 'Off') { $script:onStationChannel = $true }
                Start-Sleep -Seconds 2
                continue
            }
            if (-not $sta -and $script:onStationChannel -and $state -eq 'On') {
                $r = Await ($mgr.StopTetheringAsync()) ([Windows.Networking.NetworkOperators.NetworkOperatorTetheringOperationResult])
                "$(Get-Date -Format o) Wi-Fi left the network the hotspot followed: restarting it for $Band ($($r.Status))"
                $script:onStationChannel = $false
                Start-Sleep -Seconds 2
                continue
            }
            if ($state -ne 'On') {
                "$(Get-Date -Format o) hotspot is $state, starting"
                Start-Hotspot $mgr
            }
        } catch { "$(Get-Date -Format o) keep: $_" }
        Start-Sleep -Seconds 5
    }
    try { Await ((Get-Manager).StopTetheringAsync()) ([Windows.Networking.NetworkOperators.NetworkOperatorTetheringOperationResult]) | Out-Null } catch {}
    return
}

$mgr = Get-Manager
switch ($Action) {
    'start' { Start-Hotspot $mgr }
    'stop' {
        $r = Await ($mgr.StopTetheringAsync()) ([Windows.Networking.NetworkOperators.NetworkOperatorTetheringOperationResult])
        "stop: $($r.Status)"
    }
    'status' {
        "state=$($mgr.TetheringOperationalState) clients=$($mgr.ClientCount) band=$($mgr.GetCurrentAccessPointConfiguration().Band) station='$((Get-Station).Ssid)' fullroutes=$(Test-FullRoutes)"
        [Windows.Networking.Connectivity.NetworkInformation]::GetConnectionProfiles() | ForEach-Object {
            "profile '$($_.ProfileName)' adapter=$($_.NetworkAdapter.NetworkAdapterId) level=$($_.GetNetworkConnectivityLevel())"
        }
    }
}
