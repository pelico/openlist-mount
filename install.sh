#!/usr/bin/env bash
# openlist-mount 一键安装脚本
# 用法:
#   curl -fsSL https://raw.githubusercontent.com/<owner>/<repo>/main/install.sh | bash
#   或自定义仓库:
#   curl -fsSL https://raw.githubusercontent.com/<owner>/<repo>/main/install.sh | REPO=<owner>/<repo> bash
#   或本地:
#   REPO="" BIN_DIR=./dist ./install.sh

set -euo pipefail

# ---- 配置项 ----
REPO="${REPO:-}"                            # 例: yourname/openlist-mount;为空时从 BIN_DIR 取本地二进制
BIN_DIR="${BIN_DIR:-}"                       # 本地二进制目录(无网络/GitHub 时用)
VERSION="${VERSION:-latest}"                 # latest 或 v0.1.0
INSTALL_BIN="${INSTALL_BIN:-/usr/local/bin/openlist-mount}"
DATA_DIR="${DATA_DIR:-/var/lib/openlist-mount}"
ADDR="${ADDR:-:7777}"
RCLONE_BIN="${RCLONE_BIN:-rclone}"

# 颜色
if [ -t 1 ]; then
  C_GREEN=$'\e[32m'; C_RED=$'\e[31m'; C_YELLOW=$'\e[33m'; C_BLUE=$'\e[34m'; C_RESET=$'\e[0m'
else
  C_GREEN=''; C_RED=''; C_YELLOW=''; C_BLUE=''; C_RESET=''
fi

log()  { printf "${C_BLUE}[%s]${C_RESET} %s\n" "$(date +%H:%M:%S)" "$*"; }
ok()   { printf "${C_GREEN}[%s] ✓ %s${C_RESET}\n" "$(date +%H:%M:%S)" "$*"; }
warn() { printf "${C_YELLOW}[%s] ! %s${C_RESET}\n" "$(date +%H:%M:%S)" "$*"; }
die()  { printf "${C_RED}[%s] ✗ %s${C_RESET}\n" "$(date +%H:%M:%S)" "$*"; exit 1; }

# 临时文件路径,失败时清理
TMP_BIN=""
cleanup() {
  rc=$?
  if [ $rc -ne 0 ]; then
    warn "安装中断(exit=$rc),清理临时文件..."
    [ -n "$TMP_BIN" ] && [ -e "$TMP_BIN" ] && rm -f "$TMP_BIN"
    # 如果 install_bin 已写入但服务未起来,移除避免残留
    if [ -n "${INSTALL_DONE:-}" ] && [ ! -f /etc/systemd/system/openlist-mount.service ]; then
      rm -f "$INSTALL_BIN" 2>/dev/null || true
    fi
  fi
  exit $rc
}
trap cleanup EXIT INT TERM

# ---- 1. 权限检查 ----
if [ "$(id -u)" -ne 0 ]; then
  die "请用 root 运行: sudo curl ... | sudo bash"
fi

# ---- 2. 架构检测 ----
ARCH_RAW=$(uname -m)
case "$ARCH_RAW" in
  aarch64|arm64)  ARCH=arm64 ;;
  armv7l|armv6l|armhf) ARCH=armhf; [ "$ARCH_RAW" = "armv6l" ] && warn "armv6 可能需要 GOARM=6 重新编译,尝试 armhf..." ;;
  x86_64|amd64)   ARCH=arm64; warn "x86_64 主机不在本工具设计目标,但二进制仍可运行" ;;
  *)              die "不支持的架构: $ARCH_RAW" ;;
esac
ok "检测到架构: $ARCH_RAW → $ARCH"

# ---- 3. 包管理器 ----
PKG=""
if   command -v apt-get >/dev/null 2>&1; then PKG=apt
elif command -v dnf     >/dev/null 2>&1; then PKG=dnf
elif command -v yum     >/dev/null 2>&1; then PKG=yum
elif command -v pacman  >/dev/null 2>&1; then PKG=pacman
else die "找不到 apt/dnf/yum/pacman,请手动安装 rclone + fuse3"; fi
ok "包管理器: $PKG"

# ---- 4. 安装 rclone + fuse3 ----
install_deps() {
  case "$PKG" in
    apt)
      export DEBIAN_FRONTEND=noninteractive
      apt-get update -qq
      apt-get install -y --no-install-recommends rclone fuse3 ca-certificates curl
      ;;
    dnf)
      dnf install -y rclone fuse3 ca-certificates curl
      ;;
    yum)
      yum install -y rclone fuse3 ca-certificates curl
      ;;
    pacman)
      pacman -Sy --noconfirm rclone fuse3 ca-certificates curl
      ;;
  esac
}
if ! command -v rclone >/dev/null 2>&1 || ! command -v fusermount >/dev/null 2>&1 && \
   ! command -v fusermount3 >/dev/null 2>&1; then
  log "安装依赖 rclone + fuse3..."
  install_deps
fi
command -v rclone >/dev/null 2>&1 || die "rclone 安装失败,请手动 apt install rclone"
command -v fusermount3 >/dev/null 2>&1 || command -v fusermount >/dev/null 2>&1 || die "fuse3 安装失败"
ok "rclone: $(rclone version 2>&1 | head -1)"

