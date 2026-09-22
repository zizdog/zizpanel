#!/usr/bin/env bash
# 发布 ZizPanel 到**公网源**（zizdog.com：安装源 + 面板在线升级源）与**你自己的镜像机**，并复验线上产物。
#
# 为什么做成脚本：这套流程每次发版都要跑，而它有两个反复踩过的坑（坑清单 坑 149）：
#   ① 两台主机的**目录布局不同**（公网 `download/<版本>/`、镜像机同布局但另有 latest 软链），
#      而清单里的 url **写死在签名覆盖的字节里** —— 一份清单只能对应一台主机；
#   ② 镜像机的 `download/latest/*` 是**软链**，用 `cp -f` 覆盖会 `not writing through dangling symlink`
#      并在 `set -e` 下把发布断在半路。
# 所以这里把"构建 → 两份清单各签一次 → 分别上传 → 复验"固化，避免每次手工摸索。
#
# 地址由调用者提供，仓库里不留任何内网默认值。
# 用法：
#   bash tools/publish-release.sh build          # 构建 + 生成并签名两份清单（zizdog / 镜像站）
#   ZIZDOG_VPS_PASS='...' bash tools/publish-release.sh push-zizdog
#     # 同时把"镜像版清单"作为 manifest-mirror.json(.sig) 放到源站，供面板镜像同步优先取用（坑 218）
#   NAS_HOST='<你的镜像机>' NAS_USER='<用户>' NAS_ROOT='<镜像目录>' \
#     bash tools/publish-release.sh push-nas    # 走 SSH 密钥
#   bash tools/publish-release.sh verify        # 复验公网：HTTP + sha256 + Ed25519 验签
#
# 口令来源：优先环境变量 ZIZDOG_VPS_PASS，其次 `.panel-credential.local`（gitignored）。
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT" || exit 1

DIST="${DIST:-dist}"
RELDIR="$DIST/release"
RELEASE_KEY="${RELEASE_KEY:-.release-key/zizpanel-ed25519.key}"

ZIZDOG_HOST="${ZIZDOG_HOST:-81.70.248.115}"
ZIZDOG_USER="${ZIZDOG_USER:-root}"
ZIZDOG_ROOT="${ZIZDOG_ROOT:-/www/wwwroot/zizdog.com/zizpanel}"
ZIZDOG_URL="${ZIZDOG_URL:-https://zizdog.com/zizpanel}"

# 地址由调用者提供，仓库里不留任何内网默认值（push-nas 才需要 NAS_HOST/NAS_USER/NAS_ROOT）。
NAS_HOST="${NAS_HOST:-}"
NAS_USER="${NAS_USER:-}"
NAS_ROOT="${NAS_ROOT:-}"
NAS_PUBLIC_URL="${NAS_PUBLIC_URL:-https://mirror.zizdog.com:8888/zizpanel}"

SSH_OPTS=(-o StrictHostKeyChecking=accept-new -o ConnectTimeout=15)

ok()   { printf '  ✓ %s\n' "$*"; }
info() { printf '==> %s\n' "$*"; }
warn() { printf '!! %s\n' "$*" >&2; }
die()  { printf '!! %s\n' "$*" >&2; exit 1; }

version() {
  sed -n 's/^var Version = "\([0-9][0-9.]*\)".*/\1/p' internal/version/version.go | head -1
}

load_pass() {
  if [ -z "${ZIZDOG_VPS_PASS:-}" ] && [ -f .panel-credential.local ]; then
    # shellcheck disable=SC1091
    . ./.panel-credential.local
  fi
  [ -n "${ZIZDOG_VPS_PASS:-}" ] || die "缺少 ZIZDOG_VPS_PASS（放环境变量或 .panel-credential.local）"
}

# sshpass 需要显式给口令：非交互 SSH 里没有别的办法把口令送进去。
zizdog_ssh() {
  sshpass -p "$ZIZDOG_VPS_PASS" ssh "${SSH_OPTS[@]}" "$ZIZDOG_USER@$ZIZDOG_HOST" "$@"
}

zizdog_scp() {
  sshpass -p "$ZIZDOG_VPS_PASS" scp "${SSH_OPTS[@]}" "$@"
}

# verify_sig <manifest> <sig> <pubkey-hex>：用临时目录里的 stdlib 小程序验签。
# 不放进仓库（它只在发布时用），也不用第三方库（避免装依赖）。
verify_sig() {
  local manifest="$1" sig="$2" pub="$3" dir
  dir="$(mktemp -d)"
  cat >"$dir/main.go" <<'GO'
package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
)

