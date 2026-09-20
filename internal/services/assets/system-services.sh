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
#    sudo bash tools/system-services.sh nginx php@8.2   # 指定服务
#    sudo bash tools/system-services.sh --dry-run       # 只显示要做什么
# =============================================================================
set -uo pipefail

DEFAULT_FORMULAS=(nginx php@8.2 mysql@8.4)
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
#
# 优先级（真机教训 2026-09-17，坑 133）：
#   ① SUDO_USER           —— 人工 `sudo bash system-services.sh` 的场景
#   ② ZIZPANEL_REAL_USER  —— 面板传进来的（面板自己已经是 root，没有 sudo，
#                            所以 SUDO_USER 永远是空的）
#   ③ USER / LOGNAME      —— 面板 export 的真实用户（非 root 才认）
#   ④ /dev/console 属主    —— 兜底
# 只认 ①+④ 不够：**无头 Mac**（服务器模式，重启后停在登录界面、没人登录图形界面）
# 上 SUDO_USER 为空、/dev/console 的属主是 root，脚本会以"无法确定真实用户"退出 ——
# 而那正是本脚本最主要的目标场景（mini 真机实测：整批迁移全线失败）。
REAL_USER="${SUDO_USER:-}"
if [ -z "$REAL_USER" ] || [ "$REAL_USER" = "root" ]; then
  REAL_USER="${ZIZPANEL_REAL_USER:-}"
fi
if [ -z "$REAL_USER" ] || [ "$REAL_USER" = "root" ]; then
  case "${USER:-}" in ""|root) ;; *) REAL_USER="$USER" ;; esac
fi
if [ -z "$REAL_USER" ] || [ "$REAL_USER" = "root" ]; then
  case "${LOGNAME:-}" in ""|root) ;; *) REAL_USER="$LOGNAME" ;; esac
fi
if [ -z "$REAL_USER" ] || [ "$REAL_USER" = "root" ]; then
  REAL_USER="$(stat -f '%Su' /dev/console 2>/dev/null || echo "")"
fi
[ -n "$REAL_USER" ] && [ "$REAL_USER" != "root" ] || {
  echo "无法确定真实用户：请用 sudo 执行，或显式设置 ZIZPANEL_REAL_USER=<用户名>" >&2; exit 1; }
# 用户必须真实存在：写进 plist 的 UserName 不存在时 launchd 会拒绝加载，
# 而那时的报错完全指不到原因。
if ! id -u "$REAL_USER" >/dev/null 2>&1; then
  echo "用户 $REAL_USER 在本机不存在（ZIZPANEL_REAL_USER / SUDO_USER 传错了？）" >&2; exit 1
fi

BREW_PREFIX="/opt/homebrew"
[ -x "$BREW_PREFIX/bin/brew" ] || BREW_PREFIX="/usr/local"

# 兜底：面板（LaunchDaemon / root）环境里 HOME 是**未定义**的，而本脚本开了
# set -u —— 后面任何一处 $HOME 都会让脚本当场退出。这里统一补上真实用户家目录。
if [ -z "${HOME:-}" ] || [ "${HOME:-}" = "/var/root" ]; then
  HOME="$(dscl . -read "/Users/$REAL_USER" NFSHomeDirectory 2>/dev/null | awk '{print $2}')"
  [ -n "${HOME:-}" ] || HOME="/Users/$REAL_USER"
  export HOME
