#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

usage() { printf '用法：%s [--yes] /绝对路径/dbbak\n' "${0##*/}" >&2; }
assume_yes=0
if [[ ${1:-} == '--yes' ]]; then assume_yes=1; shift; fi
[[ $# -eq 1 ]] || { usage; exit 2; }
binary=$1
[[ $EUID -eq 0 ]] || { echo '必须以 root 执行。' >&2; exit 1; }
[[ -f $binary ]] || { echo "找不到新二进制：$binary" >&2; exit 1; }
[[ -x /usr/local/bin/dbbak && -f /etc/systemd/system/dbbak.service ]] || { echo 'DBBak 尚未安装。' >&2; exit 1; }
[[ -x /usr/local/sbin/dbbak-backup ]] || { echo '缺少一致性备份工具 /usr/local/sbin/dbbak-backup。' >&2; exit 1; }
for command_name in systemctl curl install; do
    command -v "$command_name" >/dev/null 2>&1 || { echo "缺少命令：$command_name" >&2; exit 1; }
done
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
health_url=${DBBAK_HEALTH_URL:-http://127.0.0.1:8080/health/ready}

cat <<'EOF'
即将升级 DBBak：
  - 停止 dbbak.service，升级期间不可访问
  - 停服后备份 SQLite、WAL 侧车文件和独立主密钥，失败则不替换程序
  - 在 /var/lib/dbbak/maintenance 保留旧二进制，再安装新二进制和 unit
  - 保留 /etc/dbbak/dbbak.env、业务备份和全部维护归档，启动后检查 readiness
EOF
if [[ $assume_yes -eq 0 ]]; then
    read -r -p '确认执行请输入 YES：' answer
    [[ $answer == 'YES' ]] || { echo '已取消，未做更改。'; exit 0; }
fi

systemctl stop dbbak.service
/usr/local/sbin/dbbak-backup --yes
previous="/var/lib/dbbak/maintenance/dbbak-binary-$(date -u +%Y%m%dT%H%M%SZ)-$$"
cp --preserve=mode,timestamps -- /usr/local/bin/dbbak "$previous"
install -o root -g root -m 0755 "$binary" /usr/local/bin/dbbak.next
mv -f -- /usr/local/bin/dbbak.next /usr/local/bin/dbbak
install -o root -g root -m 0700 "$script_dir/backup.sh" /usr/local/sbin/dbbak-backup
install -o root -g root -m 0644 "$script_dir/dbbak.service" /etc/systemd/system/dbbak.service
systemctl daemon-reload
systemctl start dbbak.service

ready=0
for ((attempt = 0; attempt < 30; attempt++)); do
    if curl --fail --silent --show-error --max-time 2 "$health_url" >/dev/null 2>&1; then
        ready=1
        break
    fi
    sleep 1
done
if [[ $ready -ne 1 ]]; then
    systemctl stop dbbak.service
    printf '升级后 readiness 失败，服务已停止；旧程序 %s 和元数据归档均已保留。\n' "$previous" >&2
    exit 1
fi
printf 'DBBak 升级并通过 readiness 检查；旧程序 %s 与全部备份均已保留。\n' "$previous"