func main() {
	d, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Println("读清单失败:", err)
		os.Exit(1)
	}
	s, err := os.ReadFile(os.Args[2])
	if err != nil {
		fmt.Println("读签名失败:", err)
		os.Exit(1)
	}
	p, err := hex.DecodeString(os.Args[3])
	if err != nil || len(p) != ed25519.PublicKeySize || len(s) != ed25519.SignatureSize {
		fmt.Println("长度不对（公钥或签名格式错）")
		os.Exit(1)
	}
	if !ed25519.Verify(ed25519.PublicKey(p), d, s) {
		fmt.Println("验签失败")
		os.Exit(1)
	}
	fmt.Printf("验签通过（清单 %d 字节）\n", len(d))
}
GO
  ( cd "$dir" && go run main.go "$manifest" "$sig" "$pub" )
  local rc=$?
  rm -rf "$dir"
  return $rc
}

# pubkey_hash <manifest-url> <sig-url>：从线上拉清单，按内嵌/密钥公钥验签。
remote_pubkey() {
  go build -trimpath -o "$DIST/host-zizpanel" ./cmd/zizpanel || die "构建 host-zizpanel 失败"
  "$DIST/host-zizpanel" sign-manifest --pub-from-key "$RELEASE_KEY" || die "从私钥推导公钥失败"
}

cmd_build() {
  local v; v="$(version)"; [ -n "$v" ] || die "读不到版本号"

  # 发布前必须看清"树里还有谁的没写完的东西"（坑 177：2026-09-18 另一个子代理正在写
  # 备份功能时，我用 `git add -A` 把它的半成品一起提交进了发布提交 —— 虽然产物核对证明
  # 二进制里没有它们，但提交历史被污染，只能 reset 重做）。
  # 这里只**列出并要求显式确认**，不自动改任何东西：发布是人的决定，脚本只负责别让人瞎着眼过。
  local dirty
  dirty="$(git status --porcelain -- '*.go' '*.js' '*.mjs' '*.sh' '*.css' '*.html' '*.py' 2>/dev/null || true)"
  if [ -n "$dirty" ]; then
    warn "工作区里有未提交/未跟踪的源码文件（下面这些**都会**被打进这次发布）："
    printf '%s\n' "$dirty" | sed 's/^/      /'
    if [ "${ZP_ALLOW_DIRTY_PUBLISH:-0}" != "1" ]; then
      die "拒绝在「半成品可能混入」的状态下发布：确认上面这些文件都是你要发的，再设 ZP_ALLOW_DIRTY_PUBLISH=1 重跑"
    fi
    warn "已按 ZP_ALLOW_DIRTY_PUBLISH=1 继续（请确认上面清单里没有别人的半成品）"
  fi
  info "构建 ${v}（公网清单 url = $ZIZDOG_URL/download/${v}/<name>）"
  make release RELEASE_BASE_URL="$ZIZDOG_URL/download/{version}/{name}" || die "make release 失败"
  cp "$RELDIR/manifest.json" "$RELDIR/manifest-zizdog.json"
  cp "$RELDIR/manifest.json.sig" "$RELDIR/manifest-zizdog.json.sig"
  ok "公网清单已存为 manifest-zizdog.json（签名随之留存，避免被 NAS 版覆盖 —— 坑 149）"

  info "生成公网镜像版清单（url 用 download/<版本>/ 布局）"
  make mirror-public || die "make mirror-public 失败"
  cp "$RELDIR/manifest.json" "$RELDIR/manifest-nas.json"
  cp "$RELDIR/manifest.json.sig" "$RELDIR/manifest-nas.json.sig"
  ls -lh "$RELDIR" | sed -n '1,20p'
}

