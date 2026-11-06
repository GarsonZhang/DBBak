#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

usage() { printf '用法：%s [--yes] /绝对路径/dbbak\n' "${0##*/}" >&2; }
assume_yes=0
if [[ ${1:-} == '--yes' ]]; then assume_yes=1; shift; fi
[[ $# -eq 1 ]] || { usage; exit 2; }
binary=$1
[[ $EUID -eq 0 ]] || { echo '必须以 root 执行。' >&2; exit 1; }
[[ -f $binary ]] || { echo "找不到 DBBak 二进制：$binary" >&2; exit 1; }
for command_name in systemctl curl install tar sha256sum; do
    command -v "$command_name" >/dev/null 2>&1 || { echo "缺少命令：$command_name" >&2; exit 1; }
done
if [[ -e /etc/systemd/system/dbbak.service || -e /usr/local/bin/dbbak ]]; then
    echo '检测到已有安装；升级请使用 upgrade.sh，避免覆盖配置。' >&2
    exit 1
fi
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
health_url=${DBBAK_HEALTH_URL:-http://127.0.0.1:8080/health/ready}

cat <<EOF
即将安装 DBBak：
  - 创建专用系统用户/组 dbbak（无登录 shell），不创建普通用户 home
  - 创建 /var/lib/dbbak、backups、key、maintenance，权限 0700
  - 安装 /usr/local/bin/dbbak、/usr/local/sbin/dbbak-backup 和 systemd unit
  - 安装 /etc/dbbak/dbbak.env（0600），创建并启动 dbbak.service
  - 不写入任何数据库凭据，不删除或清理任何现有备份
EOF
if [[ $assume_yes -eq 0 ]]; then
    read -r -p '确认执行请输入 YES：' answer
    [[ $answer == 'YES' ]] || { echo '已取消，未做更改。'; exit 0; }
fi

if ! id dbbak >/dev/null 2>&1; then
    nologin_shell=$(command -v nologin || true)
    useradd --system --user-group --home-dir /var/lib/dbbak --no-create-home \
        --shell "${nologin_shell:-/usr/sbin/nologin}" dbbak
fi
install -d -o dbbak -g dbbak -m 0700 /var/lib/dbbak /var/lib/dbbak/backups \
    /var/lib/dbbak/key /var/lib/dbbak/maintenance
install -d -o root -g root -m 0700 /etc/dbbak
if [[ ! -f /etc/dbbak/dbbak.env ]]; then
    install -o root -g root -m 0600 "$script_dir/dbbak.env.example" /etc/dbbak/dbbak.env
else
    echo '保留现有 /etc/dbbak/dbbak.env，不覆盖管理员配置。'
fi
install -o root -g root -m 0755 "$binary" /usr/local/bin/dbbak
install -o root -g root -m 0700 "$script_dir/backup.sh" /usr/local/sbin/dbbak-backup
install -o root -g root -m 0644 "$script_dir/dbbak.service" /etc/systemd/system/dbbak.service
systemctl daemon-reload
systemctl enable dbbak.service
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
    echo '服务未在 30 秒内通过 readiness 检查；请检查 journalctl -u dbbak.service。' >&2
    exit 1
fi
printf 'DBBak 安装并通过 readiness 检查。主密钥独立于 SQLite，维护备份默认保留在 /var/lib/dbbak/maintenance。\n'
