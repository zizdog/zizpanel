#!/usr/bin/env bash
# =============================================================================
#  tools/server-mode-test.sh —— 服务器模式脚本的回归测试
#
#  为什么需要它：
#    server-mode.sh 里最关键、也最容易搞错的一段是「开启 SSH」。
#    它踩过一次真实的坑并且被我误判成了"不可能自动化"：
#        launchctl enable system/com.openssh.sshd   → 返回成功，但 22 端口不监听
#        launchctl bootstrap system .../ssh.plist   → 这一条才是真正加载服务
#    当时只做了 enable，看到端口没起来，就错误地得出"必须手工去系统设置点"的结论。
#
#    所以本测试的核心断言就是：**从 SSH 关闭状态运行脚本，
#    必须同时发出 enable 和 bootstrap 两条命令**。
#    只保留其中任何一条，测试都会失败。
#
#  做法：
#    用桩件替换 launchctl / systemsetup / lsof / pmset / defaults / mdutil，
#    桩件维护一个"服务状态文件"，让 lsof 的输出取决于 bootstrap 是否真的执行过
#    —— 这样"只 enable 不 bootstrap"就会被如实识别为失败，而不是假通过。
#    不需要 root（脚本支持 ZIZPANEL_SANDBOX=1 跳过 root 检查）。
#
#  用法：bash tools/server-mode-test.sh
#  退出码 0 表示通过。
# =============================================================================
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SANDBOX="${SERVER_MODE_TEST_DIR:-/tmp/zizpanel-servermode-test}"
rm -rf "$SANDBOX"; mkdir -p "$SANDBOX/stubs"

FAILURES=0
C_GREEN=$'\033[32m'; C_RED=$'\033[31m'; C_BOLD=$'\033[1m'; C_RESET=$'\033[0m'
pass() { printf '  %s✓%s %s\n' "$C_GREEN" "$C_RESET" "$1"; }
fail() { printf '  %s✗%s %s\n' "$C_RED" "$C_RESET" "$1"; FAILURES=$((FAILURES + 1)); }
step() { printf '\n%s▸ %s%s\n' "$C_BOLD" "$1" "$C_RESET"; }

cleanup() {
  [ "${KEEP_SERVER_MODE_SANDBOX:-0}" = "1" ] && { echo "沙箱已保留：$SANDBOX"; return 0; }
  rm -rf "$SANDBOX"
  return 0
}
trap cleanup EXIT INT TERM

printf '%s=== 服务器模式脚本测试 ===%s\n' "$C_BOLD" "$C_RESET"
printf '沙箱：%s\n' "$SANDBOX"

STUB="$SANDBOX/stubs"
STATE="$SANDBOX/state"        # 每行一个状态：ssh_enabled / ssh_bootstrapped
LOG="$SANDBOX/calls.log"
: > "$STATE"; : > "$LOG"

# ------------------------------------------------------------------ 准备桩件 --
step "准备系统命令桩件"

# launchctl：记录调用，并维护状态。**故意让 enable 不产生"已加载"效果**，
# 只有 bootstrap 才会让 lsof 看到端口 —— 这正是真实 launchd 的行为。
cat > "$STUB/launchctl" <<STUBEOF
#!/usr/bin/env bash
echo "launchctl \$*" >> "$LOG"
case "\$*" in
  "enable system/com.openssh.sshd")  echo ssh_enabled >> "$STATE" ;;
  "disable system/com.openssh.sshd") : > "$STATE" ;;
  *"bootstrap"*ssh.plist*)           echo ssh_bootstrapped >> "$STATE" ;;
  *"bootout"*)                       : > "$STATE" ;;
esac
exit 0
STUBEOF

# lsof：只有 bootstrap 执行过才报告 22 端口在监听
cat > "$STUB/lsof" <<STUBEOF
#!/usr/bin/env bash
grep -q ssh_bootstrapped "$STATE" 2>/dev/null || exit 1
case "\$*" in *":22"*) echo "launchd 1 root 42u IPv6 TCP *:22 (LISTEN)"; exit 0 ;; esac
exit 1
STUBEOF

# systemsetup：状态由 STUB_STUBBORN 决定是否"拒绝配合"
cat > "$STUB/systemsetup" <<STUBEOF
#!/usr/bin/env bash
echo "systemsetup \$*" >> "$LOG"
case "\$*" in
  -getremotelogin)
    grep -q ssh_bootstrapped "$STATE" 2>/dev/null && echo "Remote Login: On" || echo "Remote Login: Off" ;;
  -setremotelogin)
    # 模拟"没有完全磁盘访问权限"的失败
    [ "\${STUB_SYSTEMSETUP_WORKS:-0}" = "1" ] || { echo "You need administrator access to run this tool... exiting!"; exit 1; }
    echo ssh_bootstrapped >> "$STATE" ;;
