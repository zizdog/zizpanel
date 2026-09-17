#!/usr/bin/env bash
# =============================================================================
#  tools/server-mode.sh —— 把一台 Mac 配置成"长期稳定运行的服务器"
#
#  为什么需要它：macOS 的默认设置是为"桌面电脑"优化的 ——
#  会睡眠、会自动更新并重启、会为搜索做全盘索引。
#  这些行为对家用服务器是致命的：睡眠 = 服务离线，
#  自动重启 = 半夜断服，索引 = 磁盘与 CPU 被持续占用。
#
#  本脚本做三件事：
#    1. 关闭会打断服务的行为（睡眠、自动更新重启、磁盘索引）
#    2. 打开断电自恢复（家里跳闸后能自己起来）
#    3. 报告哪些设置**脚本做不到**、需要你手工点一次
#
#  幂等：可重复执行，不会累积副作用。
#
#  用法：
#    sudo bash tools/server-mode.sh [选项]
#
#  选项：
#    --dry-run            只显示将要做什么，不实际修改
#    --keep-security      保留 macOS 安全响应自动更新（推荐，默认行为）
#    --no-spotlight       同时关闭系统卷的 Spotlight 索引
#    --no-ssh             不开启「远程登录（SSH）」（默认会开启）
#    --ssh-only           只处理 SSH，不碰电源/更新/Spotlight 设置
#                         （install.sh 的"本机安装是否开 SSH"用它，避免顺手改用户的电源策略）
#    --with-tailscale     顺带安装 Tailscale（远程访问用，需联网）
#    --help               显示帮助
# =============================================================================
set -uo pipefail

DRY_RUN=0
KEEP_SECURITY=1
NO_SPOTLIGHT=0
WITH_TAILSCALE=0
ENABLE_SSH=1
# SSH_ONLY=1 时只做"开启 SSH"这一件事。
# 为什么需要它：install.sh 会在本机安装时单独问一句"要不要开 SSH"，
# 那时用户只答应了开 SSH，**没有**答应关睡眠/禁自动更新 —— 不能顺手改。
SSH_ONLY=0
# SSH 的最终结果：ok / failed / skipped / dry-run。
# 汇总必须读它，不能只看"--no-ssh 没传"，否则失败时汇总会谎报成功。
SSH_RESULT=""

C_RESET=$'\033[0m'; C_RED=$'\033[31m'; C_GREEN=$'\033[32m'
C_YELLOW=$'\033[33m'; C_BLUE=$'\033[34m'; C_BOLD=$'\033[1m'
info() { printf '%s[信息]%s %s\n' "$C_BLUE" "$C_RESET" "$*"; }
ok()   { printf '%s[完成]%s %s\n' "$C_GREEN" "$C_RESET" "$*"; }
warn() { printf '%s[跳过]%s %s\n' "$C_YELLOW" "$C_RESET" "$*"; }
err()  { printf '%s[需手工]%s %s\n' "$C_RED" "$C_RESET" "$*" >&2; }
title() { printf '\n%s━━━ %s ━━━%s\n' "$C_BOLD" "$*" "$C_RESET"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run)        DRY_RUN=1; shift ;;
    --keep-security)  KEEP_SECURITY=1; shift ;;
    --no-spotlight)   NO_SPOTLIGHT=1; shift ;;
    --with-tailscale) WITH_TAILSCALE=1; shift ;;
    --no-ssh)         ENABLE_SSH=0; shift ;;
    --ssh-only)       SSH_ONLY=1; shift ;;
    -h|--help)        sed -n '2,35p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 1 ;;
  esac
done

# 沙箱模式用于自动化测试（tools/server-mode-test.sh）；
# 生产环境必须 root —— 本脚本会改电源设置、系统更新策略与 SSH 状态。
if [ "${ZIZPANEL_SANDBOX:-0}" != "1" ]; then
  [ "$(id -u)" -eq 0 ] || { echo "请用 sudo 执行：sudo bash $0" >&2; exit 1; }
fi

# 真实用户（面板要以它身份运行，Tailscale 也要装在它的环境里）
REAL_USER="${SUDO_USER:-}"
if [ -z "$REAL_USER" ] || [ "$REAL_USER" = "root" ]; then
  REAL_USER="$(stat -f '%Su' /dev/console 2>/dev/null || echo "")"
fi
[ -n "$REAL_USER" ] && [ "$REAL_USER" != "root" ] || {
  echo "无法确定真实用户，请用 sudo -u <用户名> 方式执行" >&2; exit 1; }

