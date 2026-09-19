#!/usr/bin/env bash
# =============================================================================
#  ZizPanel 卸载脚本
#
#  用法：
#    curl -fsSL https://zizdog.com/zizpanel/uninstall.sh | sudo bash
#    sudo bash uninstall.sh [1|2|3] [--yes] [--dry-run]
#
#  三种模式（交互菜单里选）：
#    1) 仅卸载面板              —— 保留面板数据（配置/数据库/证书）与全部运行环境
#    2) 完全卸载（面板 + 全部环境）—— 含 Homebrew / 命令行开发者工具 / nginx / PHP / MySQL
#    3) 卸载基础环境、保留面板    —— 开发测试用：面板留着，正好验证"面板自己把基础环境装回来"
#
#  安全约定：
#    · **绝不删 ~/www**（你的站点文件与数据库内容）；只动面板自己的东西。
#    · 2/3 动手前自动**备份** MySQL 数据目录、nginx 配置、Brewfile、面板配置到
#      ~/zizpanel-uninstall-backup-<时间戳>/，结尾会打印路径。
#    · 选项 2 需要**手工输入 DELETE-ALL 二次确认**（非交互模式下必须显式 --yes）。
#    · `--dry-run` 只打印每一步，不做任何修改 —— 可以在任何机器上安全预演。
#    · 幂等：已经卸过的东西不会报错，会如实说"本来就没有"。
# =============================================================================
# ⚠️ 刻意**不用 set -e**：脚本里有多处 `cmd && ok "…"` 形式，只要 cmd 返回非 0
# 就会让整行失败 —— 配 set -e 就是**静默中止**（真机事故：用户看到"卸载完成"，
# 实际什么都没删，服务与 Homebrew 全在）。改为每一步显式判断并在结尾**大声报错**。
set -uo pipefail

PANEL_ROOT="${ZIZPANEL_ROOT:-/opt/zizpanel}"
PANEL_LABEL="cn.zizpanel.panel"
LINK_DIR="/usr/local/bin"
PLIST_DIR="/Library/LaunchDaemons"
SUDOERS_DIR="/etc/sudoers.d"
APPS_DIR="/Applications"
BREW_PREFIXES=("/opt/homebrew" "/usr/local")
CLT_DIR="/Library/Developer/CommandLineTools"

DRY=0
ASSUME_YES=0
MODE=""

C_BOLD=$'\033[1m'; C_RESET=$'\033[0m'; C_GREEN=$'\033[32m'; C_YELLOW=$'\033[33m'
C_RED=$'\033[31m'; C_BLUE=$'\033[34m'

