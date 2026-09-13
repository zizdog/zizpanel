#!/usr/bin/env bash
# =============================================================================
#  tools/remote-install-test.sh —— "一条命令远程安装"路径的端到端回归测试
#
#  为什么需要这个测试：
#   用户在 Mac mini 上真正要执行的是
#       curl -fsSL http://<工作电脑>:8899/install-remote.sh | sudo bash
#   这条路径上有三个独立环节，任何一个坏掉用户都会卡住：
#      1) serve-for-install.sh 生成的服务目录与引导脚本是否自洽；
#      2) 引导脚本能否真的把压缩包下载下来、校验、解压、找到 install.sh；
#      3) 解压出来的 install.sh 能否在干净机器上装成。
#   本测试把这三步全部串起来跑一遍，并且**不需要 root**：
#   引导脚本在 ZIZPANEL_SANDBOX=1 下会跳过 root 检查，
#   后续断言完全复用 tools/sandbox-install-test.sh（含 nginx 隔离检查）。
#
#  用法：bash tools/remote-install-test.sh
#        KEEP_REMOTE_TEST=1 bash tools/remote-install-test.sh   # 保留现场
#  退出码 0 表示通过。
# =============================================================================
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORK="${REMOTE_TEST_DIR:-/tmp/zizpanel-remote-test}"

FAILURES=0
C_GREEN=$'\033[32m'; C_RED=$'\033[31m'; C_BOLD=$'\033[1m'; C_RESET=$'\033[0m'
pass() { printf '  %s✓%s %s\n' "$C_GREEN" "$C_RESET" "$1"; }
fail() { printf '  %s✗%s %s\n' "$C_RED" "$C_RESET" "$1"; FAILURES=$((FAILURES + 1)); }
step() { printf '\n%s▸ %s%s\n' "$C_BOLD" "$1" "$C_RESET"; }

HTTP_PID=""
cleanup() {
  [ -n "$HTTP_PID" ] && kill "$HTTP_PID" 2>/dev/null
  pkill -f "http.server .*--directory $WORK" 2>/dev/null || true
  if [ "${KEEP_REMOTE_TEST:-0}" = "1" ]; then
    printf '\n现场已保留：%s\n' "$WORK"
  else
    rm -rf "$WORK"
  fi
  return 0
}
trap cleanup EXIT INT TERM

rm -rf "$WORK"; mkdir -p "$WORK"

# 取一个空闲端口（避免和本机已在运行的服务撞车）
free_port() {
  python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
}
HTTP_PORT="${REMOTE_TEST_HTTP_PORT:-$(free_port)}"
PANEL_PORT="${REMOTE_TEST_PANEL_PORT:-$(free_port)}"

VERSION="$(grep -oE '[0-9]+\.[0-9]+\.[0-9]+' "$REPO/internal/version/version.go" | head -1)"
[ -n "$VERSION" ] || { echo "无法读取版本号"; exit 1; }

printf '%s=== 远程一键安装端到端测试 ===%s\n' "$C_BOLD" "$C_RESET"
printf '版本: %s\nHTTP 端口: %s\n面板沙箱端口: %s\n临时目录: %s\n' \
  "$VERSION" "$HTTP_PORT" "$PANEL_PORT" "$WORK"

# ------------------------------------------------------------- 构建发布包 --
step "构建发布包"
if ( cd "$REPO" && make release > "$WORK/release.log" 2>&1 ); then
  pass "make release 成功"
else
  fail "make release 失败："
  tail -25 "$WORK/release.log" | sed 's/^/      /'
fi

PKG_ARM="$REPO/dist/release/zizpanel_${VERSION}_darwin_arm64.tar.gz"
if [ -f "$PKG_ARM" ]; then
  pass "产出 arm64 安装包（$(du -h "$PKG_ARM" | cut -f1 | tr -d ' ')）"
else
  fail "缺少 $PKG_ARM"
  exit 1
fi

# 压缩包内容必须完整：二进制、安装脚本、以及 install.sh 在目标机上要用的工具脚本。
for f in ./zizpanel ./zizpanel-helper ./install.sh; do
  if tar -tzf "$PKG_ARM" | grep -qx "$f"; then
    pass "安装包内含 ${f#./}"
  else
    fail "安装包缺少 ${f#./}"
  fi
done

