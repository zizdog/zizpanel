#!/usr/bin/env bash
# =============================================================================
#  ZizPanel 卸载脚本（三档 · 实时进度）
#
#  用法：
#    curl -fsSL https://zizdog.com/zizpanel/uninstall.sh | sudo bash
#    sudo bash uninstall.sh [1|2|3] [--yes] [--dry-run] [--purge-backups]
#    sudo bash uninstall.sh --mode 2 --dry-run
#
#  三档（交互菜单里选）：
#    1 仅卸载面板      删程序 / LaunchDaemon / sudoers / CLI 入口 / 证书信任；
#                      保留面板数据（配置·数据库·证书·备份）与全部运行环境
#    2 面板 + LNMP     再卸 nginx / 各版本 PHP(-FPM) / MySQL / MariaDB 及其配置；
#                      保留 ~/www（绝不删）与数据库数据目录（结尾会告知位置）
#    3 面板 + 所有环境 再卸面板登记表里"由面板装过"的全部软件，并删所有配置数据
#                      （面板数据·数据库数据目录·应用数据·证书信任）；需 DELETE-ALL
#
#  向后兼容：`--purge` == `--mode 3 --yes`（旧的"彻底卸载"语义）。
#  实时进度：每个动作动手前打印 `→ 正在…`，完成 `✓`，跳过 `·`，失败 `✗`；
#            删除前打印绝对路径与大小（du -sh）；结尾按"已卸载 / 已删除 / 已保留 / 失败"汇总。
#  铁律：绝不删 ~/www；绝不拿 `brew list` 反推要删的软件 —— 模式 3 只认面板自己的登记表
#        （<面板根>/data/panel.db 的 services 表）。读不到登记表就拒绝执行，绝不猜。
#  刻意不用 set -e：脚本里有多处 `cmd && ok "…"`，配 set -e 会**静默中止**
#  （真机事故：用户看到"卸载完成"，实际什么都没删）。改为每步显式判断并在结尾大声报错。
# =============================================================================
set -uo pipefail

PANEL_ROOT="${ZIZPANEL_ROOT:-/opt/zizpanel}"
PANEL_LABEL="${ZIZPANEL_PANEL_LABEL:-cn.zizpanel.panel}"
LINK_DIR="${ZIZPANEL_LINK_DIR:-/usr/local/bin}"
PLIST_DIR="${ZIZPANEL_PLIST_DIR:-/Library/LaunchDaemons}"
SUDOERS_DIR="${ZIZPANEL_SUDOERS_DIR:-/etc/sudoers.d}"
APPS_DIR="${ZIZPANEL_APPS_DIR:-/Applications}"
DB_FILE="${ZIZPANEL_DB_FILE:-$PANEL_ROOT/data/panel.db}"

# Homebrew 前缀：测试/沙箱用 ZIZPANEL_BREW_PREFIX 整体覆盖；设了它就**只认它**，
# 绝不回落到真实前缀（否则沙箱测试会去删真机的 /opt/homebrew）。
BREW_PREFIX_OVERRIDE="${ZIZPANEL_BREW_PREFIX:-}"
BREW_PREFIXES=()
if [ -n "$BREW_PREFIX_OVERRIDE" ]; then
  BREW_PREFIXES=("$BREW_PREFIX_OVERRIDE")
else
  BREW_PREFIXES=("/opt/homebrew" "/usr/local")
fi

# 面板自有命名空间（cn.zizpanel.*）。`cn.zizdog.*` **不是**面板独占 —— 用户自己的
# 服务也在那个前缀下，所以只能按明确名单处理，绝不一刀切。
PANEL_OWN_LABELS=("$PANEL_LABEL" "cn.zizpanel.upgrade-watchdog")
# 面板**应用**安装器登记的固定标签（来自 Go 侧常量；managed 字段不可靠，见文件尾说明）。
PANEL_APP_LABELS=(
  com.zizdog.iopaint com.zizdog.qwen3tts com.zizdog.voicereceiver
  com.zizdog.imgcompress com.zizdog.macosspeech com.zizdog.stt
  com.zizdog.colima com.zizdog.filebrowser com.zizdog.frpc
  com.zizdog.orbien-client com.zizdog.ddns-go com.zizdog.alist
  cn.zizpanel.macsaber cn.macsaber.web
)

DRY=0
ASSUME_YES=0
PURGE_BACKUPS=0
MODE=""
MODE_DESC=""
BACKUP_DIR=""

C_BOLD=$'\033[1m'; C_RESET=$'\033[0m'; C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'
C_RED=$'\033[31m'; C_BLUE=$'\033[34m'