fi

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
  #
  # 这里**不能**用 ${HOME}：面板是 LaunchDaemon（root）拉起的，环境里没有 HOME，
  # 而本脚本开着 set -u —— 真机实测（2026-09-17 mini）就是这一行让整个注册以
  #   /opt/zizpanel/system-services.sh: line 105: HOME: unbound variable
  # 自杀：一键 LNMP 装完却卡在最后一步，服务管理里一条都没有，而报错只有这一句。
  # 真实家目录用 dscl 查；查不到再退回 /Users/<用户>（host 上一定存在这个约定）。
  local user_home
  user_home="$(dscl . -read "/Users/$REAL_USER" NFSHomeDirectory 2>/dev/null | awk '{print $2}')"
  [ -n "$user_home" ] || user_home="/Users/$REAL_USER"
  local agent="$user_home/Library/LaunchAgents/${label}.plist"
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

  # 先卸载可能已存在的旧实例，保证幂等。
  #
  # 但 bootout 是**异步**的：它返回时 launchd 未必已经把服务摘掉，紧接着
  # bootstrap 会以 "Bootstrap failed: 5: Input/output error" 失败
  # （真机 2026-09-17 实测：mysql 的第一次系统级转换就撞上，服务直接没了，
  #   脚本只打印一行 "✗ 加载失败：Bootstrap failed: 5: Input/output error"）。
  # 所以必须**等它真的消失**再装 —— 与 install.sh 的 wait_service_stopped、
  # Go 侧 bootstrapService 是同一个坑、同一套修法。
  # 单测抓不到：launchctl 的异步时序只在真机上存在，脚本又是被 Go 以 root 调起的。
  launchctl bootout "system/$label" 2>/dev/null || true
  local gone=0
  while [ "$gone" -lt 40 ]; do
    launchctl print "system/$label" >/dev/null 2>&1 || break
    sleep 0.3
    gone=$((gone + 1))
  done

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

  # 带重试地装载：卸载刚结束的一小段时间内 bootstrap 仍可能失败；
  # 已经加载上（launchd 里有它）就直接 kickstart 拉起，不再重复 bootstrap。
  local out="" ok=0
  for _ in 1 2 3 4 5; do
    if out="$(launchctl bootstrap system "$dst" 2>&1)"; then
      ok=1; break
    fi
    if launchctl print "system/$label" >/dev/null 2>&1; then
      if launchctl kickstart -k "system/$label" >/dev/null 2>&1; then
        ok=1; out="(已加载，改用 kickstart 拉起)"; break
      fi
    fi
    sleep 0.5
  done
  if [ "$ok" = "1" ]; then
    pass "已加载到系统域 ${out}"
  else
    fail "加载失败（已重试 5 次）：${out}"
    return 1
  fi
  return 0
}

# ---------------------------------------------------------------------------
#  数据库特殊处理：数据目录没初始化会直接启动失败
#
#  两个引擎共用 <prefix>/var/mysql，但初始化命令**不通用**：
#    · MySQL   → mysqld --initialize-insecure（root 空口令）
#    · MariaDB → mariadb-install-db（MariaDB 不认 --initialize-insecure）
#  这里与 internal/services/lnmp.go 的 mysqlInitCommand 保持同一口径。
# ---------------------------------------------------------------------------
prepare_mysql() {
  local formula="$1"
  local datadir="$BREW_PREFIX/var/mysql"
  if [ -d "$datadir" ] && [ -n "$(ls -A "$datadir" 2>/dev/null)" ]; then
    return 0
  fi
  warn "数据库数据目录未初始化（${datadir}），需要先初始化才能启动"
  local -a init_cmd
  case "$formula" in
    mariadb*)
      local mariadb_dir="$BREW_PREFIX/opt/mariadb"
      init_cmd=("$mariadb_dir/bin/mariadb-install-db" --no-defaults
        "--basedir=$mariadb_dir" "--datadir=$datadir"
        --auth-root-authentication-method=normal)
      ;;
    *)
      local mysqld="$BREW_PREFIX/opt/$formula/bin/mysqld"
      [ -x "$mysqld" ] || mysqld="$BREW_PREFIX/bin/mysqld"
      init_cmd=("$mysqld" --initialize-insecure "--datadir=$datadir")
      ;;
  esac
  # dry-run 先打印"会执行什么"，再去检查二进制在不在（否则空机器上 dry-run 只会报错）。
  if [ "$DRY_RUN" = "1" ]; then
    printf '    (dry-run) 以 %s 身份执行 %s\n' "$REAL_USER" "${init_cmd[*]}"
    return 0
  fi
  if [ ! -x "${init_cmd[0]}" ]; then
    fail "找不到 ${init_cmd[0]}（${formula} 没装好？）"
    return 1
  fi
  mkdir -p "$datadir"
  chown "$REAL_USER" "$datadir" 2>/dev/null || true
  # shellcheck disable=SC2024
  # 重定向由当前（root）shell 打开，日志留给用户排障用
  if sudo -u "$REAL_USER" -H "${init_cmd[@]}" >/tmp/zizpanel-mysql-init.log 2>&1; then
    # 不要把"root 初始无密码"当成最终状态：面板的安装流程随后会为它设置
    # 强随机口令（或用户输入的口令）并写进 config.json，见
    # internal/services/lnmp_mysql_credentials.go。这里说清楚，免得用户看到
    # 这句日志就以为"root 一直是空口令"（2026-09-16 真机上正是这种误解）。
    pass "数据库数据目录已初始化（root 初始无密码；面板单跑本脚本时不会改它，装 LNMP 时会设置并记入口令）"
  else
    fail "数据库初始化失败，日志末尾："
    tail -8 /tmp/zizpanel-mysql-init.log 2>/dev/null | sed 's/^/      /'
  fi
}