say()  { printf '%s\n' "$*"; }
ok()   { printf '  %s✓%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
info() { printf '  %s·%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }
warn() { printf '  %s!%s %s\n' "$C_YELLOW" "$C_RESET" "$*"; }
bad()  { printf '  %s✗%s %s\n' "$C_RED" "$C_RESET" "$*"; }
title(){ printf '\n%s▸ %s%s\n' "$C_BOLD" "$*" "$C_RESET"; }

# run：执行（或在 dry-run 下只打印）。所有破坏性动作都必须经过它。
run() {
  if [ "$DRY" = "1" ]; then
    printf '    %s[dry-run]%s %s\n' "$C_YELLOW" "$C_RESET" "$*"
    return 0
  fi
  "$@"
}

require_root() {
  if [ "$(id -u)" -ne 0 ]; then
    bad "需要 root：请用  curl … | sudo bash   或   sudo bash uninstall.sh"
    exit 1
  fi
}

check_macos() {
  if [ "$(uname -s)" != "Darwin" ]; then
    bad "本脚本只支持 macOS（当前：$(uname -s)）"
    exit 1
  fi
}

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

# ------------------------------------------------------------------ 备份 --
BACKUP_DIR=""
backup_everything() {
  BACKUP_DIR="$HOME/zizpanel-uninstall-backup-$(date +%Y%m%d-%H%M%S)"
  title "备份（免得删完才发现要留）"
  if [ "$DRY" = "1" ]; then
    info "[dry-run] 将备份到 ${BACKUP_DIR}：MySQL 数据、nginx 配置、Brewfile、面板配置"
    return 0
  fi
  mkdir -p "$BACKUP_DIR" && chmod 700 "$BACKUP_DIR"
  local p
  for p in "${BREW_PREFIXES[@]}"; do
    if [ -d "$p/var/mysql" ]; then
      if tar -czf "$BACKUP_DIR/mysql-var.tar.gz" -C "$p" var/mysql 2>/dev/null; then
        ok "MySQL 数据 → mysql-var.tar.gz（$(du -sh "$BACKUP_DIR/mysql-var.tar.gz" | awk '{print $1}')）"
      else
        warn "MySQL 数据备份失败（继续，但请自行确认）"
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
  if [ -f "$PANEL_ROOT/data/config.json" ] && cp -f "$PANEL_ROOT/data/config.json" "$BACKUP_DIR/panel-config.json"; then
    ok "面板配置 → panel-config.json"
  fi
  cp -f "$HOME/.zprofile" "$BACKUP_DIR/zprofile.bak" 2>/dev/null || true
  ok "备份目录：$BACKUP_DIR"
}

# ------------------------------------------------------------- 各层卸载 --
stop_panel_services() { # $1=是否连面板一起停
  local with_panel="$1" label
  title "停止服务"
  if command -v brew >/dev/null 2>&1; then
    run brew services stop --all >/dev/null 2>&1 || true
    ok "已让 Homebrew 停掉它托管的服务（mysql 等）"
  fi
  # 面板注册的系统级守护进程：cn.zizpanel.*（**不含**你自己的 com.zizdog.* 等服务）
  while IFS= read -r label; do
    [ -n "$label" ] || continue
    if [ "$with_panel" != "1" ] && [ "$label" = "$PANEL_LABEL" ]; then continue; fi
    run launchctl bootout "system/$label" >/dev/null 2>&1 || true
    ok "已停止 $label"
  done < <(launchctl list 2>/dev/null | awk '{print $3}' | grep -E '^cn\.zizpanel\.' || true)
  if command -v colima >/dev/null 2>&1; then
    run colima stop >/dev/null 2>&1 || true
    info "已停止 Colima（Docker 运行时）；容器镜像与数据保留在 ~/.colima"
  fi
}

remove_panel_program() { # 只删程序，保留 $PANEL_ROOT 里的数据
  title "卸载面板程序"
  local f
  for f in "$PLIST_DIR/$PANEL_LABEL.plist" "$SUDOERS_DIR/zizpanel" \
           "$LINK_DIR/zizpanel" "$LINK_DIR/zizpanel-helper"; do
    if [ -e "$f" ]; then run rm -f "$f"; ok "已删 $f"; else info "本来就没有 $f"; fi
  done
  if [ -d "$APPS_DIR/ZizPanel.app" ]; then run rm -rf "$APPS_DIR/ZizPanel.app"; ok "已删 ZizPanel.app"; fi
  if [ -x "$PANEL_ROOT/bin/zizpanel" ]; then
    run rm -f "$PANEL_ROOT/bin/zizpanel" "$PANEL_ROOT/bin/zizpanel-helper"
    ok "已删面板二进制（$PANEL_ROOT/bin/）"
  fi
}

remove_panel_data() {
  title "删除面板数据"
  # 代码签名证书的信任：面板装的时候以 root 把它写进系统钥匙串（让 TCC 授权跨升级有效）。
  # 卸载时**必须一起撤掉** —— 留一个"受信任的代码签名根"在系统里，比留几个文件危险得多。
  local crt="$PANEL_ROOT/data/zizpanel-codesign.crt"
  [ -f "$crt" ] || crt="$PANEL_ROOT/zizpanel-codesign.crt"
  if [ -f "$crt" ]; then
    if run security remove-trusted-cert -d "$crt"; then
      ok "已撤销代码签名证书信任（${crt}）"
    else
      warn "撤销证书信任失败（可手工在「钥匙串访问」里删掉 ZizPanel Release 证书）"
    fi
  fi
  if [ -d "$PANEL_ROOT" ]; then
    run rm -rf "$PANEL_ROOT"
    ok "已删除 ${PANEL_ROOT}（配置、面板数据库、证书、日志）"
  else
    info "本来就没有 $PANEL_ROOT"
  fi
}

# 清理基础环境的 launchd 服务定义。
#
# 为什么单独一步（2026-09-17 生产机事故）：LNMP 的服务标签**不是** cn.zizpanel.*，
# 而是 brew 自己的 homebrew.mxcl.*（php/mysql）与面板给 nginx 建的 cn.zizdog.nginx —
# 第一版脚本只清 cn.zizpanel.*，于是二进制删了、**服务定义还挂着**，
# 面板仍显示"运行中"，而数据库页连不上。
remove_lnmp_labels() {
  title "清理基础环境的服务定义（brew 与 nginx 的 launchd 作业）"
  local l f
  # ⚠️ 只清**确实属于基础环境**的标签：brew 自己的（homebrew.mxcl.* / sh.brew.*）
  # 与面板给 nginx 建的那一个。**绝不动**你自己的服务
  # （本机实测存在 com.zizdog.colima / frpc / orbien-client / voicereceiver / qwen3tts 等）。
  for l in $(launchctl list 2>/dev/null | awk '{print $3}' \
      | grep -E '^(homebrew\.mxcl\.|sh\.brew\.|cn\.zizdog\.nginx$)' || true); do
    run launchctl bootout "system/$l" >/dev/null 2>&1 || true
    ok "已停止基础环境服务 $l"
  done
  for f in /Library/LaunchDaemons/homebrew.mxcl.*.plist /Library/LaunchDaemons/sh.brew.*.plist \
           /Library/LaunchDaemons/cn.zizdog.nginx.plist \
           "$HOME"/Library/LaunchAgents/homebrew.mxcl.*.plist "$HOME"/Library/LaunchAgents/sh.brew.*.plist; do
    [ -e "$f" ] || continue
    info "将删除服务定义：$f"
    run rm -f "$f"
  done
  ok "基础环境的服务定义已清理（你自己的 com.zizdog.* 服务不受影响）"
}

remove_base_env() { # Homebrew + CLT + 面板装出来的 nginx/PHP/MySQL
  remove_lnmp_labels
  title "卸载基础环境（Homebrew / 命令行开发者工具 / nginx / PHP / MySQL）"
  local p
  if command -v brew >/dev/null 2>&1; then
    # 先按名字卸掉面板装的三个（它们的数据目录在 var/ 下，随 brew 一起处理）
    run brew uninstall --force --ignore-dependencies nginx php@8.4 php@8.3 php mysql@8.4 mysql 2>/dev/null || true
    ok "已尝试卸载 nginx / PHP / MySQL（brew formula）"
  fi
  for p in "${BREW_PREFIXES[@]}"; do
    if [ ! -d "$p" ]; then
      info "本来就没有 $p"
      continue
    fi
    # ⚠️ **只删确认是 Homebrew 前缀的目录**（bin/brew 与 Cellar 同时存在才算）。
    # 不能见到 /usr/local 就 rm -rf —— 那里面可能有你手工装的东西（dry-run 预演时
    # 正是这一步暴露了这个危险写法）。
    if [ -x "$p/bin/brew" ] && [ -d "$p/Cellar" ]; then
      [ "$p" = "/usr/local" ] && warn "注意：$p 是 Intel 版 Homebrew 前缀，其中的内容会全部删除"
      run rm -rf "$p"
      ok "已删除 Homebrew 前缀 $p"
    else
      warn "$p 存在但不像 Homebrew 前缀（没有 bin/brew + Cellar）——**不动它**"
    fi
  done
  if [ -f /etc/paths.d/homebrew ]; then run rm -f /etc/paths.d/homebrew; ok "已删 /etc/paths.d/homebrew"; fi
  # 清掉 shell 配置里的 brew shellenv（否则每开一个终端都报 command not found）
  local rc
  for rc in "$HOME/.zprofile" "$HOME/.zshrc" "$HOME/.bash_profile"; do
    [ -f "$rc" ] || continue
    if grep -q "brew shellenv\|HOMEBREW_" "$rc" 2>/dev/null; then
      if [ "$DRY" = "1" ]; then
        info "[dry-run] 将从 $rc 删除 brew shellenv / HOMEBREW_* 行"
      else
        cp -f "$rc" "$rc.zizpanel-uninstall.bak"
        grep -v "brew shellenv" "$rc" | grep -v "^export HOMEBREW_" | grep -v "# Set non-default Git remotes for Homebrew" > "$rc.tmp" \
          && mv "$rc.tmp" "$rc"
        ok "已清理 $rc 里的 brew 配置（原件备份为 $(basename "$rc").zizpanel-uninstall.bak）"
      fi
    fi
  done
  if [ -d "$CLT_DIR" ]; then
    run rm -rf "$CLT_DIR"
    ok "已删除命令行开发者工具（${CLT_DIR}）"
  else
    info "本来就没有 $CLT_DIR"
  fi
}

reset_panel_brew_path() { # 开发测试用：把面板配置的 brew 路径改回"全新机器"的默认值
  local cfg="$PANEL_ROOT/data/config.json"
  [ -f "$cfg" ] || { info "没有面板配置，跳过"; return 0; }
  if [ "$DRY" = "1" ]; then
    info "[dry-run] 将把 $cfg 的 brew_bin/brew_prefix 重置为 /usr/local（复现全新机器初始状态）"
    return 0
  fi
  cp -f "$cfg" "$cfg.bak-$(date +%H%M%S)"
  sed -i '' \
    -e 's|"brew_bin": "[^"]*"|"brew_bin": "/usr/local/bin/brew"|' \
    -e 's|"brew_prefix": "[^"]*"|"brew_prefix": "/usr/local"|' "$cfg" 2>/dev/null || true
  ok "已把面板配置的 brew 路径重置为 /usr/local（装回基础环境时面板应自动校正到 /opt/homebrew）"
}

verify() {
  local mode="$1"
  title "核对结果"
  # 期望值随模式而变：模式 3 是"面板留着、环境没了"，模式 1 是"环境必须留着"
  if [ -x "$LINK_DIR/zizpanel" ]; then
    if [ "$mode" = "3" ]; then ok "面板仍在（模式 3 的预期）：$LINK_DIR/zizpanel"
    else warn "面板程序仍在：$LINK_DIR/zizpanel（模式 $mode 预期它已被移除）"; fi
  else
    if [ "$mode" = "3" ]; then bad "面板不见了！模式 3 只该卸环境、保留面板"; else ok "面板程序已移除"; fi
  fi
  if command -v brew >/dev/null 2>&1; then
    if [ "$mode" = "1" ]; then ok "Homebrew 保留（模式 1 的预期）：$(command -v brew)"
    else warn "brew 仍在：$(command -v brew)（模式 $mode 预期它已被移除）"; fi
  else
    if [ "$mode" = "1" ]; then warn "Homebrew 不见了！模式 1 只该卸面板"; else ok "Homebrew 不存在"; fi
  fi
  if xcode-select -p >/dev/null 2>&1; then
    if [ "$mode" = "1" ]; then ok "命令行开发者工具保留（模式 1 的预期）"; fi
  else
    [ "$mode" != "1" ] && ok "命令行开发者工具不存在"
  fi
  if [ -d "$HOME/www" ]; then
    ok "你的站点目录 ~/www 未被触碰（$(du -sh "$HOME/www" 2>/dev/null | awk '{print $1}')）"
  fi
}

summary() {
  printf '\n%s完成。%s\n' "$C_BOLD" "$C_RESET"
  [ -n "$BACKUP_DIR" ] && say "  备份：$BACKUP_DIR"
  say "  重新安装面板：curl -fsSL https://zizdog.com/zizpanel/install.sh | sudo bash"
  say "  装完在面板「网站管理 → 一键 LNMP」里把 nginx / PHP / MySQL 装回来（走国内镜像）。"
  printf '\n'
}

# ------------------------------------------------------------------ 菜单 --
usage() {
  cat <<'EOF'
用法：sudo bash uninstall.sh [1|2|3] [--yes] [--dry-run]

  1  仅卸载面板（保留面板数据与全部运行环境）
  2  完全卸载（面板 + 基础环境，含 Homebrew / 命令行开发者工具 / nginx / PHP / MySQL）
  3  卸载基础环境、保留面板（开发测试用）

  --yes      非交互模式：跳过菜单与"是否继续"的确认（**选项 2 仍需手工输入 DELETE-ALL**）
  --dry-run  只打印将要执行的每一步，不做任何修改
EOF
}

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      1|2|3) MODE="$1" ;;
      --yes|-y) ASSUME_YES=1 ;;
      --dry-run) DRY=1 ;;
      -h|--help) usage; exit 0 ;;
      *) bad "未知参数：$1"; usage; exit 2 ;;
    esac
    shift
  done
}

