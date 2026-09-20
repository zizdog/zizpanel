#!/usr/bin/env bash
# ============================================================================
#  macsaber-release.sh —— 打包 mac军刀（macsaber）的 darwin/arm64 产物
#
#  做四件事，**默认一步都不上传**：
#    ① cd macsaber && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath
#       -ldflags "-s -w -X main.version=<ver>" -o macsaber .
#    ② 运行产物核对版本（-X 在 -trimpath 下会静默失效，所以必须真跑一次）
#    ③ 打 tar.gz（内含 macsaber + macsaber/README.md）→ dist/macsaber/
#    ④ 打印 sha256 与 manifest.json 片段（贴到镜像站 apps/macsaber/<ver>/）
#
#  用法：
#    bash tools/macsaber-release.sh                     # 默认 0.1.0，只产出到 dist/macsaber/
#    bash tools/macsaber-release.sh --version 0.2.0
#    bash tools/macsaber-release.sh --out /tmp/out
#    bash tools/macsaber-release.sh --host <你的镜像机> --user <用户> --apps-root <apps 目录>
#        加 --host 才会上传（走 ssh/scp，绝不自动触发）
#
#  环境变量：GOPROXY 默认 https://goproxy.cn,direct（本机 proxy.golang.org 不可达）。
#  上传：MACSABER_UPLOAD_DIR 覆盖镜像机上的目标目录（默认 <apps-root>/macsaber/<ver>/）。
#  ⚠️ 仓库里**不留任何内网地址**：host / user / root 一律由调用者提供。
# ============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

VERSION="0.1.0"
OUT_DIR="$REPO_ROOT/dist/macsaber"
MIRROR_HOST=""
MIRROR_USER=""
APPS_ROOT=""
UPLOAD_DIR=""
GO_BIN="${GO_BIN:-go}"

usage() { sed -n '2,26p' "$0"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --version)   VERSION="${2:-}"; shift ;;
    --out)       OUT_DIR="${2:-}"; shift ;;
    --host)      MIRROR_HOST="${2:-}"; shift ;;
    --user)      MIRROR_USER="${2:-}"; shift ;;
    --apps-root) APPS_ROOT="${2:-}"; shift ;;
    --upload-dir) UPLOAD_DIR="${2:-}"; shift ;;
    --go)        GO_BIN="${2:-}"; shift ;;
    -h|--help)   usage; exit 0 ;;
    *) echo "未知参数：$1" >&2; usage; exit 2 ;;
  esac
  shift
done

if [ -z "$VERSION" ]; then
  echo "版本号不能为空（--version <x.y.z>）" >&2
  exit 2
fi

# 与仓库其它构建一致：本机 proxy.golang.org 不可达。
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"

if [ ! -d "$REPO_ROOT/macsaber" ]; then
  echo "找不到 macsaber/ 目录（$REPO_ROOT/macsaber）" >&2
  exit 1
fi
if ! command -v "$GO_BIN" >/dev/null 2>&1; then
  echo "找不到 go（用 --go <路径> 或设 GO_BIN）" >&2
  exit 1
fi

ARCH="$(/usr/bin/uname -m)"
if [ "$ARCH" != "arm64" ]; then
  # 交叉编译本身没问题（CGO_ENABLED=0），但"产物能在这台机器上跑"这条复核做不了。
  echo "⚠️ 本机架构是 ${ARCH}，不是 arm64：产物仍会是 darwin/arm64，但下面的运行复核会被跳过（如实标注）"
fi

ASSET="macsaber_${VERSION}_darwin_arm64.tar.gz"
STAGE="$(/usr/bin/mktemp -d "${TMPDIR:-/tmp}/macsaber-release.XXXXXX")"
trap '/bin/rm -rf "$STAGE"' EXIT

echo "==> 编译 mac军刀 ${VERSION}（CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 -trimpath）"
BIN="$STAGE/macsaber"
(
  cd "$REPO_ROOT/macsaber"
  CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 GOFLAGS=-buildvcs=false \
    "$GO_BIN" build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$BIN" .
)

if [ ! -f "$BIN" ]; then
  echo "编译没有产出 $BIN" >&2
  exit 1
fi

# -X 在 -trimpath 下会**静默失效**（退出码仍是 0），所以必须真跑一次产物核对。
echo "==> 运行产物核对版本"
GOT_VER=""
if [ "$ARCH" = "arm64" ]; then
  GOT_VER="$("$BIN" version)"
  echo "    $GOT_VER"
  case "$GOT_VER" in
    *"$VERSION"*) : ;;
    *) echo "产物自报版本与 --version $VERSION 不一致：$GOT_VER" >&2; exit 1 ;;
  esac
else
  echo "    跳过（本机非 arm64，无法执行 darwin/arm64 产物）—— **未验证**，如实标注"
fi