# 本机 pmset 支持哪些键 —— 必须探测，不能假设。
# 反例（真机上踩过）：MacBook Air 的 pmset 不支持 autorestart，
# `pmset -a autorestart 1` 会**静默返回 0 但什么都不做**，
# 脚本却照样报告"已开启断电自恢复"——这是最危险的一类谎报。
PMSET_CAPS="$(pmset -g cap 2>/dev/null | sed 's/^Capabilities.*://' | tr '\n' ' ')"
supports() { printf '%s' "$PMSET_CAPS" | grep -qw "$1"; }

IS_PORTABLE=0
if system_profiler SPPowerDataType 2>/dev/null | grep -q 'Battery Information'; then
  IS_PORTABLE=1
fi

# run 执行命令；dry-run 时只打印
run() {
  if [ "$DRY_RUN" = "1" ]; then
    printf '    (dry-run) %s\n' "$*"
    return 0
  fi
  "$@" >/dev/null 2>&1
}

# run_cap KEY args... —— 仅在本机支持该 pmset 键时才执行
run_cap() {
  local key="$1"; shift
  if supports "$key"; then
    run "$@"
    return 0
  fi
  return 1
}

printf '\n%s╭──────────────────────────────────────────────╮%s\n' "$C_BOLD" "$C_RESET"
printf '%s│   Mac 服务器模式配置                          │%s\n' "$C_BOLD" "$C_RESET"
printf '%s│   目标：长期无人值守稳定运行                   │%s\n' "$C_BOLD" "$C_RESET"
printf '%s╰──────────────────────────────────────────────╯%s\n' "$C_BOLD" "$C_RESET"
[ "$DRY_RUN" = "1" ] && warn "这是 dry-run，不会做任何修改"
[ "$SSH_ONLY" = "1" ] && info "只处理 SSH（--ssh-only）：电源/更新/Spotlight 设置一律不动"

# ---------------------------------------------------------------------------
# 第 1–4 步（睡眠/自动更新/崩溃报告/Spotlight）在 --ssh-only 时整段跳过。
# 这是 install.sh"本机安装是否开 SSH"那个提问的落地方式：用户只答应了开 SSH。
# ---------------------------------------------------------------------------
if [ "$SSH_ONLY" != "1" ]; then
# ---------------------------------------------------------------------------
title "1/5 关闭睡眠（服务器睡眠等于服务离线）"
# ---------------------------------------------------------------------------
# 桌面 Mac 默认几分钟就睡眠。作为服务器必须全关：
#   sleep 0        系统不睡眠
#   disksleep 0    磁盘不休眠（避免休眠后首次访问要等磁盘唤醒）
#   displaysleep  显示器可以关（省电且不影响服务）
#   womp 1        网络唤醒（远程能叫醒它，作为双保险）
#   powernap 0    关闭 Power Nap（睡眠中后台活动会干扰服务）
#   autorestart 1 断电恢复后自动开机（家里跳闸能自己起来）
run_cap sleep        pmset -a sleep 0
run_cap disksleep    pmset -a disksleep 0
run_cap displaysleep pmset -a displaysleep 10
run_cap womp         pmset -a womp 1
run_cap powernap     pmset -a powernap 0
run_cap autorestart  pmset -a autorestart 1
if [ "$DRY_RUN" != "1" ]; then
  ok "已关闭系统与磁盘睡眠"
  printf '    当前电源设置：\n'
  pmset -g | grep -E '^ *(sleep|disksleep|displaysleep|womp|powernap)' | sed 's/^/      /'
fi

# 断电自恢复单独汇报：它是"跳闸后能不能自己起来"的关键，
# 但只有台式机（Mac mini / Studio / Pro）支持，笔记本无法设置。
if supports autorestart; then
  if [ "$DRY_RUN" != "1" ]; then
    ok "已开启断电自恢复（跳闸/断电后自动开机）"
  fi
else
  warn "本机 pmset 不支持 autorestart，已跳过（型号 $(sysctl -n hw.model 2>/dev/null)，不是台式机）"
  printf '    断电/跳闸后这台机器不会自动开机，需要人工按电源键。\n'
  printf '    长期做服务器建议换 Mac mini / Mac Studio（都支持该设置）。\n'
  printf '    若只能用这台笔记本：合盖会休眠，需外接电源+显示器+键鼠才能合盖运行。\n'
fi

# ---------------------------------------------------------------------------
title "2/5 关闭会打断服务的自动更新"
# ---------------------------------------------------------------------------
# 关键取舍：**不能把安全更新一起关掉**。
# 服务器长期联网，缺安全补丁的风险远大于"半夜重启"。
# 因此策略是：
#   关掉 自动下载/自动安装/自动重启（这些会不经你同意就重启）
#   保留 安全响应（CriticalUpdateInstall）与检查更新
# 需要装系统更新时由你在面板里手动触发（后续阶段会加这个功能）。
run defaults write /Library/Preferences/com.apple.SoftwareUpdate AutomaticDownload -bool false
run defaults write /Library/Preferences/com.apple.SoftwareUpdate AutomaticCheckEnabled -bool true
run defaults write /Library/Preferences/com.apple.SoftwareUpdate AutomaticallyInstallMacOSUpdates -bool false
run defaults write /Library/Preferences/com.apple.commerce AutoUpdate -bool false
if [ "$KEEP_SECURITY" = "1" ]; then
  run defaults write /Library/Preferences/com.apple.SoftwareUpdate CriticalUpdateInstall -bool true
  ok "已关闭自动下载与自动安装；保留安全检查与安全响应安装"
