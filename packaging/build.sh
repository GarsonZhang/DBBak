#!/usr/bin/env bash
# 可复现本机/交叉构建：VERSION=x GOOS=linux GOARCH=amd64 ./packaging/build.sh
# 交叉构建只证明可编译，不构成目标系统运行验收。
set -Eeuo pipefail
umask 022

project_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
# Go 模块根位于仓库根目录，构建在根下执行，产物写入 dist/。
[[ -f "$project_root/go.mod" ]] || { echo "未找到 $project_root/go.mod，模块根应位于仓库根目录。" >&2; exit 2; }
version=${VERSION:-0.2.0-phase2}
export GOTOOLCHAIN=go1.27.3
export GOWORK=off
export CGO_ENABLED=0
export SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-0}
goos=${GOOS:-$(go env GOOS)}
goarch=${GOARCH:-$(go env GOARCH)}
[[ $version =~ ^[0-9A-Za-z][0-9A-Za-z._-]*$ ]] || { echo 'VERSION 含非法字符。' >&2; exit 2; }
[[ $goos == 'linux' || $goos == 'windows' ]] || { echo '仅支持 GOOS=linux 或 windows。' >&2; exit 2; }
[[ $goarch == 'amd64' || $goarch == 'arm64' ]] || { echo '仅支持 GOARCH=amd64 或 arm64。' >&2; exit 2; }

output_dir=${OUTPUT_DIR:-"$project_root/dist/$version/${goos}_${goarch}"}
binary=vault
[[ $goos == 'windows' ]] && binary=vault.exe
mkdir -p -- "$output_dir"
temp_binary="$output_dir/.$binary.tmp.$$"
cleanup() { rm -f -- "$temp_binary"; }
trap cleanup EXIT

export GOOS=$goos
export GOARCH=$goarch
(
    cd "$project_root"
    go build -mod=readonly -trimpath -buildvcs=false \
        -ldflags='-s -w -buildid=' -o "$temp_binary" ./cmd/vault
)
mv -f -- "$temp_binary" "$output_dir/$binary"
chmod 0755 "$output_dir/$binary"
(
    cd "$output_dir"
    sha256sum "$binary" > SHA256SUMS
)
printf '构建完成：%s\n校验和：%s\n注意：仍需在目标系统执行运行、备份和恢复验收。\n' \
    "$output_dir/$binary" "$output_dir/SHA256SUMS"
