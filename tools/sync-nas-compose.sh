#!/usr/bin/env bash
# ============================================================================
#  sync-nas-compose.sh —— 把「推荐 Docker 项目」的预配置 compose 参考文件
#  发布到 NAS 镜像站，并逐条 curl 验收。
#
#  背景（用户 2026-09-17）：Docker 类应用从"可安装"改成"面板建议的项目"，
#  不提供安装；这些推荐项目的 compose 必须在 NAS 镜像上有一份供拉取。
#
#  布局（与 internal/services/compose_reference.go 的 ComposeReferenceURL 严格一致）：
#    <mirror-root>/compose/README.md                  总索引（项目 / 端口 / 网络方式）
#    <mirror-root>/compose/<id>/docker-compose.yml    预配置 compose（含 ${VAR} 占位符）
#    <mirror-root>/compose/<id>/.env.example          变量样例（只有占位符，无真密钥）
#    <mirror-root>/compose/<id>/README.md             单个项目的用法说明
#  对外 URL：<mirror-base>/compose/<id>/docker-compose.yml
#
#  文件内容**全部从代码生成**（go run ./cmd/zizpanel-assets compose），
#  本脚本不手抄任何 compose —— 手抄的那份一定会和面板展示的漂移。
#
#  认证：走 SSH **密钥**登录（NAS 已配好），不需要口令、也没有 sshpass 依赖。
#
#  用法：
#    bash tools/sync-nas-compose.sh --dry-run        # 只打印计划，不落盘不上传
#    bash tools/sync-nas-compose.sh                  # 真发布 + curl 验收
#    bash tools/sync-nas-compose.sh --app it-tools   # 只发布一个项目
#    bash tools/sync-nas-compose.sh --prune          # 同时删除镜像上多余的旧文件
#
#  环境变量：
#    NAS_HOST / NAS_USER / NAS_MIRROR_ROOT / MIRROR_BASE_URL
# ============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

NAS_HOST="${NAS_HOST:-192.168.1.8}"
NAS_USER="${NAS_USER:-zizdog}"
# 镜像站根目录（nginx 的 root；/apps、/sites、/zizpanel 都与它同级）。
NAS_MIRROR_ROOT="${NAS_MIRROR_ROOT:-/vol2/zizpanel-mirror}"
# 验收基址：默认走局域网（比公网入口快，且不依赖外部反代）。
MIRROR_BASE_URL="${MIRROR_BASE_URL:-http://192.168.1.8:8090}"

DRY_RUN=0
PRUNE=0
ONLY_APP=""

usage() { sed -n '2,32p' "$0"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) DRY_RUN=1 ;;
    --prune)   PRUNE=1 ;;
    --app)     ONLY_APP="${2:-}"; shift ;;
    --host)    NAS_HOST="${2:-}"; shift ;;
    --user)    NAS_USER="${2:-}"; shift ;;
    --root)    NAS_MIRROR_ROOT="${2:-}"; shift ;;
    --base)    MIRROR_BASE_URL="${2:-}"; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数：$1" >&2; usage; exit 2 ;;
  esac
  shift
done

MIRROR_BASE_URL="${MIRROR_BASE_URL%/}"
NAS_MIRROR_ROOT="${NAS_MIRROR_ROOT%/}"
DEST="$NAS_MIRROR_ROOT/compose"

echo "==> 生成参考文件（来源：代码 -- go run ./cmd/zizpanel-assets compose）"
if ! JSON="$(cd "$REPO_ROOT" && MIRROR_BASE_URL="$MIRROR_BASE_URL" go run ./cmd/zizpanel-assets compose)"; then
  echo "生成失败（需要在仓库根目录、且本机有 Go）" >&2
  exit 1
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
TREE="$TMP/tree"
printf '%s' "$JSON" > "$TMP/compose.json"

# 把 JSON 拆成文件树：compose/<id>/{docker-compose.yml,.env.example,README.md} + 根 README.md
#
# ⚠️ JSON 用**文件**传给 python，不要用管道：`python3 - <<'PY'` 时 heredoc
# 已经占用了 stdin（那是脚本源码），管道里的 JSON 会被 heredoc 覆盖。
python3 - "$TMP/compose.json" "$TREE" "$ONLY_APP" <<'PY'
import json, os, sys
src, tree, only = sys.argv[1], sys.argv[2], sys.argv[3]
with open(src, encoding="utf-8") as f:
    data = json.load(f)
items = data.get("items") or []
if only:
    items = [i for i in items if i.get("id") == only]
    if not items:
        print(f"生成结果里没有 {only}", file=sys.stderr)
        sys.exit(2)
base = os.path.join(tree, "compose")
os.makedirs(base, exist_ok=True)
if not only:
    with open(os.path.join(base, "README.md"), "w", encoding="utf-8") as f:
        f.write(data.get("index", ""))
