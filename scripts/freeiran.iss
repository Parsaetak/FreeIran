; FreeIran Windows installer (v0.9.8.2, §17/§18).
;
; A minimal, reliable, standard Windows installer built on the mature
; Inno Setup ecosystem — no custom installer engine. The installed
; deployment is the SAME product as the portable ZIP:
;
;   - the installer writes installed.marker next to the executable;
;     the workspace then lives in the per-user application-data
;     directory (%AppData%\FreeIran) because Program Files is
;     read-only (system/workspace.go resolves this);
;   - the portable ZIP keeps portable.marker and stores everything
;     next to the executable.
;
; User data is preserved: uninstalling removes only {app} (Program
; Files files); %AppData%\FreeIran is never touched.
;
; CI passes the version explicitly (single source of truth: VERSION):
;   ISCC.exe /DAPP_VERSION=0.9.8.2 scripts/freeiran.iss

#ifndef APP_VERSION
  #define APP_VERSION "0.9.8.2"
#endif

#define MyAppName "FreeIran"
#define MyAppPublisher "SHEYTAN Digital System"
#define MyAppExeName "FreeIran.exe"

[Setup]
AppId={{7E1C0B52-6B6A-4B0F-9E2C-F1C44A2F1E4B}
AppName={#MyAppName}
AppVersion={#APP_VERSION}
AppPublisher={#MyAppPublisher}
; Uninstall keeps its registry entry consistent across updates.
DefaultDirName={autopf}\{#MyAppName}
DisableProgramGroupPage=yes
; GUI-subsystem executable: no console window ever (PE verified in CI).
OutputBaseFilename=FreeIran-Setup-v{#APP_VERSION}-windows-amd64
SetupIconFile=..\assets\freeiran-icon.ico
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
ArchitecturesInstallIn64BitMode=x64compatible
UninstallDisplayIcon={app}\{#MyAppExeName}
; Stop a running FreeIran before replacing files (§18).
CloseApplications=yes
RestartApplications=no
VersionInfoVersion={#APP_VERSION}
VersionInfoCompany={#MyAppPublisher}
VersionInfoDescription={#MyAppName} installer
VersionInfoProductName={#MyAppName}

[Tasks]
Name: "desktopicon"; Description: "{cm:CreateDesktopIcon}"; \
    GroupDescription: "{cm:AdditionalIcons}"; Flags: unchecked

[Files]
Source: "..\FreeIran-windows-amd64.exe"; DestDir: "{app}"; DestName: "{#MyAppExeName}"; \
    Flags: ignoreversion
Source: "..\README.md"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\LICENSE"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\docs\*.md"; DestDir: "{app}\docs"; Flags: ignoreversion recursesubdirs createallsubdirs

[Icons]
Name: "{autoprograms}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"
Name: "{autodesktop}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"; \
    Tasks: desktopicon

[Run]
Filename: "{app}\{#MyAppExeName}"; Description: "{cm:LaunchProgram,{#MyAppName}}"; \
    Flags: nowait postinstall skipifsilent

[Code]
// Write the installed-deployment marker after files are in place.
// system/workspace.go relocates the workspace to the per-user
// application-data directory when this file exists next to the exe.
procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep = ssPostInstall then
    SaveStringToFile(ExpandConstant('{app}\installed.marker'),
      'FreeIran installed deployment v' + '{#APP_VERSION}', False);
end;
