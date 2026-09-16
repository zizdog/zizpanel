#!/usr/bin/env bash
# ============================================================================
#  sync-nas-apps.sh —— 把"官方 release 原生二进制"应用的安装包同步到镜像站
#
#  布局（面板侧按同一约定取包，见 internal/services/mirror.go）：
#      <apps-root>/<app-id>/<version>/<原始文件名>
#      <apps-root>/<app-id>/<version>/manifest.json    ← sha256/大小，面板据此校验
#  对外 URL： <mirror-base>/apps/<app-id>/<version>/<文件名>
#
#  要同步哪些应用**从代码注册表读**（`go run ./cmd/zizpanel-assets`），
#  不在本脚本里手抄 —— 手抄的那份在加应用/升版本时一定会漏，
#  而漏掉的后果是"镜像上没有这个包"，按设计那会直接让面板的安装失败。
#
#  用法：
#    bash tools/sync-nas-apps.sh --dry-run          # 只打印计划，不下载不上传
#    NAS_PASS='...' bash tools/sync-nas-apps.sh     # 真同步（需要 sshpass）
#    只同步一个应用：加 --app frpc
#
#  环境变量：NAS_HOST / NAS_USER / NAS_ROOT / NAS_PASS / MIRROR_BASE_URL
#  ⚠️ 口令只从环境变量进、只交给 sshpass，绝不写进任何文件、也不打印。
# ============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

NAS_HOST="${NAS_HOST:-192.168.1.8}"
NAS_USER="${NAS_USER:-zizdog}"
# NAS_ROOT 是 apps 目录的父目录（与 Makefile 的 zizpanel 镜像目录同级）
NAS_ROOT="${NAS_ROOT:-/vol2/zizpanel-mirror/apps}"
NAS_PASS="${NAS_PASS:-}"
# 校验用的对外基址：默认取面板的默认镜像基址，确保"面板真的能取到"
MIRROR_BASE_URL="${MIRROR_BASE_URL:-https://mirror.zizdog.com:8888}"

DRY_RUN=0
ONLY_APP=""

usage() { sed -n '2,25p' "$0"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run)    DRY_RUN=1 ;;
    --app)        ONLY_APP="${2:-}"; shift ;;
    --host)       NAS_HOST="${2:-}"; shift ;;
    --user)       NAS_USER="${2:-}"; shift ;;
    --apps-root)  NAS_ROOT="${2:-}"; shift ;;
    --base)       MIRROR_BASE_URL="${2:-}"; shift ;;
    -h|--help)    usage; exit 0 ;;
    *) echo "未知参数：$1" >&2; usage; exit 2 ;;
  esac
  shift
done

MIRROR_BASE_URL="${MIRROR_BASE_URL%/}"

sha256_of() { /usr/bin/shasum -a 256 "$1" | awk '{print $1}'; }
http_code() { /usr/bin/curl -s -o /dev/null -m 10 -w '%{http_code}' "$1" 2>/dev/null || echo 000; }

# 上游候选：官方优先，其次国内加速（同步机通常在国内，官方经常 0 字节）
upstream_candidates() { # <upstream-url>
  printf '%s\n' "$1" "https://gh-proxy.com/$1" "https://ghfast.top/$1"
}

# 远端清单里某个文件的 sha256（取不到就输出空串）
remote_sha256() { # <app> <tag> <asset>
  local body
  body="$(/usr/bin/curl -s -m 10 "$MIRROR_BASE_URL/apps/$1/$2/manifest.json" 2>/dev/null || true)"
  [ -n "$body" ] || { echo ""; return 0; }
  printf '%s' "$body" | python3 -c '
import json,sys
want = sys.argv[1]
try:
    d = json.load(sys.stdin)
except Exception:
    print(""); raise SystemExit
print(next((a.get("sha256","") for a in d.get("assets",[]) if a.get("name") == want), ""))
' "$3" 2>/dev/null || echo ""
}

echo "==> 应用清单来源：代码注册表（go run ./cmd/zizpanel-assets）"
if ! APPS_JSON="$(cd "$REPO_ROOT" && go run ./cmd/zizpanel-assets)"; then
  echo "读取注册表失败（需要在仓库根目录、且本机有 Go）" >&2
  exit 1
fi

PLAN="$(printf '%s' "$APPS_JSON" | python3 -c '
import json,sys
for a in json.load(sys.stdin)["apps"]:
    print("\t".join([a["id"], a["tag"], a["asset"], a["upstream_url"]]))
')"
if [ -z "$PLAN" ]; then
  echo "注册表里没有任何应用包？" >&2
  exit 1
fi
echo "    共 $(printf '%s\n' "$PLAN" | wc -l | tr -d ' ') 个应用包"

if [ "$DRY_RUN" != "1" ] && [ -z "$NAS_PASS" ]; then
  echo "需要 NAS 口令：NAS_PASS='...' bash tools/sync-nas-apps.sh（或用 ssh 密钥 + 自行改这里的传输方式）" >&2
  exit 1
fi
if [ "$DRY_RUN" != "1" ] && ! command -v sshpass >/dev/null 2>&1; then
  echo "需要 sshpass（brew install hudochenkov/sshpass/sshpass）" >&2
  exit 1
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
RESULTS="$TMP/results"
: > "$RESULTS"

