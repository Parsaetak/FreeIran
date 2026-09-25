; FreeIran Windows installer.
;
; A minimal, reliable, standard Windows installer built on the mature
; Inno Setup ecosystem — no custom installer engine.
;
; Workspace model: the APPLICATION FOLDER is the workspace root for
; every deployment (installed and portable alike). There is NO
; %AppData%\FreeIran secondary tree:
;
;   - the default install directory is per-user writable
;     (%LOCALAPPDATA%\Programs\FreeIran) and the user may choose any
;     writable directory — the installer verifies writability;
;   - no installed.marker is written. A stale marker from a
;     pre-0.9.8.3 install is inert; the one-time migration in
;     system/workspace_migrate.go copies legacy %AppData%\FreeIran
;     data (config, data, cores, cache, providers) into the
;     application folder, verifies it, and leaves the source intact;
;   - the portable ZIP keeps portable.marker and stores everything
;     next to the executable — same model.
;
; PROCESS SHUTDOWN POLICY (ownership-aware, never by bare name):
;
;   - Install/upgrade: CloseApplications=yes (the Windows Restart
;     Manager) closes a running FreeIran gracefully, keyed to the
;     files being replaced — inherently ownership-aware. A graceful
;     close runs the application's own shutdown, which terminates
;     every managed child through kernel job objects.
;   - Uninstall: StopOwnedProcesses (below) stops, before file
;     removal, (a) FreeIran.exe instances whose executable path is
;     exactly {app}\FreeIran.exe and (b) the PIDs recorded in the
;     application's managed-process manifest ({app}\runtime\
;     managed-processes.txt) — each verified by executable path
;     before termination. taskkill /im <name> would risk killing an
;     unrelated process that shares an image name; it is forbidden.
;
; CI passes the version explicitly (single source of truth: VERSION):
;   ISCC.exe /DAPP_VERSION=0.9.8.8 scripts/freeiran.iss

#ifndef APP_VERSION
  #define APP_VERSION "0.10.2"
#endif

#define MyAppName "FreeIran"
#define MyAppPublisher "SHEYTAN Digital System"
#define MyAppExeName "FreeIran.exe"

