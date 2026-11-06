#Requires -Version 5.1
<#
.SYNOPSIS
    DBBak 本地开发一键启动：自检、自愈、构建、启动、就绪检查、打开页面。

.DESCRIPTION
    依次处理：
      1. 端口已被本服务占用时直接打开页面；-Restart 则先停旧进程再启动。
      2. 数据目录缺失时从 .test-runtime/vault-data 复制迁移（原目录保留）；两者都无则引导初始化管理员。
      3. 二进制缺失或源码比二进制新时，用 .tools 便携工具链构建（默认走本地模块缓存）。
      4. 启动后轮询 /health/ready，通过才认为启动成功，随后打开浏览器。

.PARAMETER Port
    监听端口，默认 38080。

.PARAMETER Build
    无条件重新构建。

.PARAMETER Restart
    端口被本服务占用时，先停止旧进程再启动。

.PARAMETER NoBrowser
    不自动打开浏览器。

.PARAMETER InitPassword
    首次初始化时使用的管理员密码（至少 6 个字符）。不传则交互输入。

.EXAMPLE
    .\scripts\start-dev.ps1

.EXAMPLE
    .\scripts\start-dev.ps1 -Restart -Port 38081
#>
[CmdletBinding()]
param(
    [int]$Port = 38080,
    [switch]$Build,
    [switch]$Restart,
    [switch]$NoBrowser,
    [string]$InitPassword
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$Root = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..')).Path
. (Join-Path $PSScriptRoot 'dev-env.ps1')

$Runtime      = Join-Path $Root '.test-runtime'
$DataDir      = Join-Path $Runtime 'dbbak-data'
$BackupRoot   = Join-Path $Runtime 'backup-root'
$Binary       = Join-Path $Runtime 'dbbak.exe'
$GoExe        = Join-Path $Root '.tools\go\bin\go.exe'
$PasswordFile = Join-Path $Runtime 'admin-password'
$BaseUrl      = "http://127.0.0.1:$Port"

function Write-Step {
    param([string]$Message)
    Write-Host "[dbbak] $Message" -ForegroundColor Cyan
}

function Write-Note {
    param([string]$Message)
    Write-Host "        $Message" -ForegroundColor DarkGray
}

function Get-PortOwner {
    $listeners = @(Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue)
    if ($listeners.Count -eq 0) { return $null }
    return $listeners[0].OwningProcess
}

function Open-Console {
    $url = "$BaseUrl/"
    if ($NoBrowser) {
        Write-Note "已跳过自动打开浏览器，请手动访问 $url"
        return
    }
    try {
        # 不用 Start-Process：当前会话同时存在 HTTPS_PROXY 与 https_proxy 时，
        # PowerShell 5.1 构造环境变量字典会抛 ArgumentException。
        [System.Diagnostics.Process]::Start($url) | Out-Null
        Write-Note "已打开 $url"
    } catch {
        Write-Note "打开浏览器失败，请手动访问 $url"
    }
}

function Test-NeedsBuild {
    if ($Build) { return $true }
    if (-not (Test-Path -LiteralPath $Binary)) { return $true }

    $exeTime = (Get-Item -LiteralPath $Binary).LastWriteTimeUtc
    $watched = @()

    foreach ($dir in @('cmd', 'internal', 'web')) {
        $path = Join-Path $Root $dir
        if (Test-Path -LiteralPath $path) {
            $watched += @(
                Get-ChildItem -LiteralPath $path -Recurse -File -ErrorAction SilentlyContinue |
                    Where-Object { $_.Extension -in '.go', '.html', '.css', '.js' -and $_.Name -notlike '*_test.go' }
            )
        }
    }
    foreach ($name in @('embed.go', 'go.mod', 'go.sum')) {
        $file = Join-Path $Root $name
        if (Test-Path -LiteralPath $file) { $watched += @(Get-Item -LiteralPath $file) }
    }

    foreach ($file in $watched) {
        if ($file.LastWriteTimeUtc -gt $exeTime) { return $true }
    }
    return $false
}

function Initialize-DataDirectory {
    if (Test-Path -LiteralPath (Join-Path $DataDir 'dbbak.db')) { return }

    $legacy = Join-Path $Runtime 'vault-data'
    $legacyDb = Join-Path $legacy 'vault.db'
    if (Test-Path -LiteralPath $legacyDb) {
        Write-Step '检测到旧 Vault 运行数据，复制迁移到 dbbak-data（原目录保留不动）'
        New-Item -ItemType Directory -Force -Path $DataDir | Out-Null
        # master.key 必须与数据库同源，否则已加密的数据源凭据无法解密。
        Copy-Item -LiteralPath (Join-Path $legacy 'master.key') -Destination $DataDir
        foreach ($suffix in @('', '-wal', '-shm')) {
            $source = Join-Path $legacy ("vault.db$suffix")
            if (Test-Path -LiteralPath $source) {
                Copy-Item -LiteralPath $source -Destination (Join-Path $DataDir "dbbak.db$suffix")
            }
        }
        $legacyBackups = Join-Path $legacy 'backups'
        if (Test-Path -LiteralPath $legacyBackups) {
            Copy-Item -LiteralPath $legacyBackups -Destination $DataDir -Recurse
        }
        Write-Note "已迁移到 $DataDir"
        return
    }

    Write-Step '未找到运行数据，初始化全新数据目录与管理员账号'
    New-Item -ItemType Directory -Force -Path $DataDir | Out-Null

    $password = $InitPassword
    if (-not $password) {
        $secure = Read-Host -AsSecureString '请输入初始管理员密码（至少 6 个字符）'
        $password = [System.Net.NetworkCredential]::new('', $secure).Password
    }
    if ($password.Length -lt 6) { throw '管理员密码至少 6 个字符。' }

    $env:DBBAK_ADMIN_PASSWORD = $password
    try {
        & $Binary init-admin
        if ($LASTEXITCODE -ne 0) { throw "init-admin 失败，退出码 $LASTEXITCODE。" }
    } finally {
        Remove-Item Env:\DBBAK_ADMIN_PASSWORD -ErrorAction SilentlyContinue
    }
    Set-Content -LiteralPath $PasswordFile -Value $password -Encoding UTF8 -NoNewline
    Write-Note "管理员 admin 已创建，密码写入 $PasswordFile"
}

# 1. 端口检查：已在运行则不再拉起第二个实例。
$ownerId = Get-PortOwner
if ($null -ne $ownerId) {
    $owner = Get-Process -Id $ownerId -ErrorAction SilentlyContinue
    $ownerName = 'unknown'
    if ($null -ne $owner) { $ownerName = $owner.ProcessName }

    if ($ownerName -ne 'dbbak') {
        throw "端口 $Port 被 $ownerName（PID $ownerId）占用。请先释放该端口，或用 -Port 指定其他端口。"
    }
    if (-not $Restart) {
        Write-Step "服务已在运行（PID $ownerId），无需重复启动。"
        Open-Console
        return
    }

    Write-Step "停止已运行的 dbbak（PID $ownerId）"
    Stop-Process -Id $ownerId -Force
    for ($i = 0; $i -lt 50; $i++) {
        if ($null -eq (Get-PortOwner)) { Break }
        Start-Sleep -Milliseconds 200
    }
    if ($null -ne (Get-PortOwner)) { throw "停止旧进程后端口 $Port 仍被占用。" }
}

# 2. 构建：缺失、源码更新或显式 -Build。
if (Test-NeedsBuild) {
    if (-not (Test-Path -LiteralPath $GoExe)) {
        throw "未找到便携 Go 工具链 $GoExe；也可安装 Go 1.27.3 后手动构建。"
    }
    Write-Step '构建 dbbak.exe ...'
    Set-DbbakGoEnv -Root $Root
    Push-Location $Root
    try {
        & $GoExe build -trimpath -o $Binary ./cmd/dbbak
        if ($LASTEXITCODE -ne 0) { throw "构建失败，退出码 $LASTEXITCODE。" }
    } finally {
        Pop-Location
    }
    Write-Note "产物 $Binary"
} else {
    Write-Step '二进制已是最新，跳过构建（-Build 可强制重建）'
}

# 3. 数据目录与备份根目录。
New-Item -ItemType Directory -Force -Path $Runtime | Out-Null
Initialize-DataDirectory
if (-not (Test-Path -LiteralPath $BackupRoot)) {
    New-Item -ItemType Directory -Force -Path $BackupRoot | Out-Null
}
if (-not (Test-Path -LiteralPath $PasswordFile)) {
    Write-Warning "未找到 $PasswordFile；登录请使用初始化时设置的管理员密码。"
}

# 4. 启动并等待就绪。
Write-Step "启动服务 $BaseUrl"
Set-DbbakDevEnv -Root $Root -Port $Port
Write-Note "数据目录 $DataDir"
Write-Note "备份目录 $BackupRoot"
Write-Note '调度随本进程运行，按 Ctrl+C 停止。'

$process = $null
try {
    # 同上，避免 Start-Process 在代理环境变量大小写冲突时失败。
    $startInfo = New-Object System.Diagnostics.ProcessStartInfo
    $startInfo.FileName = $Binary
    $startInfo.Arguments = 'serve'
    $startInfo.WorkingDirectory = $Root
    $startInfo.UseShellExecute = $false
    $process = [System.Diagnostics.Process]::Start($startInfo)

    $ready = $false
    for ($i = 0; $i -lt 75; $i++) {
        if ($process.HasExited) { throw "服务启动后立即退出，退出码 $($process.ExitCode)。" }
        try {
            $response = Invoke-WebRequest -Uri "$BaseUrl/health/ready" -UseBasicParsing -TimeoutSec 2
            if ($response.StatusCode -eq 200) { $ready = $true; Break }
        } catch {
            # 端口尚未监听，继续等待。
        }
        Start-Sleep -Milliseconds 400
    }
    if (-not $ready) { throw '服务在 30 秒内未通过 /health/ready 检查。' }

    Write-Step '服务已就绪'
    Open-Console
    $process.WaitForExit()
} finally {
    if ($null -ne $process -and -not $process.HasExited) {
        Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
    }
}
