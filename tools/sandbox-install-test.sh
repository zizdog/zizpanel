#!/usr/bin/env bash
# =============================================================================
#  tools/sandbox-install-test.sh —— 安装脚本的端到端回归测试
#
#  问题：真实安装要写 /Library/LaunchDaemons、/etc/sudoers.d、/Applications，
#  必须有 root。CI 或开发机上没有 root 时就没法验证安装脚本。
#
#  做法：把系统路径全部重定向到沙箱目录，并用桩件替换 launchctl / visudo /
#  socketfilterfw 等系统命令，然后跑**真实的 install.sh**。
#  这样能验证：路径处理、plist 内容、sudoers 内容、幂等性、探活逻辑、
#  卸载流程 —— 唯一没被验证的是"macOS 真的会加载这个 plist"，
#  那部分由用户在真机执行 sudo bash install.sh 完成。
#
#  用法：bash tools/sandbox-install-test.sh
#  退出码 0 表示通过。
# =============================================================================
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# 固定沙箱路径（不用 mktemp）：出问题时可以事后直接进目录查看现场。
# 每次运行前清空，避免上一次的残留影响结果。
SANDBOX="${ZIZPANEL_SANDBOX_DIR:-/tmp/zizpanel-sandbox-test}"

# ---------------------------------------------------------------- 互斥锁 --
# 为什么必须有：两个 sandbox-install-test 同时跑（例如 `make check` 与单独一次
# `make install-test` 撞在一起）会共用同一个沙箱目录与 18444 端口，互相把对方的
# 文件删掉、把端口抢走。结果是**幻影失败**：报"升级清掉了用户数据""卸载脚本不存在"，
# 而代码其实没问题 —— 2026-09-17 真的为此误判过一次，两边各看到不同的失败项
# （7 项 / 3 项 / 5 项），追了很久。
# 锁用 mkdir（原子操作）；拿不到锁就**明确报错退出**，不给出一份不可信的结论。
LOCK_DIR="$SANDBOX.lock"
take_lock() {
  if mkdir "$LOCK_DIR" 2>/dev/null; then
    printf '%s\n' "$$" > "$LOCK_DIR/pid" 2>/dev/null || true
    return 0
  fi
  local other=""
  other="$(cat "$LOCK_DIR/pid" 2>/dev/null || echo '')"
  if [ -n "$other" ] && kill -0 "$other" 2>/dev/null; then
    printf '%s\n' "${C_RED:-}✗ 另一次沙箱安装测试正在运行（pid ${other}），共用沙箱 $SANDBOX 会互相踩踏。${C_RESET:-}"
    printf '%s\n' "  等它结束，或用独立沙箱：ZIZPANEL_SANDBOX_DIR=/tmp/zp-\$\$ make install-test"
    exit 2
  fi
  # 残留锁（进程已不在）：清掉重试一次
  rm -rf "$LOCK_DIR"
  mkdir "$LOCK_DIR" 2>/dev/null || { echo "无法获取沙箱锁：$LOCK_DIR"; exit 2; }
  printf '%s\n' "$$" > "$LOCK_DIR/pid" 2>/dev/null || true
  return 0
}
take_lock
release_lock() { rm -rf "$LOCK_DIR" 2>/dev/null || true; }

rm -rf "$SANDBOX"
mkdir -p "$SANDBOX"

# 关键：入口接管会改写 nginx 的 000-default.conf。
# 如果不把它指向沙箱目录，测试就会**污染生产环境的 nginx 配置**
# （曾经真的发生过：沙箱测试把生产 vhost 的 proxy_pass 端口改成了 18444，
#  导致 http://localhost/_panel 全部 502）。
# 这里把 vhost 目录与 nginx 前缀都指向沙箱，
# 让接管流程能在完全隔离的环境里被完整验证。
export ZIZPANEL_NGINX_ETC="$SANDBOX/nginx/etc/nginx"
export ZIZPANEL_VHOST_DIR="$ZIZPANEL_NGINX_ETC/vhosts"
export ZIZPANEL_NGINX_PREFIX="$SANDBOX/nginx"
PORT="${SANDBOX_PORT:-18444}"
# 默认跑仓库里的 install.sh；可以被远程安装测试指向"下载回来的引导脚本"，
# 这样同一套断言就能覆盖 curl 一键安装路径（见 tools/remote-install-test.sh）。
INSTALL_SH="${ZIZPANEL_INSTALL_SH:-$REPO/install.sh}"
FAILURES=0

