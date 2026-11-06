#Requires -Version 5.1
#Requires -RunAsAdministrator
[CmdletBinding()]
param(
    [switch]$Yes,
    [switch]$StopService,
    [string]$DataRoot = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ServiceName = 'DBBak'
if ([string]::IsNullOrWhiteSpace($DataRoot)) {
    # 安装器允许自定义数据目录，并会写进 HKLM\Software\DBBak\DataDir；
    # 快照必须跟服务实际用的目录一致，否则会复制到别处或直接报“找不到元数据库”。
    $DataRoot = 'C:\ProgramData\DBBak'
    $registered = Get-ItemProperty -Path 'HKLM:\Software\DBBak' -Name 'DataDir' -ErrorAction SilentlyContinue
    if ($registered -and $registered.PSObject.Properties['DataDir'] -and
        -not [string]::IsNullOrWhiteSpace([string]$registered.PSObject.Properties['DataDir'].Value)) {
        $DataRoot = [string]$registered.PSObject.Properties['DataDir'].Value
    }
}
# 现行布局：元数据库直接位于数据根目录（由服务命令行 --data-dir 决定）。
# 旧 WinSW 布局放在 data 子目录，这里保留回退以便升级前仍能取到快照。
$Database = Join-Path $DataRoot 'dbbak.db'
$LegacyDatabase = Join-Path $DataRoot 'data\dbbak.db'
if (-not (Test-Path -LiteralPath $Database -PathType Leaf) -and (Test-Path -LiteralPath $LegacyDatabase -PathType Leaf)) {
    $Database = $LegacyDatabase
}
$MasterKey = Join-Path $DataRoot 'key\master.key'
$MaintenanceRoot = Join-Path $DataRoot 'maintenance'

$service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if (-not $service) { throw "服务 $ServiceName 不存在。" }
if ($service.Status -ne 'Stopped') {
    if (-not $StopService) {
        throw '必须先停止 DBBak 服务；运行中只复制 dbbak.db 不符合 WAL 一致性要求。加 -StopService 可让本脚本先停服。'
    }
    Write-Host "停止服务 $ServiceName ..."
    Stop-Service -Name $ServiceName -Force
    $service.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(60))
}
if (-not (Test-Path -LiteralPath $Database -PathType Leaf)) { throw "找不到元数据库 $Database。" }
if (-not (Test-Path -LiteralPath $MasterKey -PathType Leaf)) { throw "找不到独立主密钥 $MasterKey。" }

Write-Host '即将创建停服元数据快照：'
Write-Host "  - 复制 $Database 及存在的 -wal/-shm 侧车文件"
Write-Host "  - 一并复制独立主密钥 $MasterKey"
Write-Host "  - 在 $MaintenanceRoot 生成限权 ZIP 与 SHA-256 文件；归档含敏感密钥，必须异地保护"
Write-Host '  - 不删除或覆盖业务备份，不自动清理旧归档'
if (-not $Yes) {
    if ((Read-Host '确认执行请输入 YES') -cne 'YES') { Write-Host '已取消，未做更改。'; exit 0 }
}

New-Item -ItemType Directory -Path $MaintenanceRoot -Force | Out-Null
$stage = Join-Path $MaintenanceRoot ('.staging-' + [Guid]::NewGuid().ToString('N'))
$databaseStage = Join-Path $stage 'database'
$keyStage = Join-Path $stage 'key'
$copiedFiles = New-Object 'System.Collections.Generic.List[string]'
$timestamp = [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ')
$archive = Join-Path $MaintenanceRoot "dbbak-metadata-$timestamp-$([Guid]::NewGuid().ToString('N').Substring(0, 8)).zip"
$tempArchive = "$archive.tmp.zip"

try {
    New-Item -ItemType Directory -Path $databaseStage -Force | Out-Null
    New-Item -ItemType Directory -Path $keyStage -Force | Out-Null
    foreach ($source in @($Database, "$Database-wal", "$Database-shm")) {
        if (Test-Path -LiteralPath $source -PathType Leaf) {
            $destination = Join-Path $databaseStage (Split-Path -Leaf $source)
            Copy-Item -LiteralPath $source -Destination $destination
            $copiedFiles.Add($destination) | Out-Null
        }
    }
    $stagedKey = Join-Path $keyStage 'master.key'
    Copy-Item -LiteralPath $MasterKey -Destination $stagedKey
    $copiedFiles.Add($stagedKey) | Out-Null

    Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $tempArchive -Force
    Move-Item -LiteralPath $tempArchive -Destination $archive
    $hash = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText("$archive.sha256", "$hash  $(Split-Path -Leaf $archive)`n", [Text.Encoding]::ASCII)
    Write-Host "一致性备份已创建：$archive"
    Write-Output $archive
} finally {
    foreach ($file in @($tempArchive) + $copiedFiles) {
        if (Test-Path -LiteralPath $file) { Remove-Item -LiteralPath $file -Force -ErrorAction SilentlyContinue }
    }
    foreach ($directory in @($keyStage, $databaseStage, $stage)) {
        if (Test-Path -LiteralPath $directory) { Remove-Item -LiteralPath $directory -Force -ErrorAction SilentlyContinue }
    }
}
