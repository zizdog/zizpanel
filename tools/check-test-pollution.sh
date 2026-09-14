#!/usr/bin/env bash
# =============================================================================
#  tools/check-test-pollution.sh —— 单元测试不许碰用户真实家目录
#
#  为什么需要这个：
#    2026-09-14 的事故。internal/web/api_market_test.go 为了复刻"本机实况"，
#    往 srv.Cfg.UserHome 里写 plist；而 newTestServer 当时只把 DataDir / LogDir
#    等隔离到 t.TempDir()，**漏了 UserHome** —— config.Default() 解析出来的是
#    用户真实家目录。于是 `make check` 把
#        ~/Library/LaunchAgents/sh.brew.php@8.3.plist
#        ~/Library/LaunchAgents/sh.brew.mysql@8.4.plist
#    覆盖成了 8 字节的 `<plist/>`。PHP-FPM 与 MySQL 当时已经在跑，所以**毫无症状**；
#    但只要机器重启、或面板上点一次"重启服务"，这两个服务就再也起不来了。
#    同一轮测试还在 ~/iopaint/.venv/bin/ 下造了个假的可执行文件。
#
#    这类错误靠人眼 review 是防不住的（测试代码看起来完全合理），
#    所以用"跑测试前后给真实目录拍指纹"的方式做成门禁。
#
#  用法：bash tools/check-test-pollution.sh            # 拍指纹 → go test → 比对
#        bash tools/check-test-pollution.sh --verify /tmp/fp
#        bash tools/check-test-pollution.sh --snapshot /tmp/fp
#  退出码 0 表示测试没有污染用户环境。
# =============================================================================
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HOME_DIR="${ZP_TEST_HOME_GUARD_HOME:-$HOME}"

FAILURES=0
C_GREEN=$'\033[32m'; C_RED=$'\033[31m'; C_BOLD=$'\033[1m'; C_RESET=$'\033[0m'
pass() { printf '  %s✓%s %s\n' "$C_GREEN" "$C_RESET" "$1"; }
fail() { printf '  %s✗%s %s\n' "$C_RED" "$C_RESET" "$1"; FAILURES=$((FAILURES + 1)); }
step() { printf '\n%s▸ %s%s\n' "$C_BOLD" "$1" "$C_RESET"; }

# 被监视的位置。只收"测试绝不该写"的：
#   LaunchAgents —— 真实 launchd 服务定义（今天被覆盖的就是这里，要内容指纹）
#   www          —— 用户网站目录（只比名字，用户在里面改文件不该误报）
#   安装产物根    —— 面板市场"孤儿态"探测的那几个路径（假产物就造在这里）
snapshot() {
  local out="$1"
  : > "$out"

  local agents="$HOME_DIR/Library/LaunchAgents"
  if [ -d "$agents" ]; then
    # 逐文件算哈希：内容被改成 <plist/> 这种事只有内容指纹能发现
    while IFS= read -r f; do
      printf 'agents\t%s\t%s\n' "$(basename "$f")" "$(shasum -a 256 "$f" | awk '{print $1}')" >> "$out"
    done < <(find "$agents" -maxdepth 1 -type f -name '*.plist' | sort)
  fi

  local www="$HOME_DIR/www"
  if [ -d "$www" ]; then
    while IFS= read -r d; do
      printf 'www\t%s\t-\n' "$(basename "$d")" >> "$out"
    done < <(find "$www" -maxdepth 1 -mindepth 1 -type d | sort)
  fi

  local p
  for p in "iopaint/.venv/bin/iopaint" "tts/qwen3/.venv/bin/python" "tts/voice-receiver/receiver.py"; do
    if [ -e "$HOME_DIR/$p" ]; then
      printf 'artifact\t%s\t%s\n' "$p" "$(shasum -a 256 "$HOME_DIR/$p" | awk '{print $1}')" >> "$out"
    fi
  done

  sort -o "$out" "$out"
}

verify() {
  local before="$1" after="$2"
  local diffout
  diffout="$(diff "$before" "$after" || true)"
  if [ -z "$diffout" ]; then
    pass "用户家目录（LaunchAgents / www / 安装产物）跑测试前后完全一致"
    return 0
  fi

  fail "测试改了用户的真实文件！"
  printf '%s\n' "$diffout" | sed 's/^/      /'
  printf '      %s注意：diff 里 < 是测试前、> 是测试后。%s\n' "$C_BOLD" "$C_RESET"
  printf '      这正是 2026-09-14 那次事故的形态：测试往真实家目录写文件，\n'
  printf '      把用户的 launchd plist 覆盖成空的 plist（服务重启后就起不来了）。\n'
  printf '      修法：让被测代码拿到临时家目录（见 internal/web/server_test.go 的\n'
  printf '      newTestServer 与 internal/services/services_test.go 的注释）。\n'
  printf '      如果是你自己（人）在这期间动了这些目录，重跑一次即可。\n'
  return 1
}

MODE="run"; TARGET=""
case "${1:-}" in
  --snapshot) MODE="snapshot"; TARGET="${2:?--snapshot 需要一个路径}" ;;
  --verify)   MODE="verify";   TARGET="${2:?--verify 需要一个路径}" ;;
  "")         ;;
  *) echo "用法: $0 [--snapshot <文件> | --verify <文件>]" >&2; exit 2 ;;
esac

FP="${TARGET:-/tmp/zizpanel-test-pollution.$$.fp}"

if [ "$MODE" = "snapshot" ]; then
  mkdir -p "$(dirname "$FP")"
  snapshot "$FP"
  exit 0
fi

step "测试污染门禁：真实家目录指纹"
printf '  监视：%s/Library/LaunchAgents、%s/www、安装产物根\n' "$HOME_DIR" "$HOME_DIR"
printf '  指纹：%s\n' "$FP"

if [ "$MODE" = "verify" ]; then
  # --verify <旧指纹>：现场重新拍一次，与旧指纹比对（供人工排查/回归自测用）
  NOW="$(mktemp -t zizpanel-fp-now)"
  snapshot "$NOW"
  verify "$FP" "$NOW"
  rm -f "$NOW"
  [ "$FAILURES" -eq 0 ] || exit 1
  exit 0
fi

# 默认模式：拍 → 跑测试 → 比对。go test 的退出码必须原样保留（它才是主结果）。
snapshot "$FP.before"
rm -f "$FP"
printf '\n%s▸ go test ./... -count=1%s\n' "$C_BOLD" "$C_RESET"
( cd "$REPO" && go test ./... -count=1 )
TEST_RC=$?
snapshot "$FP"
verify "$FP.before" "$FP"
rm -f "$FP.before" "$FP"

if [ "$TEST_RC" -ne 0 ]; then
  printf '\n%s单元测试失败（退出码 %s）%s\n' "$C_RED" "$TEST_RC" "$C_RESET"
  exit "$TEST_RC"
fi
[ "$FAILURES" -eq 0 ] || exit 1
exit 0
