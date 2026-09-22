#!/usr/bin/env bash
# ============================================================================
#  check-no-real-credentials.sh —— 门禁：真实面板口令不许出现在被跟踪文件里
#
#  为什么需要它（这条坑复发过两次）：
#    v0.3.1 基线就把真实面板口令写进了测试与验证脚本等 9 个文件，仓库是 public；
#    af2b887 清理过一次，但只清了部分文件、还给某个文件留了"已改掉"的假注释（值没改）。
#    第九轮又发现工作区新增 3 个文件带着同一个口令。人的记性靠不住，所以做成 `make check` 的一步。
#
#  判定方式：拿**本机真实凭据文件**（.panel-credential.local，已 gitignore）里的
#  每个值，去搜 `git ls-files` 里的每个文本文件。命中即失败并列出文件。
#  凭据文件不存在（CI / 别人的机器）时**明确打印"跳过"**，不假装通过。
#
#  注意：本脚本只读凭据、从不打印凭据内容（失败信息里也只给掩码）。
# ============================================================================
set -uo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.." || exit 1

CRED_FILE="${CRED_FILE:-.panel-credential.local}"

if [ ! -f "$CRED_FILE" ]; then
  echo "（没有 $CRED_FILE，跳过真实口令泄漏检查）"
  exit 0
fi

# 收集凭据字面值。只认**像秘密**的键（*PASS*/*SECRET*/*TOKEN*/*KEY*）：
# 凭据文件里还有用户名与 URL（如 zizdog、https://panel.zizdog.com:8888）—— 那些不是秘密，
# 拿它们去搜会把每一份提到域名的文件都判成泄漏（2026-09-22 实测：281 个假红）。
# 值去掉两端的引号（文件里写 ZP_MINI_PASS='…' 时，要搜的是引号里的那个串）。
VALUES=()
while IFS='=' read -r _k v; do
  case "$_k" in
    *PASS*|*SECRET*|*TOKEN*|*KEY*) ;;
    *) continue ;;
  esac
  v="${v%$'\r'}"
  v="${v#\'}"; v="${v%\'}"; v="${v#\"}"; v="${v%\"}"
  [ "${#v}" -ge 6 ] && VALUES+=("$v")
done < <(grep -E '^[A-Za-z_][A-Za-z0-9_]*=' "$CRED_FILE" 2>/dev/null || true)

if [ "${#VALUES[@]}" -eq 0 ]; then
  echo "（$CRED_FILE 里没有可用作比对的凭据值，跳过）"
  exit 0
fi

HITS=0
while IFS= read -r f; do
  [ -f "$f" ] || continue
  # 凭据文件自己不算
  [ "$f" = "$CRED_FILE" ] && continue
  # 只查文本文件（二进制里出现同样的字节没有意义）
  if ! file -b --mime "$f" 2>/dev/null | grep -qE '^text/|json|xml|javascript'; then continue; fi
  for v in "${VALUES[@]}"; do
    if grep -qF -- "$v" "$f" 2>/dev/null; then
      masked="${v:0:2}***${v: -2}"
      echo "  !! $f 含真实凭据 <$masked>（长度 ${#v}）"
      HITS=$((HITS+1))
    fi
  done
done < <(git ls-files --cached --others --exclude-standard)

if [ "$HITS" -gt 0 ]; then
  echo ""
  echo "真实口令出现在 $HITS 个**将要提交**的文件里 —— 一旦提交就等于公开。"
  echo "改法：换成明显的假值（例如 zizpanel-test-fixture-pass），"
  echo "      需要真实口令的脚本改成从环境变量读（不要写进被跟踪的文件）。"
  echo "      如果口令已经提交/推送过，还必须**轮换**它（改历史不够，旧口令已经在别人手里）。"
  exit 1
fi

echo "真实凭据泄漏检查通过（比对 ${#VALUES[@]} 个值，扫描 $(git ls-files --cached --others --exclude-standard | wc -l | tr -d ' ') 个将提交文件）"
