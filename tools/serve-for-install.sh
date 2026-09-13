#!/usr/bin/env bash
# =============================================================================
#  tools/serve-for-install.sh —— 在工作电脑上开一个只读 HTTP 服务，
#  让 Mac mini 能用一条 curl 命令安装 ZizPanel。
#
#  用法（在工作电脑上）：
#      bash tools/serve-for-install.sh              # 自动探测局域网 IP，端口 8899
#      bash tools/serve-for-install.sh --port 9000
#      bash tools/serve-for-install.sh --no-build   # 不重新构建，用已有 dist/release
#
#  然后按屏幕提示，在 Mac mini 上粘贴那条 curl 命令即可。
#
#  安全说明：
#    * 服务只绑定到本机局域网 IP，只暴露 dist/release、tools/ 两个目录，
#      不接受任何写操作，Ctrl-C 即关闭。
#    * 面板安装包本身不含密钥；安装完成后建议立即关掉本服务。
# =============================================================================
set -uo pipefail

PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PORT="${ZP_PORT:-8899}"
DO_BUILD=1
BIND_ADDR=""

C_RESET=$'\033[0m'; C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'
C_BLUE=$'\033[34m'; C_BOLD=$'\033[1m'; C_RED=$'\033[31m'
info() { printf '%s[信息]%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }
ok()   { printf '%s[完成]%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn() { printf '%s[警告]%s %s\n' "$C_YELLOW" "$C_RESET" "$*" >&2; }
die()  { printf '%s[错误]%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --port)     PORT="$2"; shift 2 ;;
    --bind)     BIND_ADDR="$2"; shift 2 ;;
    --no-build) DO_BUILD=0; shift ;;
    -h|--help)  sed -n '2,20p' "$0"; exit 0 ;;
    *) die "未知参数：$1" ;;
  esac
done

case "$PORT" in
  ''|*[!0-9]*) die "端口必须是数字：$PORT" ;;
esac
[ "$PORT" -ge 1024 ] && [ "$PORT" -le 65535 ] || die "端口需在 1024-65535 之间"

# ---------------------------------------------------------------- 探测本机 IP --
if [ -z "$BIND_ADDR" ]; then
  for iface in en0 en1 en2 en3 bridge0; do
    addr="$(ipconfig getifaddr "$iface" 2>/dev/null || true)"
    if [ -n "$addr" ]; then BIND_ADDR="$addr"; BIND_IFACE="$iface"; break; fi
  done
fi
[ -n "$BIND_ADDR" ] || die "无法自动探测局域网 IP，请用 --bind <本机IP> 手动指定"
info "使用网卡 ${BIND_IFACE:-手动指定}，地址 $BIND_ADDR"

# ------------------------------------------------------------------- 构建包 --
VERSION="$(grep -oE '[0-9]+\.[0-9]+\.[0-9]+' "$PROJECT_DIR/internal/version/version.go" | head -1)"
[ -n "$VERSION" ] || die "无法读取版本号"
RELDIR="$PROJECT_DIR/dist/release"

if [ "$DO_BUILD" -eq 1 ]; then
  info "构建发布包（版本 ${VERSION}，arm64 + amd64）…"
  ( cd "$PROJECT_DIR" && make release >/tmp/zp-release.log 2>&1 ) \
    || { tail -30 /tmp/zp-release.log; die "构建失败，日志见 /tmp/zp-release.log"; }
  ok "构建完成"
else
  info "跳过构建，使用已有 dist/release"
fi

ARM_PKG="$RELDIR/zizpanel_${VERSION}_darwin_arm64.tar.gz"
[ -f "$ARM_PKG" ] || die "找不到 ${ARM_PKG}，请先执行 make release"

# ------------------------------------------------- 组装一个临时服务根目录 --
# 之所以复制而不是直接服务项目目录：避免把源码、git 元数据、配置暴露到网络上。
SERVEDIR="$(mktemp -d "${TMPDIR:-/tmp}/zizpanel-serve.XXXXXX")"
cleanup() {
  rm -rf "$SERVEDIR"
  printf '\n'
  info "已停止共享，临时目录已清理。"
}
trap cleanup EXIT INT TERM