# ---- 5. 启用 user_allow_other ----
FUSE_CONF=/etc/fuse.conf
if [ -f "$FUSE_CONF" ]; then
  if grep -qE '^[[:space:]]*user_allow_other' "$FUSE_CONF"; then
    ok "fuse.conf 已启用 user_allow_other"
  else
    if grep -qE '^[[:space:]]*#user_allow_other' "$FUSE_CONF"; then
      sed -i 's|^[[:space:]]*#user_allow_other|user_allow_other|' "$FUSE_CONF"
      ok "fuse.conf: 取消注释 user_allow_other"
    else
      echo "user_allow_other" >> "$FUSE_CONF"
      ok "fuse.conf: 追加 user_allow_other"
    fi
  fi
else
  warn "fuse.conf 不存在,创建一个最小配置"
  mkdir -p /etc
  printf 'user_allow_other\n' > "$FUSE_CONF"
fi

# ---- 6. 下载/拷贝二进制 ----
TMP_BIN=$(mktemp)

if [ -n "$BIN_DIR" ] && [ -f "$BIN_DIR/openlist-mount-$ARCH" ]; then
  log "从本地 $BIN_DIR 复制二进制..."
  cp "$BIN_DIR/openlist-mount-$ARCH" "$TMP_BIN"
elif [ -n "$REPO" ]; then
  URL_BASE="https://github.com/$REPO/releases/download"
  if [ "$VERSION" = "latest" ]; then
    URL="$URL_BASE/latest/download/openlist-mount-$ARCH"
  else
    URL="$URL_BASE/$VERSION/openlist-mount-$ARCH"
  fi
  log "从 $URL 下载二进制..."
  if ! curl -fL --retry 3 --retry-delay 2 -o "$TMP_BIN" "$URL"; then
    die "下载失败,请检查 REPO=$REPO 是否正确,或在 release 上传了 openlist-mount-$ARCH"
  fi
else
  die "无法获取二进制: 请设置 REPO=<owner>/<repo> 或 BIN_DIR=./dist"
fi

chmod +x "$TMP_BIN"
# 简易校验:是 ELF 可执行
if ! head -c 4 "$TMP_BIN" | grep -q $'\x7fELF'; then
  warn "下载内容看起来不是 ELF,可能是 404 HTML 页面"
  head -c 200 "$TMP_BIN"
  die "二进制校验失败"
fi
ok "二进制就绪: $(du -h "$TMP_BIN" | cut -f1)"

install -m 0755 "$TMP_BIN" "$INSTALL_BIN"
INSTALL_DONE=1
ok "已安装到 $INSTALL_BIN"

# ---- 7. 数据目录 ----
mkdir -p "$DATA_DIR"
chmod 0755 "$DATA_DIR"
ok "数据目录: $DATA_DIR (config.json / rclone.conf / 日志都在这里)"

# ---- 8. systemd unit ----
UNIT_FILE=/etc/systemd/system/openlist-mount.service
cat > "$UNIT_FILE" <<EOF
[Unit]
Description=openlist-mount - mount openlist WebDAV via rclone
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=$INSTALL_BIN -addr $ADDR -data $DATA_DIR -rclone $RCLONE_BIN
Restart=on-failure
RestartSec=3s
AmbientCapabilities=CAP_SYS_ADMIN
CapabilityBoundingSet=CAP_SYS_ADMIN

[Install]
WantedBy=multi-user.target
EOF
ok "已生成 systemd unit: $UNIT_FILE"

# ---- 9. 启动 ----
systemctl daemon-reload
systemctl enable openlist-mount.service >/dev/null 2>&1
systemctl restart openlist-mount.service

# 等服务起来
for i in 1 2 3 4 5; do
  if systemctl is-active --quiet openlist-mount.service; then
    ok "服务已启动"
    break
  fi
  sleep 1
done

if ! systemctl is-active --quiet openlist-mount.service; then
  warn "服务启动异常,最近 20 行日志:"
  journalctl -u openlist-mount.service -n 20 --no-pager || true
  die "请检查日志后: systemctl status openlist-mount"
fi

# ---- 10. 打印访问信息 ----
IPS=$(hostname -I 2>/dev/null | tr ' ' '\n' | head -3)
PORT=${ADDR##*:}
[ "$PORT" = "$ADDR" ] && PORT=7777

cat <<EOF

${C_GREEN}安装完成!${C_RESET}

Web 控制台:
  http://localhost:$PORT
$(for ip in $IPS; do echo "  http://$ip:$PORT"; done)

常用命令:
  systemctl status openlist-mount
  systemctl restart openlist-mount
  journalctl -u openlist-mount -f

数据目录(包含 config.json / rclone.conf / 各挂载日志):
  $DATA_DIR

下一步: 浏览器打开 Web 控制台 → 填入 openlist 的 WebDAV 地址
(形如 http://<openlist-ip>:5244/dav) → 勾选"允许其他用户访问"和
"开机自动挂载" → 保存 → 点"启动"。

EOF
