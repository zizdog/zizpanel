#!/usr/bin/env bash
# ============================================================================
#  从 NAS 镜像把 Ollama 模型落到本机模型仓库
#
#  背景：应用市场里的 ollama 条目只提示 `ollama pull qwen2.5:7b`，而模型来自
#  registry.ollama.ai。本脚本把"从 NAS 拉"变成一条明确、可复核的命令：
#
#      bash tools/ollama-model-from-nas.sh --model qwen2.5:7b
#
#  它做的事（与 internal/services/ollama_models.go 同一套政策）：
#    1. NAS 优先：先探 <base>/manifests/registry.ollama.ai/<ns>/<name>/<tag>；
#       探得通就用 NAS，否则**回落**公网 registry.ollama.ai 并如实打印；
#    2. 逐 blob 按它自己的 digest 校验 sha256（内容寻址，不符就删掉重下）；
#    3. 落成 Ollama 本地模型仓库的布局（blobs/sha256-<hex> + manifests/...），
#       之后 `ollama run <name>:<tag>` 直接用本地文件、不再联网。
#
#  局限（如实写在这里）：
#    · 这不是反代 OCI registry 协议，而是"预置 blob + 写清单"——绕开了
#      Ollama 对自定义 registry 的 HTTPS/域名要求，但因此只能拉**已镜像**的模型；
#      没镜像的模型仍走 `ollama pull`（公网）。
#    · 大模型（qwen2.5:7b 单个 blob 4.68 GB）首次从公网同步到 NAS 要 NAS 能访问
#      registry.ollama.ai；之后本机从 NAS 取是局域网速度。
# ============================================================================
set -euo pipefail

MODEL="${OLLAMA_MIRROR_MODEL:-qwen2.5:7b}"
NAS_BASE="${OLLAMA_MIRROR_BASE:-https://mirror.zizdog.com:8888/models/ollama}"
UPSTREAM="${OLLAMA_MIRROR_UPSTREAM:-https://registry.ollama.ai}"
MODELS_DIR="${OLLAMA_MODELS:-$HOME/.ollama/models}"
MANIFEST_SHA=""
CHECK_ONLY=0
REGISTRY_HOST="registry.ollama.ai"

usage() {
  cat <<'EOF'
用法: ollama-model-from-nas.sh [选项]

  --model <name:tag>     要拉的模型（默认 qwen2.5:7b）
  --models-dir <dir>     本地模型仓库（默认 $OLLAMA_MODELS 或 ~/.ollama/models）
  --base <url>           NAS 镜像基址（默认 https://mirror.zizdog.com:8888/models/ollama）
  --upstream <url>       公网回落源（默认 https://registry.ollama.ai）
  --manifest-sha <hex>   可选：钉住清单本身的 sha256（不符即失败，不落盘）
  --check                只校验本地已有的模型，不下载
  -h, --help             显示本帮助
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --model) MODEL="$2"; shift 2 ;;
    --models-dir) MODELS_DIR="$2"; shift 2 ;;
    --base) NAS_BASE="$2"; shift 2 ;;
    --upstream) UPSTREAM="$2"; shift 2 ;;
    --manifest-sha) MANIFEST_SHA="$2"; shift 2 ;;
    --check) CHECK_ONLY=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数: $1" >&2; usage >&2; exit 2 ;;
  esac
done

NAS_BASE="${NAS_BASE%/}"
UPSTREAM="${UPSTREAM%/}"

# 解析 name:tag 与 namespace（与 ollama 的模型名一致：namespace 缺省是 library）。
ref="$MODEL"; tag="latest"
case "$ref" in
  *:*) tag="${ref##*:}"; ref="${ref%:*}" ;;
esac
case "$ref" in
  */*) ns="${ref%%/*}"; name="${ref##*/}" ;;
  *)   ns="library";    name="$ref" ;;
esac

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

probe() { curl -fsI --max-time 6 "$1" >/dev/null 2>&1; }

MANIFEST_REL="$REGISTRY_HOST/$ns/$name/$tag"
MANIFEST_DEST="$MODELS_DIR/manifests/$MANIFEST_REL"
BLOB_DIR="$MODELS_DIR/blobs"

