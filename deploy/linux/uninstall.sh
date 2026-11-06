#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

assume_yes=0
if [[ ${1:-} == '--yes' ]]; then assume_yes=1; shift; fi
[[ $# -eq 0 ]] || { printf '用法：%s [--yes]\n' "${0##*/}" >&2; exit 2; }
[[ $EUID -eq 0 ]] || { echo '必须以 root 执行。' >&2; exit 1; }
command -v systemctl >/dev/null 2>&1 || { echo '缺少 systemctl。' >&2; exit 1; }

cat <<'EOF'
即将卸载 DBBak：
  - 停止并禁用 dbbak.service，删除 unit、程序和备份辅助脚本
  - 保留专用用户 dbbak、/etc/dbbak/dbbak.env 和 /var/lib/dbbak 全部内容
  - 明确保留 SQLite、独立主密钥、业务备份、日志和维护归档
  - 不删除任何用户数据，不执行递归目录清理
EOF
if [[ $assume_yes -eq 0 ]]; then
    read -r -p '确认执行请输入 YES：' answer
    [[ $answer == 'YES' ]] || { echo '已取消，未做更改。'; exit 0; }
fi

systemctl disable --now dbbak.service 2>/dev/null || true
rm -f -- /etc/systemd/system/dbbak.service /usr/local/bin/dbbak /usr/local/bin/dbbak.next /usr/local/sbin/dbbak-backup
systemctl daemon-reload
systemctl reset-failed dbbak.service 2>/dev/null || true
printf '服务与程序已卸载；配置、主密钥、元数据和全部备份仍保留。需要再次部署时请先保护这些文件。\n'