say()  { printf '%s\n' "$*"; }
title(){ printf '\n%s▸ %s%s\n' "$C_BOLD" "$*" "$C_RESET"; }
begin(){ printf '  %s→%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }   # 动手前
ok()   { printf '  %s✓%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
skip() { printf '  %s·%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }
warn() { printf '  %s!%s %s\n' "$C_YELLOW" "$C_RESET" "$*"; }
bad()  { printf '  %s✗%s %s\n' "$C_RED" "$C_RESET" "$*"; }

# --------------------------------------------------------------- 失败/结果 --
FAILED_COUNT=0
FAILED_LIST=""
DELETED_LIST=""
UNINSTALLED_LIST=""
record_failed() { FAILED_COUNT=$((FAILED_COUNT + 1)); FAILED_LIST="${FAILED_LIST}$1"$'\n'; }
record_deleted() { DELETED_LIST="${DELETED_LIST}$1"$'\n'; }
record_uninstalled() { UNINSTALLED_LIST="${UNINSTALLED_LIST}$1"$'\n'; }

# ---------------------------------------------------------------- 交互输入 --
# 与 install.sh 同一套：从 /dev/tty 读，管道执行时也能问人。
TTY_FD=""
if { exec 3</dev/tty; } 2>/dev/null; then TTY_FD=3; fi
have_tty() { [ -n "$TTY_FD" ]; }

ask_line() { # ask_line <提示> <变量名>；没有终端时返回失败（调用方据此安全取消）
  local prompt="$1" __var="$2" line=""
  have_tty || return 1
  printf '%s' "$prompt" >&2
  IFS= read -r -u "$TTY_FD" line || return 1
  printf -v "$__var" '%s' "$line"
}

confirm_phrase() { # confirm_phrase <要求输入的字> <说明>
  local want="$1" note="$2" got=""
  say ""
  say "  $note"
  ask_line "  请输入 ${C_BOLD}${want}${C_RESET} 确认（其它任何输入都会取消）：" got || return 1
  [ "$got" = "$want" ]
}

# ------------------------------------------------------------ 通用小工具 --
list_contains() { # list_contains <needle> <item>...
  local needle="$1"; shift
  local x
  for x in "$@"; do
    [ "$x" = "$needle" ] && return 0
  done
  return 1
}

path_size() {
  [ -e "$1" ] || return 0
  du -sh "$1" 2>/dev/null | awk 'NR==1{print $1}'
}

user_exists() { id -u "$1" >/dev/null 2>&1; }

# ---------------------------------------------------------- 真实用户/家目录 --
# sudo 下 $HOME 是 /var/root，用它去拼 ~/www 会看错地方（甚至删错），所以必须解析真实用户。
detect_real_user() {
  local u="${SUDO_USER:-}"
  if [ -z "$u" ] || [ "$u" = "root" ]; then u="${ZIZPANEL_REAL_USER:-}"; fi
  if [ -z "$u" ] || [ "$u" = "root" ]; then
    u="$(stat -f '%Su' /dev/console 2>/dev/null || echo '')"
  fi
  if [ -z "$u" ] || [ "$u" = "root" ]; then u="$(id -un)"; fi
  printf '%s' "$u"
}

REAL_USER="${ZIZPANEL_REAL_USER:-}"
if [ -z "$REAL_USER" ]; then REAL_USER="$(detect_real_user)"; fi
REAL_HOME="${ZIZPANEL_REAL_HOME:-}"
if [ -z "$REAL_HOME" ]; then
  if [ "$(id -u)" -eq 0 ] && user_exists "$REAL_USER" && [ "$REAL_USER" != "root" ]; then
    REAL_HOME="$(dscl . -read "/Users/$REAL_USER" NFSHomeDirectory 2>/dev/null | awk '{print $2}')"
    if [ -z "$REAL_HOME" ]; then REAL_HOME="$(eval echo "~$REAL_USER" 2>/dev/null || echo "$HOME")"; fi
  else
    REAL_HOME="$HOME"
  fi
fi

# ------------------------------------------------------------- 计划（清单） --
PLAN_STOP=(); PLAN_UNINSTALL=(); PLAN_DELETE=(); PLAN_KEEP=()
PLAN_STOP_KEYS=""; PLAN_UNINSTALL_KEYS=""; PLAN_DELETE_KEYS=""; PLAN_KEEP_KEYS=""
plan_stop() {
  [ -n "$1" ] || return 0
  case "$PLAN_STOP_KEYS" in *$'\x1f'"$1"$'\x1f'*) return 0 ;; esac
  PLAN_STOP_KEYS="${PLAN_STOP_KEYS}"$'\x1f'"$1"$'\x1f'; PLAN_STOP+=("$1")
}
plan_uninstall() {
  [ -n "$1" ] || return 0
  case "$PLAN_UNINSTALL_KEYS" in *$'\x1f'"$1"$'\x1f'*) return 0 ;; esac
  PLAN_UNINSTALL_KEYS="${PLAN_UNINSTALL_KEYS}"$'\x1f'"$1"$'\x1f'; PLAN_UNINSTALL+=("$1")
}
plan_delete() {
  [ -n "$1" ] || return 0
  case "$PLAN_DELETE_KEYS" in *$'\x1f'"$1"$'\x1f'*) return 0 ;; esac
  PLAN_DELETE_KEYS="${PLAN_DELETE_KEYS}"$'\x1f'"$1"$'\x1f'; PLAN_DELETE+=("$1")
}
plan_keep() {
  [ -n "$1" ] || return 0
  case "$PLAN_KEEP_KEYS" in *$'\x1f'"$1"$'\x1f'*) return 0 ;; esac
  PLAN_KEEP_KEYS="${PLAN_KEEP_KEYS}"$'\x1f'"$1"$'\x1f'; PLAN_KEEP+=("$1")
}
# 面板根需要特殊处理（保留备份时先转移），但清单里要能看到
PANEL_ROOT_SPECIAL=0

emit_plan() { # emit_plan：把"将要删的清单"打到 stdout（dry-run 预览 / 写进备份）
  printf '# ZizPanel 卸载计划（本文件就是"将要删的清单"）\n'
  printf '# 生成时间：%s\n' "$(date '+%Y-%m-%d %H:%M:%S')"
  printf '# 模式：%s（%s）\n\n' "$MODE" "$MODE_DESC"
  printf '## 将停止的服务\n'
  local x
  for x in ${PLAN_STOP[@]+"${PLAN_STOP[@]}"}; do printf '  - %s\n' "$x"; done
  printf '\n## 将卸载的软件\n'
  for x in ${PLAN_UNINSTALL[@]+"${PLAN_UNINSTALL[@]}"}; do printf '  - %s\n' "$x"; done
  printf '\n## 将删除的路径\n'
  for x in ${PLAN_DELETE[@]+"${PLAN_DELETE[@]}"}; do printf '  - %s\n' "$x"; done
  printf '\n## 将保留\n'
  for x in ${PLAN_KEEP[@]+"${PLAN_KEEP[@]}"}; do printf '  - %s\n' "$x"; done
}

write_plan_file() {
  local f="$1"
  emit_plan > "$f" 2>/dev/null || warn "写清单失败：$f"
}

# ------------------------------------------------------------- 危险路径护栏 --
is_safe_delete_path() { # 只用于**从登记表推导出来**的路径（work_dir 等）
  local p="$1" prot
  [ -n "$p" ] || return 1
  case "$p" in /) return 1 ;; /*) ;; *) return 1 ;; esac
  # 铁律：绝不动 ~/www 及其子路径
  case "$p" in
    "$REAL_HOME/www"|"$REAL_HOME/www/"*) return 1 ;;
  esac
  for prot in "/Users" "/opt" "/usr" "/usr/local" /opt/homebrew "/Library" "/Applications" \
              "/System" "/Volumes" "/private" "/etc" "/var" "/bin" "/sbin" "/tmp" \
              "$REAL_HOME" "$PANEL_ROOT"; do
    [ "$p" = "$prot" ] && return 1
  done
  # 至少 /a/b 两级，避免把顶层目录当数据目录删掉
  case "$p" in /*/*) return 0 ;; *) return 1 ;; esac
}