# 所有 install.sh 在目标机上要用到的工具脚本都必须在包里。
# 曾经漏打过 takeover-panel-entry.sh，结果是装完后 /_panel 入口根本不存在，
# 而安装过程"成功"退出——用户只能看到一个打不开的地址。
PKG_LIST="$WORK/pkg-list.txt"
tar -tzf "$PKG_ARM" > "$PKG_LIST"
for t in tools/takeover-panel-entry.sh tools/panel-entry.awk tools/check-shell-vars.py tools/server-mode.sh; do
  if grep -qx "./$t" "$PKG_LIST"; then
    pass "安装包内含 $t"
  else
    fail "安装包缺少 $t —— 目标机上会静默降级"
  fi
done

# --------------------------------------------------------- 启动共享服务 --
step "启动远程安装共享服务"
HTTP_LOG="$WORK/http.log"
ZP_PORT="$HTTP_PORT" bash "$REPO/tools/serve-for-install.sh" --no-build \
  --bind 127.0.0.1 --port "$HTTP_PORT" > "$HTTP_LOG" 2>&1 &
HTTP_PID=$!

READY=0
for _ in $(seq 1 40); do
  if curl -fsS --max-time 1 "http://127.0.0.1:$HTTP_PORT/install-remote.sh" -o /dev/null 2>/dev/null; then
    READY=1; break
  fi
  sleep 0.25
done
if [ "$READY" = "1" ]; then
  pass "服务已就绪（http://127.0.0.1:$HTTP_PORT/）"
else
  fail "服务未就绪，日志末尾："
  tail -20 "$HTTP_LOG" | sed 's/^/      /'
  exit 1
fi

# ------------------------------------------------------------- 引导脚本 --
step "校验生成的引导脚本"
BOOT="$WORK/install-remote.sh"
if curl -fsSL --max-time 10 "http://127.0.0.1:$HTTP_PORT/install-remote.sh" -o "$BOOT"; then
  pass "引导脚本可下载（$(wc -l < "$BOOT" | tr -d ' ') 行）"
else
  fail "引导脚本下载失败"
  exit 1
fi

if bash -n "$BOOT" 2>"$WORK/boot-syntax.err"; then
  pass "引导脚本语法正确"
else
  fail "引导脚本语法错误："
  sed 's/^/      /' "$WORK/boot-syntax.err"
fi

if grep -q "^ZP_BASE_URL=\"http://127.0.0.1:$HTTP_PORT\"$" "$BOOT"; then
  pass "服务地址已正确注入（不是占位符）"
else
  fail "服务地址注入不正确，实际内容："
  grep -n '^ZP_BASE_URL=' "$BOOT" | sed 's/^/      /'
fi
if grep -q "^ZP_VERSION=\"$VERSION\"$" "$BOOT"; then
  pass "版本号已正确注入（${VERSION}）"
else
  fail "版本号注入不正确"
fi
# 拼接生成的脚本必须恰好只有一个 set -uo pipefail：多一个说明 sed 剥离没生效，
# 少一个说明前面的 set 被误删（会导致变量未定义悄悄通过）。
SET_COUNT="$(grep -c '^set -uo pipefail$' "$BOOT" || true)"
if [ "$SET_COUNT" = "1" ]; then
  pass "set -uo pipefail 数量正确（1）"
else
  fail "set -uo pipefail 出现 $SET_COUNT 次（应为 1），拼接逻辑有问题"
fi
if grep -q 'exec bash "$WORK/install.sh" "\$@"' "$BOOT"; then
  pass "参数会原样透传给 install.sh（--server-mode 等可用）"
else
  fail "引导脚本没有透传参数给 install.sh"
fi

# ------------------------------------------------------- 安装包可下载性 --
step "校验安装包下载与完整性"
HDR="$WORK/head.txt"
if curl -fsSI --max-time 10 "http://127.0.0.1:$HTTP_PORT/pkg/zizpanel_${VERSION}_darwin_arm64.tar.gz" > "$HDR"; then
  pass "安装包可直接下载（$(grep -i '^content-length:' "$HDR" | awk '{print $2}' | tr -d '\r' | awk '{printf "%.1f MB", $1/1048576}')）"
else
  fail "安装包下载失败（引导脚本在目标机上同样会失败）"
fi

