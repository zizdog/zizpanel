#!/usr/bin/env bash
# =============================================================================
#  tools/install-from-remote.sh —— 通过 curl 在目标 Mac 上一键拉取并安装
#
#  用途：在工作电脑上跑一个临时 HTTP 服务，然后在 Mac mini 上用一条命令安装，
#  不需要 U 盘、不需要先把文件拷过去。
#
#  工作电脑（本机）执行：
#      cd <项目目录> && bash tools/serve-for-install.sh
#      它会启动一个只服务本项目目录的 HTTP 服务，并打印 mini 上要执行的命令
#
#  Mac mini 上执行（把 URL 换成上一步打印的）：
#      curl -fsSL http://<工作电脑IP>:8899/install-remote.sh | sudo bash
#
#  这个脚本做的事：
#    1. 检查是否为 macOS、是否有管理员权限
#    2. 下载发布包（或从源码构建）
#    3. 调用安装脚本，并把参数原样透传（例如 --server-mode）
#
#  参数：全部透传给 install.sh，例如
#      curl -fsSL .../install-remote.sh | sudo bash -s -- --server-mode
# =============================================================================
set -uo pipefail

# 发布包地址：由 serve-for-install.sh 注入，或通过环境变量指定
ZP_BASE_URL="${ZP_BASE_URL:-}"
ZP_VERSION="${ZP_VERSION:-0.1.0}"

C_RESET=$'\033[0m'; C_RED=$'\033[31m'; C_GREEN=$'\033[32m'
C_YELLOW=$'\033[33m'; C_BLUE=$'\033[34m'; C_BOLD=$'\033[1m'
info() { printf '%s[信息]%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }
ok()   { printf '%s[完成]%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn() { printf '%s[警告]%s %s\n' "$C_YELLOW" "$C_RESET" "$*" >&2; }
die()  { printf '%s[错误]%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; exit 1; }

printf '%s%s  ZizPanel 远程安装程序%s\n' "$C_BOLD" "$C_BLUE" "$C_RESET"
printf '  （下载 → 校验 → 安装，中途无需人工干预；仅首次需要输入开机密码）\n\n'

[ "$(uname -s)" = "Darwin" ] || die "本面板仅支持 macOS"
# 沙箱模式（仅供工具链自测）跳过 root 检查；生产安装必须 sudo。
if [ "${ZIZPANEL_SANDBOX:-0}" != "1" ]; then
  [ "$(id -u)" -eq 0 ] || die "请用 sudo 执行：curl -fsSL <url> | sudo bash"
fi

if [ -z "$ZP_BASE_URL" ]; then
  die "未指定 ZP_BASE_URL。请用 tools/serve-for-install.sh 生成正确的命令。"
fi

ARCH="$(uname -m)"
case "$ARCH" in
  arm64) PKG_ARCH="arm64" ;;
  x86_64) PKG_ARCH="amd64" ;;
  *) die "不支持的架构: $ARCH" ;;
esac
ok "系统：macOS $(sw_vers -productVersion)（${ARCH}）"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/zizpanel-install.XXXXXX")"
# 清理工作目录。**保留现场**用 ZP_KEEP_INSTALL_DIR=1（排查安装失败时要用：
# 里面是解压好的安装包 + install.sh 的真实运行环境）。
cleanup() {
  if [ "${ZP_KEEP_INSTALL_DIR:-0}" = "1" ]; then
    printf '\n安装工作目录已保留：%s\n' "$WORK"
    return 0
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

# 顺手清掉**上一次**遗留的同名前缀目录（超过 24 小时的）。
#
# 为什么需要这道保险：见文件末尾 —— 老版本用 `exec` 调 install.sh，
# trap 永远不会执行，于是每次安装都留下一个 ~96 MiB 的目录
#（2026-09-18 真机事故：开发机上累积 1424 个、97 GB，把系统盘撑到 99%）。
# 已经发出去的老版本装机还会留，所以新版本要能把那些陈旧残留清掉。
find "${TMPDIR:-/tmp}" -maxdepth 1 -name 'zizpanel-install.*' -type d -mtime +0 \
  -exec rm -rf {} + 2>/dev/null || true

PKG="zizpanel_${ZP_VERSION}_darwin_${PKG_ARCH}.tar.gz"
URL="$ZP_BASE_URL/pkg/$PKG"
info "下载安装包：$URL"
if ! curl -fL --progress-bar --max-time 600 -o "$WORK/$PKG" "$URL"; then
  die "下载失败。请确认工作电脑上的服务仍在运行、且网络可达。"
fi
info "解压安装包…"
tar -xzf "$WORK/$PKG" -C "$WORK" || die "解压失败，文件可能不完整"
[ -x "$WORK/zizpanel" ] || die "安装包里缺少可执行文件"
[ -x "$WORK/zizpanel-helper" ] || die "安装包里缺少提权助手"
[ -f "$WORK/install.sh" ] || die "安装包里缺少 install.sh"
ok "安装包已就绪"

info "开始安装（参数：$*）…"
cd "$WORK" || die "无法进入工作目录"

# ⚠️ 这里**不能**用 `exec`。
#
# 老实现是 `exec bash "$WORK/install.sh" "$@"` —— exec 会把本进程**替换**掉，
# 上面的 trap 随之消失，于是每次安装都在 $TMPDIR 留下一个 ~96 MiB 的
# `zizpanel-install.XXXXXX`（解压好的安装包 + 沙箱 root）。
# 2026-09-18 真机事故：开发机上累积 **1424 个、97 GB**，把系统盘撑到 99%，
# 门禁因此报"沙箱安装失败: No space left on device"（当时还误判成代码问题）。
#
# 现在改成普通调用：EXIT trap 会在脚本退出时执行；退出码照原样传出去；
# Ctrl-C 时两者同在前台进程组，信号照样能到 install.sh。
bash "$WORK/install.sh" "$@"
rc=$?
cleanup   # 显式清一次（EXIT trap 兜底；ZP_KEEP_INSTALL_DIR=1 时只打印路径）
exit "$rc"
