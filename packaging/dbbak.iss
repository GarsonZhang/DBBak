; DBBak 图形安装器（Inno Setup 7）
;
; 这个脚本只负责“装成常规 Windows 程序 + 服务”的交互外壳：
; 选择安装目录、选择数据目录、设置监听地址与初始管理员密码，然后调用
; dbbak.exe setup 完成注册服务、初始化管理员与就绪等待。
;
; 构建：packaging/build-installer.ps1 会先编译 dbbak.exe，再用 ISCC 编译本脚本。

#define AppName "DBBak"
#ifndef AppVersion
  #define AppVersion "0.2.0-phase2"
#endif
#ifndef AppExe
  #define AppExe "..\dist\" + AppVersion + "\windows_amd64\dbbak.exe"
#endif
#ifndef AppDir
  #define AppDir "..\dist\" + AppVersion + "\app"
#endif
#ifndef VersionQuad
  #define VersionQuad "0.2.0.0"
#endif
; 仅用于界面预览：正式构建固定为 admin。
#ifndef PrivilegesLevel
  #define PrivilegesLevel "admin"
#endif

[Setup]
; AppId 固定不变，升级/卸载才能识别成同一个产品。
AppId={{A2E4F1C8-7B3D-4E62-9C15-3D8A6F0B7E21}
AppName={#AppName}
AppVersion={#AppVersion}
AppVerName={#AppName} {#AppVersion}
AppPublisher=DBBak
VersionInfoVersion={#VersionQuad}
VersionInfoProductName={#AppName}
DefaultDirName={autopf}\DBBak
DefaultGroupName={#AppName}
DisableWelcomePage=no
DisableDirPage=no
DisableProgramGroupPage=yes
AllowNoIcons=yes
PrivilegesRequired={#PrivilegesLevel}
OutputDir=..\dist\{#AppVersion}
OutputBaseFilename=DBBak-Setup-{#AppVersion}-windows-amd64
Compression=lzma2/max
SolidCompression=yes
WizardStyle=modern
SetupLogging=yes
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
UninstallDisplayName={#AppName} 备份服务
UninstallDisplayIcon={app}\dbbak.exe

[Languages]
Name: "chinesesimp"; MessagesFile: "compiler:Languages\ChineseSimplified.isl"

[Files]
; AppDir 由构建脚本传入“程序目录暂存区”，整体安装：dbbak.exe + tools\* + backup.ps1。
; 自带 tools 是“装完即用”的关键：客户端工具在安装目录内，服务不需要外部 PATH。
Source: "{#AppDir}\*"; DestDir: "{app}"; Flags: ignoreversion recursesubdirs createallsubdirs

[Icons]
Name: "{autoprograms}\DBBak 控制台"; Filename: "http://{code:GetAddr}/"; Comment: "打开 DBBak 控制台"

[Registry]
Root: HKLM; Subkey: "Software\DBBak"; ValueType: string; ValueName: "DataDir"; ValueData: "{code:GetDataDir}"; Flags: uninsdeletekey
Root: HKLM; Subkey: "Software\DBBak"; ValueType: string; ValueName: "Addr"; ValueData: "{code:GetAddr}"
Root: HKLM; Subkey: "Software\DBBak"; ValueType: string; ValueName: "AdminUser"; ValueData: "{code:GetAdminUser}"

[Run]
Filename: "http://{code:GetAddr}/"; Description: "打开 DBBak 控制台"; Flags: postinstall shellexec nowait skipifsilent

[UninstallRun]
Filename: "{app}\dbbak.exe"; Parameters: "uninstall --yes --prefix ""{app}"" --data-dir ""{code:GetStoredDataDir}"""; Flags: runhidden waituntilterminated; RunOnceId: "StopAndRemoveService"

[Code]

var
  DataPage: TInputDirWizardPage;
  AccountPage: TInputQueryWizardPage;
  StoredDataDir: String;
  ExistingInstall: Boolean;
  NeedStopService: Boolean;

function GetDataDir(Param: String): String;
begin
  Result := DataPage.Values[0];
end;

function GetAddr(Param: String): String;
begin
  Result := AccountPage.Values[0];
end;

function GetAdminUser(Param: String): String;
begin
  Result := AccountPage.Values[1];
end;

function GetStoredDataDir(Param: String): String;
begin
  Result := StoredDataDir;
end;

function ServiceInstalled(): Boolean;
begin
  Result := RegValueExists(HKLM, 'SYSTEM\CurrentControlSet\Services\DBBak', 'ImagePath');
end;

function RunAndWait(const FileName, Params: String): Integer;
var
  ResultCode: Integer;
begin
  if not Exec(FileName, Params, '', SW_HIDE, ewWaitUntilTerminated, ResultCode) then
    ResultCode := -1;
  Result := ResultCode;
end;

// 从注册表读出已安装服务的程序目录。
//
// 不能直接用 {app}：用户重装时可能把安装目录改到别处，这时旧服务仍在原目录运行，
// 只有 ImagePath 才是它真正的落脚点。ImagePath 可能带引号、也常带启动参数，需要剥掉。
// 路径本身可能含空格，所以未加引号时按 ".exe" 截断，而不是按第一个空格切。
function GetServiceDir(): String;
var
  ImagePath, S: String;
  I, P: Integer;
begin
  Result := '';
  if not RegQueryStringValue(HKLM, 'SYSTEM\CurrentControlSet\Services\DBBak', 'ImagePath', ImagePath) then
    Exit;
  S := Trim(ImagePath);
  if (Length(S) > 0) and (S[1] = '"') then
  begin
    Delete(S, 1, 1);
    I := Pos('"', S);
    if I > 0 then
      S := Copy(S, 1, I - 1);
  end
  else
  begin
    P := Pos('.exe', Lowercase(S));
    if P > 0 then
      S := Copy(S, 1, P + 3);
  end;
  if S <> '' then
    Result := ExtractFileDir(S);
end;

// 已登记的数据目录：安装时写进 HKLM\Software\DBBak\DataDir，重装要沿用它。
function GetRegisteredDataDir(): String;
begin
  Result := '';
  if not RegQueryStringValue(HKLM, 'Software\DBBak', 'DataDir', Result) then
    Result := '';
end;

// 重新安装前先停服并做停服快照。
//
// 文件替换发生在 [Files] 阶段，早于安装收尾；服务正在运行会占用 dbbak.exe，
// 直接覆盖会失败。但停服又不能放在 InitializeSetup：那时 {app} 常量尚未初始化，
// 展开会抛 "expand the app constant before it was initialized"。
// 所以实际动作放在 CurStepChanged(ssInstall)：常量已就绪，文件还没复制。
//
// 停服用 net stop，不依赖快照脚本的版本；net stop 会等到服务真正进入 Stopped 才返回。
// 快照失败只提示、不阻断：用户可能是来修复损坏的程序目录的。
function StopServiceForUpgrade(): Boolean;
var
  Code: Integer;
  ServiceDir, BackupScript, MaintRoot, DataDir, Args, StopText: String;
begin
  Result := True;
  if not ServiceInstalled() then
    Exit;

  ServiceDir := GetServiceDir();
  if ServiceDir = '' then
    ServiceDir := ExpandConstant('{app}');
  BackupScript := ServiceDir + '\backup.ps1';

  MaintRoot := ExpandConstant('{commonappdata}\DBBak\maintenance');
  DataDir := GetRegisteredDataDir();
  if DataDir <> '' then
    MaintRoot := DataDir + '\maintenance';
  ForceDirectories(MaintRoot);

  // 1) 停服。net stop 退出码 2 = 服务本来就没在运行，视为成功。
  Code := RunAndWait(ExpandConstant('{sys}\net.exe'), 'stop DBBak');
  StopText := '';
  if (Code <> 0) and (Code <> 2) then
    StopText := '停止 DBBak 服务返回退出码 ' + IntToStr(Code) + '。' + #13#10;

  // 2) 快照。只传 -Yes：服务已经在上面停住，旧版脚本（没有 -StopService/-DataRoot）
  //    也能直接用；新版脚本的数据目录会自动取注册表里登记的值，无需额外参数。
  if not FileExists(BackupScript) then
  begin
    if StopText <> '' then
      MsgBox(StopText + #13#10 +
             '若覆盖程序文件时报“文件正在使用”，请手动停止 DBBak 服务后重新运行安装包。',
             mbInformation, MB_OK);
    Exit;
  end;

  Args := '-NoProfile -ExecutionPolicy Bypass -File "' + BackupScript + '" -Yes';

  Code := RunAndWait(ExpandConstant('{sys}\WindowsPowerShell\v1.0\powershell.exe'), Args);
  // 快照成功必然意味着服务已停住（脚本要求 Stopped 才复制），此时不必再提停服的旧报错。
  if Code = 0 then
    StopText := '';
  if (Code <> 0) or (StopText <> '') then
  begin
    if StopText = '' then
      StopText := '停服快照返回退出码 ' + IntToStr(Code) + '（快照目录：' + MaintRoot + '）。' + #13#10;
    if MsgBox(StopText + #13#10 +
              '可以继续安装：服务会被重新注册，数据目录不变。' + #13#10 +
              '如果覆盖程序文件时报“文件正在使用”，请手动停止 DBBak 服务后重新运行安装包。',
              mbConfirmation, MB_OKCANCEL) = IDCANCEL then
      Result := False;
  end;
end;

function InitializeSetup(): Boolean;
begin
  Result := True;
  NeedStopService := ServiceInstalled();
  if not NeedStopService then
    Exit;

  // 静默安装没有交互对象，对话框会直接返回默认按钮（此处默认是“取消”），
  // 所以在弹框前就分流：静默安装一律按“继续”处理，避免 /VERYSILENT 批量升级被挡死。
  if WizardSilent() then
    Exit;

  // MB_OKCANCEL 返回值是 IDOK / IDCANCEL，绝不能用 IDYES 判断（那样点「确定」会当成取消，
  // 安装器静默退出，日志里只留 "InitializeSetup returned False"）。
  // 默认焦点放在「取消」，误按回车不会直接把正在运行的服务停掉。
  if MsgBox('检测到系统里已经安装 DBBak 服务。' + #13#10 + #13#10 +
            '继续将：停止服务 → 备份元数据与主密钥 → 覆盖程序文件与自带工具 → 重新注册并启动服务。' + #13#10 +
            '数据目录、已有任务与备份文件保持不变，管理员账号与密码不会重置。' + #13#10 + #13#10 +
            '继续安装？', mbConfirmation, MB_OKCANCEL or MB_DEFBUTTON2) = IDCANCEL then
  begin
    MsgBox('已取消安装，未做任何更改，DBBak 服务仍在运行。', mbInformation, MB_OK);
    Result := False;
  end;
end;

procedure InitializeWizard();
var
  StoredDir, StoredAddr, StoredUser: String;
begin
  ExistingInstall := ServiceInstalled();

  DataPage := CreateInputDirPage(wpSelectDir,
    '选择数据目录',
    '元数据库、主密钥、备份文件都会保存在这个目录。',
    '数据目录必须与程序目录分开：升级会覆盖程序目录，数据不能跟着被覆盖。',
    False, SetupMessage(msgNewFolderName));
  DataPage.Add('数据目录：');
  DataPage.Values[0] := ExpandConstant('{commonappdata}\DBBak');
  // 重装时沿用已登记的数据目录与监听地址，避免用户误填导致“看起来数据没了”。
  if ExistingInstall then
  begin
    if RegQueryStringValue(HKLM, 'Software\DBBak', 'DataDir', StoredDir) and (StoredDir <> '') then
      DataPage.Values[0] := StoredDir;
  end;

  AccountPage := CreateInputQueryPage(DataPage.ID,
    '管理员与监听设置',
    '创建用于登录控制台的第一个管理员账号，并指定服务监听地址。',
    '监听地址默认只绑定回环地址，仅本机可访问；首次安装密码至少 6 个字符。重装时留空密码表示保留原密码。');
  AccountPage.Add('监听地址：', False);
  AccountPage.Add('管理员用户名：', False);
  AccountPage.Add('初始密码：', True);
  AccountPage.Add('确认密码：', True);
  AccountPage.Values[0] := '127.0.0.1:8080';
  AccountPage.Values[1] := 'admin';
  if ExistingInstall then
  begin
    if RegQueryStringValue(HKLM, 'Software\DBBak', 'Addr', StoredAddr) and (StoredAddr <> '') then
      AccountPage.Values[0] := StoredAddr;
    if RegQueryStringValue(HKLM, 'Software\DBBak', 'AdminUser', StoredUser) and (StoredUser <> '') then
      AccountPage.Values[1] := StoredUser;
  end;
end;

function IsValidAddr(const Value: String): Boolean;
var
  Port: Integer;
  ColonPos: Integer;
  PortText: String;
begin
  Result := False;
  ColonPos := Pos(':', Value);
  if (ColonPos <= 1) or (ColonPos = Length(Value)) then
    Exit;
  PortText := Copy(Value, ColonPos + 1, Length(Value) - ColonPos);
  Port := StrToIntDef(PortText, -1);
  Result := (Port > 0) and (Port < 65536);
end;

function IsValidUser(const Value: String): Boolean;
var
  i: Integer;
  Ch: Char;
begin
  Result := (Length(Value) > 0) and (Length(Value) <= 64);
  if not Result then
    Exit;
  for i := 1 to Length(Value) do
  begin
    Ch := Value[i];
    if not (((Ch >= 'a') and (Ch <= 'z')) or ((Ch >= 'A') and (Ch <= 'Z'))
      or ((Ch >= '0') and (Ch <= '9')) or (Ch = '_') or (Ch = '-') or (Ch = '.')) then
    begin
      Result := False;
      Exit;
    end;
  end;
end;

function NextButtonClick(CurPageID: Integer): Boolean;
var
  Password, Confirm, Addr, User: String;
begin
  Result := True;

  if CurPageID = DataPage.ID then
  begin
    if CompareText(RemoveBackslashUnlessRoot(DataPage.Values[0]),
                   RemoveBackslashUnlessRoot(WizardDirValue)) = 0 then
    begin
      MsgBox('数据目录不能与程序目录相同，请换一个目录。', mbError, MB_OK);
      Result := False;
    end;
    Exit;
  end;

  if CurPageID = AccountPage.ID then
  begin
    Addr := Trim(AccountPage.Values[0]);
    User := Trim(AccountPage.Values[1]);
    Password := AccountPage.Values[2];
    Confirm := AccountPage.Values[3];

    if not IsValidAddr(Addr) then
    begin
      MsgBox('监听地址要写成 host:port，例如 127.0.0.1:8080（端口 1-65535）。', mbError, MB_OK);
      Result := False;
      Exit;
    end;
    if not IsValidUser(User) then
    begin
      MsgBox('管理员用户名只能包含字母、数字、下划线、短横线和点，长度 1-64。', mbError, MB_OK);
      Result := False;
      Exit;
    end;
    // 首次安装必须设初始密码；重装沿用已有管理员，密码留空表示不重置。
    if (not ExistingInstall) and (Length(Password) < 6) then
    begin
      MsgBox('初始密码至少 6 个字符。', mbError, MB_OK);
      Result := False;
      Exit;
    end;
    if Password <> Confirm then
    begin
      MsgBox('两次输入的密码不一致。', mbError, MB_OK);
      Result := False;
      Exit;
    end;
  end;
end;

function ReadLogTail(const Path: String; MaxLines: Integer): String;
var
  Lines: TArrayOfString;
  I, Start: Integer;
begin
  Result := '';
  if not LoadStringsFromFile(Path, Lines) then
  begin
    Result := '（没有读到安装日志：' + Path + '）';
    Exit;
  end;
  Start := GetArrayLength(Lines) - MaxLines;
  if Start < 0 then
    Start := 0;
  for I := Start to GetArrayLength(Lines) - 1 do
    Result := Result + Lines[I] + #13#10;
  // 空日志不是"没问题"，而是安装程序在写日志之前就退出了：
  // 这类失败只能靠事件的先后顺序判断，必须明确说出来，否则用户以为日志坏了。
  if Trim(Result) = '' then
    Result := '（日志为空：安装程序在写出任何信息之前就退出了）';
end;

// 安装器退出时 {tmp} 会被整体删除，只把退出码丢给用户等于让他无从下手。
// 失败时把日志复制到数据目录，客户可以直接把这个文件发回来。
function PersistInstallLog(const Source: String): String;
var
  Dir: String;
begin
  Result := '';
  Dir := DataPage.Values[0] + '\logs';
  if not ForceDirectories(Dir) then
    Exit;
  Result := Dir + '\setup-error.log';
  if not FileCopy(Source, Result, False) then
    Result := '';
end;

// 安装收尾：把服务注册、管理员初始化、就绪等待交给 dbbak.exe setup。
// 失败时把日志尾部显示出来并中止安装，而不是留下一个装了一半、没有服务的目录。
// 已存在服务时也可以运行：用于修复或补装自带工具（重装保留原有账号与密码）。
procedure RegisterService();
var
  ResultCode: Integer;
  Params, PasswordArg, PasswordFile, LogPath, Msg, PersistedLog: String;
begin
  PasswordFile := ExpandConstant('{tmp}\dbbak-admin.pass');
  LogPath := ExpandConstant('{tmp}\dbbak-install.log');
  DeleteFile(PasswordFile);

  // 首次安装必须设置初始密码（向导已校验）；重装时密码留空表示不重置管理员。
  PasswordArg := '';
  if AccountPage.Values[2] <> '' then
  begin
    SaveStringToFile(PasswordFile, AccountPage.Values[2], False);
    PasswordArg := ' --admin-password-file "' + PasswordFile + '"';
  end;

  // 不传 --no-uninstall-key：已存在时 setup 只刷新不覆盖，首次安装时写入卸载登记。
  Params := 'setup --yes --no-browser'
    + ' --prefix "' + ExpandConstant('{app}') + '"'
    + ' --data-dir "' + DataPage.Values[0] + '"'
    + ' --addr "' + Trim(AccountPage.Values[0]) + '"'
    + ' --admin-user "' + Trim(AccountPage.Values[1]) + '"'
    + PasswordArg
    + ' --log-file "' + LogPath + '"';

  if not Exec(ExpandConstant('{app}\dbbak.exe'), Params, '', SW_HIDE,
              ewWaitUntilTerminated, ResultCode) then
  begin
    MsgBox('无法启动服务安装程序：' + ExpandConstant('{app}\dbbak.exe'), mbError, MB_OK);
    Abort;
  end;

  DeleteFile(PasswordFile);

  if ResultCode <> 0 then
  begin
    PersistedLog := PersistInstallLog(LogPath);
    Msg := '服务注册或启动失败（退出码 ' + IntToStr(ResultCode) + '）。' + #13#10 + #13#10;
    if PersistedLog <> '' then
      Msg := Msg + '安装日志已保存到：' + #13#10 + PersistedLog + #13#10 + #13#10;
    Msg := Msg + '最近的安装日志：' + #13#10 + ReadLogTail(LogPath, 40);
    MsgBox(Msg, mbError, MB_OK);
    Abort;
  end;
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep = ssInstall then
  begin
    if NeedStopService and (not StopServiceForUpgrade()) then
      Abort;
  end
  else if CurStep = ssPostInstall then
    RegisterService();
end;

function InitializeUninstall(): Boolean;
begin
  Result := True;
  if not RegQueryStringValue(HKLM, 'Software\DBBak', 'DataDir', StoredDataDir) then
    StoredDataDir := ExpandConstant('{commonappdata}\DBBak');
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usPostUninstall then
    MsgBox('DBBak 服务与程序已移除。' + #13#10 + #13#10 +
           '数据目录仍然保留：' + #13#10 + StoredDataDir + #13#10 + #13#10 +
           '其中包含元数据库、主密钥与备份文件；确认不再需要后可手动删除。',
           mbInformation, MB_OK);
end;