# ---------------------------------------------------------------- 删除动作 --
remove_path() { # remove_path <路径> [说明]
  local path="$1" desc="${2:-}"
  case "$path" in
    ""|"/") bad "拒绝删除危险路径：${path}"; record_failed "拒绝删除危险路径"; return 1 ;;
  esac
  if [ ! -e "$path" ] && [ ! -L "$path" ]; then
    skip "本来就没有 ${path}"
    return 0
  fi
  local sz=""
  sz="$(path_size "$path")"
  if [ -n "$desc" ]; then
    begin "正在删除 ${desc}：${path}（${sz}）"
  else
    begin "正在删除 ${path}（${sz}）"
  fi
  if [ "$DRY" = "1" ]; then
    printf '    %s[dry-run]%s 将执行：rm -rf %s\n' "$C_YELLOW" "$C_RESET" "$path"
    return 0
  fi
  if rm -rf -- "$path" 2>/dev/null; then
    ok "已删除 ${path}"
    record_deleted "$path"
  else
    bad "删除失败：${path}"
    record_failed "删除失败 ${path}"
    return 1
  fi
}

# ------------------------------------------------------------- launchd 动作 --
launchd_has() {
  launchctl list 2>/dev/null | awk '{print $3}' | grep -qx -- "$1"
}

bootout_label() {
  local label="$1"
  [ -n "$label" ] || return 0
  if [ "$DRY" = "1" ]; then
    begin "正在停止服务 ${label}"
    printf '    %s[dry-run]%s 将执行：launchctl bootout system/%s\n' "$C_YELLOW" "$C_RESET" "$label"
    return 0
  fi
  if ! launchd_has "$label"; then
    skip "本来就没有服务 ${label}"
    return 0
  fi
  begin "正在停止服务 ${label}"
  if launchctl bootout "system/$label" >/dev/null 2>&1; then
    ok "已停止 ${label}"
  else
    bad "停止失败：${label}"
    record_failed "停止服务失败 ${label}"
  fi
}

# --------------------------------------------------------------- brew 动作 --
brew_bin() {
  local p
  for p in "${BREW_PREFIXES[@]}"; do
    if [ -x "$p/bin/brew" ]; then printf '%s' "$p/bin/brew"; return 0; fi
  done
  if command -v brew >/dev/null 2>&1; then command -v brew; return 0; fi
  return 1
}

# brew 明确拒绝 root 运行；以真实用户身份执行（面板侧同款做法）。
as_user_exec() {
  if [ "$(id -u)" -eq 0 ] && [ -n "$REAL_USER" ] && [ "$REAL_USER" != "root" ]; then
    /usr/bin/sudo -n -u "$REAL_USER" -H "$@"
  else
    "$@"
  fi
}

# 只读查询：dry-run 下也执行（用来算真实清单）。
brew_query() {
  local b=""
  b="$(brew_bin)" || return 1
  as_user_exec "$b" "$@"
}

brew_services_stop() {
  local f="$1"
  begin "正在停止 ${f} 服务（brew services stop）"
  if [ "$DRY" = "1" ]; then
    printf '    %s[dry-run]%s 将执行：brew services stop %s\n' "$C_YELLOW" "$C_RESET" "$f"
    return 0
  fi
  if brew_query services stop "$f" >/dev/null 2>&1; then
    ok "已停止 ${f}"
  else
    skip "${f} 本来就没有在跑（或 brew 未托管它）"
  fi
}

brew_uninstall_formula() {
  local f="$1"
  [ -n "$f" ] || return 0
  begin "正在卸载 brew 软件 ${f}"
  if [ "$DRY" = "1" ]; then
    printf '    %s[dry-run]%s 将执行：brew uninstall --ignore-dependencies %s\n' "$C_YELLOW" "$C_RESET" "$f"
    return 0
  fi
  local out=""
  if out="$(brew_query uninstall --ignore-dependencies "$f" 2>&1)"; then
    ok "已卸载 ${f}"
    record_uninstalled "${f}（brew formula）"
  else
    bad "卸载失败：${f} —— $(printf '%s' "$out" | tail -n 1)"
    record_failed "卸载失败 ${f}"
    return 1
  fi
}

# --------------------------------------------------------------- 软件探测 --
formula_from_label() { # 只认 brew 自己的两种 launchd 标签
  case "$1" in
    homebrew.mxcl.*) printf '%s' "${1#homebrew.mxcl.}" ;;
    sh.brew.*)       printf '%s' "${1#sh.brew.}" ;;
    *) printf '' ;;
  esac
}

is_lnmp_formula() {
  case "$1" in
    nginx|nginx@*|php|php@*|mysql|mysql@*|mariadb|mariadb@*) return 0 ;;
  esac
  return 1
}

# 少数面板应用的"引擎"是另一个 brew 包，登记表只记了应用的 launchd 标签。
# 这里是 Go 侧 ImgCompressFormula / STTBrewFormula 的镜像；只对登记表已确认由面板
# 安装的行生效（绝不用 brew list 反推）。新增这类应用时要同步这里。
engine_formula_for_label() {
  case "$1" in
    com.zizdog.imgcompress) printf '%s' "vips" ;;
    com.zizdog.stt)         printf '%s' "whisper.cpp" ;;
    *) printf '' ;;
  esac
}

is_panel_app_label() {
  [ -n "$1" ] || return 1
  list_contains "$1" ${PANEL_APP_LABELS[@]+"${PANEL_APP_LABELS[@]}"}
}

# LNMP = nginx + 各版本 PHP(-FPM) + MySQL/MariaDB。
# 刻意**只**按白名单匹配 `brew list` 的结果（不是"看到什么删什么"）：用户自己装的
# redis/ffmpeg/其它语言运行时一律不碰。PostgreSQL 不在此列（用户点名的 LNMP 只含
# nginx/PHP/MySQL/MariaDB；PG 多为某个应用（Miniflux）的依赖，轻率删会连带弄坏它）。
LNMP_DETECTED=()
detect_lnmp_formulas() {
  LNMP_DETECTED=()
  local b="" out="" f
  b="$(brew_bin)" || return 0
  out="$(as_user_exec "$b" list --formula 2>/dev/null || true)"
  for f in $out; do
    if is_lnmp_formula "$f"; then LNMP_DETECTED+=("$f"); fi
  done
}

# --------------------------------------------------- 面板登记表（唯一软件源） --
# 模式 3 的软件清单**只**来自这里：面板自己的 SQLite 库。绝不用 brew list 反推，
# 因为"用户自己 brew install 的"与"面板装的"在 brew 里长得一模一样。
REG_NAME=(); REG_DISPLAY=(); REG_KIND=(); REG_LABEL=(); REG_PLIST=()
REG_WORKDIR=(); REG_COMPOSE=(); REG_CONTAINER=(); REG_FORMULA=(); REG_ACT=()
REG_WHY=""

