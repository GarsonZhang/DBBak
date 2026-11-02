#Requires -Version 5.1
#Requires -RunAsAdministrator
[CmdletBinding()]
param([switch]$Yes)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ServiceName = 'Vault'
$InstallRoot = 'C:\Program Files\Vault'
$DataRoot = 'C:\ProgramData\Vault'
$ServiceExecutable = Join-Path $InstallRoot 'VaultService.exe'

$service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
Write-Host '即将卸载 Vault 服务，影响如下：'
Write-Host "  - 停止并删除 $ServiceName 服务及其自动启动配置"
Write-Host "  - 删除 $InstallRoot 中的程序、WinSW 包装器和维护脚本"
Write-Host "  - 保留 $DataRoot 中的 SQLite、独立主密钥、业务备份、日志和维护归档"
Write-Host '  - 不删除任何系统账号，不执行任何递归数据清理'
if (-not $Yes) {
    if ((Read-Host '确认执行请输入 YES') -cne 'YES') { Write-Host '已取消，未做更改。'; exit 0 }
}
if (-not $service -and -not (Test-Path -LiteralPath $InstallRoot)) { Write-Host 'Vault 未安装，无需卸载。'; exit 0 }

if ($service -and $service.Status -ne 'Stopped') { Stop-Service -Name $ServiceName -Force }
if ($service) {
    if (-not (Test-Path -LiteralPath $ServiceExecutable -PathType Leaf)) { throw '找不到 WinSW 包装器，未删除程序或数据。' }
    & $ServiceExecutable uninstall
    if ($LASTEXITCODE -ne 0) { throw "WinSW 卸载失败，退出码 $LASTEXITCODE。" }
}

foreach ($file in @('vault.exe', 'vault.new.exe', 'VaultService.exe', 'VaultService.xml', 'backup.ps1')) {
    $path = Join-Path $InstallRoot $file
    if (Test-Path -LiteralPath $path) { Remove-Item -LiteralPath $path -Force }
}
if (Test-Path -LiteralPath $InstallRoot) {
    try { Remove-Item -LiteralPath $InstallRoot -Force -ErrorAction Stop } catch { Write-Warning '安装目录尚有其他文件，已保留该目录。' }
}
Write-Host "服务与程序已卸载；所有数据、主密钥和备份仍保留在 $DataRoot。"