if curl -fsSL --max-time 60 "http://127.0.0.1:$HTTP_PORT/pkg/SHA256SUMS.txt" -o "$WORK/SHA256SUMS.txt"; then
  pass "校验和清单可下载"
  curl -fsSL --max-time 120 "http://127.0.0.1:$HTTP_PORT/pkg/zizpanel_${VERSION}_darwin_arm64.tar.gz" -o "$WORK/dl.tar.gz" || fail "下载安装包失败"
  WANT="$(awk '/arm64/{print $1}' "$WORK/SHA256SUMS.txt" | head -1)"
  GOT="$(shasum -a 256 "$WORK/dl.tar.gz" 2>/dev/null | awk '{print $1}')"
  if [ -n "$WANT" ] && [ "$WANT" = "$GOT" ]; then
    pass "下载到的安装包与校验和一致"
  else
    fail "校验和不一致（期望 ${WANT:-空}，实际 ${GOT:-空}）"
  fi
else
  fail "校验和清单下载失败"
fi

# ----------------------------------------------- 参数透传（真跑一遍） --
step "校验参数透传行为"
# install.sh 对未知参数是宽容的（故意不报错），所以不能靠非法参数验证透传。
# 改用 --help：它会打印 install.sh 自己的用法说明并退出 0，
# 只要看到那段说明，就证明 "$@" 真的从引导脚本传到了 install.sh。
if ZIZPANEL_SANDBOX=1 bash "$BOOT" --help > "$WORK/help.log" 2>&1; then
  if grep -q 'ZIZPANEL_DOWNLOAD_BASE' "$WORK/help.log"; then
    pass "参数已透传到 install.sh（--help 打印了安装脚本自身的用法）"
  else
    fail "参数疑似没有透传，输出末尾："
    tail -5 "$WORK/help.log" | sed 's/^/      /'
  fi
else
  fail "boot 脚本 --help 非零退出，末尾："
  tail -5 "$WORK/help.log" | sed 's/^/      /'
fi

# ------------------------------------------------- 完整安装（复用断言） --
step "端到端安装：下载 → 解压 → 安装（沙箱，无 root）"
INSTALL_LOG="$WORK/sandbox-e2e.log"
SUB_OK=0
if ZIZPANEL_INSTALL_SH="$BOOT" \
   ZIZPANEL_SANDBOX_DIR="$WORK/sandbox" \
   SANDBOX_PORT="$PANEL_PORT" \
   bash "$REPO/tools/sandbox-install-test.sh" > "$INSTALL_LOG" 2>&1; then
  SUB_OK=1
  SUB_ASSERTS="$(grep -c '✓' "$INSTALL_LOG" | tr -d ' ')"
  pass "远程引导安装全流程通过（子断言 $SUB_ASSERTS 项：plist/sudoers/幂等/卸载等）"
else
  fail "远程引导安装失败，日志末尾："
  tail -35 "$INSTALL_LOG" | sed 's/^/      /'
fi

# 防"假通过"：子测试必须真的跑了足够多的断言，而不是中途静默退出。
if [ "$SUB_OK" = "1" ]; then
  SUB_ASSERTS="${SUB_ASSERTS:-0}"
  if [ "$SUB_ASSERTS" -ge 25 ]; then
    pass "子测试断言数量合理（$SUB_ASSERTS ≥ 25），不是空跑"
  else
    fail "子测试只跑了 $SUB_ASSERTS 项断言，疑似中途静默退出"
  fi
fi

# --------------------------------------------------- 生产环境未被污染 --
step "确认测试没有污染生产环境"
PROD_VHOST="/opt/homebrew/etc/nginx/vhosts/000-default.conf"
if [ -f "$PROD_VHOST" ]; then
  if grep -q "127.0.0.1:${PANEL_PORT}" "$PROD_VHOST" 2>/dev/null; then
    fail "生产 nginx vhost 被测试改写了（端口 ${PANEL_PORT}）！"
  else
    pass "生产 nginx vhost 未被改动"
  fi
else
  pass "本机无生产 vhost（跳过）"
fi

# ------------------------------------------------------------------ 汇总 --
printf '\n%s' "$C_BOLD"
if [ "$FAILURES" -eq 0 ]; then
  printf '%s=== 远程一键安装测试全部通过 ✅ ===%s\n' "$C_GREEN" "$C_RESET"
  exit 0
fi
printf '%s=== 远程一键安装测试失败 %s 项 ❌ ===%s\n' "$C_RED" "$FAILURES" "$C_RESET"
exit 1