registry_collect() {
  REG_WHY=""
  REG_NAME=(); REG_DISPLAY=(); REG_KIND=(); REG_LABEL=(); REG_PLIST=()
  REG_WORKDIR=(); REG_COMPOSE=(); REG_CONTAINER=(); REG_FORMULA=(); REG_ACT=()
  if ! command -v sqlite3 >/dev/null 2>&1; then
    REG_WHY="系统里找不到 sqlite3，无法读取面板登记表"
    return 1
  fi
  if [ ! -f "$DB_FILE" ]; then
    REG_WHY="面板登记表不存在：${DB_FILE}（面板从未启动过，或数据已被删）"
    return 1
  fi
  if ! sqlite3 -readonly "$DB_FILE" "SELECT count(*) FROM services;" >/dev/null 2>&1; then
    REG_WHY="读不到面板登记表里的 services 表：${DB_FILE}"
    return 1
  fi
  # ⚠️ 分隔符必须是**非 IFS 空白**（用 0x1f 单元分隔符）：bash 的 read 会把
  # 连续 TAB（IFS 空白）折叠、并吃掉空字段 —— 登记表里"空的 launch_label / work_dir"
  # 一折叠，字段整体左移，compose_file 会被当成 launchd 标签去 bootout（真踩过）。
  # 行首放哨兵 ZPEND：保证字段数与 read 变量数一致，末尾空字段不会被并入上一个。
  local sep="" s="" n d k c m l pl w cf ct
  sep="$(printf '\037')"
  while IFS="$sep" read -r s n d k c m l pl w cf ct; do
    [ "$s" = "ZPEND" ] || continue
    local formula="" act=0
    formula="$(formula_from_label "$l")"
    if [ -n "$formula" ] && is_lnmp_formula "$formula"; then
      continue   # LNMP 层负责，登记表这里不重复
    fi
    if [ "$m" = "1" ]; then
      act=1
    elif is_panel_app_label "$l"; then
      act=1
    else
      act=0
    fi
    if [ "$act" = "1" ] && [ -z "$formula" ]; then
      formula="$(engine_formula_for_label "$l")"
    fi
    REG_NAME+=("$n"); REG_DISPLAY+=("$d"); REG_KIND+=("$k"); REG_LABEL+=("$l")
    REG_PLIST+=("$pl"); REG_WORKDIR+=("$w"); REG_COMPOSE+=("$cf")
    REG_CONTAINER+=("$ct"); REG_FORMULA+=("$formula"); REG_ACT+=("$act")
  done < <(sqlite3 -readonly -separator "$sep" "$DB_FILE" \
    "SELECT 'ZPEND',name,display_name,kind,category,managed,launch_label,plist_path,work_dir,compose_file,container FROM services;" 2>/dev/null)
  return 0
}

# --------------------------------------------------------------- 计划构建 --
collect_panel_program_paths() {
  local f
  for f in "$PLIST_DIR/$PANEL_LABEL.plist" "$SUDOERS_DIR/zizpanel" \
           "$LINK_DIR/zizpanel" "$LINK_DIR/zizpanel-helper" \
           "$APPS_DIR/ZizPanel.app" \
           "$PANEL_ROOT/bin/zizpanel" "$PANEL_ROOT/bin/zizpanel-helper" \
           "$PLIST_DIR/cn.zizpanel.upgrade-watchdog.plist"; do
    [ -e "$f" ] || [ -L "$f" ] || continue
    plan_delete "$f"
  done
  for f in "$PLIST_DIR"/cn.zizpanel.cron.*.plist "$REAL_HOME"/Library/LaunchAgents/cn.zizpanel.cron.*.plist; do
    [ -e "$f" ] || continue
    plan_delete "$f"
  done
}

collect_panel_labels() {
  local l
  for l in ${PANEL_OWN_LABELS[@]+"${PANEL_OWN_LABELS[@]}"}; do plan_stop "$l"; done
  plan_stop "cn.zizpanel.cron.*（面板自建的计划任务作业）"
}

lnmp_config_paths() {
  local p f ver
  for p in "${BREW_PREFIXES[@]}"; do
    for f in "$p/etc/nginx" "$p/etc/my.cnf" "$p/etc/my.cnf.d"; do
      [ -e "$f" ] && plan_delete "$f"
    done
    for f in ${LNMP_DETECTED[@]+"${LNMP_DETECTED[@]}"}; do
      case "$f" in
        php@*) ver="${f#php@}"; [ -e "$p/etc/php/$ver" ] && plan_delete "$p/etc/php/$ver" ;;
        php)   [ -e "$p/etc/php" ] && plan_delete "$p/etc/php" ;;
      esac
    done
  done
}

plan_lnmp() {
  local f
  for f in ${LNMP_DETECTED[@]+"${LNMP_DETECTED[@]}"}; do
    plan_uninstall "${f}（brew formula）"
    plan_stop "homebrew.mxcl.${f}"
    plan_stop "sh.brew.${f}"
    [ -e "$PLIST_DIR/homebrew.mxcl.${f}.plist" ] && plan_delete "$PLIST_DIR/homebrew.mxcl.${f}.plist"
    [ -e "$PLIST_DIR/sh.brew.${f}.plist" ] && plan_delete "$PLIST_DIR/sh.brew.${f}.plist"
    [ -e "$REAL_HOME/Library/LaunchAgents/homebrew.mxcl.${f}.plist" ] && plan_delete "$REAL_HOME/Library/LaunchAgents/homebrew.mxcl.${f}.plist"
    [ -e "$REAL_HOME/Library/LaunchAgents/sh.brew.${f}.plist" ] && plan_delete "$REAL_HOME/Library/LaunchAgents/sh.brew.${f}.plist"
  done
  plan_stop "cn.zizdog.nginx"
  [ -e "$PLIST_DIR/cn.zizdog.nginx.plist" ] && plan_delete "$PLIST_DIR/cn.zizdog.nginx.plist"
  [ -e "$REAL_HOME/Library/LaunchAgents/cn.zizdog.nginx.plist" ] && plan_delete "$REAL_HOME/Library/LaunchAgents/cn.zizdog.nginx.plist"
  lnmp_config_paths
}

database_data_paths() { # 仅模式 3：数据库数据目录
  local p f
  for p in "${BREW_PREFIXES[@]}"; do
    for f in "$p/var/mysql" "$p"/var/mysql.*; do
      [ -e "$f" ] && plan_delete "$f"
    done
  done
}

colima_data_paths() {
  local d
  for d in "$REAL_HOME/.colima" "$REAL_HOME/.lima/colima"; do
    case "$d" in "$REAL_HOME/www"*) continue ;; esac
    [ -e "$d" ] && plan_delete "$d"
  done
}

