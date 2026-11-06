#Requires -Version 5.1
<#
.SYNOPSIS
把客户机必需的命令行工具收进安装包暂存目录。

.DESCRIPTION
发行形态的目标是“装完即用”：客户机不配 DBBAK_* 环境变量、不改 PATH。
因此安装包必须自带数据库客户端，并保证它们能在自己的目录里独立运行。

当前只随包分发自带的 MySQL 客户端（mysql / mysqldump / mysqlbinlog）。
做法是只从完整安装包里挑必需文件，而不是整个 bin 目录：MySQL 8.4 官方包的
bin 有 850 MB，其中绝大多数是服务端和调试文件，与客户端无关。

PostgreSQL（psql / pg_basebackup）和 SQL Server（sqlcmd）暂不随包：
前者需要 VC++ 运行库与较新的 Windows，后者属于微软专有组件。
未自带时行为不变：按 DBBAK_* 或系统 PATH 查找，找不到就在能力矩阵里如实显示。

.PARAMETER SourceBin
MySQL 客户端所在目录，通常是 portable 包解压后的 bin。

.PARAMETER Destination
暂存目录下的 tools 子目录。

.PARAMETER Version
写入工具的版本文件，便于运行期核对。
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$SourceBin,
    [Parameter(Mandatory)][string]$Destination,
    [Parameter(Mandatory)][string]$Version
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# 客户端可执行文件与它们运行时必须同目录的 OpenSSL 库。
# 依赖来自对 mysql.exe / mysqldump.exe / mysqlbinlog.exe 导入表的实际读取：
# 除系统 DLL 与 VC++ 运行库外，只依赖这两个 libcrypto/libssl。
$files = @(
    'mysql.exe',
    'mysqldump.exe',
    'mysqlbinlog.exe',
    'libcrypto-3-x64.dll',
    'libssl-3-x64.dll'
)

if (-not (Test-Path -LiteralPath $SourceBin -PathType Container)) {
    throw "MySQL 客户端目录不存在：$SourceBin"
}
New-Item -ItemType Directory -Path $Destination -Force | Out-Null

$copied = 0
foreach ($name in $files) {
    $source = Join-Path $SourceBin $name
    if (-not (Test-Path -LiteralPath $source -PathType Leaf)) {
        throw "缺少 $name（$SourceBin）；安装包不能自带不完整的 MySQL 客户端。"
    }
    Copy-Item -LiteralPath $source -Destination (Join-Path $Destination $name) -Force
    $copied++
}

# 许可证随二进制分发：MySQL 采用 GPLv2，再分发时必须带上许可证与版权说明。
$licenseCandidates = @(
    (Join-Path (Split-Path -Parent $SourceBin) 'LICENSE'),
    (Join-Path $SourceBin 'LICENSE')
)
$licenseCopied = $false
foreach ($license in $licenseCandidates) {
    if (Test-Path -LiteralPath $license -PathType Leaf) {
        Copy-Item -LiteralPath $license -Destination (Join-Path $Destination 'LICENSE-mysql.txt') -Force
        $licenseCopied = $true
        break
    }
}

# 冒烟验证：工具必须能在该目录独立启动，否则客户机上必然失败。
$dump = Join-Path $Destination 'mysqldump.exe'
$reported = (& $dump --version 2>&1 | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $reported -notmatch 'mysqldump') {
    throw "自带的 mysqldump 无法运行：$reported"
}

$manifest = @(
    "MySQL 8.4 Community 客户端（随 DBBak $Version 分发）"
    "来源：$SourceBin"
    "版本输出：$reported"
    "许可证：GPLv2，详见 LICENSE-mysql.txt"
    '文件：' + ($files -join ', ')
) -join "`n"
[IO.File]::WriteAllText((Join-Path $Destination 'README-tools.txt'), $manifest + "`n", (New-Object Text.UTF8Encoding($false)))

Write-Host "已收集 $copied 个 MySQL 客户端文件到 $Destination"
if (-not $licenseCopied) { Write-Warning '未找到 MySQL LICENSE 文件：分发前必须补齐许可证。' }
Write-Host "  版本：$reported"
