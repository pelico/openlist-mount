#!/usr/bin/env bash
# openlist-mount 卸载脚本
# 用法: curl -fsSL .../uninstall.sh | bash
#       KEEP_DATA=0 curl -fsSL .../uninstall.sh | bash   # 同时删除数据目录

set -euo pipefail

INSTALL_BIN="${INSTALL_BIN:-/usr/local/bin/openlist-mount}"
DATA_DIR="${DATA_DIR:-/var/lib/openlist-mount}"
KEEP_DATA="${KEEP_DATA:-1}"   # 1=保留(默认,保护密码/配置)  0=连数据一起删

if [ -t 1 ]; then
  C_GREEN=$'\e[32m'; C_YELLOW=$'\e[33m'; C_BLUE=$'\e[34m'; C_RESET=$'\e[0m'
else
  C_GREEN=''; C_YELLOW=''; C_BLUE=''; C_RESET=''
fi
log()  { printf "${C_BLUE}[%s]${C_RESET} %s\n" "$(date +%H:%M:%S)" "$*"; }
ok()   { printf "${C_GREEN}[%s] ✓ %s${C_RESET}\n" "$(date +%H:%M:%S)" "$*"; }
warn() { printf "${C_YELLOW}[%s] ! %s${C_RESET}\n" "$(date +%H:%M:%S)" "$*"; }

[ "$(id -u)" -ne 0 ] && { echo "请用 root 运行"; exit 1; }

# 1. 停服务 + 卸载所有 rclone 挂载点
if systemctl list-unit-files 2>/dev/null | grep -q openlist-mount.service; then
  log "停止 openlist-mount 服务..."
  systemctl stop    openlist-mount.service 2>/dev/null || true
  systemctl disable openlist-mount.service 2>/dev/null || true

  # 用二进制自身的 stop 把 rclone 子进程一起停掉
  if [ -x "$INSTALL_BIN" ]; then
    :
  fi
  # 兜底:把残留的 rclone mount 全部 umount
  for mp in $(mount 2>/dev/null | grep -E 'rclone|fuse' | awk '{print $3}'); do
    warn "残留挂载点 $mp,尝试 umount..."
    umount -f "$mp" 2>/dev/null || umount -l "$mp" 2>/dev/null || true
  done
  ok "服务已停止"
fi

# 2. 删 systemd unit
if [ -f /etc/systemd/system/openlist-mount.service ]; then
  rm -f /etc/systemd/system/openlist-mount.service
  systemctl daemon-reload
  ok "已移除 systemd unit"
fi

# 3. 删二进制
if [ -f "$INSTALL_BIN" ]; then
  rm -f "$INSTALL_BIN"
  ok "已删除 $INSTALL_BIN"
fi

# 4. 数据目录
if [ -d "$DATA_DIR" ]; then
  if [ "$KEEP_DATA" = "0" ]; then
    rm -rf "$DATA_DIR"
    ok "已删除数据目录 $DATA_DIR"
  else
    warn "已保留数据目录 $DATA_DIR (含 config.json / rclone.conf / 日志)"
    warn "如需彻底删除: rm -rf $DATA_DIR"
  fi
fi

# 5. 提示 rclone/fuse3 保留
echo
echo "rclone + fuse3 仍然保留(rclone 是系统软件,可能有其它用途)"
echo "卸载完成"
