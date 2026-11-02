#Requires -Version 5.1
[CmdletBinding()]
param(
    [ValidatePattern('^[0-9A-Za-z][0-9A-Za-z._-]*$')][string]$Version = '0.2.0-phase2',
    [ValidateSet('linux', 'windows')][string]$GOOS = '',
    [ValidateSet('amd64', 'arm64')][string]$GOARCH = '',
    [string]$OutputDir = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProjectRoot = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot '..')).Path
# Go 模块根位于仓库根目录，构建在根下执行，产物写入 dist/。
if (-not (Test-Path -LiteralPath (Join-Path $ProjectRoot 'go.mod'))) { throw "未找到 $ProjectRoot\go.mod，模块根应位于仓库根目录。" }
$env:GOTOOLCHAIN = 'go1.27.3'
$env:GOWORK = 'off'
$env:CGO_ENABLED = '0'
if (-not $env:SOURCE_DATE_EPOCH) { $env:SOURCE_DATE_EPOCH = '0' }
if (-not $GOOS) { $GOOS = (& go env GOOS); if ($LASTEXITCODE -ne 0) { throw 'go env GOOS 失败。' } }
if (-not $GOARCH) { $GOARCH = (& go env GOARCH); if ($LASTEXITCODE -ne 0) { throw 'go env GOARCH 失败。' } }
if (-not $OutputDir) { $OutputDir = Join-Path $ProjectRoot "dist\$Version\${GOOS}_$GOARCH" }
$BinaryName = if ($GOOS -eq 'windows') { 'vault.exe' } else { 'vault' }
$TempBinary = Join-Path $OutputDir ".$BinaryName.tmp.$PID"

New-Item -ItemType Directory -Path $OutputDir -Force | Out-Null
$env:GOOS = $GOOS
$env:GOARCH = $GOARCH
try {
    Push-Location $ProjectRoot
    try {
        & go build -mod=readonly -trimpath -buildvcs=false '-ldflags=-s -w -buildid=' -o $TempBinary ./cmd/vault
        if ($LASTEXITCODE -ne 0) { throw "go build 失败，退出码 $LASTEXITCODE。" }
    } finally {
        Pop-Location
    }
    Move-Item -LiteralPath $TempBinary -Destination (Join-Path $OutputDir $BinaryName) -Force
    $Hash = (Get-FileHash -LiteralPath (Join-Path $OutputDir $BinaryName) -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText((Join-Path $OutputDir 'SHA256SUMS'), "$Hash  $BinaryName`n", [Text.Encoding]::ASCII)
} finally {
    if (Test-Path -LiteralPath $TempBinary) { Remove-Item -LiteralPath $TempBinary -Force -ErrorAction SilentlyContinue }
}
Write-Host "构建完成：$(Join-Path $OutputDir $BinaryName)"
Write-Host '注意：交叉构建只证明可编译，仍需在目标系统执行运行、备份和恢复验收。'
