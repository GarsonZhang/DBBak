#Requires -Version 5.1
#Requires -RunAsAdministrator
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$BinaryPath,
    [switch]$Yes
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ServiceName = 'DBBak'
$InstallRoot = 'C:\Program Files\DBBak'
$DataRoot = 'C:\ProgramData\DBBak'
$InstalledBinary = Join-Path $InstallRoot 'dbbak.exe'
$MaintenanceRoot = Join-Path $DataRoot 'maintenance'

# 升级只替换可执行文件：服务注册、账号、ImagePath 参数与数据目录全部保持不动。

function Wait-Ready {
    for ($attempt = 0; $attempt -lt 30; $attempt++) {
        try {
            $response = Invoke-WebRequest -Uri 'http://127.0.0.1:8080/health/ready' -UseBasicParsing -TimeoutSec 2
            if ($response.StatusCode -eq 200) { return $true }
        } catch { }
        Start-Sleep -Seconds 1
    }
    return $false
}

$BinaryPath = (Resolve-Path -LiteralPath $BinaryPath).Path
if (-not (Test-Path -LiteralPath $BinaryPath -PathType Leaf)) { throw '找不到新的 dbbak.exe。' }
if (-not (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue)) { throw "服务 $ServiceName 不存在，请先安装。" }
if (-not (Test-Path -LiteralPath $InstalledBinary -PathType Leaf)) { throw "找不到已安装文件 $InstalledBinary。" }

Write-Host '即将执行 DBBak 升级，影响如下：'
Write-Host "  - 停止 $ServiceName 服务，升级期间不可访问"
Write-Host '  - 停服后备份 SQLite、WAL 侧车文件与独立主密钥，备份失败则不替换程序'
Write-Host "  - 保留旧程序到 $MaintenanceRoot，再替换 $InstalledBinary"
Write-Host '  - 保留配置、业务备份和全部维护归档；启动后检查 readiness'
if (-not $Yes) {
    if ((Read-Host '确认执行请输入 YES') -cne 'YES') { Write-Host '已取消，未做更改。'; exit 0 }
}

Stop-Service -Name $ServiceName -Force
& (Join-Path $PSScriptRoot 'backup.ps1') -Yes
New-Item -ItemType Directory -Path $MaintenanceRoot -Force | Out-Null
$previous = Join-Path $MaintenanceRoot "dbbak-binary-$([DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ'))-$([Guid]::NewGuid().ToString('N').Substring(0, 8)).exe"
Copy-Item -LiteralPath $InstalledBinary -Destination $previous
$newBinary = Join-Path $InstallRoot 'dbbak.new.exe'
try {
    Copy-Item -LiteralPath $BinaryPath -Destination $newBinary -Force
    Move-Item -LiteralPath $newBinary -Destination $InstalledBinary -Force
    Start-Service -Name $ServiceName
    if (-not (Wait-Ready)) { throw '新版本未通过 readiness 检查。' }
} catch {
    if (Test-Path -LiteralPath $newBinary) { Remove-Item -LiteralPath $newBinary -Force -ErrorAction SilentlyContinue }
    Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
    throw "升级失败，服务已停止；旧程序位于 $previous，元数据维护归档均未删除。原始错误：$($_.Exception.Message)"
}
Write-Host 'DBBak 升级并通过 readiness 检查；旧程序与备份均已保留。'
