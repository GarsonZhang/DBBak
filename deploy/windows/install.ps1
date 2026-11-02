#Requires -Version 5.1
#Requires -RunAsAdministrator
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$BinaryPath,
    [Parameter(Mandatory = $true)][string]$WinSWPath,
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9A-Fa-f]{64}$')][string]$WinSWSha256,
    [string]$ServiceAccount = 'NT SERVICE\Vault',
    [switch]$Yes
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ServiceName = 'Vault'
$InstallRoot = 'C:\Program Files\Vault'
$DataRoot = 'C:\ProgramData\Vault'
$ServiceExecutable = Join-Path $InstallRoot 'VaultService.exe'

function Confirm-Installation {
    Write-Host '即将执行 Vault 安装，影响如下：'
    Write-Host "  - 在 $InstallRoot 安装 vault.exe 与 WinSW 服务包装器"
    Write-Host "  - 创建 $DataRoot 下的 data、backups、key、logs、maintenance 目录"
    Write-Host "  - 创建并启动 $ServiceName Windows 服务，设置为延迟自动启动"
    Write-Host "  - 使用专用服务账号 $ServiceAccount，并限制 ProgramData ACL"
    Write-Host '  - 不初始化管理员，不写入数据库账号或密码，不删除任何现有备份'
    if (-not $Yes) {
        if ((Read-Host '确认执行请输入 YES') -cne 'YES') { Write-Host '已取消，未做更改。'; exit 0 }
    }
}

function Wait-Ready {
    for ($attempt = 0; $attempt -lt 30; $attempt++) {
        try {
            $response = Invoke-WebRequest -Uri 'http://127.0.0.1:8080/health/ready' -UseBasicParsing -TimeoutSec 2
            if ($response.StatusCode -eq 200) { return }
        } catch { }
        Start-Sleep -Seconds 1
    }
    throw '服务未在 30 秒内通过 readiness 检查，请检查 ProgramData 日志。'
}

$BinaryPath = (Resolve-Path -LiteralPath $BinaryPath).Path
$WinSWPath = (Resolve-Path -LiteralPath $WinSWPath).Path
if (-not (Test-Path -LiteralPath $BinaryPath -PathType Leaf)) { throw '找不到 vault.exe。' }
if (-not (Test-Path -LiteralPath $WinSWPath -PathType Leaf)) { throw '找不到 WinSW.exe；请使用 WinSW v2.12.0。' }
if ((Get-FileHash -LiteralPath $WinSWPath -Algorithm SHA256).Hash -cne $WinSWSha256.ToUpperInvariant()) {
    throw 'WinSW SHA-256 与指定值不符，已中止。'
}
if ($ServiceAccount -ne 'NT SERVICE\Vault' -and -not $ServiceAccount.EndsWith('$')) {
    throw '自动配置仅支持 NT SERVICE\Vault 虚拟账号或无密码 gMSA；普通用户账号请改用虚拟账号。'
}
if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
    throw "服务 $ServiceName 已存在；升级请使用 upgrade.ps1。"
}

Confirm-Installation

foreach ($directory in @($InstallRoot, $DataRoot, (Join-Path $DataRoot 'data'), (Join-Path $DataRoot 'backups'),
        (Join-Path $DataRoot 'key'), (Join-Path $DataRoot 'logs'), (Join-Path $DataRoot 'maintenance'))) {
    New-Item -ItemType Directory -Path $directory -Force | Out-Null
}
Copy-Item -LiteralPath $BinaryPath -Destination (Join-Path $InstallRoot 'vault.exe')
Copy-Item -LiteralPath $WinSWPath -Destination $ServiceExecutable
Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'vault-service.xml') -Destination (Join-Path $InstallRoot 'VaultService.xml')
Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'backup.ps1') -Destination (Join-Path $InstallRoot 'backup.ps1')

& $ServiceExecutable install
if ($LASTEXITCODE -ne 0) { throw "WinSW 安装失败，退出码 $LASTEXITCODE。" }
& sc.exe config $ServiceName obj= $ServiceAccount start= delayed-auto
if ($LASTEXITCODE -ne 0) { throw "配置服务账号失败，退出码 $LASTEXITCODE。" }

# SID 避免 Windows 本地化账号名差异；gMSA 需由管理员预先配置服务登录权限。
& icacls.exe $DataRoot /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' "${ServiceAccount}:(OI)(CI)M"
if ($LASTEXITCODE -ne 0) { throw "配置 ProgramData ACL 失败，退出码 $LASTEXITCODE。" }

Start-Service -Name $ServiceName
Wait-Ready
Write-Host 'Vault 安装并通过 readiness 检查。'
Write-Host '主密钥与 SQLite 分文件保存；请使用 backup.ps1 做停服一致性备份并异地保护归档。'
