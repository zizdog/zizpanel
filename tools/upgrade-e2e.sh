#!/usr/bin/env bash
# =============================================================================
#  tools/upgrade-e2e.sh —— 在线升级的真实端到端演练（对已安装的面板跑）
#
#  它做的事：
#    1. 造一个"更高版本"的发布包（默认 0.2.0），并用发布私钥签名清单
#    2. 在工作机上起一个只读 HTTP 源
#    3. 通过面板 API 登录 → 配置升级源 → 检查更新 → 下载暂存 → 执行升级
#    4. 等面板重启，确认版本真的变成了新版本（看门狗判定为 success）
#    5. 校验脚本自身逻辑：确认升级后 /_panel 与数据都还在
#
#  为什么值得有：单元测试只能证明"逻辑对"，证明不了
#  "真的替换了正在运行的二进制、launchd 真的把它拉起来了"。
#  这几步只有在真实安装上才能验证。
#
#  用法：
#    bash tools/upgrade-e2e.sh                     # 升级到 0.2.0
#    bash tools/upgrade-e2e.sh --to 0.3.0
#    bash tools/upgrade-e2e.sh --restore           # 演练后恢复为本机源码构建的版本
#
#  注意：升级成功后 /opt/zizpanel/bin/zizpanel 会变成 0.2.0 那个构建。
#  用 --restore 可以把它换回当前源码构建（版本号回到 0.1.0）。
# =============================================================================
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TO_VERSION="${TO_VERSION:-0.2.0}"
PORT="${UPGRADE_E2E_PORT:-18877}"
PANEL="${PANEL_URL:-https://127.0.0.1:8443}"
USER_NAME="${ZP_USER:-admin}"
PASSWORD="${ZP_PASS:-}"
RESTORE_ONLY=0

C_GREEN=$'\033[32m'; C_RED=$'\033[31m'
C_BOLD=$'\033[1m'; C_RESET=$'\033[0m'; C_BLUE=$'\033[34m'
pass() { printf '  %s✓%s %s\n' "$C_GREEN" "$C_RESET" "$1"; }
fail() { printf '  %s✗%s %s\n' "$C_RED" "$C_RESET" "$1"; FAILURES=$((FAILURES + 1)); }
step() { printf '\n%s▸ %s%s\n' "$C_BOLD" "$1" "$C_RESET"; }
info() { printf '  %s[信息]%s %s\n' "$C_BLUE" "$C_RESET" "$1"; }
FAILURES=0

while [ $# -gt 0 ]; do
  case "$1" in
    --to) TO_VERSION="$2"; shift 2 ;;
    --restore) RESTORE_ONLY=1; shift ;;
    *) echo "未知参数: $1" >&2; exit 2 ;;
  esac
done

HTTP_PID=""
cleanup() {
  [ -n "$HTTP_PID" ] && kill "$HTTP_PID" 2>/dev/null
  return 0
}
trap cleanup EXIT INT TERM

# ------------------------------------------------------------------ 辅助 --
# api METHOD PATH [JSON] —— 带会话与 CSRF 调用面板 API
COOKIE_JAR="$(mktemp)"
api() {
  local method="$1" path="$2" body="${3:-}"
  local csrf
  csrf="$(awk '/zp_csrf/ {print $7}' "$COOKIE_JAR" | tail -1)"
  local args=(-sSk --max-time 30 -X "$method" "$PANEL$path"
              -b "$COOKIE_JAR" -c "$COOKIE_JAR" -H 'Accept: application/json')
  [ -n "$csrf" ] && args+=(-H "X-CSRF-Token: $csrf")
  if [ -n "$body" ]; then
    args+=(-H 'Content-Type: application/json' --data "$body")
  fi
  curl "${args[@]}"
}

json_get() { python3 -c "import json,sys;d=json.load(sys.stdin);
p='$1'.split('.')
for k in p:
    d = d.get(k) if isinstance(d, dict) else None
    if d is None: break
print(d if d is not None else '')" 2>/dev/null; }

# 等待面板恢复，并返回它报告的版本
wait_panel_version() {
  local tries="${1:-60}" v=""
  for _ in $(seq 1 "$tries"); do
    v="$(curl -sSk --max-time 3 "$PANEL/api/v1/health" 2>/dev/null |
         python3 -c "import json,sys;print(json.load(sys.stdin)['data']['version'])" 2>/dev/null || true)"
    if [ -n "$v" ]; then printf '%s' "$v"; return 0; fi
    sleep 1
  done
  return 1
}