FILE_DESC="$(/usr/bin/file -b "$BIN" 2>/dev/null || echo 'file(1) 不可用')"
echo "==> 架构复核：$FILE_DESC"
case "$FILE_DESC" in
  *arm64*) : ;;
  *) echo "产物不是 arm64（铁律：不许 amd64、不许 Rosetta）：$FILE_DESC" >&2; exit 1 ;;
esac

# 打包：归档里是平铺的 macsaber + README.md（安装器不剥层、只挑 macsaber 成员）。
echo "==> 打包 $ASSET"
STAGE_PKG="$STAGE/pkg/macsaber"
/bin/mkdir -p "$STAGE_PKG"
/bin/cp "$BIN" "$STAGE_PKG/macsaber"
/bin/cp "$REPO_ROOT/macsaber/README.md" "$STAGE_PKG/README.md"
/bin/chmod 0755 "$STAGE_PKG/macsaber"

/bin/mkdir -p "$OUT_DIR"
OUT="$OUT_DIR/$ASSET"
# 归档必须可复现（同样的源码 → 同样的 sha256）：固定 mtime、不写扩展属性，
# 而且 gzip 头也要 `-n`（否则头里带打包时刻，两次打包哈希不同 —— 实测踩过）。
# macOS 的 bsdtar 不支持 GNU 的 --mtime/--uid/--gid，所以固定时间用 touch 做。
/usr/bin/touch -t 202001010000 "$STAGE_PKG/macsaber" "$STAGE_PKG/README.md"
( cd "$STAGE_PKG" && COPYFILE_DISABLE=1 /usr/bin/tar --no-xattrs -cf - macsaber README.md ) \
  | /usr/bin/gzip -9n > "$OUT"

SHA256="$(/usr/bin/shasum -a 256 "$OUT" | /usr/bin/awk '{print $1}')"
SIZE="$(/usr/bin/stat -f %z "$OUT" 2>/dev/null || /usr/bin/wc -c <"$OUT")"

echo
echo "==> 产物就绪（未上传）"
echo "路径   : $OUT"
echo "大小   : $SIZE B"
echo "sha256 : $SHA256"
echo
echo "==> manifest.json 片段（贴到镜像站 apps/macsaber/$VERSION/manifest.json）"
/usr/bin/printf '{\n  "app": "macsaber",\n  "version": "%s",\n  "assets": [\n    {"name": "%s", "sha256": "%s", "size": %s}\n  ]\n}\n' \
  "$VERSION" "$ASSET" "$SHA256" "$SIZE"
echo
echo "==> 面板侧需要同步的两个值（internal/services/macsaber.go）"
echo "MacSaberVersion        = \"$VERSION\""
echo "MacSaberArchiveSHA256  = \"$SHA256\""
echo "macSaberArchiveSizeHint = $SIZE"

if [ -z "$MIRROR_HOST" ]; then
  echo
  echo "（没有 --host：**只产出到 ${OUT_DIR}，未上传**。要上传请显式传："
  echo "   bash tools/macsaber-release.sh --host <镜像机> --user <用户> --apps-root <apps 目录>）"
  exit 0
fi

# ---- 显式上传（只有传了 --host 才走到这里）----
: "${MIRROR_USER:?上传需要 --user <镜像机用户>}"
: "${APPS_ROOT:?上传需要 --apps-root <镜像机上的 apps 目录>}"
TARGET="${UPLOAD_DIR:-$APPS_ROOT/macsaber/$VERSION}"

echo
echo "==> 上传到 $MIRROR_HOST:${TARGET}（显式请求）"
/usr/bin/ssh "$MIRROR_USER@$MIRROR_HOST" "/bin/mkdir -p '$TARGET'"
/usr/bin/scp "$OUT" "$MIRROR_USER@$MIRROR_HOST:$TARGET/"
/usr/bin/printf '{\n  "app": "macsaber",\n  "version": "%s",\n  "assets": [\n    {"name": "%s", "sha256": "%s", "size": %s}\n  ]\n}\n' \
  "$VERSION" "$ASSET" "$SHA256" "$SIZE" > "$STAGE/manifest.json"
/usr/bin/scp "$STAGE/manifest.json" "$MIRROR_USER@$MIRROR_HOST:$TARGET/manifest.json"

echo "==> 复核镜像站上的字节（HEAD + 实算 sha256 不可行时至少核对大小）"
REMOTE_SIZE="$(/usr/bin/ssh "$MIRROR_USER@$MIRROR_HOST" "/usr/bin/stat -f %z '$TARGET/$ASSET' 2>/dev/null || /usr/bin/stat -c %s '$TARGET/$ASSET'")"
if [ "$REMOTE_SIZE" != "$SIZE" ]; then
  echo "⚠️ 镜像站上的大小（${REMOTE_SIZE}）与本地（${SIZE}）不一致 —— 同步可能被截断" >&2
  exit 1
fi
echo "已上传：$TARGET/${ASSET}（$REMOTE_SIZE B）与 manifest.json"
