#!/usr/bin/env bash
# ============================================================================
#  build-offline-bundle.sh —— 在 NAS 上汇总出"某个应用的离线安装包"
#
#  用户原话（2026-09-16）：
#    "要确保应用市场里的所有软件都能顺利安装！有必要的话可以把所有文件卡点
#     都放到 nas 镜像。甚至直接将软件'打包'一键迁移回 mac 系统里。"
#
#  ---------------------------------------------------------------------------
#  离线包格式（清单驱动，见 internal/services/offline_plan.go 的 schema 常量）
#
#    <base>/offline/<app-id>/index.json                ← 有哪些版本、current 是哪个
#    <base>/offline/<app-id>/<version>/manifest.json   ← 本应用安装所需的**全部**文件
#    <base>/offline/<app-id>/<version>/artifacts/...    ← 真实文件（sha256 实算）
#
#  manifest.json 里每个 artifact 一定有：
#    kind         brew_bottle / brew_manifest / brew_formula_json / pip_wheel /
#                 model / site_tarball / github_binary / docker_image_tar /
#                 vm_image / other
#    path         在离线包里的相对路径（= 需要的 served 路径）
#    url          上游地址（**仅作参考**，离线安装永远不用它）
#    sha256       ★本工具**下载后真算出来的**，不是抄的
#    size         实测字节数
#  另外带 source_url（这次实际从哪取的，通常是 NAS 镜像）与 missing 数组。
#
#  ⚠️ 为什么强调 sha256 是"真算的"：这个仓库曾经因为编造 sha256 导致镜像
#  永远不被选中、静默回落慢源，用户装了 21 分钟。所以本工具的纪律是：
#    ① sha256 一律用 `shasum -a 256` 对**落盘后的文件**重算；
#    ② 上游清单里声明的 sha256 只用来**判等**，不等就判为缺件并失败；
#    ③ 缺件逐条列出，**绝不静默跳过**；最后按"计划数 = 制品数 + 缺件数"对账。
#
#  ---------------------------------------------------------------------------
#  用法：
#    bash tools/build-offline-bundle.sh --list                    # 只看计划与缺口
#    bash tools/build-offline-bundle.sh --app frpc               # 本地生成（不上传）
#    NAS_PASS='…' bash tools/build-offline-bundle.sh --app frpc --upload
#    NAS_PASS='…' bash tools/build-offline-bundle.sh --all --upload
#    bash tools/build-offline-bundle.sh --verify frpc            # 只做 HTTP 侧核对
#
#  环境变量：NAS_HOST / NAS_USER / NAS_ROOT / NAS_PASS / MIRROR_BASE
#  ⚠️ 口令只从环境变量进、只交给 sshpass，绝不写进任何文件、也不打印。
# ============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

NAS_HOST="${NAS_HOST:-192.168.1.8}"
NAS_USER="${NAS_USER:-zizdog}"
# NAS_ROOT 是 offline 目录（与 /vol2/zizpanel-mirror 下的 apps/ brew/ 同级）
NAS_ROOT="${NAS_ROOT:-/vol2/zizpanel-mirror/offline}"
NAS_PASS="${NAS_PASS:-}"
# 取件与验收都用这个基址。默认走里网直连 8090（不经过公网 8888，测试更干净）
MIRROR_BASE="${MIRROR_BASE:-http://192.168.1.8:8090}"
TAG="${TAG:-arm64_sequoia}"

MODE="build"
UPLOAD=0
DO_VERIFY=1
ALLOW_MISSING=0
DRY_RUN=""
STAGE=""
ONLY_APPS=()
VERIFY_ONLY=""