else
  run defaults write /Library/Preferences/com.apple.SoftwareUpdate CriticalUpdateInstall -bool false
  warn "已连安全响应一起关闭（不推荐：服务器长期联网，缺补丁风险更大）"
fi

# 关闭"夜间自动重启安装"这类会打断服务的机制
run defaults write /Library/Preferences/com.apple.SoftwareUpdate AutomaticRestartAfterUpdate -bool false 2>/dev/null || true

# ---------------------------------------------------------------------------
title "3/5 关闭崩溃报告弹窗与诊断上报"
# ---------------------------------------------------------------------------
# 服务器无人值守，弹窗会一直堆在那里；诊断上报会占带宽。
# 注意：崩溃报告本身**保留**（面板要看它来定位问题），只是不弹窗、不上报。
run defaults write /Library/Preferences/com.apple.CrashReporter DialogType -string none
run defaults write /Library/Application\ Support/CrashReporter/DiagnosticMessagesHistory.plist AutoSubmit -bool false
run defaults write /Library/Application\ Support/CrashReporter/DiagnosticMessagesHistory.plist SeedExpirationDays -int 0
ok "崩溃报告不再弹窗（日志仍保留在 /Library/Logs/DiagnosticReports）"

# ---------------------------------------------------------------------------
title "4/5 Spotlight 索引"
# ---------------------------------------------------------------------------
if [ "$NO_SPOTLIGHT" = "1" ]; then
  # 全盘索引会持续占用 CPU 与磁盘。对服务器没有价值（没人用 Spotlight 搜服务器）。
  run mdutil -a -i off
  ok "已关闭 Spotlight 索引（系统卷与外接卷）"
else
  info "Spotlight 索引保持开启。它会持续占用 CPU 与磁盘，"
  info "但关闭后系统设置里的搜索与邮件搜索也会失效。"
  info "如果这台机器只当服务器用，建议加 --no-spotlight 关闭。"
fi
fi  # SSH_ONLY != 1 的块结束

# ---------------------------------------------------------------------------
title "5/5 远程访问（SSH / Tailscale）"
# ---------------------------------------------------------------------------
# 关键事实（实测更正）：SSH **可以**由脚本开启，既不需要 GUI 点击，
# 也不需要「完全磁盘访问权限」。
#
# 网上普遍流传"命令行开不了远程登录"，那是因为唯一被文档化的命令
# `systemsetup -setremotelogin on` 确实需要调用方拥有完全磁盘访问（TCC），
# 这条路径走不通。但 root 还有第二条等价路径 —— 直接操作 launchd：
#
#     launchctl enable system/com.openssh.sshd     # 清禁用标记（写入持久化数据库）
#     launchctl bootstrap system /System/Library/LaunchDaemons/ssh.plist
#
# 只做 enable 是**不够的**（这是我一开始踩的坑：enable 返回成功但 22 端口不监听），
# 必须再 bootstrap 一次把服务真正加载进来。
#
# macOS 15.6.1 实测：两条命令后 launchd 立刻监听 22，
# `systemsetup -getremotelogin` 报告 On，与在系统设置里手工打开完全等价。
enable_ssh() {
  local state
  state="$(systemsetup -getremotelogin 2>/dev/null | sed 's/.*: //')"
  if [ "$state" = "On" ]; then
    SSH_RESULT="ok"
    ok "远程登录（SSH）已开启，无需处理"
    return 0
  fi

  if [ "$DRY_RUN" = "1" ]; then
    SSH_RESULT="dry-run"
    printf '    (dry-run) launchctl enable system/com.openssh.sshd\n'
    printf '    (dry-run) launchctl bootstrap system /System/Library/LaunchDaemons/ssh.plist\n'
    return 0
  fi

  info "正在开启远程登录（SSH）…"
  run launchctl enable system/com.openssh.sshd
  run launchctl bootstrap system /System/Library/LaunchDaemons/ssh.plist

  # 只信"真的在监听"，不信命令退出码 —— launchctl 的退出码相当不可靠。
  local waited=0
  while [ "$waited" -lt 20 ]; do
    if lsof -nP -iTCP:22 -sTCP:LISTEN >/dev/null 2>&1; then
      break
    fi
    sleep 0.25
    waited=$((waited + 1))
  done

  if lsof -nP -iTCP:22 -sTCP:LISTEN >/dev/null 2>&1; then
    SSH_RESULT="ok"
    ok "远程登录（SSH）已开启，22 端口正在监听"
    printf '    连接方式：ssh %s@<本机IP>\n' "$REAL_USER"
    printf '    关闭方式：sudo launchctl bootout system/com.openssh.sshd\n'
    printf '              sudo launchctl disable system/com.openssh.sshd\n'
    return 0
  fi

  # 兜底：极少数机型/版本上 launchd 路径可能被策略挡住，
  # 退回文档化命令（仅当调用方已有完全磁盘访问权限时才会成功）。
  warn "launchd 方式未能让 22 端口监听，改试文档化命令…"
  run systemsetup -setremotelogin on
  sleep 1
  if lsof -nP -iTCP:22 -sTCP:LISTEN >/dev/null 2>&1; then
    SSH_RESULT="ok"
    ok "远程登录（SSH）已开启（走 systemsetup 路径）"
    return 0
  fi

  SSH_RESULT="failed"
  err "无法自动开启 SSH。请在系统设置 → 通用 → 共享 手工打开「远程登录」"
  printf '    请把本机信息反馈给开发者：%s / macOS %s\n' \
    "$(sysctl -n hw.model 2>/dev/null)" "$(sw_vers -productVersion 2>/dev/null)"
}