build_plan_mode1() {
  collect_panel_program_paths
  collect_panel_labels
  plan_keep "面板数据（配置/数据库/证书/备份）：${PANEL_ROOT}/data"
  plan_keep "全部运行环境（nginx / PHP / MySQL / MariaDB、Homebrew）"
  plan_keep "站点文件：${REAL_HOME}/www（绝不删）"
}

build_plan_mode2() {
  detect_lnmp_formulas
  collect_panel_program_paths
  collect_panel_labels
  plan_lnmp
  plan_keep "站点文件：${REAL_HOME}/www（绝不删）"
  local p
  for p in "${BREW_PREFIXES[@]}"; do
    [ -d "$p/var/mysql" ] && plan_keep "数据库数据目录：${p}/var/mysql（保留；模式 3 才删）"
  done
  plan_keep "面板数据：${PANEL_ROOT}/data（保留；模式 3 才删）"
}

build_plan_mode3() {
  detect_lnmp_formulas
  collect_panel_program_paths
  collect_panel_labels
  plan_lnmp
  local i=0 label formula wd cf
  while [ "$i" -lt "${#REG_NAME[@]}" ]; do
    label="${REG_LABEL[$i]}"; formula="${REG_FORMULA[$i]}"
    wd="${REG_WORKDIR[$i]}"; cf="${REG_COMPOSE[$i]}"
    if [ "${REG_ACT[$i]}" = "1" ]; then
      plan_uninstall "${REG_DISPLAY[$i]}（登记表 ${REG_NAME[$i]}）"
      if [ -n "$label" ]; then
        plan_stop "$label"
        [ -e "${REG_PLIST[$i]}" ] && plan_delete "${REG_PLIST[$i]}"
        [ -e "$PLIST_DIR/$label.plist" ] && plan_delete "$PLIST_DIR/$label.plist"
        [ -e "$REAL_HOME/Library/LaunchAgents/$label.plist" ] && plan_delete "$REAL_HOME/Library/LaunchAgents/$label.plist"
      fi
      [ -n "$formula" ] && plan_uninstall "${formula}（brew formula）"
      case "$label" in
        cn.zizpanel.macsaber|cn.macsaber.web)
          [ -e /opt/macsaber ] && plan_delete "/opt/macsaber" ;;
        com.zizdog.colima) : ;;   # 数据目录在 colima_data_paths 里
        *) [ -n "$wd" ] && is_safe_delete_path "$wd" && plan_delete "$wd" ;;
      esac
      if [ -n "$cf" ]; then
        local cdir=""
        cdir="$(dirname "$cf")"
        [ -n "$cdir" ] && is_safe_delete_path "$cdir" && plan_delete "$cdir"
      fi
    else
      plan_keep "未确认由面板安装：${REG_DISPLAY[$i]}（${label:-${REG_NAME[$i]}}）"
    fi
    i=$((i + 1))
  done
  colima_data_paths
  database_data_paths
  plan_delete "$PANEL_ROOT"
  PANEL_ROOT_SPECIAL=1
  plan_keep "站点文件：${REAL_HOME}/www（绝不删）"
  plan_keep "Homebrew 本身，以及你自己 brew 装的软件（登记表里没有的一律不碰）"
}

build_plan() {
  PLAN_STOP=(); PLAN_UNINSTALL=(); PLAN_DELETE=(); PLAN_KEEP=()
  PLAN_STOP_KEYS=""; PLAN_UNINSTALL_KEYS=""; PLAN_DELETE_KEYS=""; PLAN_KEEP_KEYS=""
  PANEL_ROOT_SPECIAL=0
  case "$MODE" in
    1) build_plan_mode1 ;;
    2) build_plan_mode2 ;;
    3) build_plan_mode3 ;;
  esac
}

# ------------------------------------------------------------------ 备份 --
backup_everything() {
  BACKUP_DIR="${REAL_HOME}/zizpanel-uninstall-backup-$(date +%Y%m%d-%H%M%S)"
  title "备份（删之前先留一份，含待删清单）"
  if [ "$DRY" = "1" ]; then
    say "  [dry-run] 将备份到：${BACKUP_DIR}"
    say "  [dry-run] 备份内容：to-delete.txt（待删清单）、面板配置、面板数据库、"
    say "             MySQL/MariaDB 数据目录、nginx 配置、Brewfile、shell 配置"
    return 0
  fi
  mkdir -p "$BACKUP_DIR" 2>/dev/null && chmod 700 "$BACKUP_DIR" 2>/dev/null
  write_plan_file "$BACKUP_DIR/to-delete.txt"
  ok "待删清单 → to-delete.txt"
  if [ -f "$PANEL_ROOT/data/config.json" ] && cp -f "$PANEL_ROOT/data/config.json" "$BACKUP_DIR/panel-config.json" 2>/dev/null; then
    ok "面板配置 → panel-config.json"
  fi
  if [ -f "$DB_FILE" ] && cp -f "$DB_FILE" "$BACKUP_DIR/panel.db" 2>/dev/null; then
    ok "面板数据库 → panel.db"
  fi
  local p
  for p in "${BREW_PREFIXES[@]}"; do
    if [ -d "$p/var/mysql" ]; then
      if tar -czf "$BACKUP_DIR/mysql-var.tar.gz" -C "$p" var/mysql 2>/dev/null; then
        ok "MySQL/MariaDB 数据 → mysql-var.tar.gz（$(path_size "$BACKUP_DIR/mysql-var.tar.gz")）"
      else
        warn "MySQL/MariaDB 数据备份失败（继续，但请自行确认）"
      fi
    fi
    if [ -d "$p/etc/nginx" ]; then
      if tar -czf "$BACKUP_DIR/nginx-etc.tar.gz" -C "$p" etc/nginx 2>/dev/null; then
        ok "nginx 配置 → nginx-etc.tar.gz"
      fi
    fi
    if [ -x "$p/bin/brew" ]; then
      if "$p/bin/brew" bundle dump --file="$BACKUP_DIR/Brewfile" --force 2>/dev/null; then
        ok "已装软件清单 → Brewfile（之后可 brew bundle install 一键装回）"
      fi
    fi
  done
  cp -f "$REAL_HOME/.zprofile" "$BACKUP_DIR/zprofile.bak" 2>/dev/null || true
  ok "备份目录：$BACKUP_DIR"
}

