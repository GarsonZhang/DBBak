#Requires -Version 5.1
# 本地开发启动：固定使用 .test-runtime 隔离数据目录与 .tools 便携工具链。
# 不启动 MySQL/PostgreSQL 实例；备份能否成功取决于对应实例与客户端是否就绪。
[CmdletBinding()]
param(
    [int]$Port = 38080,
    [switch]$Build
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$Root = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..')).Path
$Runtime = Join-Path $Root '.test-runtime'
$DataDir = Join-Path $Runtime 'vault-data'
$BackupRoot = Join-Path $Runtime 'backup-root'
$Binary = Join-Path $Runtime 'vault.exe'
$GoExe = Join-Path $Root '.tools\go\bin\go.exe'
$PasswordFile = Join-Path $Runtime 'admin-password'

if (-not (Test-Path -LiteralPath $DataDir)) {
    throw "缺少数据目录 $DataDir。先按 README 执行 init-admin 初始化管理员。"
}
if (-not (Test-Path -LiteralPath $PasswordFile)) {
    Write-Warning "未找到 $PasswordFile；登录请使用初始化时设置的管理员密码。"
}

# 先检查端口，避免进程起来后才看到晦涩的 bind 失败。
$busy = Get-NetTCPConnection -LocalPort $Port -State Listen -ErrorAction SilentlyContinue
if ($busy) {
    throw "端口 $Port 已被进程 $($busy[0].OwningProcess) 占用。先停止它，或用 -Port 指定其他端口。"
}

if ($Build -or -not (Test-Path -LiteralPath $Binary)) {
    if (-not (Test-Path -LiteralPath $GoExe)) {
        throw "未找到便携 Go 工具链 $GoExe；也可自行安装 Go 1.27.3 后手动构建。"
    }
    Write-Host "构建 $Binary ..."
    Push-Location $Root
    try {
        & $GoExe build -trimpath -o $Binary ./cmd/vault
        if ($LASTEXITCODE -ne 0) { throw "构建失败，退出码 $LASTEXITCODE。" }
    } finally {
        Pop-Location
    }
}

$env:VAULT_DATA_DIR = $DataDir
$env:VAULT_BACKUP_ROOT = $BackupRoot
$env:VAULT_ADDR = "127.0.0.1:$Port"
$env:VAULT_SCHEDULER_INTERVAL = '2s'

# 客户端路径指向便携安装；文件缺失时连接测试会返回 TOOL_MISSING，不会伪造成功。
$env:VAULT_MYSQL = Join-Path $Root '.tools\mysql-8.4.13-winx64\bin\mysql.exe'
$env:VAULT_MYSQLDUMP = Join-Path $Root '.tools\mysql-8.4.13-winx64\bin\mysqldump.exe'
$env:VAULT_PSQL = Join-Path $Root '.tools\postgresql-18.6\pgsql\bin\psql.exe'
$env:VAULT_PG_BASEBACKUP = Join-Path $Root '.tools\postgresql-18.6\pgsql\bin\pg_basebackup.exe'
if (Get-Command sqlcmd -ErrorAction SilentlyContinue) { $env:VAULT_SQLCMD = 'sqlcmd' }

Write-Host "数据目录: $DataDir"
Write-Host "备份目录: $BackupRoot"
Write-Host "访问地址: http://127.0.0.1:$Port/  （管理员账号 admin）"
Write-Host '调度随本进程运行，按 Ctrl+C 停止。'

& $Binary serve