if [ "$ENABLE_SSH" = "1" ]; then
  enable_ssh
else
  SSH_RESULT="skipped"
  info "按参数要求跳过 SSH 配置（--no-ssh）"
fi

if [ "$WITH_TAILSCALE" = "1" ]; then
  if [ -d "/Applications/Tailscale.app" ]; then
    ok "Tailscale 已安装"
  elif command -v brew >/dev/null 2>&1; then
    info "正在为 ${REAL_USER} 安装 Tailscale（需要联网，可能耗时几分钟）…"
    if [ "$DRY_RUN" = "1" ]; then
      printf '    (dry-run) brew install --cask tailscale\n'
    else
      # 必须降权：Homebrew 拒绝以 root 运行
      if sudo -u "$REAL_USER" brew install --cask tailscale >/dev/null 2>&1; then
        ok "Tailscale 已安装。接下来需要你登录一次账号："
        printf '      打开「应用程序 → Tailscale」并登录，之后这台机器会获得一个固定 IP\n'
      else
        warn "Tailscale 安装失败（可稍后在应用市场或手工安装）"
      fi
    fi
  else
    warn "未安装 Homebrew，跳过 Tailscale（可先装 Homebrew）"
  fi
fi

# ---------------------------------------------------------------------------
printf '\n%s━━━ 汇总 ━━━%s\n' "$C_BOLD" "$C_RESET"
cat <<'SUMMARY'

  脚本已完成（无需人工干预）：
    ✅ 关闭系统与磁盘睡眠
    ✅ 网络唤醒（远程可叫醒）
    ✅ 关闭自动下载/自动安装更新（保留安全补丁）
    ✅ 崩溃报告不弹窗

SUMMARY

case "$SSH_RESULT" in
  ok)      printf '    ✅ 远程登录（SSH）已开启（launchd 路径，无需 GUI 操作）\n' ;;
  dry-run) printf '    ⬜ 远程登录（SSH）未修改（dry-run）\n' ;;
  skipped) printf '    ⬜ 远程登录（SSH）按参数跳过（--no-ssh）\n' ;;
  *)       printf '    ⛔ 远程登录（SSH）**未能开启** —— 需在系统设置 → 通用 → 共享 手工打开\n' ;;
esac
if supports autorestart; then
  printf '    ✅ 断电恢复后自动开机（本机支持 autorestart）\n'
else
  printf '    ⛔ 断电恢复后自动开机：本机（%s）不支持该设置，脚本无法做到\n' \
    "$(sysctl -n hw.model 2>/dev/null)"
fi
if [ "$IS_PORTABLE" = "1" ]; then
  printf '    ⛔ 本机是笔记本：合盖会休眠，需外接电源+显示器+键鼠才能合盖当服务器\n'
fi

cat <<'SUMMARY2'

  建议但非必须：
    ⬜ 固定 IP：路由器里给这台机器绑定 DHCP 静态地址，
       这样面板地址不会变（或直接用 Tailscale 的固定 IP）
    ⬜ 系统设置 → 通用 → 登录项 → 关闭「自动登录」
       （无人值守服务器不建议自动登录。面板本身以 LaunchDaemon 运行，
         不需要有人登录；只有你希望断电重启后完全不需要人工干预、
         且能接受任何人都能物理开机进系统时，才考虑开自动登录）

SUMMARY2