C_GREEN=$'\033[32m'; C_RED=$'\033[31m'; C_BOLD=$'\033[1m'; C_RESET=$'\033[0m'
C_YELLOW=$'\033[33m'
pass() { printf '  %s✓%s %s\n' "$C_GREEN" "$C_RESET" "$1"; }
fail() { printf '  %s✗%s %s\n' "$C_RED" "$C_RESET" "$1"; FAILURES=$((FAILURES + 1)); }
step() { printf '\n%s▸ %s%s\n' "$C_BOLD" "$1" "$C_RESET"; }

cleanup() {
  release_lock
  if [ "${KEEP_SANDBOX:-0}" = "1" ]; then
    echo "沙箱已保留（进程未杀）：$SANDBOX"
    return 0
  fi
  # 失败时保留沙箱：install-1.log / install-2.log 是排障的唯一凭据，
  # 恰恰在最需要它们的时候被删掉过一次（真发生过：失败现场连日志都没了）。
  # 需要清理时用 KEEP_SANDBOX=0 + 手动 rm，或先看日志。
  if [ "${FAILURES:-0}" -ne 0 ]; then
    echo "${C_YELLOW}测试有失败，沙箱已保留以便排障：$SANDBOX${C_RESET}"
    echo "  安装日志：$SANDBOX/install-1.log  $SANDBOX/install-2.log"
    return 0
  fi
  if [ -f "$SANDBOX/root/run/panel.pid" ]; then
    local pid
    pid="$(cat "$SANDBOX/root/run/panel.pid" 2>/dev/null || echo)"
    [ -n "$pid" ] && kill "$pid" 2>/dev/null
  fi
  pkill -f "zizpanel serve --config $SANDBOX/root" 2>/dev/null || true
  rm -rf "$SANDBOX"
  return 0
}
trap cleanup EXIT INT TERM

printf '%s=== 沙箱安装端到端测试 ===%s\n' "$C_BOLD" "$C_RESET"
printf '沙箱目录: %s\n端口: %s\n' "$SANDBOX" "$PORT"

# ------------------------------------------------------------------ 准备桩件 --
step "准备系统命令桩件"
STUB="$SANDBOX/stubs"
mkdir -p "$STUB"

# launchctl 桩：记录调用，并模拟 bootstrap 时真的启动面板
cat > "$STUB/launchctl" <<'STUBEOF'
#!/usr/bin/env bash
LOG="${SANDBOX}/launchctl.log"
echo "$*" >> "$LOG"
case "$1" in
  bootstrap)
    # 卸载还没完成时 bootstrap 必然失败 —— 真实 launchd 返回
    # "Bootstrap failed: 5: Input/output error"。这是旧代码翻车的地方。
    if [ -f "${SANDBOX}/teardown.pid" ]; then exit 5; fi
    # $2=域 $3=plist  —— 解析 plist 并启动其中的程序，模拟真实 launchd 行为
    plist="$3"
    shift 3
    # 用 PlistBuddy 取出 ProgramArguments 与 EnvironmentVariables
    pb=/usr/libexec/PlistBuddy
    args=()
    i=0
    while true; do
      v="$("$pb" -c "Print :ProgramArguments:$i" "$plist" 2>/dev/null)" || break
      args+=("$v"); i=$((i+1))
    done
    [ ${#args[@]} -eq 0 ] && exit 1
    # 取出环境变量
    envs=()
    for key in $("$pb" -c "Print :EnvironmentVariables" "$plist" 2>/dev/null | sed -n 's/^ *\([A-Za-z_][A-Za-z0-9_]*\) =.*/\1/p'); do
      val="$("$pb" -c "Print :EnvironmentVariables:$key" "$plist" 2>/dev/null)"
      envs+=("$key=$val")
    done
    mkdir -p "${SANDBOX}/root/logs"
    ( env "${envs[@]}" "${args[@]}" >> "${SANDBOX}/root/logs/launchd.out.log" 2>&1 & echo $! > "${SANDBOX}/root/run/launchd.pid" )
    exit 0 ;;
  bootout)
    # 真实 launchd 的 bootout 是**异步**的：命令一返回，服务其实还在卸载中，
    # 这期间 print 仍能看到它、bootstrap 会失败（5: Input/output error）。
    # 桩件必须复现这个时序 —— 否则 install.sh 里"bootout 后立刻 bootstrap"
    # 这个真实事故（本机升级时面板彻底没起来、/_panel 502）在沙箱里永远测不出来。
    touch "${SANDBOX}/teardown.pid"
    (
      sleep 1.2
      if [ -f "${SANDBOX}/root/run/launchd.pid" ]; then
        pid="$(cat "${SANDBOX}/root/run/launchd.pid")"
        pkill -P "$pid" 2>/dev/null
        kill "$pid" 2>/dev/null
        sleep 0.5
        kill -9 "$pid" 2>/dev/null
        rm -f "${SANDBOX}/root/run/launchd.pid"
      fi
      pkill -f "zizpanel serve --config ${SANDBOX}/root" 2>/dev/null
      rm -f "${SANDBOX}/teardown.pid"
    ) &
    exit 0 ;;
  kickstart)
    # 服务已经被摘掉时 kickstart 无从下手 —— 这正是旧代码的失败路径
    [ -f "${SANDBOX}/root/run/launchd.pid" ] || exit 1
    exit 0 ;;
  print)
    # 卸载进行中：仍然报告服务存在（state = SIGTERMed），模拟真实 launchd
    if [ -f "${SANDBOX}/teardown.pid" ]; then
      echo "	state = SIGTERMed"
      exit 0
    fi
    if [ -f "${SANDBOX}/root/run/launchd.pid" ] && kill -0 "$(cat "${SANDBOX}/root/run/launchd.pid")" 2>/dev/null; then
      echo "	state = running"; echo "	pid = $(cat "${SANDBOX}/root/run/launchd.pid")"
      exit 0
    fi
    exit 1 ;;
