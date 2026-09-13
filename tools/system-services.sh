#!/usr/bin/env bash
# =============================================================================
#  tools/system-services.sh —— 把 brew 服务装成「系统级 LaunchDaemon」
#
#  要解决的问题（方案 B）：
#    `brew services start nginx` 装的是 ~/Library/LaunchAgents 下的**用户级**服务，
#    只有该用户**登录进图形界面**之后才会运行。对一台不接显示器、
#    重启后停在登录界面的服务器来说，这等于"开机不自启"。
#
#    改成系统级 LaunchDaemon 后：不依赖任何人登录，开机即运行。
#
#  为什么不是 `sudo brew services`：
#    那会把服务以 **root** 身份运行。而 mysql 明确拒绝以 root 启动
#    （"Please read Security section ... to find out how to run mysqld as root"），
#    php-fpm 以 root 跑语义也不对。所以正确做法是：
#    系统级 plist + `UserName` 指定真实用户 —— 开机自启与降权运行兼得。
#
#  关键前提（已实测）：brew 生成的服务 plist 里
#  `LimitLoadToSessionType` 本来就包含 `System` 与 `Background`，
#  因此可以原样放进 /Library/LaunchDaemons 由系统域加载，无需改写程序参数。
#
#  用法：
#    sudo bash tools/system-services.sh                 # 处理默认三个服务
#    sudo bash tools/system-services.sh nginx php@8.3   # 指定服务
#    sudo bash tools/system-services.sh --dry-run       # 只显示要做什么
# =============================================================================
set -uo pipefail

DEFAULT_FORMULAS=(nginx php@8.3 mysql@8.4)
FORMULAS=()
DRY_RUN=0
FAILURES=0

C_GREEN=$'\033[32m'; C_RED=$'\033[31m'; C_BLUE=$'\033[34m'
C_YELLOW=$'\033[33m'; C_BOLD=$'\033[1m'; C_RESET=$'\033[0m'
pass() { printf '  %s✓%s %s\n' "$C_GREEN" "$C_RESET" "$1"; }
fail() { printf '  %s✗%s %s\n' "$C_RED" "$C_RESET" "$1"; FAILURES=$((FAILURES + 1)); }
info() { printf '  %s[信息]%s %s\n' "$C_BLUE" "$C_RESET" "$1"; }
warn() { printf '  %s[警告]%s %s\n' "$C_YELLOW" "$C_RESET" "$1"; }
step() { printf '\n%s▸ %s%s\n' "$C_BOLD" "$1" "$C_RESET"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    -*) echo "未知参数: $1" >&2; exit 2 ;;
    *) FORMULAS+=("$1"); shift ;;
  esac