echo "==> 目标：$NAS_USER@$NAS_HOST:$NAS_ROOT/<应用>/<版本>/"
echo "==> 校验基址：$MIRROR_BASE_URL/apps/..."
echo

while IFS=$'\t' read -r id tag asset upstream; do
  [ -n "$id" ] || continue
  if [ -n "$ONLY_APP" ] && [ "$ONLY_APP" != "$id" ]; then
    continue
  fi
  dest="$NAS_ROOT/$id/$tag"
  public="$MIRROR_BASE_URL/apps/$id/$tag/$asset"
  had="$(remote_sha256 "$id" "$tag" "$asset")"

  if [ "$DRY_RUN" = "1" ]; then
    if [ -n "$had" ]; then
      echo "  [dry-run] $id ${tag}：镜像上已有 ${asset}（sha256 ${had:0:12}…）→ 会跳过"
    else
      echo "  [dry-run] $id ${tag}：镜像上没有 $asset → 下载并上传到 $dest/"
    fi
    echo "            上游：$upstream"
    echo "            验收：$public"
    printf 'dry\t%s\n' "$id" >> "$RESULTS"
    continue
  fi

  dir="$TMP/$id/$tag"
  mkdir -p "$dir"
  ok=0
  for cand in $(upstream_candidates "$upstream"); do
    echo "  下载 $id $tag $asset"
    echo "    候选：$cand"
    if /usr/bin/curl -fL --http1.1 --retry 1 --connect-timeout 20 --max-time 900 \
        --speed-limit 1024 --speed-time 30 -o "$dir/$asset.part" "$cand" 2>/dev/null; then
      mv "$dir/$asset.part" "$dir/$asset"
      ok=1
      break
    fi
    echo "    这个源不通，换下一个"
  done
  if [ "$ok" != "1" ]; then
    echo "  ✗ $id ${tag}：所有上游都不通，跳过（镜像上仍是旧状态）"
    printf 'fail\t%s\n' "$id" >> "$RESULTS"
    continue
  fi

  sum="$(sha256_of "$dir/$asset")"
  if [ -n "$had" ] && [ "$had" = "$sum" ]; then
    echo "  = $id ${tag}：镜像上已是同一个文件（sha256 ${sum:0:12}…），跳过上传"
    printf 'skip\t%s\n' "$id" >> "$RESULTS"
    continue
  fi

  # 清单与包放在同一目录：面板下载前会 HEAD 包 + 取清单做 sha256 校验
  python3 - "$id" "$tag" "$asset" "$sum" "$(wc -c < "$dir/$asset" | tr -d ' ')" "$upstream" > "$dir/manifest.json" <<'PY'
import json, sys, time
app, tag, asset, sha, size, upstream = sys.argv[1:7]
print(json.dumps({
    "app": app, "version": tag, "generated_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
    "assets": [{"name": asset, "sha256": sha, "size": int(size), "upstream": upstream}],
}, ensure_ascii=False, indent=2))
PY

  # ⚠️ ssh 的 -n 与两条 </dev/null 都不能省。
  # 这段循环是 `while read ... done <<< "$PLAN"`，而 ssh 默认**会把 stdin 读走**。
  # 实测（2026-09-16，注册表里 2 个应用）：需要真上传的那个一旦调用 ssh，
  # 后面待同步的应用整段被吃掉，脚本却报"成功"—— 因为第一个应用确实传上去了，
  # 汇总里 `失败：0`，看不出少传了一个。加 -n 后同样的循环 3 个应用全都跑到。
  sshpass -p "$NAS_PASS" ssh -n -o StrictHostKeyChecking=accept-new \
    "$NAS_USER@$NAS_HOST" "mkdir -p '$dest'" </dev/null >/dev/null
  sshpass -p "$NAS_PASS" rsync -az --no-perms --no-owner --no-group \
    -e "ssh -o StrictHostKeyChecking=accept-new" \
    "$dir/" "$NAS_USER@$NAS_HOST:$dest/" </dev/null >/dev/null

  code="$(http_code "$public")"
  if [ "$code" = "200" ]; then
    echo "  ✓ $id ${tag}：已上传（sha256 ${sum:0:12}…），对外 $code"
    printf 'ok\t%s\n' "$id" >> "$RESULTS"
  else
    echo "  ✗ $id ${tag}：上传后验收失败（$public → HTTP ${code}）"
    printf 'fail\t%s\n' "$id" >> "$RESULTS"
  fi
done <<< "$PLAN"

echo
echo "==> 汇总"
for k in ok skip dry fail; do
  n="$(grep -c "^$k	" "$RESULTS" 2>/dev/null || true)"
  [ -n "$n" ] || n=0
  case "$k" in
    ok)   label="成功" ;;
    skip) label="已是最新（跳过）" ;;
    dry)  label="计划（dry-run）" ;;
    fail) label="失败" ;;
  esac
  echo "    ${label}：$n"
done
grep '^fail	' "$RESULTS" | cut -f2 | sed 's/^/    失败：/' || true

if grep -q '^fail	' "$RESULTS"; then
  exit 1
fi
echo "完成。"