# ------------------------------------------------------------------ 执行 --
stop_services_from_plan() {
  title "停止服务"
  local l
  for l in ${PLAN_STOP[@]+"${PLAN_STOP[@]}"}; do
    case "$l" in
      *'*'*) : ;;   # 含通配的说明行（cron.*）由下面按前缀扫
      *) bootout_label "$l" ;;
    esac
  done
  # 面板自建的计划任务作业（cn.zizpanel.cron.*）
  local labels one
  labels="$(launchctl list 2>/dev/null | awk '{print $3}' | grep -E '^cn\.zizpanel\.cron\.' || true)"
  for one in $labels; do bootout_label "$one"; done
}

brew_services_stop_from_plan() {
  local f
  for f in ${LNMP_DETECTED[@]+"${LNMP_DETECTED[@]}"}; do brew_services_stop "$f"; done
}

uninstall_formulas_from_plan() {
  title "卸载软件"
  local f i=0
  for f in ${LNMP_DETECTED[@]+"${LNMP_DETECTED[@]}"}; do brew_uninstall_formula "$f"; done
  while [ "$i" -lt "${#REG_NAME[@]}" ]; do
    if [ "${REG_ACT[$i]}" = "1" ] && [ -n "${REG_FORMULA[$i]}" ]; then
      brew_uninstall_formula "${REG_FORMULA[$i]}"
    fi
    i=$((i + 1))
  done
}

uninstall_registry_apps() { # 结尾汇总要点名"面板装过的应用"（真正的删除动作在别处已做）
  [ "$DRY" = "1" ] && return 0
  local i=0
  while [ "$i" -lt "${#REG_NAME[@]}" ]; do
    if [ "${REG_ACT[$i]}" = "1" ]; then
      record_uninstalled "${REG_DISPLAY[$i]}（登记表 ${REG_NAME[$i]}）"
    fi
    i=$((i + 1))
  done
}

compose_and_docker_down() {
  local i=0 cf ct
  while [ "$i" -lt "${#REG_NAME[@]}" ]; do
    cf="${REG_COMPOSE[$i]}"; ct="${REG_CONTAINER[$i]}"
    if [ "${REG_ACT[$i]}" = "1" ] && [ -n "$cf" ] && command -v docker >/dev/null 2>&1; then
      begin "正在停止 compose 项目 ${REG_DISPLAY[$i]}"
      if [ "$DRY" = "1" ]; then
        printf '    %s[dry-run]%s 将执行：docker compose -f %s down\n' "$C_YELLOW" "$C_RESET" "$cf"
      elif as_user_exec docker compose -f "$cf" down >/dev/null 2>&1; then
        ok "已停止 ${REG_DISPLAY[$i]}"
      else
        skip "${REG_DISPLAY[$i]} 本来就没有在跑（或 docker 不可用）"
      fi
    fi
    if [ "${REG_ACT[$i]}" = "1" ] && [ -n "$ct" ] && command -v docker >/dev/null 2>&1; then
      begin "正在删除容器 ${ct}"
      if [ "$DRY" = "1" ]; then
        printf '    %s[dry-run]%s 将执行：docker rm -f %s\n' "$C_YELLOW" "$C_RESET" "$ct"
      elif as_user_exec docker rm -f "$ct" >/dev/null 2>&1; then
        ok "已删除容器 ${ct}"
      else
        skip "本来就没有容器 ${ct}"
      fi
    fi
    i=$((i + 1))
  done
}

colima_delete() {
  local has=0 i=0
  while [ "$i" -lt "${#REG_NAME[@]}" ]; do
    [ "${REG_LABEL[$i]}" = "com.zizdog.colima" ] && has=1
    i=$((i + 1))
  done
  [ "$has" = "1" ] || return 0
  begin "正在删除 Colima 虚拟机（其中的容器/镜像/卷都会消失）"
  if [ "$DRY" = "1" ]; then
    printf '    %s[dry-run]%s 将执行：colima delete -f\n' "$C_YELLOW" "$C_RESET"
    return 0
  fi
  if command -v colima >/dev/null 2>&1 && as_user_exec colima delete -f >/dev/null 2>&1; then
    ok "已删除 Colima 虚拟机"
  else
    warn "colima delete 未成功（可能本来就没有虚拟机）；数据目录仍按清单处理"
  fi
}

remove_firewall_entry() {
  local fw=""
  fw="$(command -v socketfilterfw 2>/dev/null || true)"
  if [ -z "$fw" ]; then fw="/usr/libexec/ApplicationFirewall/socketfilterfw"; fi
  [ -x "$fw" ] || return 0
  begin "正在移除防火墙条目：${APPS_DIR}/ZizPanel.app"
  if [ "$DRY" = "1" ]; then
    printf '    %s[dry-run]%s 将执行：%s --remove %s\n' "$C_YELLOW" "$C_RESET" "$fw" "$APPS_DIR/ZizPanel.app"
    return 0
  fi
  "$fw" --remove "$APPS_DIR/ZizPanel.app" >/dev/null 2>&1 || true
}

remove_codesign_trust() {
  local crt="" c
  for c in "$PANEL_ROOT/data/zizpanel-codesign.crt" "$PANEL_ROOT/zizpanel-codesign.crt"; do
    if [ -f "$c" ]; then crt="$c"; break; fi
  done
  if [ -z "$crt" ]; then
    skip "本来就没有代码签名证书（无需撤销信任）"
    return 0
  fi
  begin "正在撤销代码签名证书信任：${crt}"
  if [ "$DRY" = "1" ]; then
    printf '    %s[dry-run]%s 将执行：security remove-trusted-cert -d %s\n' "$C_YELLOW" "$C_RESET" "$crt"
    return 0
  fi
  if security remove-trusted-cert -d "$crt" >/dev/null 2>&1; then
    ok "已撤销证书信任"
  else
    bad "撤销证书信任失败：${crt}（可手工在「钥匙串访问」里删掉 ZizPanel Release 证书）"
    record_failed "撤销证书信任失败 ${crt}"
  fi
}

delete_paths_from_plan() {
  title "删除文件与配置"
  local p
  for p in ${PLAN_DELETE[@]+"${PLAN_DELETE[@]}"}; do
    [ "$p" = "$PANEL_ROOT" ] && continue   # 由 remove_panel_root 处理（要先保住备份）
    remove_path "$p"
  done
}