for f in "${FORMULAS[@]}"; do
  # 注意括号：早先写成 `[ A ] || [ B ] && prepare_mysql`，shell 的 && / || 左结合
  # 会让它变成"对每个 formula 都初始化数据库"（nginx 也触发一次）。虽然初始化
  # 本身幂等，但那等于在无关步骤里去动数据目录，属于不该有的副作用。
  case "$f" in
    mysql@*|mysql|mariadb|mariadb@*) prepare_mysql "$f" ;;
  esac
  install_one "$f" || true
done

# ---------------------------------------------------------------------------
#  验证：只认"端口/进程真的在运行"，不认命令退出码
# ---------------------------------------------------------------------------
step "验证运行状态"
check_port() {
  local name="$1" port="$2"
  local waited=0
  # 上限 30 秒（60×0.5）：MySQL 首次启动要建 InnoDB 表空间，实测 3~15 秒。
  # 卡 10 秒的话会把"正在启动"误判成"没在监听"，进而让整个一键安装报失败 ——
  # 而它其实马上就起来了（"装好了却报失败"和谎报成功一样伤用户）。
  while [ "$waited" -lt 60 ]; do
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

# check_php_fpm 验证某个 PHP 版本的 fpm 真的起来了。
#
# 关键：面板的多版本设计让每个 php@x.y 监听**自己专属的 Unix socket**
# （<brew>/var/run/php-fpm-<版本>.sock，见 internal/sites/phpfpm.go），
# 不再占固定的 9000。所以这里必须按 socket 判定 —— 只查 9000 会把
# 一个正常工作的 php-fpm 报成"没有监听"，进而让整个一键安装脚本以失败退出
# （真机上就会表现为"明明装好了却报安装失败"）。
check_php_fpm() {
  local formula="$1"
  local version="${formula#php@}"
  [ "$formula" = "php" ] && version=""
  local sock="$BREW_PREFIX/var/run/php-fpm-${version}.sock"
  local waited=0
  while [ "$waited" -lt 60 ]; do
    if [ -n "$version" ] && [ -S "$sock" ]; then
      pass "php-fpm ${version} 正在监听 ${sock}"
      return 0
    fi
    # 兼容"用户自己把 www.conf 改回 TCP 9000"或无法确定版本的情况
    if lsof -nP -iTCP:9000 -sTCP:LISTEN >/dev/null 2>&1; then
      pass "php-fpm 正在监听 9000（未使用面板分配的专属 socket）"
      return 0
    fi
    sleep 0.5
    waited=$((waited + 1))
  done
  fail "php-fpm 没有监听 ${sock}（也没监听 9000）"
  return 1
}

if [ "$DRY_RUN" != "1" ]; then
  for f in "${FORMULAS[@]}"; do
    case "$f" in
      nginx)   check_port "nginx" 80 ;;
      php@8.*|php) check_php_fpm "$f" ;;
      mariadb|mariadb@*) check_port "mariadb" 3306 ;;
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
