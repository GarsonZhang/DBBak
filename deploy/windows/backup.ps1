#Requires -Version 5.1
#Requires -RunAsAdministrator
[CmdletBinding()]
param([switch]$Yes)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ServiceName = 'Vault'
$DataRoot = 'C:\ProgramData\Vault'
$Database = Join-Path $DataRoot 'data\vault.db'
$MasterKey = Join-Path $DataRoot 'key\master.key'
$MaintenanceRoot = Join-Path $DataRoot 'maintenance'

$service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if (-not $service) { throw "服务 $ServiceName 不存在。" }
if ($service.Status -ne 'Stopped') { throw '必须先停止 Vault 服务；运行中只复制 vault.db 不符合 WAL 一致性要求。' }
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
$archive = Join-Path $MaintenanceRoot "vault-metadata-$timestamp-$([Guid]::NewGuid().ToString('N').Substring(0, 8)).zip"
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