usage() { sed -n '2,45p' "$0"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --list)     MODE="list" ;;
    --all)      ONLY_APPS=() ; ALLOW_MISSING="${ALLOW_MISSING}" ;;
    --app)      ONLY_APPS+=("${2:-}"); shift ;;
    --upload)   UPLOAD=1 ;;
    --stage)    STAGE="${2:-}"; shift ;;
    --base)     MIRROR_BASE="${2:-}"; shift ;;
    --nas-root) NAS_ROOT="${2:-}"; shift ;;
    --nas-host) NAS_HOST="${2:-}"; shift ;;
    --nas-user) NAS_USER="${2:-}"; shift ;;
    --tag)      TAG="${2:-}"; shift ;;
    --verify)   MODE="verify"; VERIFY_ONLY="${2:-}"; shift ;;
    --dry-run)  DRY_RUN=1 ;;
    --no-verify) DO_VERIFY=0 ;;
    --allow-missing) ALLOW_MISSING=1 ;;
    -h|--help)  usage; exit 0 ;;
    *) echo "未知参数：$1" >&2; usage; exit 2 ;;
  esac
  shift
done

MIRROR_BASE="${MIRROR_BASE%/}"
sha256_of() { /usr/bin/shasum -a 256 "$1" | awk '{print $1}'; }
http_code() { /usr/bin/curl -s -o /dev/null -m 20 -w '%{http_code}' "$1" 2>/dev/null || echo 000; }
py() { python3 "$@"; }

# ---------------------------------------------------------------- 计划
echo "==> 读离线打包计划（代码注册表 + 目录，不手抄）：go run ./cmd/zizpanel-assets plan"
PLAN_JSON="${STAGE:-$(mktemp -d)}/zp-offline-plan.json"
mkdir -p "$(dirname "$PLAN_JSON")"
if ! (cd "$REPO_ROOT" && go run ./cmd/zizpanel-assets plan) > "$PLAN_JSON"; then
  echo "读取计划失败（需要在仓库根目录、且本机有 Go）" >&2
  exit 1
fi
TOTAL_APPS="$(py -c 'import json,sys;print(len(json.load(open(sys.argv[1]))["apps"]))' "$PLAN_JSON")"
echo "    计划里共 $TOTAL_APPS 个应用"

# 选中要处理的 app 列表（一行一个）
SELECTED_FILE="$(mktemp)"
trap 'rm -f "$SELECTED_FILE"' EXIT
if [ "${#ONLY_APPS[@]}" -gt 0 ]; then
  printf '%s\n' "${ONLY_APPS[@]}" > "$SELECTED_FILE"
elif [ "$MODE" = "verify" ]; then
  printf '%s\n' "$VERIFY_ONLY" > "$SELECTED_FILE"
else
  py -c 'import json,sys;[print(a["id"]) for a in json.load(open(sys.argv[1]))["apps"]]' "$PLAN_JSON" > "$SELECTED_FILE"
fi

if [ "$MODE" = "list" ]; then
  py - "$PLAN_JSON" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1]))
print(f'{"应用":20s} {"方式":16s} 计划制品  已知缺口  说明')
n_gap = n_art = 0
for a in doc["apps"]:
    arts = [x for x in (a.get("artifacts") or []) if x.get("kind") != "other"]
    gaps = a.get("gaps") or []
    n_gap += len(gaps); n_art += len(arts)
    note = ""
    if a.get("brew_formula"):
        note = f'brew:{a["brew_formula"]}（依赖闭包在构建时展开）'
    elif a.get("docker_images"):
        note = "compose 镜像：" + ", ".join(a["docker_images"])
    print(f'{a["id"]:20s} {a["install_method"]:16s} {len(arts):^8d}  {len(gaps):^8d}  {note}')
print(f"\n合计：{len(doc['apps'])} 个应用，{n_art} 个已知可镜像制品，{n_gap} 条已知缺口")
PY
  exit 0
fi

# ---------------------------------------------------------------- 构建 / 验收
if [ -z "$STAGE" ]; then STAGE="$(mktemp -d)"; fi
mkdir -p "$STAGE"
WORK="$STAGE/_work"
mkdir -p "$WORK"

if [ "$UPLOAD" = "1" ] && [ -n "$DRY_RUN" ]; then
  echo "--dry-run 与 --upload 不能同时用" >&2; exit 2
