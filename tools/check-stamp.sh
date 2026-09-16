#!/usr/bin/env bash
# ============================================================================
#  check-stamp.sh —— 「这棵树已经跑过 make check」的凭据
#
#  为什么需要它：`make check` 是硬门禁（5~7 分钟，里面有三个真的起进程的端到端
#  测试），但**同一棵树**上重复跑它没有任何新信息。以前每次部署靠人手写
#  `SKIP_CHECK=1`，而"手写的跳过"无法自证跑没跑过 —— 于是要么白等 7 分钟，
#  要么偷偷跳过（后者更糟）。这里把"跳过"变成**可核对的事实**：
#    · `fingerprint` 算出当前工作树的指纹（含未跟踪文件的内容）；
#    · `make check` 成功时把指纹写进 dist/.check-stamp；
#    · 部署前 `verify` 比对 —— 指纹一致才跳过，并打印那个标记的时间与版本。
#  任何改动（改文件、加文件、删文件、切版本号）都会让指纹变化，从而**强制重跑**。
#
#  指纹刻意覆盖：git status（增删改）、git diff HEAD（已跟踪文件的改动内容）、
#  未跟踪文件的内容哈希。不用 `git stash`/`git write-tree`：那会动仓库状态。
# ============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# ⚠️ 刻意**不放 dist/**：`make release` 依赖 `clean`，而 clean 会 `rm -rf dist` ——
# 放那里的话，deploy 里"先跑 check 写标记、再 release 把它删掉"，标记永远留不下
# （本轮实测踩到）。放在仓库根并用 .gitignore 忽略：必须被忽略，否则它会作为
# 未跟踪文件进入自己的指纹，形成自指。
STAMP="$REPO_ROOT/.zp-check-stamp"

fingerprint() {
  cd "$REPO_ROOT"
  {
    git status --porcelain=v1 --untracked-files=all
    git diff HEAD
    # 未跟踪文件的内容也要算进去（否则"新加了一个文件"不会让指纹变化）
    while IFS= read -r f; do
      [ -n "$f" ] || continue
      [ -f "$f" ] && shasum -a 256 "$f"
    done < <(git ls-files --others --exclude-standard | LC_ALL=C sort)
  } | shasum -a 256 | awk '{print $1}'
}

cmd="${1:-verify}"
case "$cmd" in
  fingerprint)
    fingerprint
    ;;
  write)
    mkdir -p "$(dirname "$STAMP")"
    fp="$(fingerprint)"
    ver="$(grep -oE '[0-9]+\.[0-9]+\.[0-9]+' "$REPO_ROOT/internal/version/version.go" | head -1)"
    printf '%s %s %s\n' "$fp" "$ver" "$(date '+%Y-%m-%d %H:%M:%S')" > "$STAMP"
    echo "已记录 check 标记：$ver @ $(date '+%H:%M:%S')（指纹 ${fp:0:12}…）"
    ;;
  verify)
    # 退出码：0 = 这棵树确实跑过 check（可安全跳过）；1 = 没跑过/树已变（必须重跑）
    [ -f "$STAMP" ] || { echo "没有 check 标记（dist/.check-stamp 不存在）"; exit 1; }
    read -r fp ver ts < "$STAMP" || true
    now="$(fingerprint)"
    if [ "$fp" != "$now" ]; then
      echo "工作树自上次 check 后已变化（标记 ${fp:0:12}… → 现在 ${now:0:12}…），必须重跑"
      exit 1
    fi
    echo "这棵树已通过 make check：版本 ${ver}，时间 ${ts}（指纹 ${fp:0:12}…）"
    ;;
  *)
    echo "用法：$0 fingerprint|write|verify" >&2
    exit 2
    ;;
esac
