# Control the installed SmartTether service without UAC (the installer lets interactive users
# start and stop it).
#   tools\dev\service.ps1 status|start|stop|restart
# To run a new build, rebuild the installer (tools\build-release.ps1) and run it.
param([ValidateSet('status', 'start', 'stop', 'restart')][string]$Action = 'status')
$name = 'SmartTether'
function Wait-State($state) {
    for ($i = 0; $i -lt 30; $i++) {
        if ((Get-Service $name).Status -eq $state) { return }
        Start-Sleep -Milliseconds 500
    }
}
switch ($Action) {
    'status' { Get-Service $name | Select-Object Name, Status, StartType }
    'start' { sc.exe start $name | Out-Null; Wait-State Running }
    'stop' { sc.exe stop $name | Out-Null; Wait-State Stopped }
    'restart' { sc.exe stop $name | Out-Null; Wait-State Stopped; sc.exe start $name | Out-Null; Wait-State Running }
}
if ($Action -ne 'status') { Get-Service $name | Select-Object Name, Status }
