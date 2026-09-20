#!/usr/bin/env bash
# =============================================================================
#  tools/uninstall-modes-test.sh —— 三档卸载脚本的沙箱回归测试
#
#  为什么单独一套：uninstall.sh 的破坏面最大（删软件、删数据），而真机验证
#  代价极高。这里把系统路径全部重定向到沙箱，并用桩件替换 launchctl / brew /
#  security / colima / docker / rm，然后跑**真实的 uninstall.sh**。
#
#  覆盖的门禁：
#    · --dry-run 1|2|3 三档都跑通，且**没有执行任何破坏性命令**；
#    · 模式 1 不出现卸载 nginx/php/数据库；模式 2 出现 LNMP 但不删面板数据/应用；
#      模式 3 出现"所有环境 + 数据"；
#    · 三档都**绝不删 ~/www**（铁律，显式断言）；
#    · 模式 3 缺 --yes（非交互）必须中止、且什么都不删；
#    · 失败会被计入结尾汇总（注入 security 必然失败）；
#    · 模式 3 只从面板登记表枚举软件：用户自己纳管的 Redis 必须保留。
#
#  用法：bash tools/uninstall-modes-test.sh     退出码 0 表示通过。
#  不会碰到真机路径（全部走 ZIZPANEL_* 覆盖 + 沙箱 HOME）。
# =============================================================================
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SANDBOX="${ZIZPANEL_UNINSTALL_TEST_DIR:-/tmp/zizpanel-uninstall-modes-test}"
# 桩件要用 ${SANDBOX} 写日志，必须导出
export SANDBOX
STUB="$SANDBOX/stubs"
CALLS="$SANDBOX/calls.log"

HOME_DIR="$SANDBOX/home"
PREFIX="$HOME_DIR/homebrew"
ROOT="$SANDBOX/root"
PLIST="$SANDBOX/Library/LaunchDaemons"
SUDOERS="$SANDBOX/etc/sudoers.d"
LINK="$SANDBOX/usr/local/bin"
APPS="$SANDBOX/Applications"

FAILURES=0
C_GREEN=$'\033[32m'; C_RED=$'\033[31m'; C_BOLD=$'\033[1m'; C_RESET=$'\033[0m'
pass() { printf '  %s✓%s %s\n' "$C_GREEN" "$C_RESET" "$1"; }
fail() { printf '  %s✗%s %s\n' "$C_RED" "$C_RESET" "$1"; FAILURES=$((FAILURES + 1)); }
step() { printf '\n%s▸ %s%s\n' "$C_BOLD" "$1" "$C_RESET"; }

cleanup() {
  if [ "${FAILURES:-0}" -ne 0 ]; then
    printf '\n沙箱已保留以便排障：%s\n' "$SANDBOX"
    return 0
  fi
  [ "${KEEP_SANDBOX:-0}" = "1" ] || rm -rf "$SANDBOX"
}
trap cleanup EXIT INT TERM

rm -rf "$SANDBOX"
mkdir -p "$STUB" "$SANDBOX"

printf '%s=== 三档卸载脚本沙箱测试 ===%s\n' "$C_BOLD" "$C_RESET"
printf '沙箱目录: %s\n' "$SANDBOX"

# ------------------------------------------------------------------ 桩件 --
step "准备系统命令桩件"
cat > "$STUB/brew" <<'STUBEOF'
#!/usr/bin/env bash
echo "brew $*" >> "${SANDBOX}/calls.log"
case "$*" in
  "list --formula") printf '%s\n' nginx php@8.2 mysql@8.4 mariadb redis ffmpeg; exit 0 ;;
  "list --formula "*) exit 0 ;;
  uninstall*) exit 0 ;;
  "services stop"*) exit 0 ;;
  bundle*) for a in "$@"; do case "$a" in --file=*) : > "${a#--file=}" ;; esac; done; exit 0 ;;
esac
exit 0
STUBEOF

cat > "$STUB/rm" <<'STUBEOF'
#!/usr/bin/env bash
echo "rm $*" >> "${SANDBOX}/calls.log"
exec /bin/rm "$@"
STUBEOF