cmd_push_zizdog() {
  load_pass
  command -v sshpass >/dev/null 2>&1 || die "需要 sshpass（brew install hudochenkov/sshpass/sshpass）"
  local v; v="$(version)"
  [ -f "$RELDIR/manifest-zizdog.json" ] || die "先跑 build（缺 manifest-zizdog.json）"
  [ -f "$RELDIR/manifest-nas.json" ] || die "先跑 build（缺 manifest-nas.json）"

  info "上传发布件到 $ZIZDOG_USER@$ZIZDOG_HOST:$ZIZDOG_ROOT"
  # 公网清单必须**重新命名回 manifest.json** 再上传：面板只认这个名字。
  cp "$RELDIR/manifest-zizdog.json" "$RELDIR/manifest.json"
  cp "$RELDIR/manifest-zizdog.json.sig" "$RELDIR/manifest.json.sig"
  # 镜像版清单也放源上同目录：面板镜像同步优先取它，否则会写入指向源站的清单（坑 218）。
  cp "$RELDIR/manifest-nas.json" "$RELDIR/manifest-mirror.json"
  cp "$RELDIR/manifest-nas.json.sig" "$RELDIR/manifest-mirror.json.sig"
  # 站点**根目录只放脚本与清单**：包一律进 download/<版本>/ 与 download/latest/
  # （install.sh 与清单里的 url 都是 download/ 布局；根目录再放一份是 1.7.x 之前的旧布局，
  #   2026-09-22 用户点名清理，别再放回去）。
  zizdog_scp \
    "$RELDIR/manifest.json" "$RELDIR/manifest.json.sig" \
    "$RELDIR/manifest-mirror.json" "$RELDIR/manifest-mirror.json.sig" \
    install.sh uninstall.sh \
    "$ZIZDOG_USER@$ZIZDOG_HOST:$ZIZDOG_ROOT/" || die "scp 上传失败"

  info "上传包到 download/$v/ 与 download/latest/"
  zizdog_ssh "set -e; cd '$ZIZDOG_ROOT'; mkdir -p download/$v download/latest" || die "远端建目录失败"
  zizdog_scp \
    "$RELDIR/zizpanel_${v}_darwin_arm64.tar.gz" \
    "$RELDIR/zizpanel_${v}_darwin_amd64.tar.gz" \
    "$ZIZDOG_USER@$ZIZDOG_HOST:$ZIZDOG_ROOT/download/$v/" || die "scp 上传 $v 失败"
  zizdog_scp \
    "$RELDIR/zizpanel_latest_darwin_arm64.tar.gz" \
    "$RELDIR/zizpanel_latest_darwin_amd64.tar.gz" \
    "$ZIZDOG_USER@$ZIZDOG_HOST:$ZIZDOG_ROOT/download/latest/" || die "scp 上传 latest 失败"
  zizdog_ssh "set -e; cd '$ZIZDOG_ROOT'; \
    chmod 644 manifest.json manifest.json.sig manifest-mirror.json manifest-mirror.json.sig download/$v/* download/latest/*; \
    chmod 755 install.sh uninstall.sh; \
    chown -R www:www . 2>/dev/null || true; \
    ls -l manifest.json manifest-mirror.json install.sh download/$v download/latest" || die "远端布局/权限失败"
  ok "已推送（根目录只有脚本+清单，包在 download/$v/ 与 download/latest/；镜像版清单 = manifest-mirror.json）"
}

cmd_push_nas() {
  local v; v="$(version)"
  [ -f "$RELDIR/manifest-nas.json" ] || die "先跑 build（缺 manifest-nas.json）"
  : "${NAS_HOST:?请传 NAS_HOST=<你自己的镜像机>}"
  : "${NAS_USER:?请传 NAS_USER=<镜像机用户>}"
  : "${NAS_ROOT:?请传 NAS_ROOT=<镜像上的 zizpanel 目录>}"
  cp "$RELDIR/manifest-nas.json" "$RELDIR/manifest.json"
  cp "$RELDIR/manifest-nas.json.sig" "$RELDIR/manifest.json.sig"

  info "同步到镜像机 $NAS_USER@$NAS_HOST:$NAS_ROOT"
  rsync -az --no-perms --no-owner --no-group -e "ssh ${SSH_OPTS[*]}" \
    "$RELDIR/zizpanel_${v}_darwin_arm64.tar.gz" \
    "$RELDIR/zizpanel_${v}_darwin_amd64.tar.gz" \
    "$RELDIR/zizpanel_latest_darwin_arm64.tar.gz" \
    "$RELDIR/zizpanel_latest_darwin_amd64.tar.gz" \
    "$RELDIR/manifest.json" "$RELDIR/manifest.json.sig" \
    install.sh uninstall.sh \
    "$NAS_USER@$NAS_HOST:$NAS_ROOT/" || die "rsync 到 NAS 失败"

  # latest 用软链（NAS 的既有约定），**不要** cp -f 覆盖软链：坑 149。
  ssh "${SSH_OPTS[@]}" "$NAS_USER@$NAS_HOST" \
    "set -e; cd '$NAS_ROOT'; \
     mkdir -p download/$v download/latest; \
     cp -f zizpanel_${v}_darwin_arm64.tar.gz zizpanel_${v}_darwin_amd64.tar.gz download/$v/; \
     cp -f zizpanel_latest_darwin_arm64.tar.gz zizpanel_latest_darwin_amd64.tar.gz download/latest/; \
     ln -sfn download/$v/zizpanel_${v}_darwin_arm64.tar.gz zizpanel_${v}_darwin_arm64.tar.gz; \
     ln -sfn download/$v/zizpanel_${v}_darwin_amd64.tar.gz zizpanel_${v}_darwin_amd64.tar.gz; \
     ls -l download/$v" || die "NAS 布局失败"
  ok "已同步（公网镜像版清单 url 指向 $NAS_PUBLIC_URL/download/$v/）"
}

