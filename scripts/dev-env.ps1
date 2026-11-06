#Requires -Version 5.1
# 开发环境公共设置：便携工具链缓存、隔离数据目录与 DBBAK_* 变量。
# 由 run-dev.ps1 与 start-dev.ps1 dot-source，避免两处环境定义漂移。

Set-StrictMode -Version Latest

function Set-DbbakGoEnv {
    param([Parameter(Mandatory)][string]$Root)

    # 模块与构建缓存都在 .tools 下；不指定会回落到 %USERPROFILE%\go 并触发联网下载。
    $env:GOPATH = Join-Path $Root '.tools\gopath'
    $env:GOCACHE = Join-Path $Root '.tools\gocache'
    if (-not $env:GOFLAGS) { $env:GOFLAGS = '-mod=mod' }
    if (-not $env:GOPROXY) { $env:GOPROXY = 'https://goproxy.cn,direct' }
}

function Set-DbbakDevEnv {
    param(
        [Parameter(Mandatory)][string]$Root,
        [Parameter(Mandatory)][int]$Port
    )

    $runtime = Join-Path $Root '.test-runtime'
    $env:DBBAK_DATA_DIR = Join-Path $runtime 'dbbak-data'
    $env:DBBAK_BACKUP_ROOT = Join-Path $runtime 'backup-root'
    $env:DBBAK_ADDR = "127.0.0.1:$Port"
    $env:DBBAK_SCHEDULER_INTERVAL = '2s'
    # SQL Server 已于 2026-11-02 完成真实备份与异名恢复验收，开发环境显式开启任务创建。
    $env:DBBAK_SQLSERVER_ENABLED = '1'

    # 客户端路径指向便携安装；文件缺失时连接测试会返回 TOOL_MISSING，不会伪造成功。
    $env:DBBAK_MYSQL = Join-Path $Root '.tools\mysql-8.4.13-winx64\bin\mysql.exe'
    $env:DBBAK_MYSQLDUMP = Join-Path $Root '.tools\mysql-8.4.13-winx64\bin\mysqldump.exe'
    $env:DBBAK_PSQL = Join-Path $Root '.tools\postgresql-18.6\pgsql\bin\psql.exe'
    $env:DBBAK_PG_BASEBACKUP = Join-Path $Root '.tools\postgresql-18.6\pgsql\bin\pg_basebackup.exe'

    # sqlcmd 由全机安装提供，便携目录里没有；找不到时保持未设置，由能力探测如实返回 TOOL_MISSING。
    if (Get-Command sqlcmd -ErrorAction SilentlyContinue) { $env:DBBAK_SQLCMD = 'sqlcmd' }
}