for it in items:
    d = os.path.join(base, it["id"])
    os.makedirs(d, exist_ok=True)
    for name, key in (("docker-compose.yml", "compose"),
                      (".env.example", "env_example"),
                      ("README.md", "readme")):
        with open(os.path.join(d, name), "w", encoding="utf-8") as f:
            f.write(it.get(key, ""))
    print("  " + it["id"])
PY

COUNT="$(find "$TREE/compose" -mindepth 1 -maxdepth 1 -type d | wc -l | tr -d ' ')"
echo "    项目数：$COUNT"
echo "    目标：$NAS_USER@$NAS_HOST:$DEST/"
echo "    验收基址：$MIRROR_BASE_URL/compose/..."

# .env.example 必须只有占位符：这是发布到公网镜像的文件，泄一个真密钥就全完了。
# 这里做一次静态自检（生成物里出现明显的长随机串就拒绝发布）。
if grep -RInE '(SECRET|PASSWORD|KEY)=[A-Za-z0-9+/=_-]{24,}' "$TREE/compose" >/dev/null 2>&1; then
  echo "✗ 自检失败：生成的 .env.example 里出现了疑似真实密钥（长随机串）—— 拒绝发布：" >&2
  grep -RInE '(SECRET|PASSWORD|KEY)=[A-Za-z0-9+/=_-]{24,}' "$TREE/compose" >&2
  exit 1
fi

if [ "$DRY_RUN" = "1" ]; then
  echo
  echo "==> [dry-run] 会把上面这些文件上传到 $DEST/（不执行）"
  find "$TREE/compose" -type f | sed "s|$TREE/||" | sort | sed 's/^/    /'
  exit 0
fi

echo
echo "==> 上传"
ssh -n -o StrictHostKeyChecking=accept-new -o ConnectTimeout=10 \
  "$NAS_USER@$NAS_HOST" "mkdir -p '$DEST'" </dev/null
RSYNC_ARGS=(-az --no-perms --no-owner --no-group)
if [ "$PRUNE" = "1" ]; then
  # --delete 只作用于 compose/ 这一层（镜像站上这个目录完全由本脚本拥有）。
  RSYNC_ARGS+=(--delete)
fi
rsync "${RSYNC_ARGS[@]}" -e "ssh -o StrictHostKeyChecking=accept-new" \
  "$TREE/compose/" "$NAS_USER@$NAS_HOST:$DEST/" </dev/null

echo
echo "==> curl 验收（从本机经 $MIRROR_BASE_URL 取回）"
RESULTS="$TMP/results"
: > "$RESULTS"
check() { # <相对路径> <必须包含的片段（留空 = 只查 HTTP 200）>
  local rel="$1" want="$2" body code
  body="$(/usr/bin/curl -fsS -m 15 "$MIRROR_BASE_URL/$rel" 2>/dev/null || true)"
  code="$(/usr/bin/curl -s -o /dev/null -m 15 -w '%{http_code}' "$MIRROR_BASE_URL/$rel" 2>/dev/null || echo 000)"
  if [ "$code" = "200" ] && { [ -z "$want" ] || printf '%s' "$body" | grep -qF "$want"; }; then
    if [ -n "$want" ]; then
      echo "  ✓ ${rel}（HTTP 200，含 ${want}）"
    else
      echo "  ✓ ${rel}（HTTP 200，$(printf '%s' "$body" | wc -c | tr -d ' ') 字节）"
    fi
    printf 'ok\t%s\n' "$rel" >> "$RESULTS"
  else
    echo "  ✗ ${rel}（HTTP ${code}，未找到 ${want}）"
    printf 'fail\t%s\n' "$rel" >> "$RESULTS"
  fi
}

while IFS= read -r id; do
  [ -n "$id" ] || continue
  check "compose/$id/docker-compose.yml" "services:"
  check "compose/$id/.env.example" ""
done < <(find "$TREE/compose" -mindepth 1 -maxdepth 1 -type d | sed 's|.*/||' | sort)
if [ -z "$ONLY_APP" ]; then
  check "compose/README.md" "DATA_ROOT"
fi

echo
echo "==> 汇总"
ok="$(grep -c '^ok' "$RESULTS" 2>/dev/null || true)"
bad="$(grep -c '^fail' "$RESULTS" 2>/dev/null || true)"
[ -n "$ok" ] || ok=0
[ -n "$bad" ] || bad=0
echo "    成功：$ok    失败：$bad"
if [ "$bad" != "0" ]; then
  grep '^fail' "$RESULTS" | cut -f2 | sed 's/^/    失败：/'
  exit 1
fi
echo "完成。"
