#Requires -Version 5.1
<#
.SYNOPSIS
构建 Windows 图形安装包与便携包。

.DESCRIPTION
1. 复用 packaging/build.ps1 构建 windows/amd64 可执行文件（统一 ldflags，避免两套构建参数）。
2. 校验产物自报版本与包名一致，防止装出与文件名不符的程序。
3. 用 Inno Setup 编译 packaging/dbbak.iss，产出标准 Windows 安装向导：
   选择安装目录、选择数据目录、设置监听地址与初始管理员密码，然后由 dbbak.exe setup 注册并启动服务。
   Inno Setup 缺失时会自动下载固定版本到 .tools\innosetup（校验 SHA-256）。
4. 生成便携 ZIP 与 SHA256SUMS。

安装包没有代码签名，首次运行可能出现 SmartScreen 提示。
#>
[CmdletBinding()]
param(
    [ValidatePattern('^[0-9A-Za-z][0-9A-Za-z._-]*$')][string]$Version = '0.2.0-phase5',
    [string]$OutputDir = '',
    [string]$InnoSetupDir = '',
    # 自带 MySQL 客户端目录：默认取仓库内便携包，客户机不需要另装客户端。
    [string]$MySQLClientBin = '',
    [switch]$NoBundledTools,
    [switch]$NoInstaller,
    [switch]$SkipTests
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProjectRoot = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..')).Path
if (-not (Test-Path -LiteralPath (Join-Path $ProjectRoot 'go.mod'))) { throw "未找到 $ProjectRoot\go.mod，模块根应位于仓库根目录。" }
if (-not $MySQLClientBin) { $MySQLClientBin = Join-Path $ProjectRoot '.tools\mysql-8.4.13-winx64\bin' }

# Inno Setup 固定版本与校验和：自动下载时只接受这份二进制。
$InnoVersion = '7.1.1'
$InnoSha256 = '8A5A6711C3766A6E5FB74F280BA0E3551E719CE94538D901FDBD1F8F9920B3F0'
$InnoUrl = "https://github.com/jrsoftware/issrc/releases/download/is-7_1_1/innosetup-$InnoVersion-x64.exe"

# 仓库自带便携 Go 工具链时优先使用，避免依赖系统 PATH 配置。
$portableGo = Join-Path $ProjectRoot '.tools\go\bin'
if (-not (Get-Command go -ErrorAction SilentlyContinue) -and (Test-Path -LiteralPath (Join-Path $portableGo 'go.exe'))) {
    $env:PATH = "$portableGo;$env:PATH"
    Write-Host "使用便携 Go 工具链：$portableGo"
}
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw '找不到 go，请先安装 Go 1.27.3 或准备 .tools\go 便携工具链。' }

if (-not $OutputDir) { $OutputDir = Join-Path $ProjectRoot "dist\$Version" }
New-Item -ItemType Directory -Path $OutputDir -Force | Out-Null

if (-not $SkipTests) {
    Push-Location $ProjectRoot
    try {
        & go vet -p=1 ./...
        if ($LASTEXITCODE -ne 0) { throw "go vet 失败，退出码 $LASTEXITCODE。" }
        & go test ./...
        if ($LASTEXITCODE -ne 0) { throw "go test 失败，退出码 $LASTEXITCODE。" }
    } finally {
        Pop-Location
    }
}

# 1. 构建可执行文件
& (Join-Path $PSScriptRoot 'build.ps1') -Version $Version -GOOS windows -GOARCH amd64
$binary = Join-Path $ProjectRoot "dist\$Version\windows_amd64\dbbak.exe"
if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) { throw "构建产物缺失：$binary" }

# 2. 版本一致性校验
$reported = ((& $binary version) -join '').Trim()
if ($reported -ne $Version) {
    throw "产物自报版本 '$reported' 与构建版本 '$Version' 不一致；请同步 cmd/dbbak/main.go 中的 const version。"
}