esac
exit 0
STUBEOF

# pmset：能力列表可控（默认为台式机，支持 autorestart；用 PMSET_NO_AUTORESTART=1 模拟笔记本）
cat > "$STUB/pmset" <<'STUBEOF'
#!/usr/bin/env bash
case "$*" in
  "-g cap")
    echo "Capabilities for AC Power:"
    for k in displaysleep disksleep sleep womp standby powernap hibernatemode; do echo "$k"; done
    [ "${PMSET_NO_AUTORESTART:-0}" = "1" ] || echo "autorestart" ;;
  "-g"|"-g custom") echo " sleep 0"; echo " displaysleep 10" ;;
esac
exit 0
STUBEOF

for c in defaults mdutil; do
  cat > "$STUB/$c" <<STUBEOF
#!/usr/bin/env bash
echo "$c \$*" >> "$LOG"
exit 0
STUBEOF
done
# sysctl 取型号、sw_vers 取版本：走真实命令即可，但为了结果稳定这里也给桩件
cat > "$STUB/sysctl" <<'STUBEOF'
#!/usr/bin/env bash
[ "$1" = "-n" ] && [ "$2" = "hw.model" ] && { echo "Mac16,12"; exit 0; }
/usr/sbin/sysctl "$@"
STUBEOF
cat > "$STUB/system_profiler" <<'STUBEOF'
#!/usr/bin/env bash
# 模拟笔记本（有电池）—— 用 STUB_DESKTOP=1 变成台式机
[ "${STUB_DESKTOP:-0}" = "1" ] || { echo "Battery Information:"; echo "  Cycle Count: 10"; }
exit 0
STUBEOF
chmod +x "$STUB"/*
pass "桩件就绪（launchctl / lsof / systemsetup / pmset / defaults / mdutil）"

export PATH="$STUB:/usr/bin:/bin:/usr/sbin:/sbin"
export ZIZPANEL_SANDBOX=1

run_mode() {  # run_mode [额外参数...]
  : > "$LOG"
  bash "$REPO/tools/server-mode.sh" "$@" > "$SANDBOX/out.log" 2>&1
}

# ------------------------------------------------- 核心：从-off 到-on 的路径 --
step "SSH 从关闭状态开启：必须 enable + bootstrap 都发"
: > "$STATE"
run_mode
CALLS="$(cat "$LOG")"
if grep -q "enable system/com.openssh.sshd" <<< "$CALLS"; then
  pass "发出了 launchctl enable"
else
  fail "没有发出 launchctl enable"
fi
if grep -q "bootstrap" <<< "$CALLS" && grep -q "ssh.plist" <<< "$CALLS"; then
  pass "发出了 launchctl bootstrap（只 enable 是不够的 —— 这正是当初踩的坑）"
else
  fail "缺少 launchctl bootstrap：22 端口不会监听，SSH 实际没开起来"
fi
if grep -q "远程登录（SSH）已开启，22 端口正在监听" "$SANDBOX/out.log"; then
  pass "脚本如实报告 SSH 已开启（以端口监听为准）"
else
  fail "脚本没有报告 SSH 开启成功，实际输出："
  grep -E "SSH|远程登录|无法自动" "$SANDBOX/out.log" | sed 's/^/      /'
fi
if grep -q "systemsetup -setremotelogin" <<< "$CALLS"; then
  fail "launchd 路径已成功，却仍然回退到了 systemsetup（不应发生）"
else
  pass "launchd 路径成功时不会多余地调用 systemsetup"
fi

# ------------------------------------------------------------ 已经是开的情况 --
step "SSH 已经开启时不应重复操作"
: > "$STATE"; echo ssh_enabled > "$STATE"; echo ssh_bootstrapped >> "$STATE"
run_mode
if grep -q "launchctl bootstrap" "$LOG"; then
  fail "SSH 已开启却仍执行了 bootstrap（幂等性被破坏）"
else
  pass "SSH 已开启时跳过，不重复操作"
fi

# ---------------------------------------------------------------- --ssh-only --
# install.sh 的"本机安装是否开 SSH"用的就是 --ssh-only：用户只答应了开 SSH，
# **没有**答应关睡眠/禁自动更新，所以那几步必须一个都不碰。
step "--ssh-only 只动 SSH，不碰电源/更新/Spotlight"
: > "$STATE"
run_mode --ssh-only
if grep -q "enable system/com.openssh.sshd" "$LOG" && grep -q "ssh.plist" "$LOG"; then
  pass "--ssh-only 仍然真正开启了 SSH（enable + bootstrap）"
else
  fail "--ssh-only 没有开启 SSH"
fi
if grep -qE "pmset" "$LOG"; then
  fail "--ssh-only 却改了电源设置（用户没有答应这件事）"
else
  pass "--ssh-only 完全不碰 pmset"
fi
if grep -qE "com.apple.SoftwareUpdate|mdutil|com.apple.CrashReporter" "$LOG"; then
  fail "--ssh-only 却改了系统更新/Spotlight/崩溃报告设置"
else
  pass "--ssh-only 完全不碰系统更新、Spotlight、崩溃报告设置"
fi

# ----------------------------------------------------------------- --no-ssh --
step "--no-ssh 时必须完全不碰 SSH"
: > "$STATE"
run_mode --no-ssh
if grep -qE "launchctl (enable|bootstrap)" "$LOG"; then
  fail "--no-ssh 却仍然操作了 launchctl"
else
  pass "--no-ssh 时完全不碰 SSH"
fi
if grep -q "远程登录（SSH）已开启" "$SANDBOX/out.log"; then
  fail "--no-ssh 时汇总里仍声称 SSH 已开启"
else
  pass "汇总如实反映 SSH 被跳过"
fi

# --------------------------------------------- launchd 路径失败时的兜底行为 --
step "launchd 路径失败时必须回退并如实报告，不能假称成功"
: > "$STATE"
cat > "$STUB/lsof" <<'STUBEOF'
#!/usr/bin/env bash
exit 1
STUBEOF
chmod +x "$STUB/lsof"
run_mode
if grep -q "systemsetup -setremotelogin on" "$LOG"; then
  pass "launchd 路径失败后回退到 systemsetup"
else
  fail "launchd 路径失败后没有回退尝试"
fi
if grep -q "无法自动开启 SSH" "$SANDBOX/out.log"; then
  pass "两条路径都失败时明确报告失败（不谎报）"
else
  fail "两条路径都失败却没有如实报告"
fi
if grep -q "远程登录（SSH）已开启" "$SANDBOX/out.log"; then
  fail "明明失败了却报告已开启 —— 这正是最危险的一类谎报"
else
  pass "没有在失败时谎报成功"
fi
cat > "$STUB/lsof" <<STUBEOF
#!/usr/bin/env bash
grep -q ssh_bootstrapped "$STATE" 2>/dev/null || exit 1
case "\$*" in *":22"*) echo "launchd 1 root 42u IPv6 TCP *:22 (LISTEN)"; exit 0 ;; esac
exit 1
STUBEOF
chmod +x "$STUB/lsof"

# ------------------------------------------------------------ pmset 能力探测 --
step "pmset 能力探测：不支持的键不能被谎报为成功"
: > "$STATE"; echo ssh_bootstrapped > "$STATE"
PMSET_NO_AUTORESTART=1 run_mode --no-ssh
if grep -q "不支持 autorestart" "$SANDBOX/out.log"; then
  pass "不支持 autorestart 时明确报告（不谎报断电自恢复已开启）"
else
  fail "pmset 不支持 autorestart 却没有报告"
fi
if grep -q "✅ 断电恢复后自动开机" "$SANDBOX/out.log"; then
  fail "不支持却报告已开启断电自恢复"
else
  pass "不支持时没有报告已开启"
fi
: > "$STATE"; echo ssh_bootstrapped > "$STATE"
STUB_DESKTOP=1 run_mode --no-ssh
if grep -q "✅ 断电恢复后自动开机" "$SANDBOX/out.log"; then
  pass "台式机（支持 autorestart）时正常报告已开启"
else
  fail "台式机支持 autorestart 却没有报告已开启"
fi

# ------------------------------------------------------------ dry-run 安全性 --
step "dry-run 不能真的修改任何东西"
: > "$STATE"; : > "$LOG"
run_mode --dry-run
if grep -qE "launchctl (enable|bootstrap)" "$LOG"; then
  fail "dry-run 却真的执行了 launchctl"
else
  pass "dry-run 不执行任何实际修改"
fi

# ------------------------------------------------------------------ 汇总 --
printf '\n%s────────────────────────────%s\n' "$C_BOLD" "$C_RESET"
if [ "$FAILURES" -eq 0 ]; then
  printf '%s✅ 服务器模式测试全部通过%s\n' "$C_GREEN" "$C_RESET"
  exit 0
fi
printf '%s❌ 服务器模式测试失败 %d 项%s\n' "$C_RED" "$FAILURES" "$C_RESET"
exit 1