done
[ ${#FORMULAS[@]} -gt 0 ] || FORMULAS=("${DEFAULT_FORMULAS[@]}")

# 真实用户：服务要以它身份运行（不是 root）
REAL_USER="${SUDO_USER:-}"
if [ -z "$REAL_USER" ] || [ "$REAL_USER" = "root" ]; then
  REAL_USER="$(stat -f '%Su' /dev/console 2>/dev/null || echo "")"
fi
[ -n "$REAL_USER" ] && [ "$REAL_USER" != "root" ] || {
  echo "无法确定真实用户，请用 sudo 执行" >&2; exit 1; }

BREW_PREFIX="/opt/homebrew"
[ -x "$BREW_PREFIX/bin/brew" ] || BREW_PREFIX="/usr/local"

printf '%s=== 把 brew 服务装成系统级 LaunchDaemon ===%s\n' "$C_BOLD" "$C_RESET"
printf '运行身份: %s\nbrew 前缀: %s\n服务: %s\n' "$REAL_USER" "$BREW_PREFIX" "${FORMULAS[*]}"
[ "$DRY_RUN" = "1" ] && warn "dry-run：不会做任何修改"

# ---------------------------------------------------------------------------
#  每个服务的处理
# ---------------------------------------------------------------------------
install_one() {
  local formula="$1"
  local opt_dir="$BREW_PREFIX/opt/$formula"
  local src="" label=""

  step "处理 ${formula}"

  # Homebrew 有**两套**服务 label 命名：老的 homebrew.mxcl.<formula>
  # 和新的 sh.brew.<formula>。同一个 formula 可能只有其中一种
  # （实测 mysql@8.4 只有 sh.brew.*，nginx 两种都有）。
  # 所以不能写死文件名，要扫目录并读 Label —— 写死就会漏掉一半服务。
  local cand
  for cand in "$opt_dir/homebrew.mxcl.${formula}.plist" "$opt_dir/sh.brew.${formula}.plist"; do
    if [ -f "$cand" ]; then
      src="$cand"
      label="$(/usr/libexec/PlistBuddy -c 'Print :Label' "$cand" 2>/dev/null || echo "")"
      break
    fi
  done
  if [ -z "$src" ]; then
    # 兜底：扫任意 plist，取第一个能读出 Label 的
    for cand in "$opt_dir"/*.plist; do
      [ -f "$cand" ] || continue
      label="$(/usr/libexec/PlistBuddy -c 'Print :Label' "$cand" 2>/dev/null || echo "")"
      [ -n "$label" ] && { src="$cand"; break; }
    done
  fi
  if [ -z "$src" ] || [ -z "$label" ]; then
    fail "找不到可用的服务定义（$opt_dir 下没有带 Label 的 plist）"
    return 1
  fi
  info "服务定义：$(basename "$src")  label=${label}"
  local dst="/Library/LaunchDaemons/${label}.plist"

  # 已有的用户级 agent 必须先摘掉，否则会出现"同一服务两份实例"
  local agent="$HOME/Library/LaunchAgents/${label}.plist"
  local user_home
  user_home="$(dscl . -read "/Users/$REAL_USER" NFSHomeDirectory 2>/dev/null | awk '{print $2}')"
  [ -n "$user_home" ] && agent="$user_home/Library/LaunchAgents/${label}.plist"
  if [ -f "$agent" ]; then
    if [ "$DRY_RUN" = "1" ]; then
      printf '    (dry-run) 卸载用户级服务 %s\n' "$agent"
    else
      sudo -u "$REAL_USER" launchctl bootout "gui/$(id -u "$REAL_USER")/$label" 2>/dev/null || true
      rm -f "$agent"
      info "已移除用户级服务（避免与系统级重复启动）"
    fi
  fi

  if [ "$DRY_RUN" = "1" ]; then
    printf '    (dry-run) 从 %s 生成系统级 plist\n' "$src"
    printf '    (dry-run) 注入 UserName=%s\n' "$REAL_USER"
    printf '    (dry-run) 安装到 %s 并 bootstrap system\n' "$dst"
    return 0
  fi

  # 先卸载可能已存在的旧实例，保证幂等
  launchctl bootout "system/$label" 2>/dev/null || true

  local tmp
  tmp="$(mktemp "/tmp/${label}.XXXXXX")"
  cp -f "$src" "$tmp" || { fail "复制 plist 失败"; return 1; }

  # 注入运行身份。brew 的 plist 没有 UserName，缺了它系统域会以 root 运行 ——
  # mysql 会直接拒绝启动，nginx/php 产生的文件归属也会变错。
  /usr/libexec/PlistBuddy -c "Delete :UserName" "$tmp" >/dev/null 2>&1 || true
  /usr/libexec/PlistBuddy -c "Add :UserName string $REAL_USER" "$tmp" >/dev/null 2>&1 || {
    fail "写入 UserName 失败"; rm -f "$tmp"; return 1; }

  # 系统域必须有 RunAtLoad，否则开机不会自动拉起
  /usr/libexec/PlistBuddy -c "Add :RunAtLoad bool true" "$tmp" >/dev/null 2>&1 \
    || /usr/libexec/PlistBuddy -c "Set :RunAtLoad true" "$tmp" >/dev/null 2>&1 || true

  # 系统 plist 必须是 root:wheel 0644，否则 launchd 拒绝加载
  chown root:wheel "$tmp" && chmod 0644 "$tmp" || { fail "设置权限失败"; rm -f "$tmp"; return 1; }
  mv -f "$tmp" "$dst" || { fail "安装 plist 失败"; return 1; }
  pass "已安装 ${dst}（以 ${REAL_USER} 运行）"

  local out
  if out="$(launchctl bootstrap system "$dst" 2>&1)"; then
    pass "已加载到系统域"
  else
    fail "加载失败：${out}"
    return 1
  fi
  return 0
}

# ---------------------------------------------------------------------------
#  MySQL 特殊处理：数据目录没初始化会直接启动失败
# ---------------------------------------------------------------------------
prepare_mysql() {
  local datadir="$BREW_PREFIX/var/mysql"
  if [ -d "$datadir" ] && [ -n "$(ls -A "$datadir" 2>/dev/null)" ]; then
    return 0
  fi
  warn "MySQL 数据目录未初始化（${datadir}），需要先初始化才能启动"
  if [ "$DRY_RUN" = "1" ]; then
    printf '    (dry-run) 以 %s 身份执行 mysqld --initialize-insecure\n' "$REAL_USER"
    return 0
  fi
  mkdir -p "$datadir"
  chown "$REAL_USER" "$datadir" 2>/dev/null || true
  # shellcheck disable=SC2024
  # 重定向由当前（root）shell 打开，日志留给用户排障用
  if sudo -u "$REAL_USER" -H "$BREW_PREFIX/bin/mysqld" --initialize-insecure \
       --datadir="$datadir" >/tmp/zizpanel-mysql-init.log 2>&1; then
    pass "MySQL 数据目录已初始化（root 初始无密码）"
  else
    fail "MySQL 初始化失败，日志末尾："
    tail -8 /tmp/zizpanel-mysql-init.log 2>/dev/null | sed 's/^/      /'
  fi
}

for f in "${FORMULAS[@]}"; do
  [ "$f" = "mysql@8.4" ] || [ "$f" = "mysql" ] && prepare_mysql
  install_one "$f" || true
done

# ---------------------------------------------------------------------------
#  验证：只认"端口/进程真的在运行"，不认命令退出码
# ---------------------------------------------------------------------------
step "验证运行状态"
check_port() {
  local name="$1" port="$2"
  local waited=0
  while [ "$waited" -lt 20 ]; do
    if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
      pass "${name} 正在监听 ${port}"
      return 0
    fi
    sleep 0.5
    waited=$((waited + 1))
  done
  fail "${name} 没有监听 ${port}"
  return 1
}

if [ "$DRY_RUN" != "1" ]; then
  for f in "${FORMULAS[@]}"; do
    case "$f" in
      nginx)   check_port "nginx" 80 ;;
      php@8.3|php) check_port "php-fpm" 9000 ;;
      mysql@8.4|mysql) check_port "mysql" 3306 ;;
    esac
  done
fi

printf '\n%s────────────────────────────%s\n' "$C_BOLD" "$C_RESET"
if [ "$FAILURES" -eq 0 ]; then
  printf '%s✅ 全部处理完成%s\n' "$C_GREEN" "$C_RESET"
  printf '   这些服务现在是系统级守护进程：不依赖任何人登录，开机即运行。\n'
  printf '   查看：sudo launchctl print system/homebrew.mxcl.nginx\n'
  exit 0
fi
printf '%s❌ 有 %d 项未成功%s\n' "$C_RED" "$FAILURES" "$C_RESET"
exit 1
