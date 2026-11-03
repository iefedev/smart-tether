; Smart Tether installer (Inno Setup 6). Built by tools\build-release.ps1, which stages the files
; in build\release\stage and passes /DAppVersion=x.y.z.
;
; Layout under {app}:
;   SmartTether.exe            tray app and window (self-contained .NET), started at every logon
;   bin\SmartTetherService.exe the "SmartTether" Windows service (tunnel, routes, hotspot)
;   bin\wintun.dll             TUN driver the service loads
;   bin\adb.exe + DLLs         adb server the tray keeps running, for USB
;   tools\mobile-hotspot.ps1   keeps the Mobile Hotspot the phone joins over Wi-Fi
; Keys, settings and the service log live in %ProgramData%\Smart Tether.

#ifndef AppVersion
  #define AppVersion "0.2.0"
#endif
#define Stage "..\build\release\stage"

[Setup]
AppId={{2356A225-4599-4A7B-8290-81CB08E1F1BC}
AppName=Smart Tether
AppVersion={#AppVersion}
AppVerName=Smart Tether {#AppVersion}
AppPublisher=iefedev
AppCopyright=Copyright (c) 2026 iefedev (MIT License)
VersionInfoVersion={#AppVersion}
DefaultDirName={autopf}\Smart Tether
DefaultGroupName=Smart Tether
DisableProgramGroupPage=yes
DisableDirPage=auto
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0.17763
OutputDir=..\build\release
OutputBaseFilename=SmartTether-Setup
SetupIconFile=..\windows\SmartTether\SmartTether.ico
UninstallDisplayIcon={app}\SmartTether.exe
UninstallDisplayName=Smart Tether
WizardStyle=modern
Compression=lzma2/max
SolidCompression=yes
; The tray app and the service are stopped in PrepareToInstall; no Restart Manager prompt.
CloseApplications=no

[Languages]
Name: "en"; MessagesFile: "compiler:Default.isl"

[Files]
Source: "{#Stage}\SmartTether.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#Stage}\bin\*"; DestDir: "{app}\bin"; Flags: ignoreversion
Source: "{#Stage}\tools\mobile-hotspot.ps1"; DestDir: "{app}\tools"; Flags: ignoreversion
Source: "{#Stage}\licenses\*"; DestDir: "{app}\licenses"; Flags: ignoreversion

[Icons]
Name: "{autoprograms}\Smart Tether"; Filename: "{app}\SmartTether.exe"

[Registry]
; The tray app starts hidden (icon only) at every user's logon.
Root: HKLM; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; ValueName: "SmartTether"; \
    ValueData: """{app}\SmartTether.exe"" --hidden"; Flags: uninsdeletevalue

[Run]
Filename: "{app}\bin\SmartTetherService.exe"; Parameters: "install"; StatusMsg: "Starting the Smart Tether service..."; \
    Flags: runhidden waituntilterminated
Filename: "{app}\SmartTether.exe"; Description: "Open Smart Tether"; Flags: postinstall nowait skipifsilent runasoriginaluser
Filename: "{app}\SmartTether.exe"; Parameters: "--hidden"; Flags: nowait runasoriginaluser skipifnotsilent

[UninstallRun]
Filename: "{sys}\taskkill.exe"; Parameters: "/f /im SmartTether.exe"; Flags: runhidden; RunOnceId: "KillTray"
Filename: "{app}\bin\SmartTetherService.exe"; Parameters: "uninstall"; Flags: runhidden waituntilterminated; RunOnceId: "RemoveService"
Filename: "{app}\bin\adb.exe"; Parameters: "kill-server"; Flags: runhidden waituntilterminated; RunOnceId: "StopAdb"

[UninstallDelete]
; Keys, settings and logs. Removing them means a reinstall needs the phone paired again.
Type: filesandordirs; Name: "{commonappdata}\Smart Tether"

[Code]
procedure RunHidden(const FileName, Params: String);
var
  Code: Integer;
begin
  Exec(FileName, Params, '', SW_HIDE, ewWaitUntilTerminated, Code);
end;

// An update replaces files the running tray, service and adb server hold open.
function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  RunHidden(ExpandConstant('{sys}\taskkill.exe'), '/f /im SmartTether.exe');
  RunHidden(ExpandConstant('{sys}\sc.exe'), 'stop SmartTether');
  if FileExists(ExpandConstant('{app}\bin\adb.exe')) then
    RunHidden(ExpandConstant('{app}\bin\adb.exe'), 'kill-server');
  // The service needs a moment to take its routes and the hotspot down.
  Sleep(3000);
  Result := '';
end;