mkdir -p "$SERVEDIR/pkg" "$SERVEDIR/bin"
cp "$RELDIR"/zizpanel_"$VERSION"_darwin_*.tar.gz "$SERVEDIR/pkg/"
ln -sf "zizpanel_${VERSION}_darwin_arm64.tar.gz" "$SERVEDIR/pkg/zizpanel_latest_darwin_arm64.tar.gz" 2>/dev/null || true
ln -sf "zizpanel_${VERSION}_darwin_amd64.tar.gz" "$SERVEDIR/pkg/zizpanel_latest_darwin_amd64.tar.gz" 2>/dev/null || true
shasum -a 256 "$RELDIR"/zizpanel_"$VERSION"_darwin_*.tar.gz | sed "s|$RELDIR/||" > "$SERVEDIR/pkg/SHA256SUMS.txt"

# 安装引导脚本：注入本次服务的真实地址与版本
cat > "$SERVEDIR/install-remote.sh" <<REMOTE_EOF
#!/usr/bin/env bash
# 由 tools/serve-for-install.sh 自动生成 —— 在目标 Mac 上以 sudo 执行
set -uo pipefail
ZP_BASE_URL="http://${BIND_ADDR}:${PORT}"
ZP_VERSION="${VERSION}"
REMOTE_EOF
# 追加共享的安装逻辑（去掉原文件的 shebang/set 行与占位变量赋值）
sed -e '1{/^#!/d;}' \
    -e '/^ZP_BASE_URL=/d' \
    -e '/^ZP_VERSION=/d' \
    -e '/^set -uo pipefail$/d' \
    "$PROJECT_DIR/tools/install-from-remote.sh" >> "$SERVEDIR/install-remote.sh"
chmod 0755 "$SERVEDIR/install-remote.sh"

# 用 python3 起一个最小只读 HTTP 服务（macOS 自带 python3，无需额外依赖）
command -v python3 >/dev/null 2>&1 || die "需要 python3（macOS 自带，或用 xcode-select --install 安装）"

echo
printf '%s%s  ZizPanel 远程安装服务已启动%s\n' "$C_BOLD" "$C_GREEN" "$C_RESET"
echo   "  ─────────────────────────────────────────────────────────────"
printf '  服务地址：   %shttp://%s:%s/%s\n' "$C_BLUE" "$BIND_ADDR" "$PORT" "$C_RESET"
printf '  面板版本：   %s\n' "$VERSION"
printf '  服务目录：   %s（只读，仅含安装包）\n' "$SERVEDIR"
echo   "  ─────────────────────────────────────────────────────────────"
echo
printf '%s  请在 Mac mini 上执行下面这条命令：%s\n' "$C_BOLD" "$C_RESET"
echo
printf '%s    curl -fsSL http://%s:%s/install-remote.sh | sudo bash%s\n' "$C_GREEN" "$BIND_ADDR" "$PORT" "$C_RESET"
echo
printf '  想同时开启服务器模式（禁止休眠/自动重启等）：\n'
printf '%s    curl -fsSL http://%s:%s/install-remote.sh | sudo bash -s -- --server-mode%s\n' "$C_GREEN" "$BIND_ADDR" "$PORT" "$C_RESET"
echo
printf '  校验安装包完整性（可选，在 mini 上）：\n'
printf '%s    curl -fsSL http://%s:%s/pkg/SHA256SUMS.txt%s\n' "$C_BLUE" "$BIND_ADDR" "$PORT" "$C_RESET"
echo
printf '  按 %sCtrl-C%s 结束共享。\n' "$C_BOLD" "$C_RESET"
echo

cd "$SERVEDIR" || die "无法进入服务目录"
exec python3 -m http.server "$PORT" --bind "$BIND_ADDR" --directory "$SERVEDIR"
