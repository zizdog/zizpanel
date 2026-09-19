#!/usr/bin/env bash
# ============================================================================
#  check-future-dates.sh —— 门禁：注释/文档里不许出现比"今天"更晚的日期
#
#  为什么需要它：往届会话凭空写过"用户 2026-09-2X 要求…""2026-09-2X 那一类报障"
#  这类日期，而写它的那天是 09-18 —— 全是编的（用户拿系统日期一对比就发现了）。
#  日期是可核对的事实：只能来自 git blame 或当天，不许猜。教训见 docs/坑清单.md。
#
#  判定：扫描**会被提交的文本文件**（`git ls-files -co --exclude-standard`，
#  即已跟踪 + 未忽略；gitignore 的私有文档不在内），逐个找出 `20XX-XX-XX`，
#  与今天做字典序比较（ISO 零填充，字典序即时间序），比今天晚就失败并打印 文件:行。
#
#  豁免（都是"本来就该晚于今天"的测试数据，不是注释日期）—— 改这三个数组即可：
#    · 行内含 zp-date-ok                 → 单行豁免（临时/特殊情形用）
#    · ALLOW_DATES                       → 与路径无关的显式占位/哨兵日期
#    · ALLOW_PATH_DATES "<路径>:<日期>"  → 夹具里的证书到期时间等（被测试断言）
#    · PENDING_PATHS                     → 🚧 并行同事正在改的文件，暂不判失败但会警告
#                                          （改完请删掉对应条目；其中剩余数量会打印出来）
#
#  用法：bash tools/check-future-dates.sh [今天=YYYY-MM-DD]
#  退出码 0 通过，1 有未来日期，2 用法/环境不对。
# ============================================================================
set -uo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.." || exit 1

TODAY="${1:-$(date +%F)}"
if ! printf '%s' "$TODAY" | grep -qE '^20[0-9]{2}-[0-9]{2}-[0-9]{2}$'; then
  echo "用法: bash tools/check-future-dates.sh [今天=YYYY-MM-DD]" >&2
  exit 2
fi

# 显式占位/哨兵：与路径无关，任何时候都允许。
#   1970-01-01 = Unix 纪元占位；2035-01-01 = macOS 通知日期"推远"哨兵（sysconfig.go）；
#   2099-01-01 = 会话"永不过期"哨兵（store_test.go）；2026-12-31 = 文档占位示例。
ALLOW_DATES=(
  "1970-01-01"
  "2035-01-01"
  "2099-01-01"
  "2026-12-31"
)

# 夹具里的"真实未来"日期：证书到期时间必须晚于今天，且被对应脚本/测试断言。
ALLOW_PATH_DATES=(
  "tools/certs-verify.mjs:2026-09-20"          # created_at/updated_at 夹具
  "tools/certs-verify.mjs:2026-10-01"          # not_after（脚本第 820 行断言"到期：2026-10-01"）
  "tools/certs-verify.mjs:2026-11-18"          # not_after 夹具
  "tools/certs-verify.mjs:2026-12-01"          # ssl_expires（脚本第 807 行断言 /2026-12-01/）
  "internal/proxies/proxies_ssl_test.go:2026-12-01"  # SSLExpires 往返断言（第 165/171 行）
)

# 🚧 并行改动中的文件：**临时**豁免。改完请把这里删空 —— 留着等于这些文件不受门禁保护。
PENDING_PATHS=(
  "internal/sites/"
  "internal/proxies/"
  "internal/web/api_sites.go"
  "internal/web/assets/js/sites.js"
  "internal/web/api_proxies.go"
  "internal/web/api_appproxy.go"
  "internal/web/assets/js/reverseproxy.js"
  "internal/web/assets/js/files.js"
  "internal/web/api_files.go"
)

if ! git rev-parse --git-dir >/dev/null 2>&1; then
  echo "（不是 git 仓库，跳过未来日期检查）"
  exit 0
fi

is_pending() {
  local f="$1" p
  for p in "${PENDING_PATHS[@]}"; do
    case "$f" in "$p"*) return 0 ;; esac
  done
  return 1
}

is_allowed() {
  local f="$1" d="$2" a
  for a in "${ALLOW_DATES[@]}"; do [ "$d" = "$a" ] && return 0; done
  for a in "${ALLOW_PATH_DATES[@]}"; do [ "$f:$d" = "$a" ] && return 0; done
  return 1
}

hits=0
pending=0
while IFS= read -r -d '' f; do
  # 本脚本自己的注释里有 20XX 示例，不能自己判自己
  [ "$f" = "tools/check-future-dates.sh" ] && continue
  pending_file=0
  is_pending "$f" && pending_file=1

  while IFS= read -r line; do
    [ -z "$line" ] && continue
    ln="${line%%:*}"
    body="${line#*:}"
    case "$body" in *zp-date-ok*) continue ;; esac
    for d in $(printf '%s\n' "$body" | grep -oE '20[0-9]{2}-[0-9]{2}-[0-9]{2}'); do
      [[ "$d" > "$TODAY" ]] || continue
      is_allowed "$f" "$d" && continue
      if [ "$pending_file" = 1 ]; then
        pending=$((pending + 1))
        continue
      fi
      printf '\033[31m✗ %s:%s\033[0m  %s 晚于今天 %s\n' "$f" "$ln" "$d" "$TODAY"
      hits=$((hits + 1))
    done
  done < <(grep -nIE '20[0-9]{2}-[0-9]{2}-[0-9]{2}' -- "$f" 2>/dev/null)
done < <(git ls-files -z -co --exclude-standard)

if [ "$hits" -gt 0 ]; then
  echo ""
  echo -e "\033[31m共 $hits 处未来日期：日期是事实，只能来自 git blame 或当天，不许编。\033[0m"
  echo "要么删掉日期只留结论，要么改成该行 git blame 到的真实提交日期。"
  exit 1
fi

if [ "$pending" -gt 0 ]; then
  echo "⚠ 并行改动中豁免：$pending 处未来日期暂未处理（清单见本脚本 PENDING_PATHS，改完请删空）"
fi
echo -e "\033[32m✓ 未来日期检查通过（今天 ${TODAY}）\033[0m"
exit 0
