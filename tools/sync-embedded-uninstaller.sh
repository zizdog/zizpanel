#!/usr/bin/env bash
# =============================================================================
#  tools/sync-embedded-uninstaller.sh —— 单一真源：仓库根 uninstall.sh
#
#  为什么需要它：install.sh 必须自包含（`curl | sudo bash` 时没有仓库文件），所以
#  三档卸载脚本要**原样嵌入** install.sh；但"嵌入一份"最容易漂移（历史上生成版
#  卸载器就与仓库版语义不一致）。本工具从 uninstall.sh 重新生成嵌入块；`--check`
#  只校验不写，供 Makefile / 测试当门禁（改了一边忘了另一边就红）。
#
#  用法：
#    bash tools/sync-embedded-uninstaller.sh          # 重新生成嵌入块
#    bash tools/sync-embedded-uninstaller.sh --check  # 只校验，漂移则退出 1
# =============================================================================
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SRC="$REPO/uninstall.sh"
DST="$REPO/install.sh"
BEGIN_MARK="# >>> ZP_EMBEDDED_UNINSTALLER_BEGIN"
END_MARK="# <<< ZP_EMBEDDED_UNINSTALLER_END"
OPEN_LINE="  cat > \"\$ZIZPANEL_ROOT/uninstall.sh\" <<'ZP_UNINSTALL_EOF'"
CLOSE_LINE="ZP_UNINSTALL_EOF"

CHECK=0
[ "${1:-}" = "--check" ] && CHECK=1

err() { printf '%s\n' "$*" >&2; }

if [ ! -f "$SRC" ]; then err "缺少 $SRC"; exit 2; fi
if [ ! -f "$DST" ]; then err "缺少 $DST"; exit 2; fi

b_line="$(grep -n -F -x "$BEGIN_MARK" "$DST" | head -n1 | cut -d: -f1)"
e_line="$(grep -n -F -x "$END_MARK" "$DST" | head -n1 | cut -d: -f1)"
if [ -z "$b_line" ] || [ -z "$e_line" ]; then
  err "install.sh 里找不到嵌入块标记（$BEGIN_MARK / ${END_MARK}）"
  exit 2
fi
if [ "$b_line" -ge "$e_line" ]; then err "嵌入块标记顺序不对"; exit 2; fi

if [ "$CHECK" = "1" ]; then
  tmp="$(mktemp)"
  sed -n "$((b_line + 2)),$((e_line - 2))p" "$DST" > "$tmp"
  if cmp -s "$tmp" "$SRC"; then
    rm -f "$tmp"
    printf '✓ 嵌入的卸载脚本与 uninstall.sh 逐字节一致\n'
    exit 0
  fi
  err "✗ install.sh 里嵌入的卸载脚本与 uninstall.sh 不一致（改了一边忘了另一边）"
  err "  修复：bash tools/sync-embedded-uninstaller.sh"
  diff -u "$SRC" "$tmp" 2>/dev/null | head -n 40 >&2
  rm -f "$tmp"
  exit 1
fi

tmpout="$(mktemp)"
{
  head -n "$b_line" "$DST"
  printf '%s\n' "$OPEN_LINE"
  cat "$SRC"
  printf '%s\n' "$CLOSE_LINE"
  printf '%s\n' "$END_MARK"
  tail -n +"$((e_line + 1))" "$DST"
} > "$tmpout"
mv "$tmpout" "$DST"
printf '✓ 已把 uninstall.sh 嵌入 install.sh（%s 行）\n' "$(wc -l < "$SRC" | tr -d ' ')"