printf '%s=== 在线升级真实演练 ===%s\n' "$C_BOLD" "$C_RESET"
printf '目标版本: %s\n面板地址: %s\n' "$TO_VERSION" "$PANEL"

CUR_VER="$(wait_panel_version 3 || true)"
[ -n "$CUR_VER" ] || { echo "面板不可达，演练中止"; exit 1; }
info "当前版本：v$CUR_VER"

# ------------------------------------------------------------ 恢复模式 --
if [ "$RESTORE_ONLY" = 1 ]; then
  step "恢复为本机源码构建的版本"
  ( cd "$REPO" && make dev >/dev/null 2>&1 )
  printf '%s\n' "${SUDO_PASS:-}" | sudo -S bash "$REPO/install.sh" >/dev/null 2>&1 || \
    { fail "恢复失败（需要 root）"; exit 1; }
  if [ "$(wait_panel_version 30)" = "0.1.0" ]; then
    pass "已恢复为源码构建版本 v0.1.0"
  else
    fail "恢复后版本不是 0.1.0"
  fi
  exit $((FAILURES > 0))
fi

# ---------------------------------------------------- 造一个更高版本 --
step "构建 v$TO_VERSION 发布包并签名"
# 关键：清单里的资源 URL 必须指向**本次演练的本地升级源**。
# 默认值是 GitHub Releases，那样下载会 404（我第一次就踩了）。
if ! ( cd "$REPO" && make VERSION="$TO_VERSION" \
         RELEASE_BASE_URL="http://127.0.0.1:$PORT" \
         release >/tmp/zp-upgrade-e2e-release.log 2>&1 ); then
  fail "构建失败，日志尾部："
  tail -15 /tmp/zp-upgrade-e2e-release.log | sed 's/^/      /'
  exit 1
fi
REL="$REPO/dist/release"
[ -f "$REL/manifest.json" ] || { fail "没有生成 manifest.json"; exit 1; }
[ -f "$REL/manifest.json.sig" ] || { fail "没有生成签名 manifest.json.sig"; exit 1; }
pass "已生成带签名的清单 v$TO_VERSION"

# ------------------------------------------------------------ 起升级源 --
step "启动升级源（只读 HTTP）"
( cd "$REL" && exec python3 -m http.server "$PORT" --bind 127.0.0.1 ) >/tmp/zp-upgrade-e2e-http.log 2>&1 &
HTTP_PID=$!
for _ in $(seq 1 30); do
  curl -fsS --max-time 1 "http://127.0.0.1:$PORT/manifest.json" -o /dev/null 2>/dev/null && break
  sleep 0.3
done
if curl -fsS --max-time 2 "http://127.0.0.1:$PORT/manifest.json" -o /dev/null; then
  pass "升级源已就绪：http://127.0.0.1:$PORT"
  info "（回环地址，看门狗与面板都在本机，无需放通防火墙）"
else
  fail "升级源未就绪"
  exit 1
fi

# ---------------------------------------------------------------- 登录 --
step "登录面板"
COOKIE_JAR="$(mktemp)"
if [ -z "$PASSWORD" ]; then
  printf '  请输入面板管理员密码（不回显）：'
  read -rs PASSWORD
  printf '\n'
fi
# 用 heredoc 构造 JSON：嵌套引号挤在一行里迟早被 shell 吃掉一层
LOGIN_BODY="$(python3 - "$USER_NAME" "$PASSWORD" <<'JSONEOF'
import json, sys
print(json.dumps({"username": sys.argv[1], "password": sys.argv[2]}))
JSONEOF
)"
LOGIN="$(curl -sSk -X POST "$PANEL/api/v1/login" -c "$COOKIE_JAR" \
  -H 'Content-Type: application/json' --data "$LOGIN_BODY")"
if echo "$LOGIN" | grep -q '"ok":true'; then
  pass "已登录为 $USER_NAME"
else
  fail "登录失败：$LOGIN"
  exit 1
fi

# ---------------------------------------------------------------- 检查 --
step "检查更新"
CHECK="$(api POST /api/v1/system/upgrade/check "{\"source\":\"http://127.0.0.1:$PORT\"}")"
if echo "$CHECK" | grep -q "\"latest\":\"$TO_VERSION\""; then
  pass "检测到新版本 v$TO_VERSION"
else
  fail "检查更新未发现目标版本：$CHECK"
  exit 1
