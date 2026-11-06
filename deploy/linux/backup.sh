#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

usage() { printf '用法：%s [--yes] [归档目录]\n' "${0##*/}" >&2; }
assume_yes=0
if [[ ${1:-} == '--yes' ]]; then assume_yes=1; shift; fi
[[ $# -le 1 ]] || { usage; exit 2; }
[[ $EUID -eq 0 ]] || { echo '必须以 root 执行，才能读取独立主密钥。' >&2; exit 1; }
destination=${1:-/var/lib/dbbak/maintenance}
config_file=${DBBAK_CONFIG_FILE:-/etc/dbbak/dbbak.env}
[[ -f $config_file ]] || { echo "找不到配置文件：$config_file" >&2; exit 1; }
[[ $destination == /* && $destination != '/' ]] || { echo '归档目录必须是非根目录的绝对路径。' >&2; exit 1; }
for command_name in systemctl tar sha256sum; do
    command -v "$command_name" >/dev/null 2>&1 || { echo "缺少命令：$command_name" >&2; exit 1; }
done
if systemctl is-active --quiet dbbak.service; then
    echo '必须先停止 dbbak.service；运行中只复制 dbbak.db 不符合 WAL 一致性要求。' >&2
    exit 1
fi

set -a
# 配置文件由安装脚本以 root:root 0600 创建，只允许受信管理员修改。
source "$config_file"
set +a
: "${DBBAK_DATABASE:?配置缺少 DBBAK_DATABASE}"
: "${DBBAK_MASTER_KEY_FILE:?配置缺少 DBBAK_MASTER_KEY_FILE}"
[[ $DBBAK_DATABASE == /* && $DBBAK_MASTER_KEY_FILE == /* ]] || { echo '数据库和主密钥必须使用绝对路径。' >&2; exit 1; }
[[ -f $DBBAK_DATABASE ]] || { echo "找不到元数据库：$DBBAK_DATABASE" >&2; exit 1; }
[[ -f $DBBAK_MASTER_KEY_FILE ]] || { echo "找不到独立主密钥：$DBBAK_MASTER_KEY_FILE" >&2; exit 1; }

cat <<EOF
即将创建停服元数据快照：
  - 复制 $DBBAK_DATABASE 及存在的 -wal/-shm 侧车文件
  - 一并复制独立主密钥 $DBBAK_MASTER_KEY_FILE
  - 在 $destination 创建 0600 tar.gz 与 SHA-256 文件
  - 归档含敏感主密钥，必须异地保护；不删除业务备份或旧归档
EOF
if [[ $assume_yes -eq 0 ]]; then
    read -r -p '确认执行请输入 YES：' answer
    [[ $answer == 'YES' ]] || { echo '已取消，未做更改。'; exit 0; }
fi

install -d -o root -g root -m 0700 "$destination"
stage=$(mktemp -d "$destination/.staging.XXXXXXXX")
temp_archive=''
copied_files=()
cleanup() {
    local file
    if [[ -n $temp_archive ]]; then rm -f -- "$temp_archive"; fi
    for file in ${copied_files[@]+"${copied_files[@]}"}; do rm -f -- "$file"; done
    rmdir -- "$stage/key" "$stage/database" "$stage" 2>/dev/null || true
}
trap cleanup EXIT
mkdir "$stage/database" "$stage/key"
for source_file in "$DBBAK_DATABASE" "$DBBAK_DATABASE-wal" "$DBBAK_DATABASE-shm"; do
    if [[ -f $source_file ]]; then
        target_file="$stage/database/${source_file##*/}"
        cp --preserve=mode,timestamps -- "$source_file" "$target_file"
        copied_files+=("$target_file")
    fi
done
cp --preserve=mode,timestamps -- "$DBBAK_MASTER_KEY_FILE" "$stage/key/master.key"
copied_files+=("$stage/key/master.key")

timestamp=$(date -u +%Y%m%dT%H%M%SZ)
archive="$destination/dbbak-metadata-$timestamp-$$.tar.gz"
temp_archive=$(mktemp "$destination/.dbbak-metadata.XXXXXXXX.tmp")
tar --sort=name --owner=0 --group=0 --numeric-owner -C "$stage" -czf "$temp_archive" database key
chmod 0600 "$temp_archive"
mv -- "$temp_archive" "$archive"
temp_archive=''
(
    cd "$destination"
    sha256sum "${archive##*/}" > "$archive.sha256"
)
chmod 0600 "$archive.sha256"
printf '一致性备份已创建：%s\n' "$archive"