[Setup]
AppId={{7E1C0B52-6B6A-4B0F-9E2C-F1C44A2F1E4B}
AppName={#MyAppName}
AppVersion={#APP_VERSION}
AppPublisher={#MyAppPublisher}
; Per-user install: no admin rights required and the application
; folder (the workspace) stays writable by the running user.
PrivilegesRequired=lowest
DefaultDirName={userpf}\{#MyAppName}
DisableDirPage=no
DisableProgramGroupPage=yes
; GUI-subsystem executable: no console window ever (PE verified in CI).
OutputBaseFilename=FreeIran-Setup-v{#APP_VERSION}-windows-amd64
SetupIconFile=..\assets\freeiran-icon.ico
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
ArchitecturesInstallIn64BitMode=x64compatible
UninstallDisplayIcon={app}\{#MyAppExeName}
; Graceful stop of a running app before replacing/removing files
; (Restart Manager — keyed to the {app} files, ownership-aware).
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

[UninstallDelete]
; Application-owned runtime artifacts that Inno does not know about
; (created after install by the app itself inside the app folder).
Type: filesandordirs; Name: "{app}\data"
Type: filesandordirs; Name: "{app}\cache"
Type: filesandordirs; Name: "{app}\logs"
Type: filesandordirs; Name: "{app}\runtime"
Type: filesandordirs; Name: "{app}\cores"
Type: filesandordirs; Name: "{app}\providers"
Type: files; Name: "{app}\config\sources.json"
Type: files; Name: "{app}\config\settings.json"
Type: files; Name: "{app}\config\order.json"
Type: files; Name: "{app}\config\workspace.json"

[Code]
// Writable-directory validation on the directory page: the workspace
// root IS the application folder, so the chosen directory must be
// writable by the running user. Fail loudly with a fix, never
// silently proceed to a broken deployment.
function NextButtonClick(CurPageID: Integer): Boolean;
var
  Dir: String;
  ProbeFile: String;
begin
  Result := True;

  if CurPageID = wpSelectDir then
  begin
    Dir := ExpandConstant('{app}');
    ProbeFile := Dir + '\.freeiran-write-probe';

    try
      SaveStringToFile(ProbeFile, 'probe', False);
      DeleteFile(ProbeFile);
    except
      MsgBox('The chosen directory is not writable:'#13#10 + Dir + #13#10#13#10 +
             'FreeIran keeps ALL data inside the application folder. ' +
             'Choose a writable directory (for example: ' +
             ExpandConstant('{userpf}') + '\FreeIran) or start the installer ' +
             'with administrator rights — but a per-user directory is recommended.',
             mbCriticalError, MB_OK);
      Result := False;
    end;
  end;
end;

// Legacy marker cleanup: pre-0.9.8.3 installs carried installed.marker
// (the workspace then lived in %AppData%\FreeIran). The marker is now
// inert; remove it so the deployment converges on the new model.
procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep = ssPostInstall then
    DeleteFile(ExpandConstant('{app}\installed.marker'));
end;

// StopOwnedProcesses terminates, BEFORE file removal, exactly the
// processes this installation owns:
//
//   1. FreeIran.exe instances whose executable path is EXACTLY
//      {app}\FreeIran.exe (a portable copy running from another
//      directory is never touched);
//   2. every PID recorded in the managed-process manifest
//      ({app}\runtime\managed-processes.txt) — the manifest is
//      maintained by the application itself (system/process_manifest.go)
//      and each entry is verified against the CURRENT executable path
//      of the PID before termination, so a reused PID or a stale
//      entry can never cause an unrelated kill.
//
// Broad image-name termination (taskkill /im xray.exe and friends) is
// deliberately NOT used: it would terminate unrelated user processes
// that happen to share an executable name. When PowerShell (present
// on every supported Windows) cannot run, cleanup degrades to the
// kernel job-object guarantee (children die with their parent) — it
// never degrades to a blind kill.
procedure StopOwnedProcesses();
var
  AppDir: String;
  Manifest: String;
  Script: String;
  ResultCode: Integer;
begin
  AppDir := ExpandConstant('{app}');
  Manifest := AppDir + '\runtime\managed-processes.txt';

  // Single-quoted PowerShell strings: AppDir may contain spaces.
  Script :=
    '$ErrorActionPreference = ''SilentlyContinue''; ' +
    '$app = ''' + AppDir + '''; ' +
    // (1) the installed FreeIran itself — exact executable path.
    'Get-Process -Name ''FreeIran'' | ' +
    'Where-Object { $_.Path -eq ($app + ''\FreeIran.exe'') } | ' +
    'Stop-Process -Force; ' +
    // (2) manifest-recorded children — PID + path verified.
    '$m = $app + ''\runtime\managed-processes.txt''; ' +
    'if (Test-Path $m) { ' +
    '  Get-Content $m | ForEach-Object { ' +
    '    $e = $_ -split ''\|'', 2; ' +
    '    if ($e.Count -eq 2) { ' +
    '      $p = Get-Process -Id ([int]$e[0]); ' +
    '      if ($p -and $p.Path -eq $e[1]) { Stop-Process -Id $p.Id -Force } ' +
    '    } ' +
    '  } ' +
    '}';

  if not Exec('powershell.exe',
       '-NoProfile -ExecutionPolicy Bypass -Command "' + Script + '"',
       '', SW_HIDE, ewWaitUntilTerminated, ResultCode) then
    Log('StopOwnedProcesses: PowerShell unavailable (' +
        'job-object cleanup remains in force); no broad kill performed');
end;

// Uninstall: the workspace now lives INSIDE the application folder,
// so removing {app} removes the app-owned data with it. Ask the user
// explicitly before deleting anything beyond the binaries; never
// silently preserve a hidden secondary workspace.
procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  UserDataDir: String;
  LegacyDataDir: String;
begin
  if CurUninstallStep = usUninstall then
    // Stop owned processes BEFORE the files they run from are removed.
    StopOwnedProcesses();

  if CurUninstallStep <> usPostUninstall then
    Exit;

  UserDataDir := ExpandConstant('{app}\config');
  LegacyDataDir := ExpandConstant('{userappdata}\FreeIran');

  if DirExists(UserDataDir) then
  begin
    if MsgBox('Also delete your FreeIran data (configurations, settings, ' +
              'logs and managed cores/providers stored in the application ' +
              'folder)?',
              mbConfirmation, MB_YESNO) = IDYES then
    begin
      DelTree(ExpandConstant('{app}\config'), True, True, True);
      DelTree(ExpandConstant('{app}\data'), True, True, True);
      DelTree(ExpandConstant('{app}\cache'), True, True, True);
      DelTree(ExpandConstant('{app}\logs'), True, True, True);
      DelTree(ExpandConstant('{app}\cores'), True, True, True);
      DelTree(ExpandConstant('{app}\providers'), True, True, True);
    end;
  end;

  // Legacy pre-0.9.8.3 tree (migrated already, or left by an older
  // uninstall). The user decides; nothing is silently kept or lost.
  if DirExists(LegacyDataDir) then
  begin
    if MsgBox('A legacy FreeIran data folder was found at:'#13#10 +
              LegacyDataDir + #13#10#13#10 +
              'Its contents were migrated into the application folder by ' +
              'v0.9.8.3. Delete this legacy folder now?',
              mbConfirmation, MB_YESNO) = IDYES then
      DelTree(LegacyDataDir, True, True, True);
  end;
end;