# ---------- --check：只校验本地 ----------
if [ "$CHECK_ONLY" = 1 ]; then
  if [ ! -f "$MANIFEST_DEST" ]; then
    echo "本地没有 $MODEL 的清单：$MANIFEST_DEST" >&2
    exit 1
  fi
  echo "本地清单：$MANIFEST_DEST"
  if [ -n "$MANIFEST_SHA" ]; then
    got="$(sha256_of "$MANIFEST_DEST")"
    if [ "$got" != "$MANIFEST_SHA" ]; then
      echo "清单 sha256 不符（期望 ${MANIFEST_SHA}，实际 ${got}）" >&2
      exit 1
    fi
    echo "清单 sha256 一致：$got"
  fi
  fail=0
  while read -r hex; do
    [ -n "$hex" ] || continue
    f="$BLOB_DIR/sha256-$hex"
    if [ ! -f "$f" ]; then
      echo "缺 blob：sha256-$hex" >&2; fail=1; continue
    fi
    got="$(sha256_of "$f")"
    if [ "$got" != "$hex" ]; then
      echo "blob sha256 不符：sha256-${hex}（实际 ${got}）" >&2; fail=1; continue
    fi
    echo "blob 校验通过：${hex:0:12}…（$(wc -c <"$f" | tr -d ' ') 字节）"
  done < <(grep -oE 'sha256:[0-9a-f]{64}' "$MANIFEST_DEST" | sed 's/^sha256://' | sort -u)
  [ "$fail" = 0 ] || exit 1
  echo "本地 $MODEL 完整且校验通过。"
  exit 0
fi

# ---------- materialize：从某个源把清单与全部 blob 落盘 ----------
# 参数：<来源标签> <清单URL> <blob URL 模板，%s 为 64 位十六进制 digest>
materialize() {
  local label="$1" manifest_url="$2" blob_fmt="$3"
  local tmp
  tmp="$(mktemp -t ollama-manifest.XXXXXX)"
  # shellcheck disable=SC2064
  trap "rm -f '$tmp'" RETURN

  echo "拉取 $MODEL ← $label"
  curl -fSL --retry 3 --retry-delay 3 --retry-all-errors --connect-timeout 15 \
       -o "$tmp" "$manifest_url" || { echo "  清单下载失败：$manifest_url"; return 1; }

  local got
  got="$(sha256_of "$tmp")"
  if [ -n "$MANIFEST_SHA" ] && [ "$got" != "$MANIFEST_SHA" ]; then
    echo "  清单 sha256 不符（期望 ${MANIFEST_SHA}，实际 ${got}），失败" >&2
    return 1
  fi
  echo "  清单 sha256：$got"

  local digests
  digests="$(grep -oE 'sha256:[0-9a-f]{64}' "$tmp" | sed 's/^sha256://' | sort -u)"
  if [ -z "$digests" ]; then
    echo "  清单里没有解析到任何 blob digest，失败" >&2
    return 1
  fi

  mkdir -p "$BLOB_DIR" "$(dirname "$MANIFEST_DEST")"
  local hex f url
  while read -r hex; do
    [ -n "$hex" ] || continue
    f="$BLOB_DIR/sha256-$hex"
    if [ -f "$f" ] && [ "$(sha256_of "$f")" = "$hex" ]; then
      echo "  blob ${hex:0:12}… 已存在且校验通过（$(wc -c <"$f" | tr -d ' ') 字节）"
      continue
    fi
    rm -f "$f"
    url="$(printf "$blob_fmt" "$hex")"
    echo "  下载 blob ${hex:0:12}… ← $label"
    if ! curl -fSL --retry 3 --retry-delay 3 --retry-all-errors --connect-timeout 15 \
         --speed-limit 1024 --speed-time 60 -o "$f" "$url"; then
      echo "  blob ${hex:0:12}… 下载失败" >&2
      rm -f "$f"
      return 1
    fi
    got="$(sha256_of "$f")"
    if [ "$got" != "$hex" ]; then
      echo "  blob ${hex:0:12}… sha256 不符（实际 ${got}），已删除" >&2
      rm -f "$f"
      return 1
    fi
    echo "  blob ${hex:0:12}… 校验通过（$(wc -c <"$f" | tr -d ' ') 字节）"
  done <<< "$digests"

  cp "$tmp" "$MANIFEST_DEST"
  echo "  清单已写入：$MANIFEST_DEST"
  return 0
}

MIRROR_MANIFEST="$NAS_BASE/manifests/$MANIFEST_REL"
UPSTREAM_MANIFEST="$UPSTREAM/v2/$ns/$name/manifests/$tag"

if probe "$MIRROR_MANIFEST"; then
  if materialize "NAS 镜像（${NAS_BASE}）" "$MIRROR_MANIFEST" "$NAS_BASE/blobs/sha256-%s"; then
    echo "完成：$MODEL 来自 NAS 镜像，全部 blob 校验通过。"
    echo "现在可以直接：ollama run $MODEL"
    exit 0
  fi
  echo "NAS 镜像中途失败，回落到公网 $UPSTREAM" >&2
else
  echo "NAS 镜像上没有 $MODEL 或不可达，回落到公网 $UPSTREAM" >&2
fi

if materialize "公网（${UPSTREAM}）" "$UPSTREAM_MANIFEST" "$UPSTREAM/v2/$ns/$name/blobs/sha256:%s"; then
  echo "完成：$MODEL 来自公网 registry，全部 blob 校验通过。"
  echo "现在可以直接：ollama run $MODEL"
  exit 0
fi

echo "失败：NAS 与公网都没能把 $MODEL 完整拉下来。" >&2
exit 1