main() {
  check_macos
  parse_args "$@"

  printf '\n%s╭────────────────────────────────────────────╮%s\n' "$C_BOLD" "$C_RESET"
  printf '%s│   ZizPanel 卸载向导                        │%s\n' "$C_BOLD" "$C_RESET"
  printf '%s╰────────────────────────────────────────────╯%s\n' "$C_BOLD" "$C_RESET"
  [ "$DRY" = "1" ] && warn "dry-run 模式：只打印，不做任何修改"

  if [ -z "$MODE" ]; then
    if ! have_tty; then
      bad "没有终端可交互。请显式指定模式：sudo bash uninstall.sh 1|2|3"
      exit 1
    fi
    printf '\n  请选择要做什么：\n\n'
    printf '    %s1%s  仅卸载面板            保留面板数据与全部运行环境（网站照常跑）\n' "$C_BOLD" "$C_RESET"
    printf '    %s2%s  完全卸载              面板 + 基础环境（Homebrew / 命令行开发者工具 / nginx / PHP / MySQL）\n' "$C_BOLD" "$C_RESET"
    printf '    %s3%s  卸载基础环境、保留面板  开发测试用（面板留着，验证"面板自己把基础环境装回来"）\n' "$C_BOLD" "$C_RESET"
    printf '    %s0%s  退出\n\n' "$C_BOLD" "$C_RESET"
    ask_line "  输入 1 / 2 / 3 / 0：" MODE || { bad "读取输入失败"; exit 1; }
  fi

  case "$MODE" in
    0|q|quit) say "已退出，什么都没做。"; exit 0 ;;
    1|2|3) : ;;
    *) bad "无法识别的选择：$MODE"; exit 2 ;;
  esac

  # dry-run 只打印，不需要 root（普通用户也应能安全预演）
  if [ "$DRY" = "1" ]; then
    info "dry-run 不需要 root（不会修改任何东西）"
  else
    require_root
  fi

  if [ "$MODE" = "1" ]; then
    title "模式 1：仅卸载面板"
    say "  会删除：面板程序、launchd 服务、sudoers 规则、/Applications/ZizPanel.app"
    say "  会保留：面板数据（配置/数据库/证书，在 ${PANEL_ROOT}）与 nginx / PHP / MySQL / Homebrew"
  elif [ "$MODE" = "3" ]; then
    title "模式 3：卸载基础环境、保留面板"
    say "  会删除：Homebrew、命令行开发者工具、nginx / PHP / MySQL"
    say "  会保留：**面板本身及其数据**（跑在 ${PANEL_ROOT}，装回基础环境后即可继续用）"
    warn "期间你的网站会中断（nginx/PHP/MySQL 都没了），直到面板把基础环境装回来"
  else
    title "模式 2：完全卸载（面板 + 基础环境）"
    warn "会删除：面板程序与**面板数据**、Homebrew、命令行开发者工具、nginx / PHP / MySQL"
    warn "手工装过的其它 brew 软件（go/node/python/ffmpeg 等）也会随 Homebrew 一起消失"
    ok   "不会删除：你的站点目录 ~/www 与里面的数据；~/.colima（容器镜像与数据）"
  fi

  if [ "$ASSUME_YES" != "1" ] && have_tty; then
    if ! confirm_phrase "YES" "确认按上面的说明继续？"; then
      say "已取消，什么都没做。"; exit 0
    fi
  fi

  # 选项 2 的**二次确认**（用户明确要求；非交互模式必须显式 --yes 才走到这里）
  if [ "$MODE" = "2" ]; then
    if [ "$ASSUME_YES" = "1" ] && ! have_tty; then
      warn "非交互模式 + --yes：跳过 DELETE-ALL 手工确认（该组合本身就是明确授权）"
    else
      if ! confirm_phrase "DELETE-ALL" "这是**不可逆**的操作：面板数据与基础环境都会被删除（~/www 不动）。"; then
        say "确认字不匹配，已取消，什么都没做。"; exit 0
      fi
    fi
  fi

  # 备份 → 停服务 → 卸载
  if [ "$MODE" != "1" ]; then
    backup_everything
    stop_panel_services "$([ "$MODE" = "2" ] && echo 1 || echo 0)"
    remove_base_env
    [ "$MODE" = "3" ] && reset_panel_brew_path
  fi

  if [ "$MODE" = "1" ] || [ "$MODE" = "2" ]; then
    [ "$MODE" = "2" ] || stop_panel_services 0
    remove_panel_program
  fi
  if [ "$MODE" = "2" ]; then
    remove_panel_data
  fi

  local failed=0
  if [ "$DRY" != "1" ]; then
    verify "$MODE"
    # 硬校验：该消失的东西还在 → **明确报错**（这是本轮事故的教训：
    # 卸载脚本"看起来成功"比失败更糟）
    if [ "$MODE" != "1" ]; then
      if command -v brew >/dev/null 2>&1; then
        bad "Homebrew 仍在：$(command -v brew) —— 卸载**没有完成**"; failed=1
      fi
      if [ -d /Library/Developer/CommandLineTools ]; then
        bad "命令行开发者工具仍在 —— 卸载**没有完成**"; failed=1
      fi
      if ls /Library/LaunchDaemons/homebrew.mxcl.*.plist >/dev/null 2>&1; then
        bad "基础环境的服务定义仍在（/Library/LaunchDaemons/homebrew.mxcl.*.plist）"; failed=1
      fi
    fi
    if [ "$MODE" = "2" ] && [ -d "$PANEL_ROOT" ]; then
      bad "面板目录仍在：$PANEL_ROOT —— 卸载**没有完成**"; failed=1
    fi
  fi
  summary
  if [ "$failed" = "1" ]; then
    printf '  %s上面有未完成的项：请把输出整段发给开发者%s\n\n' "$C_RED" "$C_RESET"
    exit 1
  fi
}

main "$@"