remove_panel_root() {
  [ "$PANEL_ROOT_SPECIAL" = "1" ] || return 0
  local bdir="$PANEL_ROOT/work/backup"
  if [ -d "$bdir" ] && [ "$PURGE_BACKUPS" != "1" ]; then
    begin "正在保留面板备份（不删）：${bdir}"
    if [ "$DRY" = "1" ]; then
      printf '    %s[dry-run]%s 将把 %s 移到 %s/panel-backups\n' "$C_YELLOW" "$C_RESET" "$bdir" "$BACKUP_DIR"
    else
      mkdir -p "$BACKUP_DIR" 2>/dev/null
      if mv "$bdir" "$BACKUP_DIR/panel-backups" 2>/dev/null; then
        ok "面板备份已转移到 ${BACKUP_DIR}/panel-backups（加 --purge-backups 才删）"
      else
        bad "面板备份转移失败 —— 为避免丢备份，${PANEL_ROOT} 不再整体删除"
        record_failed "面板备份转移失败 ${bdir}"
        return 0
      fi
    fi
  fi
  remove_path "$PANEL_ROOT" "面板数据（配置/数据库/证书/日志/工作目录）"
}

purge_old_backups() {
  [ "$PURGE_BACKUPS" = "1" ] || return 0
  local d
  for d in "$REAL_HOME"/zizpanel-uninstall-backup-*; do
    [ -d "$d" ] || continue
    [ "$d" = "$BACKUP_DIR" ] && continue
    remove_path "$d" "旧卸载备份"
  done
}

mode2_data_notice() {
  say ""
  say "  ${C_BOLD}数据都在，没动：${C_RESET}"
  [ -d "$REAL_HOME/www" ] && say "    · 站点文件：${REAL_HOME}/www（$(path_size "$REAL_HOME/www")）"
  local p
  for p in "${BREW_PREFIXES[@]}"; do
    [ -d "$p/var/mysql" ] && say "    · 数据库数据：${p}/var/mysql（$(path_size "$p/var/mysql")）"
  done
  [ -d "$PANEL_ROOT/data" ] && say "    · 面板数据：${PANEL_ROOT}/data"
  say "  要连这些一起清掉，请用模式 3：sudo bash uninstall.sh --mode 3"
}

execute_plan() {
  title "执行卸载（模式 ${MODE}：${MODE_DESC}）"
  stop_services_from_plan
  if [ "$MODE" != "1" ]; then
    brew_services_stop_from_plan
  fi
  if [ "$MODE" = "3" ]; then
    compose_and_docker_down
    colima_delete
  fi
  remove_firewall_entry
  remove_codesign_trust
  if [ "$MODE" != "1" ]; then
    uninstall_formulas_from_plan
  fi
  if [ "$MODE" = "3" ]; then
    uninstall_registry_apps
  fi
  delete_paths_from_plan
  if [ "$MODE" = "3" ]; then
    remove_panel_root
    purge_old_backups
  fi
  if [ "$MODE" = "2" ]; then
    mode2_data_notice
  fi
}

# ------------------------------------------------------------------ 核对 --
verify() {
  [ "$DRY" = "1" ] && return 0
  title "核对结果"
  if [ -e "$LINK_DIR/zizpanel" ]; then
    bad "面板入口仍在：${LINK_DIR}/zizpanel"; record_failed "面板入口未移除"
  else
    ok "面板入口已移除"
  fi
  if [ "$MODE" = "1" ] || [ "$MODE" = "2" ]; then
    if [ -f "$DB_FILE" ]; then
      ok "面板数据按预期保留：${DB_FILE}"
    else
      warn "面板数据不在 ${DB_FILE}（模式 ${MODE} 本该保留它）"
    fi
  fi
  if [ "$MODE" = "3" ]; then
    if [ -e "$PANEL_ROOT" ]; then
      bad "面板目录仍在：${PANEL_ROOT}"; record_failed "面板目录未删除"
    else
      ok "面板目录已删除：${PANEL_ROOT}"
    fi
  fi
  if [ -d "$REAL_HOME/www" ]; then
    ok "站点目录 ~/www 未被触碰（$(path_size "$REAL_HOME/www")）"
  fi
}

summary() {
  printf '\n%s完成。%s\n' "$C_BOLD" "$C_RESET"
  if [ "$DRY" = "1" ]; then
    say "  （dry-run：以上全是预演，一个字节都没改）"
  fi
  if [ -n "$BACKUP_DIR" ] && [ "$DRY" != "1" ]; then
    say "  备份：${BACKUP_DIR}（含 to-delete.txt 待删清单）"
  fi
  say ""
  say "  ${C_BOLD}已卸载${C_RESET}"
  if [ -n "$UNINSTALLED_LIST" ]; then printf '%s' "$UNINSTALLED_LIST" | sed 's/^/    · /'; else say "    （无）"; fi
  say "  ${C_BOLD}已删除${C_RESET}"
  if [ -n "$DELETED_LIST" ]; then printf '%s' "$DELETED_LIST" | sed 's/^/    · /'; else say "    （无）"; fi
  say "  ${C_BOLD}已保留${C_RESET}"
  local any_kept=0 x
  for x in ${PLAN_KEEP[@]+"${PLAN_KEEP[@]}"}; do printf '    · %s\n' "$x"; any_kept=1; done
  [ "$any_kept" = "1" ] || say "    （无）"
  say "  ${C_BOLD}失败${C_RESET}"
  if [ "$FAILED_COUNT" != "0" ]; then
    printf '%s' "$FAILED_LIST" | sed 's/^/    ✗ /'
  else
    say "    （无）"
  fi
  say ""
  say "  重新安装：curl -fsSL https://zizdog.com/zizpanel/install.sh | sudo bash"
  printf '\n'
}

# ------------------------------------------------------------------ 菜单 --
usage() {
  cat <<'EOF'
用法：sudo bash uninstall.sh [1|2|3] [--mode 1|2|3] [--yes] [--dry-run] [--purge-backups]

  1  仅卸载面板         保留面板数据（配置/数据库/证书/备份）与全部运行环境
  2  卸载面板及 LNMP    再卸 nginx / PHP(-FPM) / MySQL / MariaDB 及其配置；
                       保留 ~/www 与数据库数据目录（结尾会告知位置）
  3  卸载面板及所有环境 再卸面板登记表里由面板装过的所有软件，并删所有配置数据
                       （面板数据、数据库数据目录、应用数据、证书信任）；不可逆

  --yes            非交互模式：跳过确认（模式 3 仍需 DELETE-ALL 或显式 --yes）
  --dry-run        只打印"将要做什么/删什么"，一个字节都不改（可在任何机器上预演）
  --purge          旧写法的向后兼容：等价于 --mode 3 --yes
  --purge-backups  连面板备份（<面板根>/work/backup）与旧卸载备份一起删；默认保留
  -h, --help       显示本帮助

  实时进度：逐行打印 `→ 正在…` / `✓ 完成` / `· 本来就没有` / `✗ 失败`，删目录带大小。
EOF
}

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      1|2|3) MODE="$1"; shift ;;
      --mode) MODE="${2:-}"; shift 2 || shift ;;
      --mode=*) MODE="${1#--mode=}"; shift ;;
      --yes|-y) ASSUME_YES=1; shift ;;
      --dry-run|-n) DRY=1; shift ;;
      --purge|-p) MODE=3; ASSUME_YES=1; shift ;;
      --purge-backups) PURGE_BACKUPS=1; shift ;;
      --keep-backups) PURGE_BACKUPS=0; shift ;;
      -h|--help) usage; exit 0 ;;
      *) bad "未知参数：$1"; usage; exit 2 ;;
    esac
  done
  case "$MODE" in
    ""|1|2|3) : ;;
    *) bad "模式只能是 1 / 2 / 3（收到：${MODE}）"; exit 2 ;;
  esac
}