function Find-InnoCompiler {
    param([string]$PreferredDir)

    $candidates = @()
    if ($PreferredDir) { $candidates += (Join-Path $PreferredDir 'ISCC.exe') }
    $candidates += (Join-Path $ProjectRoot ".tools\innosetup\ISCC.exe")
    foreach ($envName in @('ProgramFiles', 'ProgramFiles(x86)')) {
        $root = [Environment]::GetEnvironmentVariable($envName)
        if (-not $root) { continue }
        foreach ($edition in @('Inno Setup 7', 'Inno Setup 6')) {
            $candidates += (Join-Path $root "$edition\ISCC.exe")
        }
    }
    foreach ($path in $candidates) {
        if ($path -and (Test-Path -LiteralPath $path -PathType Leaf)) { return $path }
    }
    return $null
}

function Install-InnoSetup {
    $target = Join-Path $ProjectRoot '.tools\innosetup'
    $installer = Join-Path $ProjectRoot ".tools\innosetup-$InnoVersion-x64.exe"
    New-Item -ItemType Directory -Path (Join-Path $ProjectRoot '.tools') -Force | Out-Null

    if (-not (Test-Path -LiteralPath $installer -PathType Leaf)) {
        Write-Host "下载 Inno Setup $InnoVersion ..."
        $ProgressPreference = 'SilentlyContinue'
        Invoke-WebRequest -Uri $InnoUrl -OutFile $installer -UseBasicParsing
    }
    $actual = (Get-FileHash -LiteralPath $installer -Algorithm SHA256).Hash
    if ($actual -ne $InnoSha256) {
        throw "Inno Setup 安装器校验和不符：期望 $InnoSha256，实际 $actual。请删除 $installer 后重试。"
    }

    Write-Host "安装 Inno Setup 到 $target ..."
    $proc = Start-Process -FilePath $installer -ArgumentList @(
        '/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', '/CURRENTUSER', "/DIR=$target"
    ) -Wait -PassThru
    if ($proc.ExitCode -ne 0) { throw "Inno Setup 安装失败，退出码 $($proc.ExitCode)。" }

    $iscc = Join-Path $target 'ISCC.exe'
    if (-not (Test-Path -LiteralPath $iscc -PathType Leaf)) { throw "Inno Setup 安装后未找到 ISCC.exe：$iscc" }
    return $iscc
}

# 3. 图形安装包（Inno Setup）
$setupPath = $null
if (-not $NoInstaller) {
    $iscc = Find-InnoCompiler -PreferredDir $InnoSetupDir
    if (-not $iscc) {
        if ($InnoSetupDir) { throw "指定的 Inno Setup 目录中没有 ISCC.exe：$InnoSetupDir" }
        $iscc = Install-InnoSetup
    }
    Write-Host "使用 Inno Setup：$iscc"

    $setupName = "DBBak-Setup-$Version-windows-amd64.exe"
    $setupPath = Join-Path $OutputDir $setupName

    if (Test-Path -LiteralPath $setupPath) { Remove-Item -LiteralPath $setupPath -Force }

    # 程序目录暂存区：安装包把这个目录整体装进 {app}，不是只装一个 exe。
    # dbbak.exe、自带 tools 与运维脚本必须在同一层，运行时按 exe 目录找 tools。
    $appDir = Join-Path $OutputDir 'app'
    if (Test-Path -LiteralPath $appDir) { Remove-Item -LiteralPath $appDir -Recurse -Force }
    New-Item -ItemType Directory -Path $appDir -Force | Out-Null
    Copy-Item -LiteralPath $binary -Destination (Join-Path $appDir 'dbbak.exe')
    Copy-Item -LiteralPath (Join-Path $ProjectRoot 'deploy\windows\backup.ps1') -Destination (Join-Path $appDir 'backup.ps1')
    if ($NoBundledTools) {
        # 没有 tools 时 dbbak.exe 回落到 DBBAK_* 或系统 PATH；只适合内网已有客户端的场景。
        Write-Warning '已跳过自带工具（-NoBundledTools）：客户机必须自备 MySQL 客户端并在 PATH 中。'
    } else {
        & (Join-Path $PSScriptRoot 'collect-tools.ps1') -SourceBin $MySQLClientBin -Destination (Join-Path $appDir 'tools') -Version $Version
    }

    # VersionInfoVersion 必须是四段数字，从语义版本里取前三段数字。
    $numeric = [regex]::Match($Version, '^[0-9]+(\.[0-9]+){0,2}').Value
    if (-not $numeric) { $numeric = '0.0.0' }
    $quadParts = @($numeric.Split('.') + @('0', '0', '0'))[0..3]
    $versionQuad = $quadParts -join '.'

    $issPath = Join-Path $PSScriptRoot 'dbbak.iss'
    & $iscc "/DAppVersion=$Version" "/DVersionQuad=$versionQuad" "/DAppExe=$binary" "/DAppDir=$appDir" "/O$OutputDir" '/Qp' $issPath
    if ($LASTEXITCODE -ne 0) { throw "Inno Setup 编译失败，退出码 $LASTEXITCODE。" }
    if (-not (Test-Path -LiteralPath $setupPath -PathType Leaf)) { throw "安装包未生成：$setupPath" }
}