fi
if [ "$UPLOAD" = "1" ]; then
  if [ -z "$NAS_PASS" ]; then
    echo "需要 NAS 口令：NAS_PASS='...' bash tools/build-offline-bundle.sh …（或用 ssh 密钥）" >&2
    exit 1
  fi
  if ! command -v sshpass >/dev/null 2>&1; then
    echo "需要 sshpass（brew install hudochenkov/sshpass/sshpass）" >&2
    exit 1
  fi
fi

RESULTS="$(mktemp)"
trap 'rm -f "$SELECTED_FILE" "$RESULTS"' EXIT
: > "$RESULTS"

upload_dir() { # <local-dir> <remote-dir>
  local l="$1" r="$2"
  # ⚠️ -n 与 </dev/null 都不能省：这个循环是 `while read … done < file`，
  # ssh 默认会把 stdin 读走，后面的应用整段被吃掉、脚本却报成功
  # （tools/sync-nas-apps.sh 真机踩过：2 个应用只跑了 1 个，汇总"失败：0"）。
  sshpass -p "$NAS_PASS" ssh -n -o StrictHostKeyChecking=accept-new \
    "$NAS_USER@$NAS_HOST" "mkdir -p '$r'" </dev/null >/dev/null
  sshpass -p "$NAS_PASS" rsync -az --no-perms --no-owner --no-group \
    -e "ssh -o StrictHostKeyChecking=accept-new" \
    "$l/" "$NAS_USER@$NAS_HOST:$r/" </dev/null >/dev/null
}

while IFS= read -r app; do
  [ -n "$app" ] || continue
  echo
  echo "==> 应用：$app"

  if [ "$MODE" = "verify" ]; then
    APP_JSON=""
    VERSION=""
    # 验收模式：拿 NAS 上已有的 manifest.json 来核对
    VERSION="$(/usr/bin/curl -s -m 20 "$MIRROR_BASE/offline/$app/index.json" \
      | py -c 'import json,sys;d=json.load(sys.stdin);print(d.get("current",""))' 2>/dev/null || true)"
    if [ -n "$VERSION" ]; then
      APP_JSON="$(/usr/bin/curl -s -m 30 "$MIRROR_BASE/offline/$app/$VERSION/manifest.json")"
    fi
    if [ -z "$APP_JSON" ]; then
      echo "  ✗ NAS 上没有 $app 的 index.json/manifest.json"
      printf 'missing\t%s\t0\t0\n' "$app" >> "$RESULTS"
      continue
    fi
  else
    # 清掉上一次构建的暂存（否则旧版本目录会被算进"磁盘文件数"，对账必错 ——
    # 真机踩过：换了目录名（current → 9.0.1_1）后旧目录还在，45 个制品数成 90 个）
    rm -rf "$STAGE/$app" "$WORK/$app"
    REPORT="$WORK/$app.json"
    if ! py "$REPO_ROOT/tools/offline-bundle-lib.py" \
        --plan "$PLAN_JSON" --app "$app" --stage "$WORK/$app" \
        --base "$MIRROR_BASE" --tag "$TAG" ${DRY_RUN:+--dry-run} > "$REPORT"; then
      echo "  ✗ 构建 $app 失败（见上面的输出）"
      printf 'fail\t%s\t0\t0\n' "$app" >> "$RESULTS"
      continue
    fi
    APP_JSON="$(cat "$REPORT")"
    VERSION="$(py -c 'import json,sys;print(json.load(open(sys.argv[1]))["version"])' "$REPORT")"
    DEST="$STAGE/$app/$VERSION"
    mkdir -p "$DEST"
    if [ -d "$WORK/$app/artifacts" ]; then
      mv "$WORK/$app/artifacts" "$DEST/artifacts"
    fi
    py - "$REPORT" "$DEST/manifest.json" <<'PY'