fi
if echo "$CHECK" | grep -q "$TO_VERSION"; then :; fi

# ---------------------------------------------------------------- 暂存 --
step "下载并校验升级包"
STAGE="$(api POST /api/v1/system/upgrade/stage "{\"source\":\"http://127.0.0.1:$PORT\"}")"
if echo "$STAGE" | grep -q '"staged":true'; then
  pass "升级包已下载、验签、解包、试运行通过"
else
  fail "暂存失败：$STAGE"
  exit 1
fi

# ---------------------------------------------------------------- 升级 --
step "执行升级（面板将重启）"
APPLY="$(api POST /api/v1/system/upgrade/apply '{}')"
if echo "$APPLY" | grep -q '"accepted":true'; then
  pass "升级已受理"
else
  fail "升级未被受理：$APPLY"
  exit 1
fi

step "等待面板重启并由看门狗判定结果"
# 必须等版本**发生变化**，不能只等"能读到版本"：
# 升级是异步的（要先把 202 响应发出去），发起后旧进程还会存活近一秒，
# 这时立刻轮询会读到旧版本，从而把一次成功的升级误判成失败。
# 我第一次就踩了这个竞态。
NEW_VER=""
for _ in $(seq 1 120); do
  v="$(curl -sSk --max-time 3 "$PANEL/api/v1/health" 2>/dev/null |
       python3 -c "import json,sys;print(json.load(sys.stdin)['data']['version'])" 2>/dev/null || true)"
  if [ -n "$v" ] && [ "$v" != "$CUR_VER" ]; then NEW_VER="$v"; break; fi
  sleep 1
done
if [ -z "$NEW_VER" ]; then
  # 没等到变化：可能自动回滚了，读一下状态给出结论
  ST_JSON="$(curl -sSk --max-time 5 "$PANEL/api/v1/system/upgrade" -b "$COOKIE_JAR" 2>/dev/null || true)"
  info "未观察到版本变化，升级状态：$(printf '%s' "$ST_JSON" | head -c 300)"
  NEW_VER="$(wait_panel_version 5 || echo '')"
  info "面板当前版本：v${NEW_VER:-未知}"
fi
if [ -z "$NEW_VER" ]; then
  fail "面板在 120 秒内没有恢复 —— 升级可能失败了"
  printf '    排查：sudo launchctl print system/cn.zizpanel.panel\n'
  printf '          cat /opt/zizpanel/work/upgrade/watchdog.*.log\n'
  printf '          cat /opt/zizpanel/work/upgrade/result.txt\n'
  exit 1
fi
if [ "$NEW_VER" = "$TO_VERSION" ]; then
  pass "面板已运行 v$NEW_VER —— 升级链路完整打通"
else
  fail "面板恢复后版本是 v${NEW_VER}，期望 v${TO_VERSION}（可能已自动回滚，需检查日志）"
fi

# ------------------------------------------------------- 升级后功能抽查 --
step "升级后功能抽查"
CODE="$(curl -sSk -o /dev/null -w '%{http_code}' "$PANEL/api/v1/health")"
[ "$CODE" = "200" ] && pass "健康检查 200" || fail "健康检查返回 $CODE"

if [ -f /opt/zizpanel/data/panel.db ]; then
  pass "数据目录完好（panel.db 仍在）"
else
  fail "panel.db 不见了"
fi

VHOST="/opt/homebrew/etc/nginx/vhosts/000-default.conf"
if [ -f "$VHOST" ] && grep -q '/_panel' "$VHOST"; then
  pass "nginx 入口 /_panel 仍在"
else
  fail "nginx 入口配置异常"
fi

# 用户名下的真实站点不能受影响
if curl -s -o /dev/null -w '%{http_code}' http://zizdog.cn/ 2>/dev/null | grep -q 200; then
  pass "既有站点 zizdog.cn 仍返回 200"
else
  info "（zizdog.cn 未返回 200，请确认这是预期情况）"
fi

printf '\n%s────────────────────────────%s\n' "$C_BOLD" "$C_RESET"
if [ "$FAILURES" -eq 0 ]; then
  printf '%s✅ 在线升级真实演练通过%s\n' "$C_GREEN" "$C_RESET"
  printf '   当前面板版本：v%s\n' "$NEW_VER"
  printf '   恢复方式：bash tools/upgrade-e2e.sh --restore\n'
  exit 0
fi
printf '%s❌ 演练发现 %d 个问题%s\n' "$C_RED" "$FAILURES" "$C_RESET"
exit 1