choose_mode() {
  if ! have_tty; then
    bad "没有终端可交互。请显式指定模式：sudo bash uninstall.sh --mode 1|2|3"
    exit 1
  fi
  printf '\n  请选择要做什么：\n\n'
  printf '    %s1%s  仅卸载面板             保留面板数据与全部运行环境\n' "$C_BOLD" "$C_RESET"
  printf '    %s2%s  卸载面板及 LNMP        保留 ~/www 与数据库数据\n' "$C_BOLD" "$C_RESET"
  printf '    %s3%s  卸载面板及所有环境     删面板数据、数据库数据与面板装过的软件（不可逆）\n' "$C_BOLD" "$C_RESET"
  printf '    %s0%s  退出\n\n' "$C_BOLD" "$C_RESET"
  local pick=""
  ask_line "  输入 1 / 2 / 3 / 0：" pick || { bad "读取输入失败"; exit 1; }
  MODE="$pick"
}

check_macos() {
  if [ "$(uname -s)" != "Darwin" ]; then
    if [ "$DRY" = "1" ]; then
      warn "非 macOS（$(uname -s)）：dry-run 仍可预演，但真跑只支持 macOS"
      return 0
    fi
    bad "本脚本只支持 macOS（当前：$(uname -s)）"
    exit 1
  fi
}

require_root() {
  [ "$DRY" = "1" ] && return 0
  [ "${ZIZPANEL_SANDBOX:-0}" = "1" ] && return 0
  if [ "$(id -u)" -ne 0 ]; then
    bad "需要 root：请用  curl … | sudo bash   或   sudo bash uninstall.sh"
    exit 1
  fi
}

describe_mode() {
  case "$MODE" in
    1)
      MODE_DESC="仅卸载面板"
      title "模式 1：仅卸载面板"
      say "  会删除：面板程序、LaunchDaemon、sudoers 规则、CLI 入口、证书信任"
      say "  会保留：面板数据（${PANEL_ROOT}/data）与全部运行环境（nginx/PHP/MySQL/MariaDB）"
      ;;
    2)
      MODE_DESC="卸载面板及 LNMP"
      title "模式 2：卸载面板及 LNMP"
      say "  会删除：模式 1 的全部，加上 nginx / 各版本 PHP(-FPM) / MySQL / MariaDB 及其配置"
      warn "期间网站会中断；面板数据与 ~/www、数据库数据目录都保留"
      ;;
    3)
      MODE_DESC="卸载面板及所有环境"
      title "模式 3：卸载面板及所有环境（不可逆）"
      warn "会删除：面板数据、数据库数据目录、应用数据、证书信任、面板装过的全部软件"
      say "  软件清单只来自面板登记表 ${DB_FILE}（读不到就拒绝执行，绝不猜）"
      say "  不会删：~/www（你的站点文件）、Homebrew 本身、你自己 brew 装的软件"
      ;;
  esac
}

confirm_all() {
  [ "$DRY" = "1" ] && return 0
  if [ "$ASSUME_YES" != "1" ]; then
    if have_tty; then
      if ! confirm_phrase "YES" "确认按上面的说明继续？"; then
        say "已取消，什么都没做。"; exit 0
      fi
    else
      bad "非交互模式必须显式授权：请加 --yes（模式 3 还需 DELETE-ALL）"
      exit 1
    fi
  fi
  if [ "$MODE" = "3" ]; then
    if [ "$ASSUME_YES" = "1" ] && ! have_tty; then
      warn "非交互模式 + --yes：跳过 DELETE-ALL 手工确认（该组合本身就是明确授权）"
    else
      if ! confirm_phrase "DELETE-ALL" "这是**不可逆**的操作：面板数据、数据库数据目录与面板装过的软件都会被删除（~/www 不动）。"; then
        say "确认字不匹配，已取消，什么都没做。"; exit 0
      fi
    fi
  fi
}

main() {
  parse_args "$@"   # 先解析：--dry-run 要能在任何机器上预演（含非 macOS）
  check_macos

  printf '\n%s╭────────────────────────────────────────────╮%s\n' "$C_BOLD" "$C_RESET"
  printf '%s│   ZizPanel 卸载向导（三档）                │%s\n' "$C_BOLD" "$C_RESET"
  printf '%s╰────────────────────────────────────────────╯%s\n' "$C_BOLD" "$C_RESET"
  [ "$DRY" = "1" ] && warn "dry-run 模式：只打印，不做任何修改"

  if [ -z "$MODE" ]; then choose_mode; fi
  case "$MODE" in
    0|q|quit) say "已退出，什么都没做。"; exit 0 ;;
    1|2|3) : ;;
    *) bad "无法识别的选择：$MODE"; exit 2 ;;
  esac

  require_root
  describe_mode

  # 模式 3 的软件清单必须在**删掉面板之前**从登记表枚举；读不到就拒绝（不许用 brew list 猜）。
  if [ "$MODE" = "3" ]; then
    if ! registry_collect; then
      if [ "$DRY" = "1" ]; then
        warn "登记表不可读：${REG_WHY}"
        warn "（真跑时模式 3 会在此拒绝执行，不会删任何东西）"
      else
        bad "登记表不可读：${REG_WHY}"
        bad "为避免误删你自己装的软件，模式 3 拒绝执行；可用模式 2（面板 + LNMP）。"
        exit 1
      fi
    fi
  fi

  confirm_all
  build_plan

  if [ "$DRY" = "1" ]; then
    title "dry-run 清单（以下是将要删除/卸载的全部内容）"
    emit_plan
  fi

  if [ "$MODE" != "1" ]; then
    backup_everything
  fi

  execute_plan
  verify
  summary

  if [ "$FAILED_COUNT" != "0" ] && [ "$DRY" != "1" ]; then
    printf '  %s上面有失败项：请把输出整段发给开发者%s\n\n' "$C_RED" "$C_RESET"
    exit 1
  fi
}

main "$@"