import json, sys
rep = json.load(open(sys.argv[1], encoding="utf-8"))
with open(sys.argv[2], "w", encoding="utf-8") as f:
    json.dump(rep, f, ensure_ascii=False, indent=2)
    f.write("\n")
PY
    # 每个应用一个 index.json：面板离线模式靠它找 current 版本
    py - "$REPORT" "$STAGE/$app/index.json" <<'PY'
import json, sys, time
rep = json.load(open(sys.argv[1], encoding="utf-8"))
doc = {
    "app": rep["app"], "name": rep.get("name", ""),
    "current": rep["version"], "versions": [rep["version"]],
    "updated_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
    "mirror_base": rep.get("mirror_base", ""),
}
with open(sys.argv[2], "w", encoding="utf-8") as f:
    json.dump(doc, f, ensure_ascii=False, indent=2)
    f.write("\n")
PY
  fi

  # 报告（构建与验收共用）
  py - "$APP_JSON" "$app" <<'PY'
import json, sys
rep = json.loads(sys.argv[1])
arts = rep.get("artifacts") or []
miss = rep.get("missing") or []
warns = rep.get("warnings") or []
tot = rep.get("totals") or {}
size = tot.get("bytes") or sum(int(a.get("size") or 0) for a in arts)
print(f"  version={rep.get('version')}  制品={len(arts)}  缺件={len(miss)}  "
      f"体积={size/1e6:.1f}MB")
for m in miss:
    print(f"    ✗ 缺件 {m.get('path') or m.get('formula')}: {m.get('why')}")
for w in warns:
    print(f"    ! 注意 {w}")
for g in (rep.get("gaps") or []):
    print(f"    · 已知缺口 {g}")