# 4. 便携包（不解压安装，直接前台运行 serve）
$zipName = "dbbak-$Version-windows-amd64.zip"
$zipPath = Join-Path $OutputDir $zipName
$stage = Join-Path $OutputDir ('.stage-' + [Guid]::NewGuid().ToString('N'))
try {
    New-Item -ItemType Directory -Path (Join-Path $stage 'deploy\windows') -Force | Out-Null
    Copy-Item -LiteralPath $binary -Destination (Join-Path $stage 'dbbak.exe')
    foreach ($doc in @('README.md', 'LICENSE')) {
        $source = Join-Path $ProjectRoot $doc
        if (Test-Path -LiteralPath $source) { Copy-Item -LiteralPath $source -Destination $stage }
    }
    foreach ($script in @('backup.ps1', 'upgrade.ps1')) {
        $source = Join-Path $ProjectRoot "deploy\windows\$script"
        if (Test-Path -LiteralPath $source) { Copy-Item -LiteralPath $source -Destination (Join-Path $stage 'deploy\windows') }
    }
    # 便携包同样自带工具：解压即可用，不要求目标机预装 MySQL 客户端。
    if (-not $NoBundledTools) {
        & (Join-Path $PSScriptRoot 'collect-tools.ps1') -SourceBin $MySQLClientBin -Destination (Join-Path $stage 'tools') -Version $Version
    }
    if (Test-Path -LiteralPath $zipPath) { Remove-Item -LiteralPath $zipPath -Force }
    Compress-Archive -Path (Join-Path $stage '*') -DestinationPath $zipPath
} finally {
    if (Test-Path -LiteralPath $stage) { Remove-Item -LiteralPath $stage -Recurse -Force -ErrorAction SilentlyContinue }
}

# 5. 校验和
$artifacts = @()
if ($setupPath) { $artifacts += $setupPath }
$artifacts += $zipPath
$lines = foreach ($file in $artifacts) {
    $hash = (Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash.ToLowerInvariant()
    "$hash  $(Split-Path -Leaf $file)"
}
[IO.File]::WriteAllText((Join-Path $OutputDir 'SHA256SUMS'), (($lines -join "`n") + "`n"), [Text.Encoding]::ASCII)

Write-Host ''
if ($setupPath) { Write-Host "安装包：$setupPath" }
Write-Host "便携包：$zipPath"
Write-Host "校验和：$(Join-Path $OutputDir 'SHA256SUMS')"
if ($setupPath) {
    Write-Host '安装步骤：双击安装包，按向导选择安装目录与数据目录、设置管理员密码，安装器会注册并启动服务。'
    Write-Host '自带 MySQL 客户端已随安装包分发，客户机无需另装 mysql/mysqldump，也无需配置 PATH。'
} else {
    Write-Host '已跳过图形安装包（-NoInstaller）；可用 dbbak.exe setup 命令行安装。'
}
Write-Host '注意：构建成功不等于安装验收；服务安装、启动与卸载仍须在目标机执行一次。'
