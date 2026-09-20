#!/usr/bin/env bash
# ============================================================================
#  check-release-notes.sh —— 发布说明门禁（防"说明永远停在 v1.3.4"复发）
#
#  背景：manifest.json 的 notes 从 1.3.4 起一直复用静态 RELEASE_NOTES.md ——
#  版本涨到 1.6.5，用户看到的"本次更新内容"还是 1.3.4 那片旧文。现在说明由
#  tools/gen-release-notes.py 按版本从 git 历史生成，本门禁钉死四件事：
#    ① 以 `v<当前版本> · ` 开头（版本对得上）；
#    ② 总长 ≤ 600 字符；
#    ③ 不含更早版本的标题（不得再出现 `v1.3.4 ·` 这类）；
#    ④ 已产出的 manifest.notes 与生成物逐字一致；另断言 Makefile 确实接生成器。
#
#  用法：bash tools/check-release-notes.sh [说明文件] [manifest ...]
#    无参数 = 校验 dist/release/NOTES.md（不存在则校验即时生成的结果）+ 已有清单。
#    负向对照：把旧文件塞进第 1 个参数，应当非零退出。
#  退出码 0 通过，1 不通过。
# ============================================================================
set -uo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.." || exit 1

RELDIR="${RELDIR:-dist/release}"
LIMIT=600

NOTES_ARG=""
if [ "$#" -gt 0 ]; then NOTES_ARG="$1"; shift; fi
MANIFESTS=("$@")

if [ ! -f internal/version/version.go ]; then
  echo "!! 不在仓库根目录" >&2
  exit 1
fi
V="$(sed -n 's/^var Version *= *"\([0-9][0-9.]*\)".*/\1/p' internal/version/version.go | head -1)"
[ -n "$V" ] || { echo "!! 读不到版本号" >&2; exit 1; }

TMP="$(mktemp)"; trap 'rm -f "$TMP"' EXIT
if ! python3 tools/gen-release-notes.py --out "$TMP" >/dev/null; then
  echo "✗ 生成发布说明失败（python3 tools/gen-release-notes.py）" >&2
  exit 1
fi

TARGET=""
if [ -n "$NOTES_ARG" ]; then
  TARGET="$NOTES_ARG"
elif [ -f "$RELDIR/NOTES.md" ]; then
  TARGET="$RELDIR/NOTES.md"
else
  TARGET="$TMP"
fi
if [ ! -f "$TARGET" ]; then
  echo "✗ 找不到发布说明文件：$TARGET" >&2
  exit 1
fi
NOTE="$(cat "$TARGET")"

if [ -z "${MANIFESTS[*]:-}" ] && [ -f "$RELDIR/manifest.json" ]; then
  MANIFESTS=("$RELDIR/manifest.json")
fi

echo "==> 发布说明门禁（版本 ${V}，说明 ${TARGET}）"
rc=0

# ① 版本标题
case "$NOTE" in
  "v${V} · "*) echo "  ✓ 以 v${V} · 开头" ;;
  *) echo "  ✗ 没有以 v${V} · 开头（说明与当前版本对不上）：${NOTE:0:60}" >&2; rc=1 ;;
esac

# ② 长度（按字符，中文一个字算一个）
LEN="$(python3 -c 'import sys;print(len(open(sys.argv[1],encoding="utf-8").read().strip()))' "$TARGET")"
if [ "$LEN" -le "$LIMIT" ]; then
  echo "  ✓ 长度 $LEN ≤ $LIMIT"
else
  echo "  ✗ 长度 $LEN 超过上限 $LIMIT" >&2; rc=1
fi

# ③ 不得出现更早版本的标题（形如 `v1.3.4 ·`）
if python3 - "$TARGET" "$V" <<'PY'
import re, sys
text = open(sys.argv[1], encoding="utf-8").read()
cur = "v" + sys.argv[2]
bad = sorted({m.group(0) for m in re.finditer(r"v\d+\.\d+\.\d+ ·", text)} - {cur + " ·"})
if bad:
    print("更早版本标题：" + "、".join(bad), file=sys.stderr)
    sys.exit(1)
PY
then
  echo "  ✓ 无更早版本标题"
else
  echo "  ✗ 含更早版本标题（说明里混进了旧版本文本）" >&2; rc=1
fi

# ④ manifest.notes 与生成物一致
if [ "${#MANIFESTS[@]}" -eq 0 ]; then
  echo "  （没有清单可校验，跳过 ④；发布后可用 make check 复核）"
else
  for m in "${MANIFESTS[@]}"; do
    [ -n "$m" ] || continue
    if [ ! -f "$m" ]; then
      echo "  ✗ 清单不存在：$m" >&2; rc=1; continue
    fi
    if python3 - "$m" "$TMP" <<'PY'
import json, sys
notes = json.load(open(sys.argv[1], encoding="utf-8")).get("notes", "")
want = open(sys.argv[2], encoding="utf-8").read().strip()
if notes.strip() != want:
    print(f"清单 notes 与生成物不一致（清单 {len(notes.strip())} 字 / 生成 {len(want)} 字）",
          file=sys.stderr)
    sys.exit(1)
PY
    then
      echo "  ✓ $m 的 notes 与生成物一致"
    else
      echo "  ✗ $m 校验失败" >&2; rc=1
    fi
  done
fi

# ⑤ Makefile 必须接生成器、不许再引用旧静态文件
if grep -q "tools/gen-release-notes.py" Makefile && ! grep -q "RELEASE_NOTES" Makefile; then
  echo "  ✓ Makefile 用生成器且不再引用旧文件"
else
  echo "  ✗ Makefile 没有正确接入 tools/gen-release-notes.py（或仍在引用 RELEASE_NOTES）" >&2; rc=1
fi

if [ "$rc" -eq 0 ]; then
  echo "发布说明门禁通过 ✅"
else
  echo "发布说明门禁失败 ❌" >&2
fi
exit "$rc"