cat > "$STUB/launchctl" <<'STUBEOF'
#!/usr/bin/env bash
echo "launchctl $*" >> "${SANDBOX}/calls.log"
if [ "$1" = "list" ]; then
  printf 'PID\tStatus\tLabel\n'
  printf '%s\t%s\t%s\n' '-' '0' 'cn.zizpanel.panel'
  printf '%s\t%s\t%s\n' '-' '0' 'cn.zizpanel.cron.daily-backup'
  printf '%s\t%s\t%s\n' '-' '0' 'homebrew.mxcl.nginx'
  printf '%s\t%s\t%s\n' '-' '0' 'com.zizdog.filebrowser'
  printf '%s\t%s\t%s\n' '-' '0' 'com.zizdog.colima'
fi
exit 0
STUBEOF

cat > "$STUB/security" <<'STUBEOF'
#!/usr/bin/env bash
echo "security $*" >> "${SANDBOX}/calls.log"
[ -f "${SANDBOX}/security-fail" ] && exit 1
exit 0
STUBEOF

cat > "$STUB/colima" <<'STUBEOF'
#!/usr/bin/env bash
echo "colima $*" >> "${SANDBOX}/calls.log"
exit 0
STUBEOF

cat > "$STUB/docker" <<'STUBEOF'
#!/usr/bin/env bash
echo "docker $*" >> "${SANDBOX}/calls.log"
exit 0
STUBEOF