esac
exit 0
STUBEOF

# visudo 桩：模拟语法检查通过
cat > "$STUB/visudo" <<'STUBEOF'
#!/usr/bin/env bash
# 真实语法校验太复杂，这里至少检查文件非空且含 NOPASSWD
f=""
while [ $# -gt 0 ]; do case "$1" in -f) f="$2"; shift 2;; *) shift;; esac; done
[ -n "$f" ] && [ -s "$f" ] && grep -q NOPASSWD "$f" && exit 0
echo "syntax error" >&2; exit 1
STUBEOF

# socketfilterfw 桩
cat > "$STUB/socketfilterfw" <<'STUBEOF'
#!/usr/bin/env bash
case "$1" in
  --getglobalstate) echo "Firewall is disabled. (State = 0)" ;;
  --add) echo "added" ;;
  --unblockapp) echo "unblocked" ;;
  *) echo "ok" ;;
esac
exit 0
STUBEOF

# codesign 桩
cat > "$STUB/codesign" <<'STUBEOF'
#!/usr/bin/env bash
echo "signed"; exit 0
STUBEOF

# df/id/other 保持真实；oepnssl 不需要
chmod +x "$STUB"/*
export SANDBOX
pass "桩件就绪：$(ls "$STUB" | tr '\n' ' ')"

# 准备沙箱 nginx 环境：vhost 目录 + 最小 nginx.conf + 假 nginx 可执行文件
mkdir -p "$ZIZPANEL_VHOST_DIR" "$SANDBOX/nginx/bin" "$ZIZPANEL_NGINX_ETC/conf.d"
cat > "$ZIZPANEL_VHOST_DIR/000-default.conf" <<'NGINXEOF'
server {
    listen      80 default_server;
    server_name localhost;
    root        /tmp;
    # 模拟旧面板遗留的入口配置块
    location ^~ /_panel {
        alias /tmp/nonexistent-panel;
    }
    location / {
        try_files $uri $uri/ =404;
    }
}
NGINXEOF
cat > "$ZIZPANEL_NGINX_ETC/nginx.conf" <<'NGINXEOF'
worker_processes 1;
events { worker_connections 64; }
http { include conf.d/*.conf; include vhosts/*.conf; }
NGINXEOF
# 假 nginx：-t 一律成功，-s reload 静默。这样接管流程能走完但不碰真实 nginx。
cat > "$SANDBOX/nginx/bin/nginx" <<'STUBEOF'
#!/usr/bin/env bash
# 假 nginx：无论参数顺序如何，只要带 -t 就报告校验通过。
# 注意不能只看 $1 —— 安装脚本用的是 `-c <conf> -t`，-t 不是第一个参数。
for arg in "$@"; do
  if [ "$arg" = "-t" ]; then
    echo "nginx: configuration file test is successful"
    exit 0
  fi
done
exit 0
STUBEOF
chmod +x "$SANDBOX/nginx/bin/nginx"
pass "沙箱 nginx 环境就绪（vhost 目录已隔离，不会影响生产配置）"

# ------------------------------------------------------------------- 构建 --
#
# 两种模式：
#   · 默认：本地构建二进制，install.sh 走"离线安装"这条路径（不起网络，最快）；
#   · ZIZPANEL_SANDBOX_DOWNLOAD_BASE=<镜像>：**不**预置本地二进制，让 install.sh
#     真的去下载 —— 这是验证"GitHub 直连/国内自建镜像"那条路唯一诚实的办法
#     （否则本地二进制会先被 detect_source 命中，下载分支永远跑不到）。
if [ -n "${ZIZPANEL_SANDBOX_DOWNLOAD_BASE:-}" ]; then
  step "跳过本地构建（本次要验证从网络下载）"
  rm -f "$REPO/dist/zizpanel" "$REPO/dist/zizpanel-helper"
else
  step "构建二进制"
  ( cd "$REPO" && PATH="/opt/homebrew/bin:$PATH" GOFLAGS=-mod=mod GOPROXY="${GOPROXY:-https://goproxy.cn,direct}" \
      go build -o dist/zizpanel ./cmd/zizpanel && \
      go build -o dist/zizpanel-helper ./cmd/zizpanel-helper ) || { fail "构建失败"; exit 1; }
  pass "构建完成"
fi

# ------------------------------------------------------------- 第一次安装 --
step "执行安装（沙箱模式）"
export PATH="$STUB:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"
export ZIZPANEL_SANDBOX=1
export ZIZPANEL_ROOT="$SANDBOX/root"
export ZIZPANEL_LISTEN=":$PORT"
export ZIZPANEL_PLIST_DIR="$SANDBOX/Library/LaunchDaemons"
export ZIZPANEL_SUDOERS_DIR="$SANDBOX/etc/sudoers.d"
export ZIZPANEL_LINK_DIR="$SANDBOX/usr/local/bin"
export ZIZPANEL_APPS_DIR="$SANDBOX/Applications"
export ZIZPANEL_SKIP_DEPS=1
export ZIZPANEL_SKIP_FIREWALL=0
if [ -n "${ZIZPANEL_SANDBOX_DOWNLOAD_BASE:-}" ]; then
  export ZIZPANEL_DOWNLOAD_BASE="${ZIZPANEL_SANDBOX_DOWNLOAD_BASE%/}"
else
  export ZIZPANEL_DOWNLOAD_BASE="http://127.0.0.1:1/nonexistent"   # 强制走本地二进制
fi

mkdir -p "$ZIZPANEL_PLIST_DIR" "$ZIZPANEL_SUDOERS_DIR" "$ZIZPANEL_LINK_DIR" "$ZIZPANEL_APPS_DIR"

LOG1="$SANDBOX/install-1.log"
if bash "$INSTALL_SH" > "$LOG1" 2>&1; then
  pass "安装脚本退出码 0"
else
  fail "安装脚本失败（退出码 $?），日志末尾："
  tail -25 "$LOG1" | sed 's/^/      /'
fi

# ------------------------------------------------------------- 安装结果校验 --
step "校验安装结果"

[ -x "$SANDBOX/root/bin/zizpanel" ] && pass "主程序已安装" || fail "主程序缺失"
[ -x "$SANDBOX/root/bin/zizpanel-helper" ] && pass "提权助手已安装" || fail "提权助手缺失"
[ -L "$SANDBOX/usr/local/bin/zizpanel" ] && pass "控制命令软链已建立" || fail "控制命令软链缺失"
[ -f "$ZIZPANEL_PLIST_DIR/cn.zizpanel.panel.plist" ] && pass "LaunchDaemon plist 已生成" || fail "plist 缺失"
[ -f "$ZIZPANEL_SUDOERS_DIR/zizpanel" ] && pass "sudoers 规则已生成" || fail "sudoers 缺失"
[ -d "$SANDBOX/root/data/tls" ] && pass "数据目录结构完整" || fail "数据目录结构不完整"
[ -x "$SANDBOX/root/uninstall.sh" ] && pass "卸载脚本已生成" || fail "卸载脚本缺失"

# plist 内容必须正确（这是最容易出错、且真机上难排查的地方）
PLIST="$ZIZPANEL_PLIST_DIR/cn.zizpanel.panel.plist"
if grep -q "KeepAlive" "$PLIST" && grep -q "<true/>" "$PLIST"; then
  pass "plist 配置了 KeepAlive（崩溃自动重启）"
else
  fail "plist 缺少 KeepAlive"
fi
if grep -q "ZIZPANEL_USER" "$PLIST"; then
  pass "plist 传递了 ZIZPANEL_USER（避免网站目录算成 /var/root/www）"
else
  fail "plist 缺少 ZIZPANEL_USER"
fi
if grep -q -- "--listen" "$PLIST" && grep -q ":$PORT" "$PLIST"; then
  pass "plist 使用指定的监听端口"
else
  fail "plist 监听端口不正确"
fi

# sudoers 必须只授权 helper，绝不能授权通用命令
SUDO_FILE="$ZIZPANEL_SUDOERS_DIR/zizpanel"
if grep -q "zizpanel-helper" "$SUDO_FILE"; then
  pass "sudoers 授权了受限助手"
else
  fail "sudoers 未授权受限助手"
fi
if grep -qE 'NOPASSWD:.*(/bin/(ba)?sh|/usr/bin/\*|ALL)$' "$SUDO_FILE"; then
  fail "sudoers 授权过宽（存在提权风险）"
else
  pass "sudoers 未授权通用 shell（安全边界正确）"
fi

# 生成的配置必须指向真实用户的家目录，而不是 /var/root
if [ -f "$SANDBOX/root/data/config.json" ]; then
  WWW_ROOT="$(python3 -c "import json;print(json.load(open('$SANDBOX/root/data/config.json'))['www_root'])" 2>/dev/null || echo)"
  if [ "$WWW_ROOT" = "/Users/$(id -un)/www" ]; then
    pass "网站根目录正确：$WWW_ROOT"
  else
    fail "网站根目录错误：${WWW_ROOT}（应为 /Users/$(id -un)/www）"
  fi
  ACCESS="$(python3 -c "import json;print(json.load(open('$SANDBOX/root/data/config.json'))['access_mode'])" 2>/dev/null || echo)"
  [ "$ACCESS" = "any" ] && pass "默认允许远程访问" || fail "默认访问模式错误：$ACCESS"
else
  fail "config.json 未生成"
fi

# ------------------------------------------------- 干跑演练（交互分支） --
# 2026-09-17 真机安装后用户明确要求（两条都是"安装界面不要这一步"）：
#   · 「网络授权 / 免授权访问内网段」：**安装界面不出现**（面板「系统设置 → 局域网访问」
#     里点一下就行，而且必须重启才生效，放在安装收尾会误导）—— 只留 ZP_LAN_PREAUTH=1 自动化通路。
#   · 「是否开启 SSH」：**安装界面不问**（面板「系统设置 → 远程登录」已验证可用）。
# 这一段把"默认不问"和"显式要求时才做"两个方向都锁住；另外静态锁住两个真机 bug：
#   python3 一次都不许调用（会弹 macOS 开发者工具对话框）、面板 API 必须带后缀。
step "干跑演练：默认不问 SSH/预授权 + 两条真机 bug 的静态锁"

# ---- 静态锁 1：install.sh 不许再调用 python3（弹窗根源）----
if grep -nE 'command -v python3|python3 -c|python3 -' "$INSTALL_SH" | grep -vE '^[[:space:]]*#' >/dev/null 2>&1; then
  fail "install.sh 里仍有 python3 调用（会弹「命令行开发者工具」对话框）："
  grep -nE 'command -v python3|python3 -c|python3 -' "$INSTALL_SH" | grep -vE '^[[:space:]]*#' | sed 's/^/      /'
else
  pass "静态锁：install.sh 不调用 python3（全新机器上不再弹开发者工具对话框）"
fi

# ---- 静态锁 2：调面板接口必须带后缀（panelGate 之外一律 404）----
if grep -qE '127\.0\.0\.1:\$\{PANEL_PORT\}/api/v1/(setup|login|system)' "$INSTALL_SH"; then
  fail "仍有不带面板后缀的接口直连（会 404，表现为「面板又问一次账号」）："
  grep -nE '127\.0\.0\.1:\$\{PANEL_PORT\}/api/v1/(setup|login|system)' "$INSTALL_SH" | sed 's/^/      /'
else
  pass "静态锁：面板接口调用统一走 panel_api_base（带后缀），不再 404"
fi

DRUN_LOG="$SANDBOX/dryrun.log"
: > "$DRUN_LOG"
# 注意：ZIZPANEL_ROOT 必须指向**独立目录**（不能是 $SANDBOX/root）——
# 否则"干跑零副作用"的断言会读到真实安装写下的 config.json，误报成干跑写了文件。
rm -rf "$SANDBOX/dryrun-root"

# ① 默认干跑（无 ZP_SSH / 无 ZP_LAN_PREAUTH）→ 两个步骤都**不问、不做**
ZIZPANEL_SANDBOX=1 ZIZPANEL_DRY_RUN=1 ZP_PASS=testpass123 \
  ZIZPANEL_ROOT="$SANDBOX/dryrun-root" ZIZPANEL_LISTEN=":$PORT" \
  bash "$INSTALL_SH" > "$DRUN_LOG" 2>&1 || true
if grep -q "需要时在面板「系统设置 → 远程登录」一键开启" "$DRUN_LOG"; then
  pass "默认：安装界面不询问 SSH，并指明去面板「系统设置 → 远程登录」开"
else
  fail "默认：没有看到「不问 SSH、去面板开」的说明"
fi
if grep -qE "现在开启 SSH|现在开启「允许免授权访问内网段」" "$DRUN_LOG"; then
  fail "默认：安装界面竟然还在问 SSH / 网络授权（用户要求删掉这两步）"
else
  pass "默认：安装界面不问 SSH、也不问「免授权访问内网段」"
fi

# ② 显式 ZP_LAN_PREAUTH=1 → 才走预授权（且要看到三件事 + 走面板接口的计划）
ZP_LAN_PREAUTH=1 ZP_LAN_CIDR="192.168.1.0/24" ZIZPANEL_SANDBOX=1 ZIZPANEL_DRY_RUN=1 ZP_PASS=testpass123 \
  ZIZPANEL_ROOT="$SANDBOX/dryrun-root" ZIZPANEL_LISTEN=":$PORT" \
  bash "$INSTALL_SH" > "$DRUN_LOG" 2>&1 || true
if grep -q "要重启电脑才生效" "$DRUN_LOG" && grep -q "Plan B（回环转发器）" "$DRUN_LOG" \
   && grep -q "面板「系统设置」里手动打开" "$DRUN_LOG"; then
  pass "显式要求预授权时：三件事（需重启 / 不必现在重启+Plan B / 系统设置里可开）逐字齐全"
else
  fail "显式要求预授权时：三件事文案缺失"
fi
if grep -q "将调用面板接口 POST /api/v1/system/settings/lan-preauth" "$DRUN_LOG"; then
  pass "显式要求预授权时：走面板已有接口（lan-preauth），不是另造一套 defaults 逻辑"
else
  fail "显式要求预授权时：没看到走面板接口的干跑行"
fi
# 干跑绝不能真的写任何东西（用独立 root 验证，见上）
if [ -f "$SANDBOX/dryrun-root/data/config.json" ]; then
  fail "干跑竟然写了 config.json（干跑必须零副作用）"
else
  pass "干跑零副作用（未写 config.json）"
fi

# ③ 显式 ZP_SSH=1 → 才开启 SSH 分支（干跑只打印计划）
ZP_SSH=1 ZIZPANEL_SANDBOX=1 ZIZPANEL_DRY_RUN=1 ZP_PASS=testpass123 \
  ZIZPANEL_ROOT="$SANDBOX/root" ZIZPANEL_LISTEN=":$PORT" \
  bash "$INSTALL_SH" > "$DRUN_LOG" 2>&1 || true
if grep -q "server-mode.sh --ssh-only" "$DRUN_LOG"; then
  pass "显式要求 SSH 时：走 server-mode.sh --ssh-only（只动 SSH，不改电源设置）"
else
  fail "显式要求 SSH 时：没看到 --ssh-only 计划"
fi

# ④ 静态锁 3：curl|bash 路径必须把 SCRIPT_DIR 指到解压目录（否则 tools/ 不会被安装，
#    真机表现为"找不到 server-mode.sh"、以及一键 LNMP 缺 system-services.sh）
if grep -q 'SCRIPT_DIR="\$TMP_DIR"' "$INSTALL_SH"; then
  pass "静态锁：下载路径把 SCRIPT_DIR 指向解包目录（tools/ 会被正确安装）"
else
  fail "下载路径没有设置 SCRIPT_DIR → 运行时工具不会被复制（真机踩到过）"
fi

# ⑤ 管理员账号：**安装界面不问**（面板初始化向导负责）；只有显式给凭据时才建账号
#    —— 这一轮刻意**不给 ZP_PASS**，走"用户真机的那条路"
ZIZPANEL_SANDBOX=1 ZIZPANEL_DRY_RUN=1 \
  ZIZPANEL_ROOT="$SANDBOX/dryrun-root" ZIZPANEL_LISTEN=":$PORT" ZP_SUFFIX=testsfx \
  bash "$INSTALL_SH" > "$DRUN_LOG" 2>&1 || true
if grep -q "管理员账号留给面板" "$DRUN_LOG"; then
  pass "默认：安装界面不问用户名/口令，明确告知去面板初始化向导设置"
else
  fail "默认：没有看到「账号留给面板」的说明"
fi
if grep -qE "设置登录口令|管理员用户名（面板登录用|再输入一次确认" "$DRUN_LOG"; then
  fail "默认：安装界面竟然还在问账号/口令（用户要求删掉）"
else
  pass "默认：不再出现任何账号/口令提问"
fi
# 显式给凭据（自动化）时才建账号
ZP_USER=ciadmin ZP_PASS=testpass123 ZIZPANEL_SANDBOX=1 ZIZPANEL_DRY_RUN=1 \
  ZIZPANEL_ROOT="$SANDBOX/dryrun-root" ZIZPANEL_LISTEN=":$PORT" ZP_SUFFIX=testsfx \
  bash "$INSTALL_SH" > "$DRUN_LOG" 2>&1 || true
if grep -q "将创建管理员账号：ciadmin（来自 ZP_USER/ZP_PASS）" "$DRUN_LOG"; then
  pass "显式给 ZP_USER/ZP_PASS 时：安装期创建账号（自动化通路仍在）"
else
  fail "显式给凭据时没有看到创建账号的干跑行"
fi

step "校验沙箱未污染生产环境"
PROD_VHOST="/opt/homebrew/etc/nginx/vhosts/000-default.conf"
if [ -f "$PROD_VHOST" ]; then
  if grep -q "127.0.0.1:${PORT}" "$PROD_VHOST" 2>/dev/null; then
    fail "生产 nginx 配置被测试污染了（出现了沙箱端口 ${PORT}）"
  else
    pass "生产 nginx 配置未被改动"
  fi
fi
if [ -d "$ZIZPANEL_VHOST_DIR" ] && grep -q "127.0.0.1:${PORT}" "$ZIZPANEL_VHOST_DIR/000-default.conf" 2>/dev/null; then
  pass "沙箱内 vhost 正确写入了入口配置"
else
  fail "沙箱 vhost 未写入入口配置（接管流程未在沙箱内生效）"
fi

# 真实 HTTP 探活
step "HTTP 探活"
health=""
for _ in $(seq 1 20); do
  health="$(curl -fsSk --max-time 2 "https://127.0.0.1:$PORT/api/v1/health" 2>/dev/null || echo)"
  [ -n "$health" ] && break
  sleep 1
done
if echo "$health" | grep -q '"status":"ok"'; then
  pass "面板 HTTPS 接口可访问：$health"
else
  fail "面板接口不可访问（端口 ${PORT}）"
  tail -20 "$SANDBOX/root/logs/launchd.out.log" 2>/dev/null | sed 's/^/      /'
fi

# 自签证书必须已生成且可用
if [ -f "$SANDBOX/root/data/tls/panel.crt" ]; then
  if /usr/bin/openssl x509 -in "$SANDBOX/root/data/tls/panel.crt" -noout -checkend 86400 >/dev/null 2>&1; then
    SAN="$(/usr/bin/openssl x509 -in "$SANDBOX/root/data/tls/panel.crt" -noout -ext subjectAltName 2>/dev/null | tail -1)"
    pass "自签证书有效（SAN:$(echo "$SAN" | tr -s ' ' | cut -c1-60)…）"
  else
    fail "证书无效或即将过期"
  fi
else
  fail "未生成自签证书"
fi

# ---------------------------------------------------------------- 幂等性 --
step "重复安装（幂等性验证）"
# 造一个标记，确认升级时数据不被清掉
echo "sentinel-$(date +%s)" > "$SANDBOX/root/data/USER_DATA_SENTINEL"
LOG2="$SANDBOX/install-2.log"
if bash "$INSTALL_SH" > "$LOG2" 2>&1; then
  pass "重复安装退出码 0"
else
  fail "重复安装失败，日志末尾："
  tail -30 "$LOG2" | sed 's/^/      /'
  echo "    [诊断] 进程: $(pgrep -f "zizpanel serve --config ${SANDBOX}/root" 2>/dev/null | tr '\n' ' ' || echo 无)"
  echo "    [诊断] 端口: $(lsof -nP -iTCP:${PORT} -sTCP:LISTEN 2>/dev/null | wc -l | tr -d ' ' || echo 0) 行"
  echo "    [诊断] 完整启动日志:"
  cat "${SANDBOX}/root/logs/launchd.out.log" 2>/dev/null | tail -25 | sed 's/^/        /'
  echo "    [诊断] launchctl 调用:"
  cat "${SANDBOX}/launchctl.log" 2>/dev/null | tail -8 | sed 's/^/        /'
fi
if [ -f "$SANDBOX/root/data/USER_DATA_SENTINEL" ]; then
  pass "已有数据被保留（未被清空）"
else
  fail "重复安装清掉了用户数据"
fi
if [ -f "$SANDBOX/root/bin/zizpanel.bak" ]; then
  pass "升级前备份了旧二进制"
else
  fail "升级未备份旧二进制"
fi
if grep -q "升级模式" "$LOG2"; then
  pass "脚本识别出升级模式"
else
  fail "脚本未识别升级模式"
fi

# 重复安装后仍可访问
health2=""
for _ in $(seq 1 15); do
  health2="$(curl -fsSk --max-time 2 "https://127.0.0.1:$PORT/api/v1/health" 2>/dev/null || echo)"
  [ -n "$health2" ] && break
  sleep 1
done
echo "$health2" | grep -q '"status":"ok"' && pass "升级后面板仍可访问" || fail "升级后面板不可访问"

# ---------------------------------------------------------------- 卸载 --
step "卸载流程（保留数据）"
# 先停掉桩 launchctl 管理的进程，避免端口残留
if ZIZPANEL_SANDBOX=1 bash "$SANDBOX/root/uninstall.sh" > "$SANDBOX/uninstall.log" 2>&1; then
  pass "卸载脚本退出码 0"
else
  fail "卸载脚本失败："
  tail -15 "$SANDBOX/uninstall.log" | sed 's/^/      /'
fi
[ ! -f "$ZIZPANEL_PLIST_DIR/cn.zizpanel.panel.plist" ] && pass "plist 已移除" || fail "plist 未移除"
[ ! -f "$ZIZPANEL_SUDOERS_DIR/zizpanel" ] && pass "sudoers 规则已移除" || fail "sudoers 未移除"
[ ! -f "$SANDBOX/root/bin/zizpanel" ] && pass "程序已移除" || fail "程序未移除"
[ -f "$SANDBOX/root/data/config.json" ] && pass "数据按预期保留（卸载不删数据）" || fail "数据被意外删除"

step "彻底卸载（--purge）"
# 重新安装一次再 purge。
# 安装日志写进沙箱；purge 日志另存一份到固定路径 —— 因为 --purge 成功时会
# 把沙箱删掉，失败时 sandbox 里那份也常常跟着没（排障时最需要它）。
bash "$INSTALL_SH" > "$SANDBOX/install-3.log" 2>&1 || true
PURGE_LOG="/tmp/zizpanel-purge.log"
if ZIZPANEL_SANDBOX=1 bash "$SANDBOX/root/uninstall.sh" --purge > "$PURGE_LOG" 2>&1; then
  if [ ! -d "$SANDBOX/root" ]; then
    pass "--purge 彻底删除安装目录"
  else
    fail "--purge 后目录仍存在"
  fi
else
  fail "--purge 执行失败（日志：${PURGE_LOG}）"
  sed 's/^/      /' "$PURGE_LOG" 2>/dev/null | tail -15
  if [ -f "$SANDBOX/install-3.log" ]; then
    printf '      重新安装日志（$SANDBOX/install-3.log）末尾：\n'
    tail -15 "$SANDBOX/install-3.log" | sed 's/^/      /'
  fi
fi

# ---------------------------------------------------------------- 汇总 --
printf '\n%s────────────────────────────%s\n' "$C_BOLD" "$C_RESET"
if [ "$FAILURES" -eq 0 ]; then
  printf '%s✅ 沙箱安装测试全部通过%s\n' "$C_GREEN" "$C_RESET"
  exit 0
fi
printf '%s❌ 沙箱安装测试失败 %d 项%s\n' "$C_RED" "$FAILURES" "$C_RESET"
exit 1