# cmd_verify：三层复验 —— HTTP 可达、包 sha256 与清单一致、清单签名能用发布公钥验过。
# 只看 HTTP 200 是不够的（历史上"latest 指向已删版本"就是 404 而清单还在）。
cmd_verify() {
  local v; v="$(version)"
  local tmp; tmp="$(mktemp -d)"
  info "复验公网 ${ZIZDOG_URL}（版本 ${v}）"
  curl -fsS --max-time 30 "$ZIZDOG_URL/manifest.json" -o "$tmp/manifest.json" || die "公网清单下载失败"
  curl -fsS --max-time 30 "$ZIZDOG_URL/manifest.json.sig" -o "$tmp/manifest.json.sig" || die "公网签名下载失败"
  local pub; pub="$(remote_pubkey)" || die "取公钥失败"
  verify_sig "$tmp/manifest.json" "$tmp/manifest.json.sig" "$pub" || die "公网清单验签失败"
  grep -q "\"version\": \"$v\"" "$tmp/manifest.json" || die "公网清单版本不是 ${v}：$(head -c 200 "$tmp/manifest.json")"

  # 镜像版清单：面板镜像同步优先取它；地址若还指向源站，镜像就白建了（坑 218）。
  info "复验源上的镜像版清单 manifest-mirror.json"
  curl -fsS --max-time 30 "$ZIZDOG_URL/manifest-mirror.json" -o "$tmp/manifest-mirror.json" || die "镜像版清单下载失败（push-zizdog 负责上传）"
  curl -fsS --max-time 30 "$ZIZDOG_URL/manifest-mirror.json.sig" -o "$tmp/manifest-mirror.json.sig" || die "镜像版清单签名下载失败"
  verify_sig "$tmp/manifest-mirror.json" "$tmp/manifest-mirror.json.sig" "$pub" || die "镜像版清单验签失败"
  grep -q "\"version\": \"$v\"" "$tmp/manifest-mirror.json" || die "镜像版清单版本不是 ${v}"
  if grep -qF "$ZIZDOG_URL" "$tmp/manifest-mirror.json"; then
    die "镜像版清单里的下载地址仍指向源站 ${ZIZDOG_URL}：镜像不会参与分发"
  fi
  ok "镜像版清单可用（签名有效、版本 ${v}、地址不指向源站）"

  local arch file url want got
  for arch in arm64 amd64; do
    file="zizpanel_${v}_darwin_${arch}.tar.gz"
    url="$ZIZDOG_URL/download/$v/$file"
    want="$(python3 - "$tmp/manifest.json" "$arch" <<'PY'
import json,sys
d=json.load(open(sys.argv[1]))
print(d["assets"]["darwin_"+sys.argv[2]]["sha256"])
PY
)"
    curl -fsS --max-time 600 "$url" -o "$tmp/$file" || die "下载失败：$url"
    got="$(shasum -a 256 "$tmp/$file" | cut -d' ' -f1)"
    [ "$want" = "$got" ] || die "$file sha256 与清单不一致：清单 $want / 实际 $got"
    ok "$file sha256 一致（$(stat -f%z "$tmp/$file") 字节）"
  done
  curl -fsS --max-time 30 "$ZIZDOG_URL/install.sh" -o "$tmp/install.sh" || die "install.sh 下载失败"
  grep -q "SCRIPT_VERSION=\"$v\"" "$tmp/install.sh" || die "线上 install.sh 的 SCRIPT_VERSION 不是 $v"
  ok "install.sh 版本号一致（${v}）"
  rm -rf "$tmp"
  info "公网复验全部通过 ✅"
}

case "${1:-}" in
  build)       cmd_build ;;
  push-zizdog) cmd_push_zizdog ;;
  push-nas)    cmd_push_nas ;;
  verify)      cmd_verify ;;
  *) sed -n '2,20p' "$0"; exit 1 ;;
esac