# 防火墙：绝不能碰真机 firewall 配置
cat > "$STUB/socketfilterfw" <<'STUBEOF'
#!/usr/bin/env bash
echo "socketfilterfw $*" >> "${SANDBOX}/calls.log"
exit 0
STUBEOF
chmod +x "$STUB"/*
pass "桩件就绪：$(ls "$STUB" | tr '\n' ' ')"

# ------------------------------------------------------------- 沙箱夹具 --
setup_fixture() {
  rm -rf "$ROOT" "$PLIST" "$SUDOERS" "$LINK" "$APPS" "$PREFIX" "$HOME_DIR" "$SANDBOX/compose"
  rm -f "$CALLS" "$SANDBOX/security-fail"
  mkdir -p "$ROOT/data" "$ROOT/bin" "$ROOT/work/backup" "$PLIST" "$SUDOERS" "$LINK" "$APPS"
  mkdir -p "$PREFIX/bin" "$PREFIX/Cellar" "$PREFIX/etc/nginx" "$PREFIX/etc/php/8.2" "$PREFIX/var/mysql"
  mkdir -p "$HOME_DIR/www/zizdog.cn" "$HOME_DIR/.colima" "$HOME_DIR/.lima/colima"
  mkdir -p "$HOME_DIR/filebrowser" "$HOME_DIR/redis" "$HOME_DIR/tts/qwen3" "$HOME_DIR/stt"
  mkdir -p "$SANDBOX/compose/uptime-kuma"

  # 铁律哨兵：~/www 一个字节都不许动
  printf '%s\n' '<?php // SENTINEL-NEVER-DELETE' > "$HOME_DIR/www/zizdog.cn/index.php"
  printf 'sentinel\n' > "$HOME_DIR/www/SENTINEL"

  printf 'data\n' > "$PREFIX/var/mysql/ibdata1"
  # 切换引擎时特意留下的旧数据副本（等同备份）：模式 3 默认必须保留
  mkdir -p "$PREFIX/var/mysql.mysql84-old"
  printf 'old-engine\n' > "$PREFIX/var/mysql.mysql84-old/ibdata1"
  printf '{"x":1}\n' > "$ROOT/data/config.json"
  printf 'cert\n' > "$ROOT/data/zizpanel-codesign.crt"
  printf 'bin\n' > "$ROOT/bin/zizpanel"
  printf 'bin\n' > "$ROOT/bin/zizpanel-helper"
  printf 'old-bin\n' > "$ROOT/bin/zizpanel.bak"
  printf 'old\n' > "$ROOT/work/backup/panel-2026.tar.gz"
  printf 'cfg\n' > "$PREFIX/etc/nginx/nginx.conf"
  printf 'cfg\n' > "$PREFIX/etc/my.cnf"
  printf 'cfg\n' > "$PREFIX/etc/php/8.2/php.ini"
  printf 'plist\n' > "$PLIST/cn.zizpanel.panel.plist"
  printf 'plist\n' > "$PLIST/cn.zizpanel.cron.daily-backup.plist"
  printf 'plist\n' > "$PLIST/cn.zizdog.nginx.plist"
  printf 'plist\n' > "$PLIST/homebrew.mxcl.nginx.plist"
  printf 'plist\n' > "$PLIST/homebrew.mxcl.php@8.2.plist"
  printf 'plist\n' > "$PLIST/sh.brew.mysql@8.4.plist"
  printf 'plist\n' > "$PLIST/com.zizdog.filebrowser.plist"
  printf 'plist\n' > "$PLIST/com.zizdog.qwen3tts.plist"
  printf 'plist\n' > "$PLIST/com.zizdog.stt.plist"
  printf '%s\n' 'zizpanel ALL=(root) NOPASSWD: /opt/zizpanel/bin/zizpanel-helper' > "$SUDOERS/zizpanel"
  printf 'link\n' > "$LINK/zizpanel"
  printf 'link\n' > "$LINK/zizpanel-helper"
  printf 'app\n' > "$APPS/ZizPanel.app"
  printf 'compose\n' > "$SANDBOX/compose/uptime-kuma/docker-compose.yml"
  cp -f "$STUB/brew" "$PREFIX/bin/brew"
  chmod +x "$PREFIX/bin/brew"

  # 真实的 SQLite 登记表（uninstall.sh 就是读它枚举"面板装过的软件"）
  sqlite3 "$ROOT/data/panel.db" <<SQL
CREATE TABLE services (name TEXT, display_name TEXT, kind TEXT, category TEXT, managed INTEGER, launch_label TEXT, plist_path TEXT, work_dir TEXT, compose_file TEXT, container TEXT);
INSERT INTO services VALUES ('nginx','Nginx','native','lnmp',1,'homebrew.mxcl.nginx','$PLIST/homebrew.mxcl.nginx.plist','','','');
INSERT INTO services VALUES ('php82','PHP 8.2','native','lnmp',1,'homebrew.mxcl.php@8.2','$PLIST/homebrew.mxcl.php@8.2.plist','','','');
INSERT INTO services VALUES ('filebrowser','File Browser','native','tool',0,'com.zizdog.filebrowser','$PLIST/com.zizdog.filebrowser.plist','$HOME_DIR/filebrowser','','');
INSERT INTO services VALUES ('user-redis','我的 Redis','native','tool',0,'homebrew.mxcl.redis','$PLIST/homebrew.mxcl.redis.plist','$HOME_DIR/redis','','');
INSERT INTO services VALUES ('docker-runtime','Docker 运行时','colima','runtime',0,'com.zizdog.colima','$PLIST/com.zizdog.colima.plist','','','');
INSERT INTO services VALUES ('qwen3tts','Qwen3 TTS','native','ai',0,'com.zizdog.qwen3tts','$PLIST/com.zizdog.qwen3tts.plist','$HOME_DIR/tts/qwen3','','');
INSERT INTO services VALUES ('stt','语音转文字','native','ai',0,'com.zizdog.stt','$PLIST/com.zizdog.stt.plist','$HOME_DIR/stt','','');
INSERT INTO services VALUES ('uptime-kuma','Uptime Kuma','compose','tool',1,'','','','$SANDBOX/compose/uptime-kuma/docker-compose.yml','');
SQL
}

run_u() {
  env ZIZPANEL_SANDBOX=1 \
      ZIZPANEL_ROOT="$ROOT" \
      ZIZPANEL_PLIST_DIR="$PLIST" \
      ZIZPANEL_SUDOERS_DIR="$SUDOERS" \
      ZIZPANEL_LINK_DIR="$LINK" \
      ZIZPANEL_APPS_DIR="$APPS" \
      ZIZPANEL_BREW_PREFIX="$PREFIX" \
      ZIZPANEL_REAL_HOME="$HOME_DIR" \
      HOME="$HOME_DIR" \
      PATH="$STUB:$PATH" \
      bash "$REPO/uninstall.sh" "$@"
}

out_has()   { grep -q -F -- "$2" "$1" 2>/dev/null; }
out_lacks() { ! grep -q -F -- "$2" "$1" 2>/dev/null; }
calls_has()   { grep -q -F -- "$1" "$CALLS" 2>/dev/null; }
calls_lacks() { ! grep -q -F -- "$1" "$CALLS" 2>/dev/null; }
no_destructive_calls() {
  ! grep -Eq '(^rm )|(^brew (uninstall|services stop))|(^launchctl bootout)|(^colima )|(^docker )' "$CALLS" 2>/dev/null
}
backup_dir() { ls -d "$HOME_DIR"/zizpanel-uninstall-backup-* 2>/dev/null | sort | tail -n 1; }

# ------------------------------------------------- 单一真源一致性（生成器） --
step "嵌入块与 uninstall.sh 是否一致（tools/sync-embedded-uninstaller.sh --check）"
if bash "$REPO/tools/sync-embedded-uninstaller.sh" --check >/dev/null 2>&1; then
  pass "install.sh 里的嵌入块与 uninstall.sh 逐字节一致"
else
  fail "install.sh 嵌入块与 uninstall.sh 不一致（跑 bash tools/sync-embedded-uninstaller.sh）"
fi

# ------------------------------------------------------------ dry-run 1 --
step "dry-run 模式 1（仅卸载面板）"
setup_fixture
if run_u --dry-run 1 > "$SANDBOX/out-dry1.log" 2>&1; then pass "退出码 0"; else fail "dry-run 1 失败"; fi
no_destructive_calls && pass "dry-run 期间没有执行任何破坏性命令" || fail "dry-run 执行了破坏性命令"
[ -f "$PLIST/cn.zizpanel.panel.plist" ] && pass "目标文件仍在（一个字节都没改）" || fail "目标文件被删除"
out_has "$SANDBOX/out-dry1.log" "正在删除" && pass "逐项实时进度已打印" || fail "缺少实时进度"
out_lacks "$SANDBOX/out-dry1.log" "brew uninstall" && pass "模式 1 不含任何 brew 卸载" || fail "模式 1 出现了 brew 卸载"
out_lacks "$SANDBOX/out-dry1.log" "/etc/nginx" && pass "模式 1 不含 LNMP 配置删除" || fail "模式 1 出现了 LNMP 删除"
[ -f "$HOME_DIR/www/SENTINEL" ] && pass "用户站点目录 www 未被触碰" || fail "用户站点目录 www 被删"

# ------------------------------------------------------------ dry-run 2 --
step "dry-run 模式 2（面板 + LNMP）"
setup_fixture
if run_u --dry-run 2 > "$SANDBOX/out-dry2.log" 2>&1; then pass "退出码 0"; else fail "dry-run 2 失败"; fi
no_destructive_calls && pass "dry-run 期间没有执行任何破坏性命令" || fail "dry-run 执行了破坏性命令"
out_has "$SANDBOX/out-dry2.log" "brew uninstall --ignore-dependencies nginx" && pass "出现 nginx 卸载" || fail "缺少 nginx 卸载"
out_has "$SANDBOX/out-dry2.log" "brew uninstall --ignore-dependencies php@8.2" && pass "出现 PHP 卸载" || fail "缺少 PHP 卸载"
out_has "$SANDBOX/out-dry2.log" "brew uninstall --ignore-dependencies mariadb" && pass "出现 MariaDB 卸载" || fail "缺少 MariaDB 卸载"
out_lacks "$SANDBOX/out-dry2.log" "File Browser" && pass "模式 2 不卸载面板应用" || fail "模式 2 误删面板应用"
out_lacks "$SANDBOX/out-dry2.log" "brew uninstall --ignore-dependencies redis" && pass "模式 2 不碰用户自己的 Redis" || fail "模式 2 误删用户软件"
if ! grep -qx -F "  - $ROOT" "$SANDBOX/out-dry2.log"; then pass "模式 2 不整体删面板数据目录"; else fail "模式 2 计划删面板数据目录"; fi
out_has "$SANDBOX/out-dry2.log" "数据库数据目录" && pass "模式 2 明确保留数据库数据目录" || fail "模式 2 未声明保留数据库数据"
[ -f "$HOME_DIR/www/SENTINEL" ] && pass "用户站点目录 www 未被触碰" || fail "用户站点目录 www 被删"

# 另一种 CLI 写法：--mode 2 --dry-run
if run_u --mode 2 --dry-run > "$SANDBOX/out-dry2b.log" 2>&1 && out_has "$SANDBOX/out-dry2b.log" "模式：2"; then
  pass "--mode 2 --dry-run 等价可用"
else
  fail "--mode 2 --dry-run 不可用"
fi

# ------------------------------------------------------------ dry-run 3 --
step "dry-run 模式 3（面板 + 所有环境）"
setup_fixture
if run_u --dry-run 3 > "$SANDBOX/out-dry3.log" 2>&1; then pass "退出码 0"; else fail "dry-run 3 失败"; fi
no_destructive_calls && pass "dry-run 期间没有执行任何破坏性命令" || fail "dry-run 执行了破坏性命令"
out_has "$SANDBOX/out-dry3.log" "卸载面板及所有环境" && pass "模式 3 语义正确" || fail "模式 3 语义缺失"
out_has "$SANDBOX/out-dry3.log" "File Browser" && pass "模式 3 从登记表列出面板应用" || fail "模式 3 未列出面板应用"
out_has "$SANDBOX/out-dry3.log" ".colima" && pass "模式 3 列出 Colima 数据" || fail "模式 3 未列出 Colima 数据"
out_has "$SANDBOX/out-dry3.log" "$PREFIX/var/mysql" && pass "模式 3 列出数据库数据目录" || fail "模式 3 未列出数据库数据目录"
out_has "$SANDBOX/out-dry3.log" "未确认由面板安装" && pass "模式 3 明确保留用户自己纳管的服务" || fail "模式 3 未标注保留项"
out_has "$SANDBOX/out-dry3.log" "备份/副本（默认保留；--purge-backups 才删）" && pass "计划分「活动数据 vs 备份/副本」两栏" || fail "计划缺备份/副本分栏"
out_has "$SANDBOX/out-dry3.log" "旧引擎数据副本" && pass "旧引擎数据副本被归为备份/副本" || fail "旧引擎数据副本未归类"
out_has "$SANDBOX/out-dry3.log" "要求手工输入 DELETE-ALL 确认" && pass "dry-run 打印真跑的 DELETE-ALL 要求" || fail "dry-run 未提示 DELETE-ALL"
grep -qx -F "  - $ROOT" "$SANDBOX/out-dry3.log" && pass "模式 3 计划整体删面板数据目录" || fail "模式 3 未计划删面板数据"
[ -f "$HOME_DIR/www/SENTINEL" ] && pass "用户站点目录 www 未被触碰" || fail "用户站点目录 www 被删"

# ------------------------------------------------- 非交互模式 3 必须中止 --
step "非交互模式 3 缺 --yes：必须中止"
setup_fixture
if run_u --mode 3 > "$SANDBOX/out-no3.log" 2>&1; then
  fail "缺 --yes 时模式 3 竟然继续了"
else
  pass "缺 --yes 时模式 3 中止"
fi
[ -d "$ROOT" ] && pass "中止后什么都没删" || fail "中止后仍删了东西"
no_destructive_calls && pass "中止前没有执行破坏性命令" || fail "中止前已执行破坏性命令"
out_has "$SANDBOX/out-no3.log" "非交互模式必须显式授权" && pass "中止原因已说明" || fail "未说明中止原因"

# ------------------------------------------------- 非交互模式 1（正常跑） --
step "沙箱真跑模式 1（--yes）"
setup_fixture
if run_u --mode 1 --yes > "$SANDBOX/out-mode1.log" 2>&1; then pass "退出码 0"; else fail "模式 1 真跑失败"; fi
[ ! -e "$PLIST/cn.zizpanel.panel.plist" ] && pass "plist 已删" || fail "plist 未删"
[ ! -e "$LINK/zizpanel" ] && pass "CLI 入口已删" || fail "CLI 入口未删"
[ ! -e "$SUDOERS/zizpanel" ] && pass "sudoers 已删" || fail "sudoers 未删"
[ ! -e "$ROOT/bin/zizpanel" ] && pass "面板二进制已删" || fail "面板二进制未删"
[ -f "$ROOT/data/panel.db" ] && pass "面板数据保留" || fail "面板数据被删"
[ -f "$HOME_DIR/www/SENTINEL" ] && pass "用户站点目录 www 未被触碰（铁律）" || fail "用户站点目录 www 被删"
calls_lacks "brew uninstall" && pass "没有卸载任何 brew 软件" || fail "模式 1 卸载了 brew 软件"
calls_lacks "brew services stop" && pass "没有停任何 brew 服务" || fail "模式 1 停了 brew 服务"
calls_has "launchctl bootout system/cn.zizpanel.panel" && pass "面板服务已停止" || fail "面板服务未停止"
calls_has "security remove-trusted-cert" && pass "证书信任已撤销" || fail "证书信任未撤销"

# ------------------------------------------- 失败必须计入结尾汇总（注入） --
step "注入必然失败的动作（security 失败）"
setup_fixture
touch "$SANDBOX/security-fail"
if run_u --mode 1 --yes > "$SANDBOX/out-fail.log" 2>&1; then
  fail "失败被吞掉了（退出码 0）"
else
  pass "失败导致退出码非 0"
fi
out_has "$SANDBOX/out-fail.log" "撤销证书信任失败" && pass "失败照实打印" || fail "失败未打印"
out_has "$SANDBOX/out-fail.log" "失败" && pass "结尾汇总含失败清单" || fail "结尾汇总缺失败清单"
rm -f "$SANDBOX/security-fail"

# ------------------------------------------------- 沙箱真跑模式 2 --
step "沙箱真跑模式 2（--yes）"
setup_fixture
if run_u --mode 2 --yes > "$SANDBOX/out-mode2.log" 2>&1; then pass "退出码 0"; else fail "模式 2 真跑失败"; fi
calls_has "brew uninstall --ignore-dependencies nginx" && pass "nginx 已卸载" || fail "nginx 未卸载"
calls_has "brew uninstall --ignore-dependencies php@8.2" && pass "PHP 已卸载" || fail "PHP 未卸载"
calls_has "brew uninstall --ignore-dependencies mariadb" && pass "MariaDB 已卸载" || fail "MariaDB 未卸载"
calls_lacks "brew uninstall --ignore-dependencies redis" && pass "用户自己的 Redis 未被动" || fail "用户软件被误删"
calls_lacks "brew uninstall --ignore-dependencies ffmpeg" && pass "基础依赖 ffmpeg 未被动" || fail "ffmpeg 被误删"
[ ! -e "$PREFIX/etc/nginx" ] && pass "LNMP 配置已删" || fail "LNMP 配置未删"
[ -f "$PREFIX/var/mysql/ibdata1" ] && pass "数据库数据目录保留" || fail "数据库数据被删"
[ -f "$ROOT/data/panel.db" ] && pass "面板数据保留" || fail "面板数据被删"
[ -f "$HOME_DIR/www/SENTINEL" ] && pass "用户站点目录 www 未被触碰（铁律）" || fail "用户站点目录 www 被删"
bd="$(backup_dir)"
if [ -n "$bd" ] && [ -f "$bd/to-delete.txt" ] && grep -q "将删除的路径" "$bd/to-delete.txt"; then
  pass "备份里含待删清单（to-delete.txt）"
else
  fail "备份缺少待删清单"
fi
out_has "$SANDBOX/out-mode2.log" "要连这些一起清掉，请用模式 3" && pass "结尾告知数据位置与模式 3" || fail "结尾未告知数据位置"

# ------------------------------------------------- 沙箱真跑模式 3 --
step "沙箱真跑模式 3（--yes）"
setup_fixture
if run_u --mode 3 --yes > "$SANDBOX/out-mode3.log" 2>&1; then pass "退出码 0"; else fail "模式 3 真跑失败"; tail -20 "$SANDBOX/out-mode3.log" | sed 's/^/      /'; fi
[ ! -d "$ROOT" ] && pass "面板数据目录已整体删除" || fail "面板数据目录仍在"
[ ! -e "$PREFIX/var/mysql" ] && pass "活动数据库数据目录已删除" || fail "活动数据库数据目录仍在"
[ -f "$PREFIX/var/mysql.mysql84-old/ibdata1" ] && pass "旧引擎数据副本默认保留" || fail "旧引擎数据副本被删"
[ ! -e "$HOME_DIR/filebrowser" ] && pass "面板应用数据已删除（File Browser）" || fail "File Browser 数据仍在"
[ ! -e "$HOME_DIR/tts/qwen3" ] && pass "面板应用数据已删除（Qwen3 TTS）" || fail "Qwen3 数据仍在"
[ ! -e "$HOME_DIR/.colima" ] && pass "Colima 数据已删除" || fail "Colima 数据仍在"
[ ! -e "$SANDBOX/compose/uptime-kuma" ] && pass "compose 项目目录已删除" || fail "compose 项目目录仍在"
calls_has "brew uninstall --ignore-dependencies whisper.cpp" && pass "面板应用引擎已卸载（whisper.cpp）" || fail "面板应用引擎未卸载"
[ ! -e "$HOME_DIR/stt" ] && pass "面板应用引擎数据已删除（stt）" || fail "stt 数据仍在"
[ -d "$HOME_DIR/redis" ] && pass "用户自己纳管的 Redis 数据保留（不误杀）" || fail "用户 Redis 数据被误删"
[ ! -e "$PLIST/com.zizdog.filebrowser.plist" ] && pass "面板应用的 launchd 定义已删" || fail "面板应用 launchd 定义仍在"
out_has "$SANDBOX/out-mode3.log" "File Browser（登记表 filebrowser）" && pass "「已卸载」清单点名面板应用" || fail "「已卸载」清单缺面板应用"
[ -f "$HOME_DIR/www/SENTINEL" ] && pass "用户站点目录 www 未被触碰（铁律）" || fail "用户站点目录 www 被删"
calls_has "docker compose -f $SANDBOX/compose/uptime-kuma/docker-compose.yml down" && pass "compose 项目已停止" || fail "compose 未停止"
calls_has "colima delete -f" && pass "Colima 虚拟机已删除" || fail "Colima 未删除"
bd="$(backup_dir)"
if [ -n "$bd" ] && [ -d "$bd/panel-preserved/work/backup" ]; then
  pass "面板旧备份已转移到备份目录（默认不删）"
else
  fail "面板旧备份未被保留"
fi
if [ -n "$bd" ] && [ -f "$bd/panel-preserved/bin/zizpanel.bak" ]; then
  pass "面板根里的 *.bak 副本已保留（不随根目录删）"
else
  fail "面板根里的 *.bak 副本未保留"
fi
out_has "$SANDBOX/out-mode3.log" "备份/副本保留：" && pass "结尾汇总列出保留的备份/副本" || fail "结尾汇总缺保留的备份/副本"
out_has "$SANDBOX/out-mode3.log" "已卸载" && out_has "$SANDBOX/out-mode3.log" "已删除" \
  && out_has "$SANDBOX/out-mode3.log" "已保留" && out_has "$SANDBOX/out-mode3.log" "失败" \
  && pass "结尾有 已卸载/已删除/已保留/失败 四段汇总" || fail "结尾汇总不完整"
out_has "$SANDBOX/out-mode3.log" "我的 Redis" && pass "保留清单点名用户自己的服务" || fail "保留清单未点名用户服务"

# ------------------------------------------- --purge-backups：备份/副本才删 --
step "--purge-backups 才删备份/副本"
setup_fixture
if run_u --mode 3 --yes --purge-backups > "$SANDBOX/out-purgebackups.log" 2>&1; then
  pass "退出码 0"
else
  fail "--purge-backups 模式 3 失败"
fi
[ ! -e "$PREFIX/var/mysql.mysql84-old" ] && pass "--purge-backups 删掉旧引擎数据副本" || fail "旧引擎数据副本未被删"
[ ! -e "$ROOT/bin/zizpanel.bak" ] && pass "--purge-backups 连面板根里的 *.bak 一起删" || fail "*.bak 未被删"
bd="$(backup_dir)"
if [ -z "$bd" ] || [ ! -d "$bd/panel-preserved" ]; then
  pass "--purge-backups 不再搬出备份（panel-preserved 不存在）"
else
  fail "--purge-backups 仍搬出了备份"
fi

# ------------------------------------------------------ --purge 兼容 --
step "--purge 向后兼容（== --mode 3 --yes）"
setup_fixture
if run_u --purge --dry-run > "$SANDBOX/out-purge.log" 2>&1 && out_has "$SANDBOX/out-purge.log" "模式：3"; then
  pass "--purge 映射到模式 3"
else
  fail "--purge 未映射到模式 3"
fi

# ---------------------------------------------------------------- 汇总 --
printf '\n%s────────────────────────────%s\n' "$C_BOLD" "$C_RESET"
if [ "$FAILURES" -eq 0 ]; then
  printf '%s✅ 三档卸载沙箱测试全部通过%s\n' "$C_GREEN" "$C_RESET"
  exit 0
fi
printf '%s❌ 三档卸载沙箱测试失败 %d 项%s\n' "$C_RED" "$FAILURES" "$C_RESET"
exit 1