PY

  N_ART="$(py -c 'import json,sys;print(len(json.loads(sys.argv[1]).get("artifacts") or []))' "$APP_JSON")"
  N_MISS="$(py -c 'import json,sys;print(len(json.loads(sys.argv[1]).get("missing") or []))' "$APP_JSON")"

  # 构建时：把"计划的制品数"与"落盘的文件数"对账 —— 少一个都要报出来。
  if [ "$MODE" != "verify" ] && [ -z "$DRY_RUN" ]; then
    N_FILES=0
    if [ -d "$STAGE/$app/$VERSION" ]; then
      # 只数**当前版本目录**下的制品；manifest/index 不算制品
      N_FILES="$(find "$STAGE/$app/$VERSION" -type f ! -name manifest.json ! -name index.json | wc -l | tr -d ' ')"
    fi
    N_ART="$(py -c 'import json,sys;print(len(json.loads(sys.argv[1]).get("artifacts") or []))' "$APP_JSON")"
    if [ "$N_FILES" != "$N_ART" ]; then
      echo "  ✗ 对账失败：清单声明 $N_ART 个制品，磁盘上有 $N_FILES 个文件"
      printf 'fail\t%s\t%s\t%s\n' "$app" "$N_ART" "$N_MISS" >> "$RESULTS"
      continue
    fi
  fi

  if [ "$UPLOAD" = "1" ]; then
    upload_dir "$STAGE/$app" "$NAS_ROOT/$app"
    echo "  已上传到 $NAS_USER@$NAS_HOST:$NAS_ROOT/$app/"
  fi

  # 核对：逐个制品重算 sha256。
  #   · 上传过 → 从 **HTTP 侧**重下再算（"证明镜像真的能提供这个文件"的硬证据）；
  #   · 没上传 → 对**本地暂存文件**重算（证明清单里的 sha256 与落盘文件一致）。
  if [ "$DO_VERIFY" = "1" ]; then
    HTTP_VERIFY=0
    if [ "$UPLOAD" = "1" ] || [ "$MODE" = "verify" ]; then HTTP_VERIFY=1; fi
    if [ "$HTTP_VERIFY" = "1" ]; then
      echo "  HTTP 核对（重下 + 重算 sha256）…"
    else
      echo "  本地核对（对暂存文件重算 sha256）…"
    fi
    VFAIL=0
    VN=0
    while IFS=$'\t' read -r rel want; do
      [ -n "$rel" ] || continue
      VN=$((VN + 1))
      tmp=""
      if [ "$HTTP_VERIFY" = "1" ]; then
        tmp="$(mktemp)"
        url="$MIRROR_BASE/offline/$app/$VERSION/$rel"
        if ! /usr/bin/curl -fsS -m 900 -o "$tmp" "$url"; then
          echo "    ✗ 取不到 $url"
          VFAIL=$((VFAIL + 1)); rm -f "$tmp"; continue
        fi
      else
        tmp="$STAGE/$app/$VERSION/$rel"
        if [ ! -f "$tmp" ]; then
          echo "    ✗ 暂存目录里没有 $tmp"
          VFAIL=$((VFAIL + 1)); continue
        fi
      fi
      got="$(sha256_of "$tmp")"
      [ "$HTTP_VERIFY" = "1" ] && rm -f "$tmp"
      if [ -n "$want" ] && [ "$got" != "$want" ]; then
        echo "    ✗ sha256 不一致 ${rel}（清单 ${want} / 实际 ${got}）"
        VFAIL=$((VFAIL + 1))
      fi
      # ⚠️ while read 的 stdin 不能被任何子命令吃掉（sync-nas-apps.sh 的 ssh 教训）
    done < <(py -c '
import json,sys
rep=json.loads(sys.argv[1])
for a in rep.get("artifacts") or []:
    print(a.get("path","")+"\t"+a.get("sha256",""))
' "$APP_JSON" </dev/null)
    if [ "$VFAIL" != "0" ]; then
      printf 'fail\t%s\t%s\t%s\n' "$app" "$VN" "$VFAIL" >> "$RESULTS"
      continue
    fi
    echo "    ✓ $VN 个制品全部核对通过"
  fi

  printf 'ok\t%s\t%s\t%s\n' "$app" "$N_ART" "$N_MISS" >> "$RESULTS"
done < "$SELECTED_FILE"

echo
echo "==> 汇总"
# ⚠️ grep -c 在"0 个匹配"时退出码是 1，而 set -o pipefail 会把整条管道判成失败、
# 直接掐掉脚本 —— 于是"全部成功"反而让脚本中途死掉（真机踩过：成功6 失败0 却 exit 1）。
{ grep -c '^ok	' "$RESULTS" 2>/dev/null || true; } | sed 's/^/    成功：/'
{ grep -c '^fail	' "$RESULTS" 2>/dev/null || true; } | sed 's/^/    失败：/'
echo "    暂存目录：$STAGE"

# 计数核对：选中的 app 数必须等于（ok + fail）
N_SEL="$(grep -c . "$SELECTED_FILE" 2>/dev/null || true)"
[ -n "$N_SEL" ] || N_SEL=0
N_DONE="$(grep -c . "$RESULTS" 2>/dev/null || true)"
[ -n "$N_DONE" ] || N_DONE=0
if [ "$N_SEL" != "$N_DONE" ]; then
  echo "  ✗ 对账失败：选中 $N_SEL 个应用，只有 $N_DONE 个进了汇总" >&2
  exit 1
fi

# 有缺件时退出码非 0（除非 --allow-missing）：让 CI/脚本不会把"缺东西"当成功。
if grep -q '^fail	' "$RESULTS"; then exit 1; fi
if [ "$ALLOW_MISSING" != "1" ]; then
  if py - "$RESULTS" >/dev/null <<'PY'
import sys
bad = 0
for line in open(sys.argv[1]):
    p = line.rstrip("\n").split("\t")
    if len(p) >= 4 and p[0] == "ok" and p[3] not in ("0", ""):
        bad += 1
raise SystemExit(1 if bad else 0)
PY
  then :; else
    echo "  ✗ 有应用存在缺件（加 --allow-missing 可忽略）" >&2
    exit 1
  fi
fi
echo "完成。"
